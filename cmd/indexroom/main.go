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
		// register reports result-delivery failures via its exit status and
		// stderr; a broken standard output must surface as an EPIPE write error,
		// not as the default SIGPIPE termination.
		ignoreSigpipe()
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
	fmt.Println("A select request may carry an optional \"sessionKey\" string: requests in the")
	fmt.Println("same service with the same trimmed key reuse the instance first chosen for")
	fmt.Println("that key while it stays registered and healthy, without moving the rotation.")
	fmt.Println("Exit status is 0 only when every request succeeds and the JSON result")
	fmt.Println("is fully written to standard output. A failed result write is reported")
	fmt.Println("on standard error with the underlying write error and also exits 1,")
	fmt.Println("without appending a second JSON document to the partial output.")
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

// runRegister processes registration requests from the process standard
// streams. It returns the process exit code: 0 when all registrations succeed
// and the result is fully written, 1 otherwise.
func runRegister() int {
	return runRegisterIO(os.Stdin, os.Stdout, os.Stderr)
}

// runRegisterIO is runRegister with injectable streams. in supplies the JSON
// request batch, out receives the single JSON result document and errOut
// receives diagnostics (in particular a result-delivery failure, which must
// never be mixed into out).
func runRegisterIO(in io.Reader, out io.Writer, errOut io.Writer) int {
	data, err := io.ReadAll(in)
	if err != nil {
		return failRegister(out, errOut, fmt.Sprintf("could not read standard input: %v", err))
	}

	var top struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return failRegister(out, errOut, fmt.Sprintf("input must be a JSON object with a requests array: %v", err))
	}
	if len(top.Requests) == 0 || strings.TrimSpace(string(top.Requests)) == "null" {
		return failRegister(out, errOut, "input must contain a \"requests\" array")
	}
	var rawRequests []json.RawMessage
	if err := json.Unmarshal(top.Requests, &rawRequests); err != nil {
		return failRegister(out, errOut, "\"requests\" must be a JSON array")
	}

	proc := &registerProcessor{
		registry: indexroom.NewRegistry(),
		results:  make([]registerResult, 0, len(rawRequests)),
	}

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
				proc.invalid(service, "type must be a string")
				continue
			}
			reqType = t
		}

		switch reqType {
		case "", "register":
			proc.registerRequest(raw, service)
		case "health":
			proc.healthRequest(raw, service)
		case "select":
			proc.selectRequest(raw, service)
		default:
			proc.invalidf(service, "unknown type %q", reqType)
		}
	}

	results, anyFailed := proc.results, proc.failed
	registry := proc.registry

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

	if err := writeRegisterOutput(out, registerOutput{Results: results, Services: services}); err != nil {
		// The request results could not be delivered. This is independent of
		// whether the requests themselves succeeded: the caller must be able to
		// tell an undelivered result from a rejected request.
		reportRegisterDeliveryFailure(errOut, err)
		return 1
	}
	if anyFailed {
		return 1
	}
	return 0
}

// registerProcessor runs the request batch in input order. Failed items keep
// their slot in results and set failed; later items still run against the
// state committed by earlier successful items.
type registerProcessor struct {
	registry *indexroom.Registry
	results  []registerResult
	failed   bool
}

// invalid records an input-validation failure for an item. Content validity is
// checked before any business rule, so the revision stamped here is the
// service's current revision when the item is handled.
func (p *registerProcessor) invalid(service, reason string) {
	p.failed = true
	p.results = append(p.results, registerResult{
		Service:  service,
		OK:       false,
		Error:    "invalid",
		Reason:   reason,
		Revision: p.registry.RevisionOf(service),
	})
}

// invalidf is invalid with a formatted reason.
func (p *registerProcessor) invalidf(service, format string, args ...any) {
	p.invalid(service, fmt.Sprintf(format, args...))
}

