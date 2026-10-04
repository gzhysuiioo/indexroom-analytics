package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
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

func TestRejectedHealthKeepsHealthyInstanceEligible(t *testing.T) {
	// A healthy instance that receives a stale or same-sequence conflicting
	// "unhealthy" report keeps its accepted observation: the rejections state
	// the reason and the accepted sequence, and the instance stays in the
	// candidate set. The rejected reports target b, the instance the rotation
	// would choose next; the cursor must neither skip it nor restart.
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":5,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":7,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":3,"healthy":false,"reason":"flapping"},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":7,"healthy":false,"reason":"flapping"},
		{"type":"select","service":"svc","expectedRevision":1},
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
	// One result per request, in input order.
	if len(got.Results) != 8 {
		t.Fatalf("results: %+v", got.Results)
	}
	// First select takes the smallest healthy id.
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 5 || r.Revision != 1 {
		t.Fatalf("first select: %+v", r)
	}
	// Stale: sequence below the accepted 7. The failure states the reason and
	// the currently accepted sequence, and keeps the service revision.
	if r := got.Results[4]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence 3 is older than the current sequence 7" ||
		r.Sequence != 7 || r.Revision != 1 {
		t.Fatalf("stale report: %+v", r)
	}
	// Conflict: the accepted sequence 7 reused with the opposite health state.
	if r := got.Results[5]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence 7 already used with different health content" ||
		r.Sequence != 7 || r.Revision != 1 {
		t.Fatalf("conflicting report: %+v", r)
	}
	// The rejected "unhealthy" reports did not knock b out: the rotation
	// continues just after a and lands on b with its accepted record.
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 7 || r.Revision != 1 {
		t.Fatalf("select after rejections should still choose b: %+v", r)
	}
	// Past the end the rotation wraps to the smallest id.
	if r := got.Results[7]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 5 {
		t.Fatalf("select should wrap to a: %+v", r)
	}
	// The final list shows only the accepted observations; nothing from the
	// rejected reports leaked in.
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 2 {
		t.Fatalf("instances: %+v", insts)
	}
	if insts[0].ID != "a" || insts[0].Health != "healthy" || insts[0].Sequence != 5 || insts[0].Reason != "" {
		t.Fatalf("a should keep its accepted record: %+v", insts[0])
	}
	if insts[1].ID != "b" || insts[1].Health != "healthy" || insts[1].Sequence != 7 || insts[1].Reason != "" {
		t.Fatalf("b should keep its accepted record: %+v", insts[1])
	}
}

func TestRejectedHealthDoesNotRestoreUnhealthyInstance(t *testing.T) {
	// The opposite direction: an unhealthy instance that receives a stale or
	// same-sequence conflicting "healthy" report stays unhealthy and never
	// re-enters the candidate set; the rotation keeps repeating the only
	// healthy instance.
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":4,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":6,"healthy":false,"reason":"connection refused"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":6,"healthy":true},
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
	if len(got.Results) != 7 {
		t.Fatalf("results: %+v", got.Results)
	}
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" || r.Sequence != 4 {
		t.Fatalf("first select: %+v", r)
	}
	// Stale "healthy" report: rejected, the accepted sequence 6 is reported.
	if r := got.Results[4]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence 2 is older than the current sequence 6" ||
		r.Sequence != 6 || r.Revision != 1 {
		t.Fatalf("stale recovery attempt: %+v", r)
	}
	// Same sequence 6 flipping unhealthy to healthy: conflict.
	if r := got.Results[5]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence 6 already used with different health content" ||
		r.Sequence != 6 || r.Revision != 1 {
		t.Fatalf("conflicting recovery attempt: %+v", r)
	}
	// b was not restored: a is still the only healthy instance and is chosen
	// again with its accepted record.
	if r := got.Results[6]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 4 || r.Revision != 1 {
		t.Fatalf("select should repeat the only healthy instance: %+v", r)
	}
	// The final list keeps the accepted unhealthy record with its reason.
	insts := got.Services[0].Instances
	if len(insts) != 2 {
		t.Fatalf("instances: %+v", insts)
	}
	if insts[0].ID != "a" || insts[0].Health != "healthy" || insts[0].Sequence != 4 || insts[0].Reason != "" {
		t.Fatalf("a should keep its accepted record: %+v", insts[0])
	}
	if insts[1].ID != "b" || insts[1].Health != "unhealthy" || insts[1].Sequence != 6 || insts[1].Reason != "connection refused" {
		t.Fatalf("b should keep its accepted unhealthy record: %+v", insts[1])
	}
}

