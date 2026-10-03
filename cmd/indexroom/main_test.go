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
		"not json":         `{not json`,
		"not object":       `[1,2]`,
		"missing requests": `{"other":[]}`,
		"requests string":  `{"requests":"x"}`,
		"requests null":    `{"requests":null}`,
		"trailing data":    `{"requests":[]} garbage`,
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
		"request not object":  `{"requests":[5]}`,
		"missing revision":    `{"requests":[{"service":"a","instances":[]}]}`,
		"revision float":      `{"requests":[{"service":"a","expectedRevision":1.5,"instances":[]}]}`,
		"revision string":     `{"requests":[{"service":"a","expectedRevision":"1","instances":[]}]}`,
		"missing instances":   `{"requests":[{"service":"a","expectedRevision":0}]}`,
		"instances object":    `{"requests":[{"service":"a","expectedRevision":0,"instances":{}}]}`,
		"instance not object": `{"requests":[{"service":"a","expectedRevision":0,"instances":["x"]}]}`,
		"instance id number":  `{"requests":[{"service":"a","expectedRevision":0,"instances":[{"id":1,"address":"h:1"}]}]}`,
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
		"empty service":          `{"requests":[{"type":"health","service":"  ","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":true}]}`,
		"empty instance id":      `{"requests":[{"type":"health","service":"s","instanceId":" ","expectedRevision":0,"sequence":1,"healthy":true}]}`,
		"negative revision":      `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":-1,"sequence":1,"healthy":true}]}`,
		"zero sequence":          `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":0,"healthy":true}]}`,
		"negative sequence":      `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":-2,"healthy":true}]}`,
		"unhealthy no reason":    `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":false}]}`,
		"unhealthy empty reason": `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":false,"reason":"  "}]}`,
		"missing revision":       `{"requests":[{"type":"health","service":"s","instanceId":"a","sequence":1,"healthy":true}]}`,
		"missing sequence":       `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"healthy":true}]}`,
		"missing healthy":        `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1}]}`,
		"healthy string":         `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":"true"}]}`,
		"healthy number":         `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":1}]}`,
		"sequence float":         `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1.5,"healthy":true}]}`,
		"revision string":        `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":"0","sequence":1,"healthy":true}]}`,
		"reason number":          `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":1,"healthy":false,"reason":5}]}`,
		"unknown type":           `{"requests":[{"type":"probe","service":"s"}]}`,
		"type number":            `{"requests":[{"type":5,"service":"s"}]}`,
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

func TestSelectSuccess(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"b","address":"h2:2"},{"id":"a","address":"h1:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":22,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 6 {
		t.Fatalf("results: %+v", got.Results)
	}
	want := []registerResult{
		{Service: "svc", OK: true, Changed: true, Revision: 1},
		{Service: "svc", OK: true, Changed: true, Revision: 1, Sequence: 11},
		{Service: "svc", OK: true, Changed: true, Revision: 1, Sequence: 22},
		{Service: "svc", OK: true, Revision: 1, InstanceID: "a", Address: "h1:1", Sequence: 11},
		{Service: "svc", OK: true, Revision: 1, InstanceID: "b", Address: "h2:2", Sequence: 22},
		{Service: "svc", OK: true, Revision: 1, InstanceID: "a", Address: "h1:1", Sequence: 11},
	}
	for i := range want {
		if got.Results[i] != want[i] {
			t.Fatalf("result %d:\n got %+v\nwant %+v", i, got.Results[i], want[i])
		}
	}
}

func TestSelectOnlySeesPriorCommittedState(t *testing.T) {
	// Selection is processed in input order and uses only state committed by
	// earlier items; an instance made healthy AFTER a select is not selectable
	// by it. Here the first select runs while everything is unknown.
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if r := got.Results[1]; r.OK || r.Error != "no_healthy" || r.Revision != 1 || r.Address != "" || r.InstanceID != "" {
		t.Fatalf("select before health should be no_healthy: %+v", r)
	}
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" || r.Address != "h:1" || r.Sequence != 1 {
		t.Fatalf("select after health should succeed: %+v", r)
	}
}