// parseRevision decodes the raw expectedRevision token for one request. An
// absent or explicitly null token is "required"; a token that is not a decimal
// integer is an integer-type invalid. A decimal integer is returned as int64 —
// still carrying the submitted value — so the registry can range-check it
// against the architecture (0..2147483647 on 32-bit, 0..9223372036854775807
// on 64-bit) before it is narrowed to int; narrowing first would wrap values
// like 4294967297 into 1 on a 32-bit build. It reports false after appending
// the item's invalid result, which stamps the service's current revision.
func (p *registerProcessor) parseRevision(service string, raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		p.invalid(service, "expectedRevision is required and must be a non-negative integer")
		return 0, false
	}
	revision, err := indexroom.ParseExpectedRevision(string(raw))
	if err != nil {
		p.invalid(service, err.Error())
		return 0, false
	}
	return revision, true
}

// reject records a business-rule failure (conflict, not_found, stale or
// no_healthy), keeping each operation's distinct result fields. Health
// rejections additionally carry the current sequence; register and select
// pass 0 so it stays omitted.
func (p *registerProcessor) reject(service string, kind indexroom.OutcomeKind, reason string, revision, expected, actual int, sequence int64) {
	p.failed = true
	p.results = append(p.results, registerResult{
		Service:          service,
		OK:               false,
		Error:            string(kind),
		Reason:           reason,
		Revision:         revision,
		ExpectedRevision: expected,
		ActualRevision:   actual,
		Sequence:         sequence,
	})
}

// succeed records an accepted item; omitempty keeps zero-valued fields out of
// the output exactly as before the refactor.
func (p *registerProcessor) succeed(result registerResult) {
	result.OK = true
	p.results = append(p.results, result)
}

// registerRequest processes one register request, appending its outcome.
func (p *registerProcessor) registerRequest(raw json.RawMessage, service string) {
	var req struct {
		Service   string          `json:"service"`
		Revision  json.RawMessage `json:"expectedRevision"`
		Instances json.RawMessage `json:"instances"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		p.invalidf(service, "request must be an object with service, expectedRevision and instances: %v", err)
		return
	}
	service = strings.TrimSpace(req.Service)

	if len(req.Revision) == 0 || strings.TrimSpace(string(req.Revision)) == "null" {
		p.invalid(service, "expectedRevision is required and must be a non-negative integer")
		return
	}
	if !isJSONArray(req.Instances) {
		p.invalid(service, "instances must be an array")
		return
	}
	var instances []indexroom.Instance
	if err := json.Unmarshal(req.Instances, &instances); err != nil {
		p.invalidf(service, "instances must be an array of objects with id and address: %v", err)
		return
	}

	// Parse the raw token (integer type); ValidateRegistration then range-checks
	// the int64 against the architecture before narrowing it to int.
	revision, ok := p.parseRevision(service, req.Revision)
	if !ok {
		return
	}
	registration, err := p.registry.ValidateRegistration(service, revision, instances)
	if err != nil {
		p.invalid(service, err.Error())
		return
	}

	outcome := p.registry.Apply(registration)
	if !outcome.OK {
		p.reject(outcome.Service, outcome.Kind, outcome.Reason, outcome.Revision, outcome.Expected, outcome.Actual, 0)
		return
	}
	p.succeed(registerResult{
		Service:  outcome.Service,
		Changed:  outcome.Changed,
		Revision: outcome.Revision,
	})
}

// healthRequest processes one health observation request, appending its outcome.
func (p *registerProcessor) healthRequest(raw json.RawMessage, service string) {
	var req struct {
		Service    string          `json:"service"`
		InstanceID string          `json:"instanceId"`
		Revision   json.RawMessage `json:"expectedRevision"`
		Sequence   *int64          `json:"sequence"`
		Healthy    *bool           `json:"healthy"`
		Reason     *string         `json:"reason"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		p.invalidf(service, "health request must be an object with service, instanceId, expectedRevision, sequence and healthy: %v", err)
		return
	}
	service = strings.TrimSpace(req.Service)

	if len(req.Revision) == 0 || strings.TrimSpace(string(req.Revision)) == "null" {
		p.invalid(service, "expectedRevision is required and must be a non-negative integer")
		return
	}
	if req.Sequence == nil {
		p.invalid(service, "sequence is required and must be a positive integer")
		return
	}
	if req.Healthy == nil {
		p.invalid(service, "healthy is required and must be a boolean")
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}

	// Parse the raw token (integer type); ValidateHealth then range-checks the
	// int64 against the architecture before narrowing it to int.
	revision, ok := p.parseRevision(service, req.Revision)
	if !ok {
		return
	}
	update, err := p.registry.ValidateHealth(service, req.InstanceID, revision, *req.Sequence, *req.Healthy, reason)
	if err != nil {
		p.invalid(service, err.Error())
		return
	}

	outcome := p.registry.ApplyHealth(update)
	if !outcome.OK {
		p.reject(outcome.Service, outcome.Kind, outcome.Reason, outcome.Revision, outcome.Expected, outcome.Actual, outcome.Sequence)
		return
	}
	p.succeed(registerResult{
		Service:  outcome.Service,
		Changed:  outcome.Changed,
		Revision: outcome.Revision,
		Sequence: outcome.Sequence,
	})
}