func TestRejectedRecoveryKeepsNoHealthyUntilNewerSequence(t *testing.T) {
	// With no healthy instance, rejected recovery attempts change nothing:
	// select keeps returning no_healthy without a target instance or address.
	// Only a later report with a higher sequence is accepted, and the next
	// selection uses that new record rather than the rejected content.
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":9,"healthy":false,"reason":"down"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":4,"healthy":true},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":9,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":10,"healthy":true},
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
	if len(got.Results) != 8 {
		t.Fatalf("results: %+v", got.Results)
	}
	// No healthy instance: no_healthy names neither an instance nor an address.
	if r := got.Results[2]; r.OK || r.Error != "no_healthy" || r.Reason == "" || r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("select with no healthy instance: %+v", r)
	}
	// Stale recovery attempt reports the accepted sequence 9.
	if r := got.Results[3]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence 4 is older than the current sequence 9" ||
		r.Sequence != 9 || r.Revision != 1 {
		t.Fatalf("stale recovery attempt: %+v", r)
	}
	// Same sequence 9 flipping to healthy conflicts.
	if r := got.Results[4]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence 9 already used with different health content" ||
		r.Sequence != 9 || r.Revision != 1 {
		t.Fatalf("conflicting recovery attempt: %+v", r)
	}
	// The rejected recoveries did not restore eligibility: still no_healthy.
	if r := got.Results[5]; r.OK || r.Error != "no_healthy" || r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("rejected recoveries must not restore eligibility: %+v", r)
	}
	// A higher sequence is accepted and flips the instance to healthy.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("newer recovery: %+v", r)
	}
	// The next selection uses the newly accepted record (sequence 10), not
	// anything from the rejected reports.
	if r := got.Results[7]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("select after accepted recovery: %+v", r)
	}
	// Final list: healthy at sequence 10 with the old reason cleared.
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 1 || insts[0].ID != "a" || insts[0].Health != "healthy" || insts[0].Sequence != 10 || insts[0].Reason != "" {
		t.Fatalf("a should be healthy at sequence 10: %+v", insts)
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

// TestRejectedReplacementKeepsAcceptedStateAndRotation is the regression guard
// for replacement requests against an existing service whose rotation is
// already in use: a rejected register must not partially replace instances,
// reset accepted health, consume a revision or move the selection position, and
// later selects in the same batch must keep using the previously accepted
// state. Two rejection conditions are exercised:
//
//   - a list with a valid prefix that changes an existing address followed by an
//     invalid instance address is invalid as a whole (field validation precedes
//     the revision check, so a simultaneously wrong expectedRevision still
//     reports invalid), and the valid prefix must not take effect;
//   - a fully valid list with a mismatched expectedRevision is a conflict
//     naming both revisions; no part of the replacement is applied.
func TestRejectedReplacementKeepsAcceptedStateAndRotation(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":22,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":9,"instances":[{"id":"i1","address":"h9:8080"},{"id":"i2","address":"bad-address"}]},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":5,"instances":[{"id":"i1","address":"hx:8080"},{"id":"i2","address":"h2:8080"}]},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// The failed items keep their slots, in input order, with the later
	// successful selects still present; a later success does not cancel the
	// earlier failures.
	if len(got.Results) != 8 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 0: accepted registration at revision 1.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	// 1-2: both offline health observations accepted without a revision bump.
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 11 {
		t.Fatalf("result 1: %+v", r)
	}
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 22 {
		t.Fatalf("result 2: %+v", r)
	}
	// 3: first selection takes the smallest healthy id, cursor rests on i1.
	if r := got.Results[3]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 3: %+v", r)
	}
	// 4: the later instance address is invalid. The whole item is invalid even
	// though expectedRevision 9 is also wrong, because field validation precedes
	// the revision check; the reason names the bad address and no revision
	// comparison is reported. The valid prefix (i1's new address) must not be
	// applied either.
	if r := got.Results[4]; r.OK || r.Error != "invalid" || r.Revision != 1 ||
		r.ExpectedRevision != 0 || r.ActualRevision != 0 ||
		r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 4 invalid rejection: %+v", r)
	}
	if !strings.Contains(got.Results[4].Reason, `instance address "bad-address" is not a host:port address`) {
		t.Fatalf("result 4 reason should name the bad address, got %q", got.Results[4].Reason)
	}
	// 5: selection after the invalid replacement continues just after i1 and
	// lands on i2 using its ORIGINAL address and accepted sequence, proving the
	// rejected list neither reset i2 to unknown nor advanced the cursor.
	if r := got.Results[5]; !r.OK || r.InstanceID != "i2" || r.Address != "h2:8080" || r.Sequence != 22 || r.Revision != 1 {
		t.Fatalf("result 5: %+v", r)
	}
	// 6: the list itself is valid, only expectedRevision mismatches: conflict
	// states the expected and current revisions; no new address is accepted.
	if r := got.Results[6]; r.OK || r.Error != "conflict" ||
		r.Reason != `service "svc" is at revision 1, not 5` ||
		r.Revision != 1 || r.ExpectedRevision != 5 || r.ActualRevision != 1 {
		t.Fatalf("result 6 conflict: %+v", r)
	}
	// 7: past the end the rotation wraps to the smallest healthy id, returning
	// i1's ORIGINAL address h1:8080 and sequence 11 — not the rejected hx:8080,
	// and not i2 again, which would mean the failed conflict reset the cursor.
	if r := got.Results[7]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 7: %+v", r)
	}

	// The final service list reflects only accepted registrations and
	// observations: revision unchanged at 1, both original addresses, both
	// instances still healthy with their accepted sequences, and neither
	// rejected address present anywhere.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	if len(svc.Instances) != 2 {
		t.Fatalf("instances: %+v", svc.Instances)
	}
	wantInsts := []registerInstance{
		{ID: "i1", Address: "h1:8080", Health: "healthy", Sequence: 11},
		{ID: "i2", Address: "h2:8080", Health: "healthy", Sequence: 22},
	}
	for i, want := range wantInsts {
		if svc.Instances[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, svc.Instances[i], want)
		}
	}
}