func TestSelectFailures(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":9},
		{"type":"select","service":"missing","expectedRevision":0},
		{"type":"select","service":"missing","expectedRevision":2},
		{"type":"register","service":"empty","expectedRevision":0,"instances":[]},
		{"type":"select","service":"empty","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 7 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 2: wrong revision on existing service -> conflict expected 9 actual 1.
	if r := got.Results[2]; r.OK || r.Error != "conflict" || r.ExpectedRevision != 9 || r.ActualRevision != 1 || r.Revision != 1 {
		t.Fatalf("result 2: %+v", r)
	}
	// 3: unknown service expected 0 -> not_found, revision 0.
	if r := got.Results[3]; r.OK || r.Error != "not_found" || r.Revision != 0 || r.InstanceID != "" {
		t.Fatalf("result 3: %+v", r)
	}
	// 4: unknown service expected 2 -> conflict actual 0.
	if r := got.Results[4]; r.OK || r.Error != "conflict" || r.ExpectedRevision != 2 || r.ActualRevision != 0 || r.Revision != 0 {
		t.Fatalf("result 4: %+v", r)
	}
	// 6: service exists with zero instances -> no_healthy, revision 1.
	if r := got.Results[6]; r.OK || r.Error != "no_healthy" || r.Revision != 1 || r.Address != "" {
		t.Fatalf("result 6: %+v", r)
	}
}

func TestSelectValidationErrors(t *testing.T) {
	cases := map[string]string{
		"empty service":     `{"requests":[{"type":"select","service":"  ","expectedRevision":0}]}`,
		"missing revision":  `{"requests":[{"type":"select","service":"s"}]}`,
		"negative revision": `{"requests":[{"type":"select","service":"s","expectedRevision":-1}]}`,
		"revision float":    `{"requests":[{"type":"select","service":"s","expectedRevision":1.5}]}`,
		"revision string":   `{"requests":[{"type":"select","service":"s","expectedRevision":"1"}]}`,
		"request as array":  `{"requests":[["select"]]}`,
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

func TestSelectCursorNotAdvancedByDuplicatesOrFailures(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"},{"id":"b","address":"h:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":9},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"h:1"},{"id":"b","address":"h:2"}]},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// First success: a. Then a failed select, a duplicate registration and a
	// duplicate health report, none of which advance the rotation.
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("first select: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.Changed {
		t.Fatalf("duplicate registration should be unchanged: %+v", r)
	}
	if r := got.Results[6]; !r.OK || r.Changed {
		t.Fatalf("duplicate health should be unchanged: %+v", r)
	}
	if r := got.Results[7]; !r.OK || r.InstanceID != "b" {
		t.Fatalf("rotation should resume at b, got %+v", r)
	}
}

func TestSelectAddressReplacementKeepsRotation(t *testing.T) {
	// One instance keeps its id but gets a new address: the registration bumps
	// once, that instance resets to unknown/0 while untouched instances keep
	// their observations, and the rotation continues after the last chosen id
	// instead of restarting at the smallest id.
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"},{"id":"d","address":"h4:4"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":12,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":13,"healthy":true},
		{"type":"health","service":"svc","instanceId":"d","expectedRevision":1,"sequence":14,"healthy":false,"reason":"connection refused"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"h9:9"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"},{"id":"d","address":"h4:4"}]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 10 {
		t.Fatalf("results: %+v", got.Results)
	}
	// Rotation before the replacement: a then b, cursor rests on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 {
		t.Fatalf("first select: %+v", r)
	}
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 12 {
		t.Fatalf("second select: %+v", r)
	}
	// The address-only replacement succeeds and bumps the revision exactly once.
	if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("address replacement: %+v", r)
	}
	// The new address of a has no observation yet, so it is skipped; the
	// rotation continues just after b and lands on c, never on a's old address.
	if r := got.Results[8]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 13 || r.Revision != 2 {
		t.Fatalf("select after replacement should continue after b at c: %+v", r)
	}
	// Past the end the rotation wraps to the smallest eligible id (b), proving
	// it did not restart from the smallest id because of the replacement.
	if r := got.Results[9]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 12 || r.Revision != 2 {
		t.Fatalf("select should wrap to smallest eligible id: %+v", r)
	}
	// Final state: a is unknown at sequence 0 with no leftover reason; the
	// untouched instances keep their health, sequence and reason.
	if len(got.Services) != 1 || got.Services[0].Revision != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 4 {
		t.Fatalf("instances: %+v", insts)
	}
	if insts[0].ID != "a" || insts[0].Address != "h9:9" || insts[0].Health != "unknown" || insts[0].Sequence != 0 || insts[0].Reason != "" {
		t.Fatalf("replaced instance should reset to unknown/0: %+v", insts[0])
	}
	if insts[1].ID != "b" || insts[1].Health != "healthy" || insts[1].Sequence != 12 {
		t.Fatalf("b should keep its observation: %+v", insts[1])
	}
	if insts[2].ID != "c" || insts[2].Health != "healthy" || insts[2].Sequence != 13 {
		t.Fatalf("c should keep its observation: %+v", insts[2])
	}
	if insts[3].ID != "d" || insts[3].Health != "unhealthy" || insts[3].Sequence != 14 || insts[3].Reason != "connection refused" {
		t.Fatalf("d should keep its observation and reason: %+v", insts[3])
	}
}

func TestSelectAddressReplacementNoHealthyThenRecovery(t *testing.T) {
	// Replacing the only healthy instance's address leaves no healthy target:
	// select reports no_healthy without moving the cursor, a health report
	// against the old revision conflicts without healing the new address, and
	// a fresh observation at the current revision (sequence only above the
	// reset 0, not above the old accepted one) makes the new address eligible
	// again.
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":20,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":21,"healthy":false,"reason":"down"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"h9:9"},{"id":"b","address":"h2:2"}]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":99,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":2,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":2,"sequence":22,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 12 {
		t.Fatalf("results: %+v", got.Results)
	}
	// Cursor rests on a before the replacement.
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 20 {
		t.Fatalf("select before replacement: %+v", r)
	}
	if r := got.Results[4]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("address replacement: %+v", r)
	}
	// No healthy instance remains: no_healthy states the reason and the current
	// revision and fabricates neither an instance id nor an address.
	if r := got.Results[5]; r.OK || r.Error != "no_healthy" || r.Reason == "" || r.Revision != 2 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("no_healthy after replacement: %+v", r)
	}
	// A health report against the old revision conflicts even though its
	// sequence (99) exceeds the old accepted one (20); it names both revisions.
	if r := got.Results[6]; r.OK || r.Error != "conflict" || r.Reason == "" || r.ExpectedRevision != 1 || r.ActualRevision != 2 || r.Revision != 2 {
		t.Fatalf("stale-revision health report: %+v", r)
	}
	// The conflicting report did not heal the new address: still no_healthy.
	if r := got.Results[7]; r.OK || r.Error != "no_healthy" || r.Revision != 2 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("new address must not regain eligibility from old revision: %+v", r)
	}
	// At the current revision a sequence just above the reset 0 suffices; it
	// does not need to exceed the 20 accepted for the old address.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 1 {
		t.Fatalf("fresh observation at current revision: %+v", r)
	}
	if r := got.Results[9]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 22 {
		t.Fatalf("b recovery: %+v", r)
	}
	// The failed selects and health report never moved the cursor off a, so
	// the rotation continues at b rather than restarting at the smallest id.
	if r := got.Results[10]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 22 || r.Revision != 2 {
		t.Fatalf("rotation should resume after a at b: %+v", r)
	}
	// The new address rejoins selection carrying its new address and the new
	// observation's sequence.
	if r := got.Results[11]; !r.OK || r.InstanceID != "a" || r.Address != "h9:9" || r.Sequence != 1 || r.Revision != 2 {
		t.Fatalf("new address should be selected with its new observation: %+v", r)
	}
	// The final list reflects only the committed changes.
	if len(got.Services) != 1 || got.Services[0].Revision != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 2 {
		t.Fatalf("instances: %+v", insts)
	}
	if insts[0].ID != "a" || insts[0].Address != "h9:9" || insts[0].Health != "healthy" || insts[0].Sequence != 1 || insts[0].Reason != "" {
		t.Fatalf("a should be healthy at its new address: %+v", insts[0])
	}
	if insts[1].ID != "b" || insts[1].Address != "h2:2" || insts[1].Health != "healthy" || insts[1].Sequence != 22 || insts[1].Reason != "" {
		t.Fatalf("b should be healthy again: %+v", insts[1])
	}
}

