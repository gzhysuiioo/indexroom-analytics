package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func runRegisterWith(t *testing.T, input string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runRegister(strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRegisterCommandSuccessAndShape(t *testing.T) {
	code, stdout, stderr := runRegisterWith(t, `{"requests":[
		{"service":"b","expectedRevision":0,"instances":[]},
		{"service":"a","expectedRevision":0,"instances":[
			{"id":"y","address":"h:2"},{"id":"x","address":"h:1"}
		]}
	]}`)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%s)", code, stderr)
	}
	var out struct {
		Results []struct {
			Status   string `json:"status"`
			Service  string `json:"service"`
			Revision int    `json:"revision"`
			Changed  bool   `json:"changed"`
		} `json:"results"`
		Services []struct {
			Service   string `json:"service"`
			Revision  int    `json:"revision"`
			Instances []struct {
				ID string `json:"id"`
			} `json:"instances"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if len(out.Results) != 2 || out.Results[0].Service != "b" {
		t.Fatalf("results not emitted in request order: %+v", out.Results)
	}
	if len(out.Services) != 2 ||
		out.Services[0].Service != "a" || out.Services[1].Service != "b" {
		t.Fatalf("services not sorted by name: %+v", out.Services)
	}
	if out.Services[0].Instances[0].ID != "x" {
		t.Fatalf("instances not sorted by id: %+v", out.Services[0].Instances)
	}
}

func TestRegisterCommandEmptyRequestsIsSuccess(t *testing.T) {
	code, stdout, _ := runRegisterWith(t, `{"requests":[]}`)
	if code != 0 {
		t.Fatalf("empty requests should exit 0, got %d", code)
	}
	if !strings.Contains(stdout, `"results": []`) || !strings.Contains(stdout, `"services": []`) {
		t.Fatalf("unexpected empty output: %s", stdout)
	}
}

func TestRegisterCommandFailedItemIsNonZero(t *testing.T) {
	code, stdout, _ := runRegisterWith(t, `{"requests":[
		{"service":"ok","expectedRevision":0,"instances":[]},
		{"service":"ok","expectedRevision":9,"instances":[]}
	]}`)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stdout, `"status": "conflict"`) ||
		!strings.Contains(stdout, `"revision": 1`) {
		t.Fatalf("conflict result missing: %s", stdout)
	}
	if !strings.Contains(stdout, `"service": "ok"`) {
		t.Fatalf("successful state must still be listed: %s", stdout)
	}
}

func TestRegisterCommandInvalidItemIsNonZero(t *testing.T) {
	code, stdout, _ := runRegisterWith(t, `{"requests":[
		{"service":"s","expectedRevision":0,"instances":[{"id":"a","address":"no-port"}]}
	]}`)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stdout, `"status": "invalid"`) {
		t.Fatalf("invalid result missing: %s", stdout)
	}
}

func TestRegisterCommandMalformedDocument(t *testing.T) {
	for _, input := range []string{`nope`, `{}`, `{"requests":null}`, `{"requests":5}`} {
		code, stdout, stderr := runRegisterWith(t, input)
		if code == 0 {
			t.Errorf("input %q should exit non-zero", input)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Errorf("input %q should produce no stdout, got %q", input, stdout)
		}
		if strings.TrimSpace(stderr) == "" {
			t.Errorf("input %q should print an explicit error", input)
		}
	}
}

func TestRegisterCommandDeterministic(t *testing.T) {
	input := `{"requests":[
		{"service":"zeta","expectedRevision":0,"instances":[{"id":"z","address":"h:1"},{"id":"a","address":"h:2"}]},
		{"service":"zeta","expectedRevision":1,"instances":[{"id":"a","address":"h:2"}]}
	]}`

	_, first, _ := runRegisterWith(t, input)
	for i := 0; i < 5; i++ {
		_, again, _ := runRegisterWith(t, input)
		if again != first {
			t.Fatalf("output differs across runs:\n%s\n%s", first, again)
		}
	}
}
