package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// runSelectBatch runs one register batch and decodes the per-item results.
func runSelectBatch(t *testing.T, input string) ([]registerResult, int) {
	t.Helper()
	out, code := runRegisterWith(t, input)
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return got.Results, code
}

const excludeBatchPrefix = `{"requests":[
	{"type":"register","service":"svc","expectedRevision":0,"instances":[
		{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},{"id":"c","address":"h3:3"}]},
	{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
	{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
	{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},`

func TestSelectExcludeEndToEnd(t *testing.T) {
	input := excludeBatchPrefix + `
	{"type":"select","service":"svc","expectedRevision":1},
	{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":["b"]},
	{"type":"select","service":"svc","expectedRevision":1},
	{"type":"select","service":"svc","expectedRevision":1},
	{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":[]}
]}`

	results, code := runSelectBatch(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, results: %+v", code, results)
	}
	// a, then c (b excluded once), then a, b, c as the rotation continues.
	wantIDs := []string{"a", "c", "a", "b", "c"}
	wantSeq := []int64{1, 3, 1, 2, 3}
	if len(results) != 4+len(wantIDs) {
		t.Fatalf("results: %+v", results)
	}
	for i, id := range wantIDs {
		got := results[4+i]
		if !got.OK || got.InstanceID != id || got.Sequence != wantSeq[i] {
			t.Fatalf("select %d: %+v, want id=%s seq=%d", i, got, id, wantSeq[i])
		}
		if got.Revision != 1 {
			t.Fatalf("select %d revision: %+v", i, got)
		}
	}
}

func TestSelectExcludeAllHealthyReportsNoHealthy(t *testing.T) {
	input := excludeBatchPrefix + `
	{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":["a","b","c","ghost"]},
	{"type":"select","service":"svc","expectedRevision":1}
]}`

	results, code := runSelectBatch(t, input)
	if code == 0 {
		t.Fatalf("exit code should be 1, results: %+v", results)
	}
	out := results[4]
	if out.OK || out.Error != "no_healthy" {
		t.Fatalf("want no_healthy, got %+v", out)
	}
	if !strings.Contains(out.Reason, "excludeInstanceIds") {
		t.Fatalf("reason should state the exclusions removed every healthy instance: %q", out.Reason)
	}
	if out.InstanceID != "" || out.Address != "" || out.Sequence != 0 {
		t.Fatalf("failure must not carry a target: %+v", out)
	}
	// The failed exclusion left the rotation untouched: the next ordinary
	// select takes the smallest healthy id.
	if last := results[5]; !last.OK || last.InstanceID != "a" {
		t.Fatalf("rotation after failed exclusion: %+v", last)
	}
}

func TestSelectExcludeInvalidShapes(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"explicit null", `null`, "excludeInstanceIds"},
		{"non-array", `"b"`, "excludeInstanceIds"},
		{"object", `{"id":"b"}`, "excludeInstanceIds"},
		{"number element", `["a",1]`, "excludeInstanceIds"},
		{"null element", `["a",null]`, "excludeInstanceIds"},
		{"boolean element", `[true]`, "excludeInstanceIds"},
		{"blank element", `["a","  "]`, "excludeInstanceIds"},
		{"empty element", `[""]`, "excludeInstanceIds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := excludeBatchPrefix + `
	{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":` + tc.token + `},
	{"type":"select","service":"svc","expectedRevision":1}
]}`
			results, code := runSelectBatch(t, input)
			if code == 0 {
				t.Fatalf("exit code should be 1, results: %+v", results)
			}
			out := results[4]
			if out.OK || out.Error != "invalid" {
				t.Fatalf("want invalid, got %+v", out)
			}
			if !strings.Contains(out.Reason, tc.want) {
				t.Fatalf("reason should name %s: %q", tc.want, out.Reason)
			}
			if out.InstanceID != "" || out.Address != "" {
				t.Fatalf("invalid item must not carry a target: %+v", out)
			}
			// The batch continues: the later ordinary select still runs and
			// starts the rotation at the smallest healthy id.
			if last := results[5]; !last.OK || last.InstanceID != "a" {
				t.Fatalf("later request after invalid item: %+v", last)
			}
		})
	}
}

func TestSelectExcludeInvalidPrecedesRevisionConflict(t *testing.T) {
	// The revision is wrong (the service is at 1), but the malformed exclusion
	// list is reported first: content validity precedes the revision check.
	input := excludeBatchPrefix + `
	{"type":"select","service":"svc","expectedRevision":99,"excludeInstanceIds":null},
	{"type":"select","service":"svc","expectedRevision":99}
]}`

	results, code := runSelectBatch(t, input)
	if code == 0 {
		t.Fatalf("exit code should be 1, results: %+v", results)
	}
	if out := results[4]; out.OK || out.Error != "invalid" || !strings.Contains(out.Reason, "excludeInstanceIds") {
		t.Fatalf("invalid exclusion beats revision conflict: %+v", out)
	}
	if out := results[5]; out.OK || out.Error != "conflict" {
		t.Fatalf("same request without the field is a conflict: %+v", out)
	}
}

func TestSelectExcludeWithSessionKey(t *testing.T) {
	input := excludeBatchPrefix + `
	{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
	{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s","excludeInstanceIds":["a"]},
	{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
	{"type":"select","service":"svc","expectedRevision":1}
]}`

	results, code := runSelectBatch(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, results: %+v", code, results)
	}
	// s binds to a; excluding a rotates to b and rebinds s; the reuse of the
	// new binding moves nothing, so the plain select continues after b at c.
	wantIDs := []string{"a", "b", "b", "c"}
	for i, id := range wantIDs {
		if got := results[4+i]; !got.OK || got.InstanceID != id {
			t.Fatalf("select %d: %+v, want id=%s", i, got, id)
		}
	}
}