// TestEmptyListReplacementKeepsRotationAndDropsHealth is the regression guard
// for replacing a service's instance list with an empty one and later
// re-adding the very same ids and addresses: the empty list must not delete
// the service or its rotation position, and the re-added instances must not
// recover the health observations recorded before the clear. The batch walks
// the full lifecycle:
//
//   - a, b, c registered at revision 1; a and b observed healthy, c observed
//     unhealthy with a reason, all at sequences above 1;
//   - two successful selections return a then b, leaving the cursor on b;
//   - a replacement with an empty instance list succeeds, reports changed and
//     bumps the revision to 2; the service survives, so a selection at
//     revision 2 is no_healthy (never not_found) and names neither an old
//     instance id nor an old address;
//   - re-registering the original ids and addresses at revision 2 bumps the
//     revision to 3; all three instances are back to unknown at sequence 0
//     with no reason, so nothing is selectable yet;
//   - health observations sent under the pre-clear revisions conflict even
//     with larger sequences, naming the request and current revisions, and
//     restore no eligibility; fresh observations at revision 3 with sequence 1
//     succeed without bumping the registration revision;
//   - the next successful selection continues just after b and returns c with
//     its current address and sequence 1, and only the following one wraps
//     back to a — the failed selections in between never moved the cursor.
//
// The batch contains failures, so the exit status is 1; the failed items keep
// their result slots and the final list is sorted by id and reflects only the
// newly accepted observations.
func TestEmptyListReplacementKeepsRotationAndDropsHealth(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":12,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":13,"healthy":false,"reason":"心跳超时"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"register","service":"svc","expectedRevision":2,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"select","service":"svc","expectedRevision":3},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":99,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":2,"sequence":98,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":97,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":3},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":3,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":3,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":3,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":3},
		{"type":"select","service":"svc","expectedRevision":3}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order; failed items keep their slots.
	if len(got.Results) != 19 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 0: registration accepted at revision 1.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	// 1-3: observations accepted without bumping the registration revision.
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 11 {
		t.Fatalf("result 1: %+v", r)
	}
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 12 {
		t.Fatalf("result 2: %+v", r)
	}
	if r := got.Results[3]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 13 {
		t.Fatalf("result 3: %+v", r)
	}
	// 4-5: the first two selections rotate a then b; the cursor rests on b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 4: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 12 || r.Revision != 1 {
		t.Fatalf("result 5: %+v", r)
	}
	// 6: clearing the instance list succeeds, reports the change and bumps the
	// revision to 2.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("result 6: %+v", r)
	}
	// 7: the empty list keeps the service alive at revision 2, so the answer is
	// no_healthy — never not_found — with a readable reason, the current
	// revision, and no leftover instance id or address from before the clear.
	if r := got.Results[7]; r.OK || r.Error != "no_healthy" || r.Reason == "" ||
		r.Revision != 2 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 7 no_healthy on empty list: %+v", r)
	}
	// 8: re-adding the original ids and addresses succeeds and bumps the
	// revision to 3.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 3 {
		t.Fatalf("result 8: %+v", r)
	}
	// 9: identical ids and addresses do not bring the old observations back:
	// all three instances are unknown again, so the selection is no_healthy.
	if r := got.Results[9]; r.OK || r.Error != "no_healthy" || r.Reason == "" ||
		r.Revision != 3 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 9 no_healthy after re-register: %+v", r)
	}
	// 10-12: observations under the pre-clear revisions conflict even though
	// their sequences exceed the old accepted ones; each names the request and
	// current revisions and restores no eligibility.
	if r := got.Results[10]; r.OK || r.Error != "conflict" || r.Reason == "" ||
		r.ExpectedRevision != 1 || r.ActualRevision != 3 || r.Revision != 3 {
		t.Fatalf("result 10 stale-revision report: %+v", r)
	}
	if r := got.Results[11]; r.OK || r.Error != "conflict" || r.Reason == "" ||
		r.ExpectedRevision != 2 || r.ActualRevision != 3 || r.Revision != 3 {
		t.Fatalf("result 11 stale-revision report: %+v", r)
	}
	if r := got.Results[12]; r.OK || r.Error != "conflict" || r.Reason == "" ||
		r.ExpectedRevision != 1 || r.ActualRevision != 3 || r.Revision != 3 {
		t.Fatalf("result 12 stale-revision report: %+v", r)
	}
	// 13: the conflicting reports healed nothing — still no_healthy, and this
	// failed selection must not move the cursor either.
	if r := got.Results[13]; r.OK || r.Error != "no_healthy" ||
		r.Revision != 3 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 13 still no_healthy: %+v", r)
	}
	// 14-16: at the current revision a sequence of 1 suffices (the reset
	// dropped the old sequences 11-13); health reports never bump the
	// registration revision, which stays 3.
	if r := got.Results[14]; !r.OK || !r.Changed || r.Revision != 3 || r.Sequence != 1 {
		t.Fatalf("result 14: %+v", r)
	}
	if r := got.Results[15]; !r.OK || !r.Changed || r.Revision != 3 || r.Sequence != 1 {
		t.Fatalf("result 15: %+v", r)
	}
	if r := got.Results[16]; !r.OK || !r.Changed || r.Revision != 3 || r.Sequence != 1 {
		t.Fatalf("result 16: %+v", r)
	}
	// 17: the cursor survived the empty list on b, so the rotation continues
	// just after b and returns c with its current address and new sequence.
	if r := got.Results[17]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 1 || r.Revision != 3 {
		t.Fatalf("result 17 should resume after b at c: %+v", r)
	}
	// 18: only now does the rotation wrap back to a.
	if r := got.Results[18]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 3 {
		t.Fatalf("result 18 should wrap to a: %+v", r)
	}

	// The final list keeps the service at revision 3 with the instances sorted
	// by id, all healthy at sequence 1 from the newly accepted observations;
	// c's pre-clear unhealthy reason is gone.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 3 {
		t.Fatalf("service view: %+v", svc)
	}
	if len(svc.Instances) != 3 {
		t.Fatalf("instances: %+v", svc.Instances)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 1},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 1},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 1},
	}
	for i, want := range wantInsts {
		if svc.Instances[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, svc.Instances[i], want)
		}
	}
}

