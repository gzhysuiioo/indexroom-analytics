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

func TestHealthSuccess(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"},{"id":"b","address":"h:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":false,"reason":"connection refused"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":2,"healthy":false,"reason":"still down"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 4 {
		t.Fatalf("results: %+v", got.Results)
	}
	want := []registerResult{
		{Service: "svc", OK: true, Changed: true, Revision: 1},
		{Service: "svc", OK: true, Changed: true, Revision: 1, Sequence: 1},
		{Service: "svc", OK: true, Changed: true, Revision: 1, Sequence: 1},
		{Service: "svc", OK: true, Changed: true, Revision: 1, Sequence: 2},
	}
	for i := range want {
		if got.Results[i] != want[i] {
			t.Fatalf("result %d: got %+v want %+v", i, got.Results[i], want[i])
		}
	}
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 2 {
		t.Fatalf("instances: %+v", insts)
	}
	// Sorted by id: a then b.
	if insts[0].ID != "a" || insts[0].Health != "unhealthy" || insts[0].Sequence != 2 || insts[0].Reason != "still down" {
		t.Fatalf("instance a: %+v", insts[0])
	}
	if insts[1].ID != "b" || insts[1].Health != "unhealthy" || insts[1].Sequence != 1 || insts[1].Reason != "connection refused" {
		t.Fatalf("instance b: %+v", insts[1])
	}
}

func TestHealthHealthyClearsReason(t *testing.T) {
	input := `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":false,"reason":"down"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":2,"healthy":true,"reason":"ignored"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	inst := got.Services[0].Instances[0]
	if inst.Health != "healthy" || inst.Sequence != 2 || inst.Reason != "" {
		t.Fatalf("healthy should clear reason: %+v", inst)
	}
}

func TestHealthFailures(t *testing.T) {
	input := `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":5,"healthy":true},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":5,"healthy":false,"reason":"different"},
		{"type":"health","service":"svc","instanceId":"ghost","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":2,"sequence":6,"healthy":true},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":6,"healthy":true}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if len(got.Results) != 7 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 0: create
	if r := got.Results[0]; !r.OK || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	// 1: healthy seq 5
	if r := got.Results[1]; !r.OK || !r.Changed || r.Sequence != 5 {
		t.Fatalf("result 1: %+v", r)
	}
	// 2: stale (3 < 5)
	if r := got.Results[2]; r.OK || r.Error != "stale" || r.Sequence != 5 {
		t.Fatalf("result 2: %+v", r)
	}
	// 3: same seq 5, different content -> conflict
	if r := got.Results[3]; r.OK || r.Error != "conflict" || r.Sequence != 5 {
		t.Fatalf("result 3: %+v", r)
	}
	// 4: instance missing -> not_found
	if r := got.Results[4]; r.OK || r.Error != "not_found" || r.Revision != 1 {
		t.Fatalf("result 4: %+v", r)
	}
	// 5: wrong revision -> conflict
	if r := got.Results[5]; r.OK || r.Error != "conflict" || r.ExpectedRevision != 2 || r.ActualRevision != 1 {
		t.Fatalf("result 5: %+v", r)
	}
	// 6: healthy seq 6 succeeds (state still healthy from result 1)
	if r := got.Results[6]; !r.OK || !r.Changed || r.Sequence != 6 {
		t.Fatalf("result 6: %+v", r)
	}
}

func TestHealthValidationErrors(t *testing.T) {
	cases := map[string]string{
		"empty service":        `{"requests":[{"type":"health","service":"  ","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":true}]}`,
		"empty instance id":    `{"requests":[{"type":"health","service":"s","instanceId":" ","expectedRevision":0,"sequence":1,"healthy":true}]}`,
		"negative revision":    `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":-1,"sequence":1,"healthy":true}]}`,
		"zero sequence":        `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":0,"healthy":true}]}`,
		"negative sequence":    `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":-2,"healthy":true}]}`,
		"unhealthy no reason":  `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":false}]}`,
		"unhealthy empty reason": `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":false,"reason":"  "}]}`,
		"missing revision":     `{"requests":[{"type":"health","service":"s","instanceId":"a","sequence":1,"healthy":true}]}`,
		"missing sequence":     `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"healthy":true}]}`,
		"missing healthy":      `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1}]}`,
		"healthy string":       `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":"true"}]}`,
		"healthy number":       `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":1}]}`,
		"sequence float":       `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1.5,"healthy":true}]}`,
		"revision string":      `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":"0","sequence":1,"healthy":true}]}`,
		"reason number":        `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":false,"reason":5}]}`,
		"unknown type":         `{"requests":[{"type":"probe","service":"s"}]}`,
		"type number":          `{"requests":[{"type":5,"service":"s"}]}`,
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

func TestHealthDefaultTypeIsRegister(t *testing.T) {
	// A request without a type field is treated as a registration.
	input := `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if len(got.Results) != 2 || !got.Results[0].OK || !got.Results[1].OK {
		t.Fatalf("results: %+v", got.Results)
	}
	if inst := got.Services[0].Instances[0]; inst.Health != "healthy" || inst.Sequence != 1 {
		t.Fatalf("instance: %+v", inst)
	}
}

func TestHealthEmptyRequestsStillSuccess(t *testing.T) {
	out, code := runRegisterWith(t, `{"requests":[]}`)
	if code != 0 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if len(got.Results) != 0 || len(got.Services) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

func TestHealthContinuesAfterFailure(t *testing.T) {
	// A failed health request must not prevent later requests from running.
	input := `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":99,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if len(got.Results) != 3 {
		t.Fatalf("results: %+v", got.Results)
	}
	if r := got.Results[1]; r.OK || r.Error != "conflict" {
		t.Fatalf("result 1: %+v", r)
	}
	if r := got.Results[2]; !r.OK || r.Sequence != 1 {
		t.Fatalf("result 2 should still succeed: %+v", r)
	}
}