func TestSelectDeterministic(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"c","address":"h3:3"},{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out1, _ := runRegisterWith(t, input)
	out2, _ := runRegisterWith(t, input)
	if out1 != out2 {
		t.Fatalf("non-deterministic output:\n%s\nvs\n%s", out1, out2)
	}
}

func TestRejectedHealthReportsDoNotAffectSelection(t *testing.T) {
	// Stale and same-sequence-conflicting health reports alternate with selects
	// in one batch. Each rejection must only produce a failed result: the
	// accepted health records, the candidate set and the rotation position all
	// stay exactly as committed earlier. Both directions are covered: a healthy
	// instance's rejected "unhealthy" report must not remove it, and an
	// unhealthy instance's rejected "healthy" report must not restore it.
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":10,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":20,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":30,"healthy":false,"reason":"c down"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":9,"healthy":false,"reason":"stale takedown"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":20,"healthy":false,"reason":"conflict takedown"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":25,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	// Successful later selects do not clear the batch failure status.
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per input item, in order: failures keep their slot and later
	// items are still processed.
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}
	// First success takes the smallest healthy id.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 {
		t.Fatalf("first select: %+v", r)
	}
	// A stale report on the healthy last-chosen instance: stale states the
	// reason, the current accepted sequence and the preserved revision.
	if r := got.Results[5]; r.OK || r.Error != "stale" || r.Reason == "" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("stale takedown of a: %+v", r)
	}
	// Rotation continues just after a at b; the rejected takedown neither
	// removed a nor advanced/reset the cursor.
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 20 {
		t.Fatalf("select after stale report: %+v", r)
	}
	// Same sequence with the opposite status is a conflict, again naming the
	// current accepted sequence and keeping the revision; a matching revision
	// carries no expected/actual revision pair.
	if r := got.Results[7]; r.OK || r.Error != "conflict" || r.Reason == "" || r.Sequence != 20 || r.Revision != 1 || r.ExpectedRevision != 0 || r.ActualRevision != 0 {
		t.Fatalf("conflicting takedown of b: %+v", r)
	}
	// b stays eligible, so the rotation wraps past it back to a carrying its
	// accepted address and sequence 10 (never the rejected sequence 9).
	if r := got.Results[8]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 {
		t.Fatalf("select after conflict should wrap to a: %+v", r)
	}
	// The unhealthy instance's stale "healthy" report must not restore it; the
	// rejection reports the accepted sequence 30.
	if r := got.Results[9]; r.OK || r.Error != "stale" || r.Reason == "" || r.Sequence != 30 || r.Revision != 1 {
		t.Fatalf("stale recovery of c: %+v", r)
	}
	// c is still excluded, so the rotation continues after a at b.
	if r := got.Results[10]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 20 {
		t.Fatalf("unhealthy c must not regain eligibility: %+v", r)
	}
	// The final service list shows only the accepted records: no rejected
	// status, sequence or reason has leaked in.
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 3 {
		t.Fatalf("instances: %+v", insts)
	}
	if insts[0].ID != "a" || insts[0].Health != "healthy" || insts[0].Sequence != 10 || insts[0].Reason != "" {
		t.Fatalf("a must keep its accepted healthy record: %+v", insts[0])
	}
	if insts[1].ID != "b" || insts[1].Health != "healthy" || insts[1].Sequence != 20 || insts[1].Reason != "" {
		t.Fatalf("b must keep its accepted healthy record: %+v", insts[1])
	}
	if insts[2].ID != "c" || insts[2].Health != "unhealthy" || insts[2].Sequence != 30 || insts[2].Reason != "c down" {
		t.Fatalf("c must keep its accepted unhealthy record: %+v", insts[2])
	}
}