// TestRejectedConflictReplacementKeepsRotationPosition isolates the rotation
// guarantee at the batch level: the conflicting replacement is the very next
// item after the first successful selection, so the following select must
// continue at the next healthy id (i2). A failure path that reset the rotation
// would select i1 again instead. The conflicting list is otherwise valid and
// changes i1's address, which also must not take effect.
func TestRejectedConflictReplacementKeepsRotationPosition(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":22,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":7,"instances":[{"id":"i1","address":"hx:8080"},{"id":"i2","address":"h2:8080"}]},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains a failure, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 6 {
		t.Fatalf("results: %+v", got.Results)
	}
	if r := got.Results[3]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 11 {
		t.Fatalf("first select: %+v", r)
	}
	if r := got.Results[4]; r.OK || r.Error != "conflict" ||
		r.Revision != 1 || r.ExpectedRevision != 7 || r.ActualRevision != 1 {
		t.Fatalf("conflicting replacement: %+v", r)
	}
	// The conflict left the cursor just after i1: i2 is next, not i1 again.
	if r := got.Results[5]; !r.OK || r.InstanceID != "i2" || r.Address != "h2:8080" || r.Sequence != 22 || r.Revision != 1 {
		t.Fatalf("select after conflict must continue at i2 with its original state: %+v", r)
	}
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	if insts := got.Services[0].Instances; len(insts) != 2 ||
		insts[0].ID != "i1" || insts[0].Address != "h1:8080" || insts[0].Health != "healthy" || insts[0].Sequence != 11 ||
		insts[1].ID != "i2" || insts[1].Address != "h2:8080" || insts[1].Health != "healthy" || insts[1].Sequence != 22 {
		t.Fatalf("final list must keep only accepted state: %+v", insts)
	}
}

// TestRejectedInvalidReplacementKeepsRotationPosition is the invalid-content
// counterpart: the malformed replacement follows the first successful
// selection immediately, so the next select must still move to i2 rather than
// repeating i1, and the valid prefix changing i1's address must not apply.
func TestRejectedInvalidReplacementKeepsRotationPosition(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":22,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":7,"instances":[{"id":"i1","address":"h9:8080"},{"id":"i2","address":"bad-address"}]},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains a failure, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 6 {
		t.Fatalf("results: %+v", got.Results)
	}
	if r := got.Results[3]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 11 {
		t.Fatalf("first select: %+v", r)
	}
	// Wrong revision AND invalid content: invalid wins; the reason is the
	// address problem, with no expected/actual revision comparison reported.
	if r := got.Results[4]; r.OK || r.Error != "invalid" || r.Revision != 1 ||
		r.ExpectedRevision != 0 || r.ActualRevision != 0 {
		t.Fatalf("invalid replacement: %+v", r)
	}
	if !strings.Contains(got.Results[4].Reason, `instance address "bad-address" is not a host:port address`) {
		t.Fatalf("invalid reason should name the bad address, got %q", got.Results[4].Reason)
	}
	// i1's valid-prefix new address and any cursor reset must both be absent:
	// the rotation continues at i2 with its original address and sequence.
	if r := got.Results[5]; !r.OK || r.InstanceID != "i2" || r.Address != "h2:8080" || r.Sequence != 22 || r.Revision != 1 {
		t.Fatalf("select after invalid replacement must continue at i2 with its original state: %+v", r)
	}
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	if insts := got.Services[0].Instances; len(insts) != 2 ||
		insts[0].ID != "i1" || insts[0].Address != "h1:8080" || insts[0].Health != "healthy" || insts[0].Sequence != 11 ||
		insts[1].ID != "i2" || insts[1].Address != "h2:8080" || insts[1].Health != "healthy" || insts[1].Sequence != 22 {
		t.Fatalf("final list must keep only accepted state: %+v", insts)
	}
}

// TestSelectSessionKeySticky covers the session flow end to end: the first
// request with a key rotates and binds, later requests with the same trimmed
// key reuse the bound instance without moving the rotation, and plain selects
// interleaved between them keep rotating from the last rotated position.
func TestSelectSessionKeySticky(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s1"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s1"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  s1  "},
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
	if len(got.Results) != 9 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 4: first use of the key rotates to the smallest id and binds s1 -> a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("first session select: %+v", r)
	}
	// 5: reuse returns a again without advancing the rotation.
	if r := got.Results[5]; !r.OK || r.InstanceID != "a" || r.Sequence != 1 {
		t.Fatalf("session reuse: %+v", r)
	}
	// 6: a plain select continues just after a, proving the reuse did not
	// move the cursor.
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Sequence != 2 {
		t.Fatalf("plain select after reuse: %+v", r)
	}
	// 7: the padded key trims to the same session and still reuses a.
	if r := got.Results[7]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("padded key should reuse the same session: %+v", r)
	}
	// 8: the next plain select continues after b.
	if r := got.Results[8]; !r.OK || r.InstanceID != "c" || r.Sequence != 3 {
		t.Fatalf("plain select should continue after b: %+v", r)
	}
}

