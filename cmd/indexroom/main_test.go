package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// runRegisterWith runs runRegister with the given stdin and captured stdout.
func runRegisterWith(t *testing.T, input string) (string, int) {
	t.Helper()
	oldStdin := os.Stdin
	oldStdout := os.Stdout
	defer func() {
		os.Stdin = oldStdin
		os.Stdout = oldStdout
	}()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdin = inR
	go func() {
		defer inW.Close()
		_, _ = inW.WriteString(input)
	}()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = outW
	code := runRegister()
	outW.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(outR); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return buf.String(), code
}

func TestRegisterSuccess(t *testing.T) {
	input := `{"requests":[
		{"service":"b","expectedRevision":0,"instances":[{"id":"b2","address":"h2:2"},{"id":"b1","address":"h1:1"}]},
		{"service":"b","expectedRevision":1,"instances":[{"id":"b1","address":"h1:1"},{"id":"b2","address":"h2:2"}]},
		{"service":"a","expectedRevision":0,"instances":[]}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 3 {
		t.Fatalf("results: %+v", got.Results)
	}
	want := []registerResult{
		{Service: "b", OK: true, Changed: true, Revision: 1},
		{Service: "b", OK: true, Changed: false, Revision: 1},
		{Service: "a", OK: true, Changed: true, Revision: 1},
	}
	for i := range want {
		if got.Results[i] != want[i] {
			t.Fatalf("result %d: got %+v want %+v", i, got.Results[i], want[i])
		}
	}
	// Services sorted by name; instances sorted by id; empty list kept.
	if len(got.Services) != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	if got.Services[0].Service != "a" || got.Services[0].Revision != 1 || len(got.Services[0].Instances) != 0 {
		t.Fatalf("service a: %+v", got.Services[0])
	}
	if got.Services[1].Service != "b" || got.Services[1].Revision != 1 {
		t.Fatalf("service b: %+v", got.Services[1])
	}
	insts := got.Services[1].Instances
	if len(insts) != 2 || insts[0].ID != "b1" || insts[0].Address != "h1:1" || insts[1].ID != "b2" {
		t.Fatalf("service b instances: %+v", insts)
	}
}

func TestRegisterFailuresDoNotAbort(t *testing.T) {
	input := `{"requests":[
		{"service":"a","expectedRevision":0,"instances":[{"id":"x","address":"h:1"}]},
		{"service":"a","expectedRevision":5,"instances":[{"id":"y","address":"h:2"}]},
		{"service":"a","expectedRevision":1,"instances":[{"id":"y","address":"h:2"}]},
		{"service":"a","expectedRevision":2,"instances":[{"id":"","address":"h:1"}]},
		{"service":"a","expectedRevision":2,"instances":[{"id":"z","address":"nope"}]},
		{"service":"b","expectedRevision":1,"instances":[]}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 6 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 0: create
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	// 1: conflict, expected 5 actual 1
	if r := got.Results[1]; r.OK || r.Error != "conflict" || r.ExpectedRevision != 5 || r.ActualRevision != 1 || r.Revision != 1 {
		t.Fatalf("result 1: %+v", r)
	}
	// 2: update to rev 2
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("result 2: %+v", r)
	}
	// 3: invalid content with a matching revision: still invalid, list untouched
	if r := got.Results[3]; r.OK || r.Error != "invalid" || r.Revision != 2 {
		t.Fatalf("result 3: %+v", r)
	}
	// 4: invalid address
	if r := got.Results[4]; r.OK || r.Error != "invalid" || r.Revision != 2 {
		t.Fatalf("result 4: %+v", r)
	}
	// 5: conflict on a brand-new service
	if r := got.Results[5]; r.OK || r.Error != "conflict" || r.ExpectedRevision != 1 || r.ActualRevision != 0 || r.Revision != 0 {
		t.Fatalf("result 5: %+v", r)
	}
	// Only the successful registrations are reflected.
	if len(got.Services) != 1 || got.Services[0].Service != "a" || got.Services[0].Revision != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	if len(got.Services[0].Instances) != 1 || got.Services[0].Instances[0].ID != "y" {
		t.Fatalf("instances: %+v", got.Services[0].Instances)
	}
}

func TestRegisterEmptyRequests(t *testing.T) {
	out, code := runRegisterWith(t, `{"requests":[]}`)
	if code != 0 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 0 || len(got.Services) != 0 {
		t.Fatalf("expected empty results, got %+v", got)
	}
}

func TestRegisterTopLevelErrors(t *testing.T) {
	cases := map[string]string{
		"not json":        `{not json`,
		"not object":      `[1,2]`,
		"missing requests": `{"other":[]}`,
		"requests string": `{"requests":"x"}`,
		"requests null":   `{"requests":null}`,
		"trailing data":   `{"requests":[]} garbage`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			out, code := runRegisterWith(t, input)
			if code != 1 {
				t.Fatalf("exit code: %d", code)
			}
			var got struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			if got.Error == "" {
				t.Fatalf("missing error message: %s", out)
			}
		})
	}
}

func TestRegisterRequestShapeErrors(t *testing.T) {
	cases := map[string]string{
		"request not object":   `{"requests":[5]}`,
		"missing revision":     `{"requests":[{"service":"a","instances":[]}]}`,
		"revision float":       `{"requests":[{"service":"a","expectedRevision":1.5,"instances":[]}]}`,
		"revision string":      `{"requests":[{"service":"a","expectedRevision":"1","instances":[]}]}`,
		"missing instances":    `{"requests":[{"service":"a","expectedRevision":0}]}`,
		"instances object":     `{"requests":[{"service":"a","expectedRevision":0,"instances":{}}]}`,
		"instance not object":  `{"requests":[{"service":"a","expectedRevision":0,"instances":["x"]}]}`,
		"instance id number":   `{"requests":[{"service":"a","expectedRevision":0,"instances":[{"id":1,"address":"h:1"}]}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			out, code := runRegisterWith(t, input)
			if code != 1 {
				t.Fatalf("exit code: %d", code)
			}
			var got registerOutput
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			if len(got.Results) != 1 || got.Results[0].OK || got.Results[0].Error != "invalid" {
				t.Fatalf("expected one invalid result, got %+v", got.Results)
			}
			if len(got.Services) != 0 {
				t.Fatalf("no service should be created: %+v", got.Services)
			}
		})
	}
}

func TestRegisterDeterministic(t *testing.T) {
	input := `{"requests":[
		{"service":"b","expectedRevision":0,"instances":[{"id":"b2","address":"h2:2"},{"id":"b1","address":"h1:1"}]},
		{"service":"a","expectedRevision":0,"instances":[{"id":"a1","address":"h1:1"}]}
	]}`
	out1, _ := runRegisterWith(t, input)
	out2, _ := runRegisterWith(t, input)
	if out1 != out2 {
		t.Fatalf("non-deterministic output:\n%s\nvs\n%s", out1, out2)
	}
}
