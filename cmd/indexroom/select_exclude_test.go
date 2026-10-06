package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file is the end-to-end guard for the select request's
// excludeInstanceIds field as decoded from JSON: the field is optional, an
// empty array selects as before, and an explicit null, a non-array token, a
// non-string element or a blank-after-trim id is that item's own invalid
// result naming excludeInstanceIds — checked before the revision comparison,
// moving no cursor, and never aborting the batch.

func TestSelectExcludeInstanceIds(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":["a","b"]},
		{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":[]},
		{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":[" a "]},
		{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":null},
		{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":"b"},
		{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":["b",3]},
		{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":["  "]},
		{"type":"select","service":"svc","expectedRevision":9,"excludeInstanceIds":null},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1,"excludeInstanceIds":["a","b","c"]}
	]}`
	out, code := runRegisterWith(t, input)
	if code == 0 {
		t.Fatalf("batch contains failures, exit code must be 1, output: %s", out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 14 {
		t.Fatalf("results: %+v", got.Results)
	}

	// Items 1-4 commit the service and three healthy instances.
	for i := 0; i < 4; i++ {
		if !got.Results[i].OK {
			t.Fatalf("setup item %d should succeed: %+v", i+1, got.Results[i])
		}
	}

	// Item 5: the first selection excludes a and b, so the only candidate is
	// c; the cursor rests on c.
	if r := got.Results[4]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("item 5 should choose c, got %+v", r)
	}
	// Item 6: an empty array selects exactly as before — wrapping past c to a.
	if r := got.Results[5]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" {
		t.Fatalf("item 6 should choose a, got %+v", r)
	}
	// Item 7: ids are trimmed; excluding a continues just after the cursor to b.
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" {
		t.Fatalf("item 7 should choose b, got %+v", r)
	}

	// Items 8-12: every malformed list is invalid with a reason naming
	// excludeInstanceIds, the service's current revision, and no target
	// fields — including item 12, whose revision also mismatches, because
	// field validation precedes the revision check.
	for i := 7; i < 12; i++ {
		r := got.Results[i]
		if r.OK || r.Error != "invalid" {
			t.Fatalf("item %d should be invalid, got %+v", i+1, r)
		}
		if !strings.Contains(r.Reason, "excludeInstanceIds") {
			t.Fatalf("item %d reason must name excludeInstanceIds, got %q", i+1, r.Reason)
		}
		if r.Revision != 1 {
			t.Fatalf("item %d must report the current revision, got %+v", i+1, r)
		}
		if r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
			t.Fatalf("item %d must fabricate no target, got %+v", i+1, r)
		}
	}

	// Item 13: the failures moved no cursor, so the plain rotation continues
	// just after b to c.
	if r := got.Results[12]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" {
		t.Fatalf("item 13 should choose c, got %+v", r)
	}
	// Item 14: excluding every healthy instance is no_healthy, and the reason
	// says the exclusion removed them all.
	if r := got.Results[13]; r.OK || r.Error != "no_healthy" ||
		!strings.Contains(r.Reason, "excludeInstanceIds") || r.Revision != 1 {
		t.Fatalf("item 14 should be no_healthy naming the exclusion, got %+v", r)
	}

	// The exclusions changed nothing durable: all three instances are still
	// registered and healthy at sequence 1, and the revision never moved.
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	for _, inst := range got.Services[0].Instances {
		if inst.Health != "healthy" || inst.Sequence != 1 {
			t.Fatalf("instance record changed by exclusion: %+v", inst)
		}
	}
}

// TestSelectExcludeInstanceIdsSession runs one session through the JSON
// surface: the bound instance is reused while it is healthy and not excluded,
// and excluding it falls back to the filtered rotation and rebinds only on
// success.
func TestSelectExcludeInstanceIdsSession(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"k"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"k","excludeInstanceIds":["a"]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"k"},
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
	if len(got.Results) != 7 {
		t.Fatalf("results: %+v", got.Results)
	}

	// Item 4 binds k -> a through the normal rotation.
	if r := got.Results[3]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("item 4 should bind a, got %+v", r)
	}
	// Item 5 excludes the bound instance: the request falls back to the
	// filtered rotation (cursor on a, candidates {b}) and rebinds k -> b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" {
		t.Fatalf("item 5 should fall back to b, got %+v", r)
	}
	// Item 6 reuses the new binding b without a list.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" {
		t.Fatalf("item 6 should reuse b, got %+v", r)
	}
	// Item 7: the plain rotation continues just after the last real rotation
	// (b) and wraps to a — the exclusion removed nothing durably.
	if r := got.Results[6]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" {
		t.Fatalf("item 7 should choose a, got %+v", r)
	}
}