// TestSelectSessionKeyFallbackAndRecovery walks a session through the bound
// instance turning unhealthy (fallback rebinds through the rotation), the old
// instance recovering (the binding stays moved), and a no_healthy gap that
// leaves the binding intact for the next reuse.
func TestSelectSessionKeyFallbackAndRecovery(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":2,"healthy":false,"reason":"down"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":4,"healthy":false,"reason":"down"},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":false,"reason":"down"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains a no_healthy failure, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 13 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 3: bind s -> a.
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("bind: %+v", r)
	}
	// 5: a is unhealthy; the session falls back to the rotation (after a) and
	// rebinds to b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 1 {
		t.Fatalf("fallback select: %+v", r)
	}
	// 7: a recovered, but the binding moved to b.
	if r := got.Results[7]; !r.OK || r.InstanceID != "b" {
		t.Fatalf("binding should have moved to b: %+v", r)
	}
	// 10: nothing healthy; the failed request keeps the binding and cursor.
	if r := got.Results[10]; r.OK || r.Error != "no_healthy" || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("no_healthy: %+v", r)
	}
	// 12: b recovered; the binding survived and the reuse reports b's latest
	// accepted sequence.
	if r := got.Results[12]; !r.OK || r.InstanceID != "b" || r.Sequence != 3 {
		t.Fatalf("reuse after recovery: %+v", r)
	}
}

func TestSelectSessionKeyValidationErrors(t *testing.T) {
	cases := map[string]string{
		"explicit null": `{"requests":[{"type":"select","service":"s","expectedRevision":0,"sessionKey":null}]}`,
		"number":        `{"requests":[{"type":"select","service":"s","expectedRevision":0,"sessionKey":5}]}`,
		"boolean":       `{"requests":[{"type":"select","service":"s","expectedRevision":0,"sessionKey":true}]}`,
		"object":        `{"requests":[{"type":"select","service":"s","expectedRevision":0,"sessionKey":{}}]}`,
		"array":         `{"requests":[{"type":"select","service":"s","expectedRevision":0,"sessionKey":[]}]}`,
		"blank string":  `{"requests":[{"type":"select","service":"s","expectedRevision":0,"sessionKey":"   "}]}`,
		"empty string":  `{"requests":[{"type":"select","service":"s","expectedRevision":0,"sessionKey":""}]}`,
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
			if !strings.Contains(got.Results[0].Reason, "sessionKey") {
				t.Fatalf("reason should name the sessionKey problem, got %q", got.Results[0].Reason)
			}
			if len(got.Services) != 0 {
				t.Fatalf("no service should be created: %+v", got.Services)
			}
		})
	}
}

// TestSelectSessionKeyInvalidPrecedesRevision locks the ordering: an invalid
// sessionKey reports invalid even when expectedRevision is also wrong, and the
// failure creates no binding — a later valid request with the same key still
// rotates as a first use.
func TestSelectSessionKeyInvalidPrecedesRevision(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":99,"sessionKey":"  "},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"}
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
	// 3: blank sessionKey wins over the wrong revision: invalid, no revision
	// comparison reported, and no binding created.
	if r := got.Results[3]; r.OK || r.Error != "invalid" || r.Revision != 1 ||
		r.ExpectedRevision != 0 || r.ActualRevision != 0 {
		t.Fatalf("invalid sessionKey: %+v", r)
	}
	if !strings.Contains(got.Results[3].Reason, "sessionKey") {
		t.Fatalf("reason should name sessionKey, got %q", got.Results[3].Reason)
	}
	// 4: the failed item left no trace — the key's first success rotates to a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("first session select: %+v", r)
	}
	// 5: reuse.
	if r := got.Results[5]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("session reuse: %+v", r)
	}
}

// TestSelectSessionKeyAddressReplacementRecoveryBeforeReuse is the regression
// guard for a session whose bound instance keeps its id but moves to a new
// address, where the new address is observed healthy BEFORE the session's next
// request: the reuse must return the new address and the new health record,
// never the stale ones, and must not drag the rotation back to a. The batch
// also locks in that the replacement resets only a's health record — a fresh
// observation at the new revision may restart its sequence at 1, below the 11
// accepted for the old address. Every request succeeds, so the exit status is 0.
func TestSelectSessionKeyAddressReplacementRecoveryBeforeReuse(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":12,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":13,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"h9:9"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":2,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("all requests succeed, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 10 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 4: the key's first success rotates to the smallest id and binds s -> a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("session bind: %+v", r)
	}
	// 5: a plain select rotates to b; the cursor rests on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 12 {
		t.Fatalf("plain select: %+v", r)
	}
	// 6: the replacement changes only a's address and bumps the revision once.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("address replacement: %+v", r)
	}
	// 7: the new address accepts an observation at the new revision; its
	// sequence restarts at 1 and is not bounded by the 11 the old address had.
	if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 1 {
		t.Fatalf("fresh observation at new address: %+v", r)
	}
	// 8: a recovered before this request, so the binding still holds — but the
	// reuse must report the CURRENT address and sequence, not the stale h1:1/11
	// recorded before the replacement reset the health record.
	if r := got.Results[8]; !r.OK || r.InstanceID != "a" || r.Address != "h9:9" || r.Sequence != 1 || r.Revision != 2 {
		t.Fatalf("session reuse must reflect the new address and sequence: %+v", r)
	}
	// 9: the reuse did not move the rotation back to a: the plain select
	// continues just after b and lands on c.
	if r := got.Results[9]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 13 || r.Revision != 2 {
		t.Fatalf("plain select should continue after b at c: %+v", r)
	}
	// The final list shows a healthy at its new address with the restarted
	// sequence; b and c kept their observations through the replacement.
	if len(got.Services) != 1 || got.Services[0].Revision != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 3 {
		t.Fatalf("instances: %+v", insts)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h9:9", Health: "healthy", Sequence: 1},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 12},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 13},
	}
	for i, want := range wantInsts {
		if insts[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, insts[i], want)
		}
	}
}

