// Command indexroom is the 链上索引与交易分析服务 entry point.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

func main() {
	command := "demo"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	switch command {
	case "demo":
		runDemo()
	case "version":
		fmt.Println("indexroom 0.1.0")
	case "register":
		os.Exit(runRegister())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println("usage: indexroom [demo|version|register|help]")
	fmt.Println()
	fmt.Println("register reads requests as JSON from standard input and maintains an")
	fmt.Println("in-memory service instance registry with offline health observations:")
	fmt.Println(`  {"requests":[{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"host:8080"}]},`)
	fmt.Println(`              {"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},`)
	fmt.Println(`              {"type":"select","service":"svc","expectedRevision":1}]}`)
	fmt.Println("A register request fully replaces the service's instance list; new services")
	fmt.Println("require expectedRevision 0, existing services the current revision. A health")
	fmt.Println("request records an offline observation (healthy/unhealthy) for one instance;")
	fmt.Println("it never probes. Health updates never bump the registration revision. A")
	fmt.Println("select request chooses one healthy instance, rotating per service by ascending")
	fmt.Println("instance id; it performs no network access and never changes revisions or")
	fmt.Println("health records, and reports invalid/conflict/not_found/no_healthy on failure.")
	fmt.Println("Exit status is 0 only when every request succeeds.")
}

// registerInstance is one instance in the final service list.
type registerInstance struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Health   string `json:"health"`
	Sequence int64  `json:"sequence"`
	Reason   string `json:"reason,omitempty"`
}

// registerService is one service in the final service list.
type registerService struct {
	Service   string             `json:"service"`
	Revision  int                `json:"revision"`
	Instances []registerInstance `json:"instances"`
}

// registerResult is the per-item outcome.
type registerResult struct {
	Service          string `json:"service"`
	OK               bool   `json:"ok"`
	Changed          bool   `json:"changed,omitempty"`
	Revision         int    `json:"revision"`
	Error            string `json:"error,omitempty"`
	Reason           string `json:"reason,omitempty"`
	ExpectedRevision int    `json:"expectedRevision,omitempty"`
	ActualRevision   int    `json:"actualRevision,omitempty"`
	Sequence         int64  `json:"sequence,omitempty"`
	InstanceID       string `json:"instanceId,omitempty"`
	Address          string `json:"address,omitempty"`
}

// registerOutput is the full register output.
type registerOutput struct {
	Results  []registerResult  `json:"results"`
	Services []registerService `json:"services"`
}

// resultRecorder accumulates per-request results in input order and tracks
// whether any request failed.
type resultRecorder struct {
	results   []registerResult
	anyFailed bool
}

// success appends a successful result.
func (rec *resultRecorder) success(result registerResult) {
	result.OK = true
	rec.results = append(rec.results, result)
}

// failure appends a failed result and marks the run as failed.
func (rec *resultRecorder) failure(result registerResult) {
	rec.anyFailed = true
	rec.results = append(rec.results, result)
}

// invalid records an invalid-input failure for service at its current
// revision in the registry.
func (rec *resultRecorder) invalid(registry *indexroom.Registry, service, reason string) {
	rec.failure(registerResult{
		Service:  service,
		Error:    "invalid",
		Reason:   reason,
		Revision: registry.RevisionOf(service),
	})
}

// rejection builds the failure result for a business-rule rejection: the
// outcome's kind and reason, plus the expected/actual revisions for
// conflicts and the revision current when the item was processed.
func rejection(service string, kind indexroom.OutcomeKind, reason string, expected, actual, revision int) registerResult {
	return registerResult{
		Service:          service,
		Error:            string(kind),
		Reason:           reason,
		ExpectedRevision: expected,
		ActualRevision:   actual,
		Revision:         revision,
	}
}

