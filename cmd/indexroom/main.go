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
	fmt.Println("register reads service registrations as JSON from standard input:")
	fmt.Println(`  {"requests":[{"service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"host:8080"}]}]}`)
	fmt.Println("Each request fully replaces the service's instance list. New services")
	fmt.Println("require expectedRevision 0; existing services require the current revision.")
	fmt.Println("Exit status is 0 only when every registration succeeds.")
}

// registerInstance is one instance in the final service list.
type registerInstance struct {
	ID      string `json:"id"`
	Address string `json:"address"`
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
}

// registerOutput is the full register output.
type registerOutput struct {
	Results  []registerResult  `json:"results"`
	Services []registerService `json:"services"`
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
	results := make([]registerResult, 0, len(rawRequests))
	anyFailed := false

	for _, raw := range rawRequests {
		var head struct {
			Service string `json:"service"`
		}
		_ = json.Unmarshal(raw, &head)
		service := strings.TrimSpace(head.Service)

		var req struct {
			Service   string          `json:"service"`
			Revision  *int64          `json:"expectedRevision"`
			Instances json.RawMessage `json:"instances"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			anyFailed = true
			results = append(results, registerResult{
				Service:  service,
				OK:       false,
				Error:    "invalid",
				Reason:   fmt.Sprintf("request must be an object with service, expectedRevision and instances: %v", err),
				Revision: registry.RevisionOf(service),
			})
			continue
		}
		service = strings.TrimSpace(req.Service)

		if req.Revision == nil {
			anyFailed = true
			results = append(results, registerResult{
				Service:  service,
				OK:       false,
				Error:    "invalid",
				Reason:   "expectedRevision is required and must be a non-negative integer",
				Revision: registry.RevisionOf(service),
			})
			continue
		}
		if !isJSONArray(req.Instances) {
			anyFailed = true
			results = append(results, registerResult{
				Service:  service,
				OK:       false,
				Error:    "invalid",
				Reason:   "instances must be an array",
				Revision: registry.RevisionOf(service),
			})
			continue
		}
		var instances []indexroom.Instance
		if err := json.Unmarshal(req.Instances, &instances); err != nil {
			anyFailed = true
			results = append(results, registerResult{
				Service:  service,
				OK:       false,
				Error:    "invalid",
				Reason:   fmt.Sprintf("instances must be an array of objects with id and address: %v", err),
				Revision: registry.RevisionOf(service),
			})
			continue
		}

		registration, err := registry.ValidateRegistration(service, int(*req.Revision), instances)
		if err != nil {
			anyFailed = true
			results = append(results, registerResult{
				Service:  service,
				OK:       false,
				Error:    "invalid",
				Reason:   err.Error(),
				Revision: registry.RevisionOf(service),
			})
			continue
		}

		outcome := registry.Apply(registration)
		if !outcome.OK {
			anyFailed = true
			results = append(results, registerResult{
				Service:          outcome.Service,
				OK:               false,
				Error:            string(outcome.Kind),
				Reason:           outcome.Reason,
				ExpectedRevision: outcome.Expected,
				ActualRevision:   outcome.Actual,
				Revision:         outcome.Revision,
			})
			continue
		}
		results = append(results, registerResult{
			Service:  outcome.Service,
			OK:       true,
			Changed:  outcome.Changed,
			Revision: outcome.Revision,
		})
	}

	services := make([]registerService, 0)
	for _, view := range registry.Snapshot() {
		insts := make([]registerInstance, 0, len(view.Instances))
		for _, inst := range view.Instances {
			insts = append(insts, registerInstance{ID: inst.ID, Address: inst.Address})
		}
		services = append(services, registerService{
			Service:   view.Service,
			Revision:  view.Revision,
			Instances: insts,
		})
	}

	writeRegisterOutput(registerOutput{Results: results, Services: services})
	if anyFailed {
		return 1
	}
	return 0
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