// TestSelectSessionKeyAddressReplacementSelectBeforeRecovery covers the other
// ordering: the session's next request arrives while the rebound instance is
// still unknown at its new address. The request falls back to the normal
// rotation — continuing after the last rotated id, not restarting — and
// rebinds to the rotated instance. The old instance recovering afterwards does
// not steal the binding back. Every request succeeds, so the exit status is 0.
func TestSelectSessionKeyAddressReplacementSelectBeforeRecovery(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":12,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":13,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"h9:9"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"select","service":"svc","expectedRevision":2,"sessionKey":"s"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":2,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("all requests succeed, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 4-5: bind s -> a, then a plain select rotates to b; the cursor rests on b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 {
		t.Fatalf("session bind: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Sequence != 12 {
		t.Fatalf("plain select: %+v", r)
	}
	// 6: the replacement resets a to unknown at its new address.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("address replacement: %+v", r)
	}
	// 7: a is unknown at selection time, so the session falls back to the
	// rotation, which continues just after b and lands on c; the success
	// rebinds s -> c.
	if r := got.Results[7]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 13 || r.Revision != 2 {
		t.Fatalf("fallback should rotate after b to c: %+v", r)
	}
	// 8: a recovers at its new address under the new revision.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 1 {
		t.Fatalf("a recovery: %+v", r)
	}
	// 9: the binding moved to c with the fallback; a's recovery does not pull
	// the session back.
	if r := got.Results[9]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 13 {
		t.Fatalf("binding should stay on c after a recovers: %+v", r)
	}
	// 10: the fallback rotation left the cursor on c, so the plain select wraps
	// to a — now eligible again at its new address with its new sequence.
	if r := got.Results[10]; !r.OK || r.InstanceID != "a" || r.Address != "h9:9" || r.Sequence != 1 || r.Revision != 2 {
		t.Fatalf("plain select should wrap to a at its new address: %+v", r)
	}
	if len(got.Services) != 1 || got.Services[0].Revision != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 3 {
		t.Fatalf("instances: %+v", insts)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h9:9", Health: "healthy", Sequence: 1},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 12},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 13},
	}
	for i, want := range wantInsts {
		if insts[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, insts[i], want)
		}
	}
}

// TestSelectSessionKeyAddressReplacementNoHealthyKeepsBinding is the regression
// guard for the all-unavailable gap after an address replacement: the session
// request fails with no_healthy — current revision, a readable reason, no
// fabricated target — and the failure neither rewrites the binding nor
// advances the rotation, so once a recovers at its new address the same
// session reuses it, and once the other instances recover the plain rotation
// resumes from its pre-failure position. The batch also rejects a health
// report sent under the OLD registration revision: even with a sequence far
// above anything accepted before, it conflicts and cannot heal the new
// address. The batch contains failures, so the exit status is 1, and the
// failed items keep their result slots while later valid requests still apply.
func TestSelectSessionKeyAddressReplacementNoHealthyKeepsBinding(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":12,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":13,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"h9:9"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":2,"sequence":14,"healthy":false,"reason":"连接拒绝"},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":2,"sequence":15,"healthy":false,"reason":"连接拒绝"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":99,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2,"sessionKey":"s"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":2,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2,"sessionKey":"s"},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":2,"sequence":16,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":2,"sequence":17,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order; failed items keep their slots.
	if len(got.Results) != 16 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 4-5: bind s -> a, then a plain select rotates to b; the cursor rests on b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 {
		t.Fatalf("session bind: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Sequence != 12 {
		t.Fatalf("plain select: %+v", r)
	}
	// 6: the replacement resets a to unknown at its new address, revision 2.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("address replacement: %+v", r)
	}
	// 7-8: b and c turn unhealthy at the current revision.
	if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 14 {
		t.Fatalf("b unhealthy: %+v", r)
	}
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 15 {
		t.Fatalf("c unhealthy: %+v", r)
	}
	// 9: a health report under the OLD revision conflicts even though its
	// sequence 99 exceeds anything accepted before; it names both revisions and
	// must not heal the new address.
	if r := got.Results[9]; r.OK || r.Error != "conflict" || r.Reason == "" ||
		r.ExpectedRevision != 1 || r.ActualRevision != 2 || r.Revision != 2 {
		t.Fatalf("old-revision health report: %+v", r)
	}
	// 10: with a unknown (the conflicting report healed nothing) and b, c
	// unhealthy, the session request is no_healthy: it states the reason and
	// the current revision and fabricates neither an instance id nor an
	// address. The failure must not rewrite the s -> a binding or move the
	// cursor off b.
	if r := got.Results[10]; r.OK || r.Error != "no_healthy" || r.Reason == "" ||
		r.Revision != 2 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("no_healthy with everything unavailable: %+v", r)
	}
	// 11: a fresh observation at the current revision heals the new address;
	// the sequence restarts at 1, below the 11 accepted for the old address.
	if r := got.Results[11]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 1 {
		t.Fatalf("fresh observation at new address: %+v", r)
	}
	// 12: the binding survived the no_healthy failure, so the session reuses a
	// again — with its CURRENT address and sequence, and without rotating.
	if r := got.Results[12]; !r.OK || r.InstanceID != "a" || r.Address != "h9:9" || r.Sequence != 1 || r.Revision != 2 {
		t.Fatalf("session should reuse a after its recovery: %+v", r)
	}
	// 13-14: b and c recover at the current revision.
	if r := got.Results[13]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 16 {
		t.Fatalf("b recovery: %+v", r)
	}
	if r := got.Results[14]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 17 {
		t.Fatalf("c recovery: %+v", r)
	}
	// 15: the no_healthy failure never moved the cursor off b and the session
	// reuse did not rotate either, so the plain select continues just after b
	// and lands on c.
	if r := got.Results[15]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 17 || r.Revision != 2 {
		t.Fatalf("plain select should resume after b at c: %+v", r)
	}
	// The final list reflects the new address and only the accepted health
	// records: a healthy at h9:9 with the restarted sequence 1, b and c
	// healthy again with their recovery sequences and no leftover reason.
	if len(got.Services) != 1 || got.Services[0].Revision != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 3 {
		t.Fatalf("instances: %+v", insts)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h9:9", Health: "healthy", Sequence: 1},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 16},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 17},
	}
	for i, want := range wantInsts {
		if insts[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, insts[i], want)
		}
	}
}