func TestRejectedRecoveryKeepsNoHealthyAndCursor(t *testing.T) {
	// With no healthy instance, stale and conflicting "healthy" reports aimed
	// at restoring an instance must keep failing selection as no_healthy and
	// must fabricate neither a target nor an address. Only a later, higher and
	// accepted sequence changes eligibility; the cursor then resumes from where
	// the earlier successful selection left it and uses the new record rather
	// than the rejected one.
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":false,"reason":"down"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":3,"healthy":false,"reason":"takedown"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":4,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":5,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 14 {
		t.Fatalf("results: %+v", got.Results)
	}
	// The last successful selection before the outage rests on a.
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("select before takedown: %+v", r)
	}
	// Accepted takedown at sequence 3 leaves no healthy instance.
	if r := got.Results[4]; !r.OK || !r.Changed || r.Sequence != 3 {
		t.Fatalf("takedown: %+v", r)
	}
	// no_healthy states reason and revision and gives no target or address.
	if r := got.Results[5]; r.OK || r.Error != "no_healthy" || r.Reason == "" || r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("no healthy after takedown: %+v", r)
	}
	// Stale recovery (2 < 3): rejected with the current accepted sequence.
	if r := got.Results[6]; r.OK || r.Error != "stale" || r.Reason == "" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("stale recovery of a: %+v", r)
	}
	if r := got.Results[7]; r.OK || r.Error != "no_healthy" || r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("a must not recover from a stale report: %+v", r)
	}
	// Same-sequence opposite status on the unhealthy instance: conflict.
	if r := got.Results[8]; r.OK || r.Error != "conflict" || r.Reason == "" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("conflicting recovery of b: %+v", r)
	}
	if r := got.Results[9]; r.OK || r.Error != "no_healthy" || r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("b must not recover from a conflicting report: %+v", r)
	}
	// A higher accepted sequence restores a.
	if r := got.Results[10]; !r.OK || !r.Changed || r.Sequence != 4 {
		t.Fatalf("accepted recovery of a: %+v", r)
	}
	// The failed selections and rejected reports never moved the cursor, and the
	// only healthy instance is a; it is picked with the new accepted sequence.
	if r := got.Results[11]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 4 {
		t.Fatalf("selection after recovery must use the new record: %+v", r)
	}
	if r := got.Results[12]; !r.OK || !r.Changed || r.Sequence != 5 {
		t.Fatalf("accepted recovery of b: %+v", r)
	}
	// Rotation continues just after a at b rather than restarting.
	if r := got.Results[13]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 5 {
		t.Fatalf("rotation should resume after a at b: %+v", r)
	}
	// Final list holds only accepted records and reasons.
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 2 {
		t.Fatalf("instances: %+v", insts)
	}
	if insts[0].ID != "a" || insts[0].Health != "healthy" || insts[0].Sequence != 4 || insts[0].Reason != "" {
		t.Fatalf("a final record: %+v", insts[0])
	}
	if insts[1].ID != "b" || insts[1].Health != "healthy" || insts[1].Sequence != 5 || insts[1].Reason != "" {
		t.Fatalf("b final record: %+v", insts[1])
	}
}