// runRegister processes registration requests from standard input.
// It returns the process exit code: 0 when all registrations succeed, 1 otherwise.
func runRegister() int {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return failRegister(fmt.Sprintf("could not read standard input: %v", err))
	}

	var top struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return failRegister(fmt.Sprintf("input must be a JSON object with a requests array: %v", err))
	}
	if len(top.Requests) == 0 || strings.TrimSpace(string(top.Requests)) == "null" {
		return failRegister("input must contain a \"requests\" array")
	}
	var rawRequests []json.RawMessage
	if err := json.Unmarshal(top.Requests, &rawRequests); err != nil {
		return failRegister("\"requests\" must be a JSON array")
	}

	registry := indexroom.NewRegistry()
	rec := &resultRecorder{results: make([]registerResult, 0, len(rawRequests))}

	for _, raw := range rawRequests {
		var head struct {
			Service string          `json:"service"`
			Type    json.RawMessage `json:"type"`
		}
		_ = json.Unmarshal(raw, &head)
		service := strings.TrimSpace(head.Service)

		reqType := "register"
		if len(head.Type) > 0 && strings.TrimSpace(string(head.Type)) != "null" {
			var t string
			if err := json.Unmarshal(head.Type, &t); err != nil {
				rec.invalid(registry, service, "type must be a string")
				continue
			}
			reqType = t
		}

		switch reqType {
		case "", "register":
			runRegisterRequest(raw, service, registry, rec)
		case "health":
			runHealthRequest(raw, service, registry, rec)
		case "select":
			runSelectRequest(raw, service, registry, rec)
		default:
			rec.invalid(registry, service, fmt.Sprintf("unknown type %q", reqType))
		}
	}

	services := make([]registerService, 0)
	for _, view := range registry.Snapshot() {
		insts := make([]registerInstance, 0, len(view.Instances))
		for _, inst := range view.Instances {
			insts = append(insts, registerInstance{
				ID:       inst.ID,
				Address:  inst.Address,
				Health:   string(inst.Health),
				Sequence: inst.Sequence,
				Reason:   inst.Reason,
			})
		}
		services = append(services, registerService{
			Service:   view.Service,
			Revision:  view.Revision,
			Instances: insts,
		})
	}

	writeRegisterOutput(registerOutput{Results: rec.results, Services: services})
	if rec.anyFailed {
		return 1
	}
	return 0
}

// runRegisterRequest processes one register request, appending its outcome.
func runRegisterRequest(raw json.RawMessage, service string, registry *indexroom.Registry, rec *resultRecorder) {
	var req struct {
		Service   string          `json:"service"`
		Revision  *int64          `json:"expectedRevision"`
		Instances json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		rec.invalid(registry, service, fmt.Sprintf("request must be an object with service, expectedRevision and instances: %v", err))
		return
	}
	service = strings.TrimSpace(req.Service)

	if req.Revision == nil {
		rec.invalid(registry, service, "expectedRevision is required and must be a non-negative integer")
		return
	}
	if !isJSONArray(req.Instances) {
		rec.invalid(registry, service, "instances must be an array")
		return
	}
	var instances []indexroom.Instance
	if err := json.Unmarshal(req.Instances, &instances); err != nil {
		rec.invalid(registry, service, fmt.Sprintf("instances must be an array of objects with id and address: %v", err))
		return
	}

	registration, err := registry.ValidateRegistration(service, int(*req.Revision), instances)
	if err != nil {
		rec.invalid(registry, service, err.Error())
		return
	}

	outcome := registry.Apply(registration)
	if !outcome.OK {
		rec.failure(rejection(outcome.Service, outcome.Kind, outcome.Reason, outcome.Expected, outcome.Actual, outcome.Revision))
		return
	}
	rec.success(registerResult{
		Service:  outcome.Service,
		Changed:  outcome.Changed,
		Revision: outcome.Revision,
	})
}