// TestSelectSessionKeyPerService checks at the batch level that the same key
// in two services binds independently.
func TestSelectSessionKeyPerService(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"s1","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"register","service":"s2","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"s1","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"s1","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"s2","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"s2","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"s1","expectedRevision":1,"sessionKey":"k"},
		{"type":"select","service":"s2","expectedRevision":1,"sessionKey":"k"},
		{"type":"select","service":"s1","expectedRevision":1},
		{"type":"select","service":"s1","expectedRevision":1,"sessionKey":"k"},
		{"type":"select","service":"s2","expectedRevision":1,"sessionKey":"k"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 6-7: the same key binds a in each service independently.
	if r := got.Results[6]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("s1 bind: %+v", r)
	}
	if r := got.Results[7]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("s2 bind: %+v", r)
	}
	// 8: a plain select on s1 rotates to b without touching s2's rotation.
	if r := got.Results[8]; !r.OK || r.InstanceID != "b" {
		t.Fatalf("s1 plain select: %+v", r)
	}
	// 9-10: both sessions still reuse their own a.
	if r := got.Results[9]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("s1 reuse: %+v", r)
	}
	if r := got.Results[10]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("s2 reuse: %+v", r)
	}
}

// TestSelectTwoSessionKeysInterleaveInOneService is the batch-level guard for
// two distinct keys sharing one service's rotation. The keys' first successes
// consume the same rotation in input order (a then b) and each remembers its
// own instance; an existing binding never selects the target for the new key.
// Interleaved reuses return each session's own instance id, address and
// latest accepted sequence without moving the rotation, and plain selects
// continue from the last real rotation — a reuse must neither rewind it nor
// skip a healthy instance. Every request succeeds, so the exit status is 0 and
// the final service list reflects only the accepted health records.
func TestSelectTwoSessionKeysInterleaveInOneService(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":10,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":20,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("all requests succeed, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 15 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 4: x's first success rotates to the smallest id and binds x -> a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("x first select: %+v", r)
	}
	// 5: y is a different session; x's binding must not pick its target. y's
	// first success continues the same rotation after a and binds y -> b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("y first select should rotate to b, not reuse x binding: %+v", r)
	}
	// 6-7: interleaved reuses answer each session's own binding with its own
	// address and sequence — never the other session's data.
	if r := got.Results[6]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("x reuse: %+v", r)
	}
	if r := got.Results[7]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("y reuse: %+v", r)
	}
	// 8: the plain select continues just after the last rotated id (b) to c;
	// neither reuse advanced the rotation.
	if r := got.Results[8]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 {
		t.Fatalf("plain select should continue after b at c: %+v", r)
	}
	// 11-12: after newer observations the reuses report each bound instance's
	// own latest accepted sequence (a=10 for x, b=20 for y), still without
	// rotating.
	if r := got.Results[11]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 {
		t.Fatalf("x reuse latest sequence: %+v", r)
	}
	if r := got.Results[12]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 20 {
		t.Fatalf("y reuse latest sequence: %+v", r)
	}
	// 13-14: plain rotation resumes after c (reusing the older binding did not
	// rewind it), wraps to a with its latest sequence, then continues to b.
	if r := got.Results[13]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 {
		t.Fatalf("plain select should wrap to a: %+v", r)
	}
	if r := got.Results[14]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 20 {
		t.Fatalf("plain select should continue to b: %+v", r)
	}
	// Selections changed neither the revision nor accepted health records.
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 10},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 20},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 3},
	}
	for i, want := range wantInsts {
		if got.Services[0].Instances[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, got.Services[0].Instances[i], want)
		}
	}
}