// selectRequest processes one select request, appending its outcome.
// A selection performs no network access and never alters registrations or
// health records; it only advances the service's healthy-instance rotation
// and, for requests carrying a sessionKey, maintains that session's binding.
func (p *registerProcessor) selectRequest(raw json.RawMessage, service string) {
	var req struct {
		Service  string          `json:"service"`
		Revision json.RawMessage `json:"expectedRevision"`
		Session  json.RawMessage `json:"sessionKey"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		p.invalidf(service, "select request must be an object with service and expectedRevision: %v", err)
		return
	}
	service = strings.TrimSpace(req.Service)

	if len(req.Revision) == 0 || strings.TrimSpace(string(req.Revision)) == "null" {
		p.invalid(service, "expectedRevision is required and must be a non-negative integer")
		return
	}

	// sessionKey is optional: absent means an ordinary rotating selection. An
	// explicitly provided null, a non-string value or a blank string is
	// invalid; the registry trims the key and treats keys equal after trimming
	// as the same session.
	var sessionKey *string
	if len(req.Session) > 0 {
		if strings.TrimSpace(string(req.Session)) == "null" {
			p.invalid(service, "sessionKey must be a string, not null")
			return
		}
		var key string
		if err := json.Unmarshal(req.Session, &key); err != nil {
			p.invalid(service, "sessionKey must be a string")
			return
		}
		sessionKey = &key
	}

	revision, ok := p.parseRevision(service, req.Revision)
	if !ok {
		return
	}

	selection, err := p.registry.ValidateSelectionWithSession(service, revision, sessionKey)
	if err != nil {
		p.invalid(service, err.Error())
		return
	}

	outcome := p.registry.Select(selection)
	if !outcome.OK {
		p.reject(outcome.Service, outcome.Kind, outcome.Reason, outcome.Revision, outcome.Expected, outcome.Actual, 0)
		return
	}
	p.succeed(registerResult{
		Service:    outcome.Service,
		Revision:   outcome.Revision,
		InstanceID: outcome.InstanceID,
		Address:    outcome.Address,
		Sequence:   outcome.Sequence,
	})
}

// failRegister emits a top-level input error as the single JSON document on
// out and returns exit code 1. If that error JSON itself cannot be written,
// the delivery failure is diagnosed on errOut (out may already hold a partial
// document, so no second JSON is appended) and the exit code stays 1.
func failRegister(out io.Writer, errOut io.Writer, reason string) int {
	if err := writeRegisterOutput(out, struct {
		Error string `json:"error"`
	}{Error: reason}); err != nil {
		reportRegisterDeliveryFailure(errOut, err)
	}
	return 1
}

// reportRegisterDeliveryFailure reports on errOut that the register command's
// JSON result could not be delivered, preserving the underlying write error.
// It never writes to standard output, which may already contain a partial
// result.
func reportRegisterDeliveryFailure(errOut io.Writer, err error) {
	fmt.Fprintf(errOut, "register: failed to write request results to standard output: %v\n", err)
}

// writeRegisterOutput writes v as the single indented JSON document on w.
// Standard output is dedicated to this document, so a write failure is returned
// instead of swallowed: the caller must not attempt to append another document
// after a partial write.
func writeRegisterOutput(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
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