// runHealthRequest processes one health observation request, appending its outcome.
func runHealthRequest(raw json.RawMessage, service string, registry *indexroom.Registry, rec *resultRecorder) {
	var req struct {
		Service    string  `json:"service"`
		InstanceID string  `json:"instanceId"`
		Revision   *int64  `json:"expectedRevision"`
		Sequence   *int64  `json:"sequence"`
		Healthy    *bool   `json:"healthy"`
		Reason     *string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		rec.invalid(registry, service, fmt.Sprintf("health request must be an object with service, instanceId, expectedRevision, sequence and healthy: %v", err))
		return
	}
	service = strings.TrimSpace(req.Service)

	if req.Revision == nil {
		rec.invalid(registry, service, "expectedRevision is required and must be a non-negative integer")
		return
	}
	if req.Sequence == nil {
		rec.invalid(registry, service, "sequence is required and must be a positive integer")
		return
	}
	if req.Healthy == nil {
		rec.invalid(registry, service, "healthy is required and must be a boolean")
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}

	update, err := registry.ValidateHealth(service, req.InstanceID, int(*req.Revision), *req.Sequence, *req.Healthy, reason)
	if err != nil {
		rec.invalid(registry, service, err.Error())
		return
	}

	outcome := registry.ApplyHealth(update)
	if !outcome.OK {
		result := rejection(outcome.Service, outcome.Kind, outcome.Reason, outcome.Expected, outcome.Actual, outcome.Revision)
		result.Sequence = outcome.Sequence
		rec.failure(result)
		return
	}
	rec.success(registerResult{
		Service:  outcome.Service,
		Changed:  outcome.Changed,
		Revision: outcome.Revision,
		Sequence: outcome.Sequence,
	})
}

// runSelectRequest processes one select request, appending its outcome.
// A selection performs no network access and never alters registrations or
// health records; it only advances the service's healthy-instance rotation.
func runSelectRequest(raw json.RawMessage, service string, registry *indexroom.Registry, rec *resultRecorder) {
	var req struct {
		Service  string `json:"service"`
		Revision *int64 `json:"expectedRevision"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		rec.invalid(registry, service, fmt.Sprintf("select request must be an object with service and expectedRevision: %v", err))
		return
	}
	service = strings.TrimSpace(req.Service)

	if req.Revision == nil {
		rec.invalid(registry, service, "expectedRevision is required and must be a non-negative integer")
		return
	}

	selection, err := registry.ValidateSelection(service, int(*req.Revision))
	if err != nil {
		rec.invalid(registry, service, err.Error())
		return
	}

	outcome := registry.Select(selection)
	if !outcome.OK {
		rec.failure(rejection(outcome.Service, outcome.Kind, outcome.Reason, outcome.Expected, outcome.Actual, outcome.Revision))
		return
	}
	rec.success(registerResult{
		Service:    outcome.Service,
		Revision:   outcome.Revision,
		InstanceID: outcome.InstanceID,
		Address:    outcome.Address,
		Sequence:   outcome.Sequence,
	})
}

// failRegister emits a top-level input error and returns exit code 1.
func failRegister(reason string) int {
	writeRegisterOutput(struct {
		Error string `json:"error"`
	}{Error: reason})
	return 1
}

func writeRegisterOutput(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// isJSONArray reports whether raw is a JSON array (ignoring leading whitespace).
func isJSONArray(raw json.RawMessage) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

func runDemo() {
	index := indexroom.New()
	blocks := []indexroom.Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t3"}},
	}
	for _, block := range blocks {
		if err := index.Append(block); err != nil {
			fmt.Printf("append refused at height %d: %v\n", block.Height, err)
			continue
		}
		fmt.Printf("indexed height=%d txs=%d\n", block.Height, len(block.Txs))
	}
	if err := index.Append(indexroom.Block{Height: 3, Hash: "h3", Parent: "h1"}); err != nil {
		fmt.Println("non-linear block refused:", err)
	}
	dropped, err := index.Reorg([]indexroom.Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"t4", "t5"}}})
	fmt.Printf("reorg dropped=%v err=%v tip=%d\n", dropped, err, index.Tip)
}