// TestSelectTwoSessionKeysRebindIsolationAndSharing drives one session's bound
// instance unhealthy while the other session's instance stays healthy: only
// the affected session falls back to the rotation and rewrites its own
// binding. The two sessions are then allowed to converge on the same instance
// (not a conflict, and neither steals the other's binding); the original
// instance recovering does not pull the rebound session back; and the plain
// rotation continues from the successful reselection's position. Every
// request succeeds, so the exit status is 0.
func TestSelectTwoSessionKeysRebindIsolationAndSharing(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":4,"healthy":false,"reason":"down"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":5,"healthy":false,"reason":"down"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":6,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("all requests succeed, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 19 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 4-5: x -> a, y -> b; the cursor rests on b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("x bind: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("y bind: %+v", r)
	}
	// 7: a (x's binding) is unhealthy: x alone falls back to the rotation just
	// after b and rebinds to c, returning c's own address and sequence.
	if r := got.Results[7]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 {
		t.Fatalf("x fallback should reselect c: %+v", r)
	}
	// 8: y is isolated from x's rebind and keeps its own b.
	if r := got.Results[8]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("y must keep its own b binding: %+v", r)
	}
	// 9: x now answers c, proving only its own binding was rewritten.
	if r := got.Results[9]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 {
		t.Fatalf("x should reuse c: %+v", r)
	}
	// 11: c turns unhealthy too, so with only b healthy x's fallback wraps to b.
	if r := got.Results[11]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("x should rebind to b through the rotation: %+v", r)
	}
	// 12-13: two sessions sharing one instance is allowed: both keep returning
	// b and neither request displaces the other session.
	if r := got.Results[12]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("y still b: %+v", r)
	}
	if r := got.Results[13]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("x still b: %+v", r)
	}
	// 15-16: a recovers, but the moved session stays on b; recovery steals no
	// binding, and the other session keeps b as well.
	if r := got.Results[15]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("x must keep b after a recovers: %+v", r)
	}
	if r := got.Results[16]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("y must keep b: %+v", r)
	}
	// 17: the plain rotation continues from x's successful reselection
	// position (cursor on b), wraps to the recovered a and reports a's latest
	// sequence, not its pre-failure record.
	if r := got.Results[17]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 6 {
		t.Fatalf("plain select should resume from the rebind position and wrap to a: %+v", r)
	}
	// 18: the rotation then continues to b.
	if r := got.Results[18]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("plain select should continue to b: %+v", r)
	}
	// Final list: a recovered healthy at 6, b healthy at 2, c unhealthy at 5
	// with its reason; revision untouched.
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 3 {
		t.Fatalf("instances: %+v", insts)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 6},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 2},
		{ID: "c", Address: "h3:3", Health: "unhealthy", Sequence: 5, Reason: "down"},
	}
	for i, want := range wantInsts {
		if insts[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, insts[i], want)
		}
	}
}

// TestSelectTwoSessionKeysNoHealthyIsolation is the batch-level failure
// guard. Once every instance is unhealthy, keyed and plain selections all
// return no_healthy with the current revision and a reason but no fabricated
// instance id, address or sequence; the failed items keep their result slots
// while later valid items still apply, and the batch exits non-zero. The
// failures preserve both sessions' bindings and the rotation position: an
// unbound instance (c) recovers first so a surviving binding is distinguishable
// from a fresh rotation, then each session's own bound instance recovers and is
// reused directly with its latest sequence (a request that had lost its binding
// would rotate from the parked cursor onto c instead). The plain rotation then
// resumes exactly where the last real rotation stopped.
func TestSelectTwoSessionKeysNoHealthyIsolation(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":4,"healthy":false,"reason":"down"},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":5,"healthy":false,"reason":"down"},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":6,"healthy":false,"reason":"down"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":7,"healthy":true},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":8,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":9,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains no_healthy failures, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order; the three failures keep slots
	// 9-11 and later legal items still land in their own positions.
	if len(got.Results) != 23 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 4-5: x -> a, y -> b; the last real rotation parks the cursor on b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("x bind: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("y bind: %+v", r)
	}
	// 9-11: keyed and plain requests are all no_healthy at revision 1 with a
	// reason, fabricating neither an instance id nor an address.
	for _, i := range []int{9, 10, 11} {
		r := got.Results[i]
		if r.OK || r.Error != "no_healthy" || r.Revision != 1 || r.Reason == "" {
			t.Fatalf("result %d should be no_healthy with revision and reason: %+v", i, r)
		}
		if r.InstanceID != "" || r.Address != "" {
			t.Fatalf("result %d must fabricate no target: %+v", i, r)
		}
	}
	// 14: c is healthy but x is bound to a; with a recovered and the cursor on
	// b, reusing the preserved binding returns a. Had the failure erased it,
	// this request would rotate to c instead — so a/8 distinguishes reuse from
	// a fresh rotation and carries a's latest sequence.
	if r := got.Results[14]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 8 {
		t.Fatalf("x should directly reuse recovered a: %+v", r)
	}
	// 16-17: y likewise reuses its recovered b directly (a fresh rotation from
	// b would return c), and x keeps a.
	if r := got.Results[16]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 9 {
		t.Fatalf("y should directly reuse recovered b: %+v", r)
	}
	if r := got.Results[17]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 8 {
		t.Fatalf("x should keep a: %+v", r)
	}
	// 18-20: the failures and direct reuses left the cursor on b, so the plain
	// rotation continues to c, wraps to a, then continues to b with each
	// instance's recovered sequence.
	if r := got.Results[18]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 7 {
		t.Fatalf("plain select should resume after b at c: %+v", r)
	}
	if r := got.Results[19]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 8 {
		t.Fatalf("plain select should wrap to a: %+v", r)
	}
	if r := got.Results[20]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 9 {
		t.Fatalf("plain select should continue to b: %+v", r)
	}
	// 21-22: the plain rotation rewrote no binding; each session still answers
	// its own target.
	if r := got.Results[21]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 8 {
		t.Fatalf("x binding intact after plain rotation: %+v", r)
	}
	if r := got.Results[22]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 9 {
		t.Fatalf("y binding intact after plain rotation: %+v", r)
	}
	// The selections changed no revision and no accepted health record; the
	// final list shows only the recovered states with reasons cleared.
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 3 {
		t.Fatalf("instances: %+v", insts)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 8},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 9},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 7},
	}
	for i, want := range wantInsts {
		if insts[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, insts[i], want)
		}
	}
}
