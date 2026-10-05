package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestReleaseSessionRejoinsRotation covers the release flow end to end: a
// session binds i1, a plain select rotates to i2, the release reports
// changed, and the session's next select is a fresh rotation continuing just
// after i2 — landing on i3 with a new binding.
func TestReleaseSessionRejoinsRotation(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:1"},{"id":"i2","address":"h2:2"},{"id":"i3","address":"h3:3"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i3","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"never-bound"}
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
	// 4: first use binds s -> i1; 5: the plain select rotates to i2.
	if r := got.Results[4]; !r.OK || r.InstanceID != "i1" {
		t.Fatalf("bind: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "i2" {
		t.Fatalf("plain select: %+v", r)
	}
	// 6: the release succeeds with changed and reports the current revision.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("release: %+v", r)
	}
	// 7: the released key rotates again from just after i2, landing on i3.
	if r := got.Results[7]; !r.OK || r.InstanceID != "i3" || r.Address != "h3:3" || r.Sequence != 3 {
		t.Fatalf("select after release: %+v", r)
	}
	// 8: the re-established binding releases with changed again.
	if r := got.Results[8]; !r.OK || !r.Changed {
		t.Fatalf("second release: %+v", r)
	}
	// 9: an unbound key releases successfully without a change.
	if r := got.Results[9]; !r.OK || r.Changed || r.Revision != 1 {
		t.Fatalf("release of unbound key: %+v", r)
	}
}

// TestReleaseSessionResultShape locks the wire shape of a release result: a
// successful release carries service, ok, revision and changed only when a
// binding existed — never an instance id, address or sequence, and no changed
// field at all when the key was unbound.
func TestReleaseSessionResultShape(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"s"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 5 {
		t.Fatalf("results: %+v", got.Results)
	}
	changed := got.Results[3]
	if changed["ok"] != true || changed["changed"] != true || changed["service"] != "svc" {
		t.Fatalf("release of a bound key: %+v", changed)
	}
	for _, field := range []string{"instanceId", "address", "sequence"} {
		if _, present := changed[field]; present {
			t.Fatalf("release must not carry %s: %+v", field, changed)
		}
	}
	unchanged := got.Results[4]
	if unchanged["ok"] != true {
		t.Fatalf("release of an unbound key should succeed: %+v", unchanged)
	}
	if _, present := unchanged["changed"]; present {
		t.Fatalf("unbound release must omit changed: %+v", unchanged)
	}
	for _, field := range []string{"instanceId", "address", "sequence"} {
		if _, present := unchanged[field]; present {
			t.Fatalf("release must not carry %s: %+v", field, unchanged)
		}
	}
}

func TestReleaseSessionKeyValidationErrors(t *testing.T) {
	cases := map[string]string{
		"missing":       `{"requests":[{"type":"release_session","service":"s","expectedRevision":0}]}`,
		"explicit null": `{"requests":[{"type":"release_session","service":"s","expectedRevision":0,"sessionKey":null}]}`,
		"number":        `{"requests":[{"type":"release_session","service":"s","expectedRevision":0,"sessionKey":5}]}`,
		"boolean":       `{"requests":[{"type":"release_session","service":"s","expectedRevision":0,"sessionKey":true}]}`,
		"object":        `{"requests":[{"type":"release_session","service":"s","expectedRevision":0,"sessionKey":{}}]}`,
		"array":         `{"requests":[{"type":"release_session","service":"s","expectedRevision":0,"sessionKey":[]}]}`,
		"blank string":  `{"requests":[{"type":"release_session","service":"s","expectedRevision":0,"sessionKey":"   "}]}`,
		"empty string":  `{"requests":[{"type":"release_session","service":"s","expectedRevision":0,"sessionKey":""}]}`,
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

// TestReleaseSessionFailuresPreserveState locks the failure ordering and
// isolation through the command: an invalid sessionKey wins over a wrong
// revision, a revision mismatch is a conflict carrying both revisions, an
// unknown service at expectedRevision 0 is not_found, and every failure keeps
// all bindings and the rotation position while later requests still run.
func TestReleaseSessionFailuresPreserveState(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"release_session","service":"svc","expectedRevision":99,"sessionKey":"  "},
		{"type":"release_session","service":"svc","expectedRevision":99,"sessionKey":"s"},
		{"type":"release_session","service":"ghost","expectedRevision":0,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
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
	if len(got.Results) != 9 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 3: bind s -> a.
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("bind: %+v", r)
	}
	// 4: blank sessionKey wins over the wrong revision: invalid, no revision
	// comparison reported.
	if r := got.Results[4]; r.OK || r.Error != "invalid" || r.Revision != 1 ||
		r.ExpectedRevision != 0 || r.ActualRevision != 0 {
		t.Fatalf("invalid sessionKey: %+v", r)
	}
	if !strings.Contains(got.Results[4].Reason, "sessionKey") {
		t.Fatalf("reason should name sessionKey, got %q", got.Results[4].Reason)
	}
	// 5: a valid key with a wrong revision is a conflict carrying both
	// revisions.
	if r := got.Results[5]; r.OK || r.Error != "conflict" || r.Revision != 1 ||
		r.ExpectedRevision != 99 || r.ActualRevision != 1 {
		t.Fatalf("revision conflict: %+v", r)
	}
	// 6: an unknown service at expectedRevision 0 is not_found.
	if r := got.Results[6]; r.OK || r.Error != "not_found" || r.Revision != 0 {
		t.Fatalf("unknown service: %+v", r)
	}
	// 7: the failures removed nothing — s still reuses its binding to a.
	if r := got.Results[7]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("binding must survive failed releases: %+v", r)
	}
	// 8: the rotation still continues just after a, the last actual rotation.
	if r := got.Results[8]; !r.OK || r.InstanceID != "b" {
		t.Fatalf("rotation must survive failed releases: %+v", r)
	}
}

// TestReleaseSessionTrimsServiceAndKey locks that the service name and the
// session key are both trimmed before use: a padded release targets the same
// service and the same session as the unpadded binding.
func TestReleaseSessionTrimsServiceAndKey(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"release_session","service":"  svc  ","expectedRevision":1,"sessionKey":"  s  "},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"}
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
	// 3: bind s -> a (cursor rests on a).
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("bind: %+v", r)
	}
	// 4: the padded release trims to svc/s and removes the binding.
	if r := got.Results[4]; !r.OK || !r.Changed || r.Service != "svc" || r.Revision != 1 {
		t.Fatalf("padded release: %+v", r)
	}
	// 5: the key rotates again from just after a, landing on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" {
		t.Fatalf("select after padded release: %+v", r)
	}
}
