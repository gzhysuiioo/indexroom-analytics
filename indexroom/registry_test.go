package indexroom

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustBatch(t *testing.T, reg *Registry, input string) []RegisterResult {
	t.Helper()
	results, err := reg.ApplyBatch([]byte(input))
	if err != nil {
		t.Fatalf("ApplyBatch returned error: %v", err)
	}
	return results
}

func TestRegisterCreatesServiceAtRevisionOne(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"orders","expectedRevision":0,"instances":[
			{"id":"b","address":"10.0.0.2:8080"},
			{"id":"a","address":"example.com:443"}
		]}
	]}`)
	r := results[0]
	if r.Kind != "success" || r.Revision != 1 || !r.Changed {
		t.Fatalf("unexpected result: %+v", r)
	}
	services := reg.Services()
	if len(services) != 1 || services[0].Service != "orders" || services[0].Revision != 1 {
		t.Fatalf("unexpected services: %+v", services)
	}
	// Final listing sorts instances by id regardless of submission order.
	if ids := []string{services[0].Instances[0].ID, services[0].Instances[1].ID}; ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("instances not sorted by id: %v", ids)
	}
}

func TestRegisterEmptyInstanceListKeepsServiceRecord(t *testing.T) {
	reg := NewRegistry()
	mustBatch(t, reg, `{"requests":[
		{"service":"quiet","expectedRevision":0,"instances":[]}
	]}`)
	services := reg.Services()
	if len(services) != 1 || services[0].Service != "quiet" ||
		services[0].Revision != 1 || len(services[0].Instances) != 0 {
		t.Fatalf("empty-list service not retained: %+v", services)
	}
}

func TestRegisterReplacesWholeListAndLeavesOtherServicesUntouched(t *testing.T) {
	reg := NewRegistry()
	mustBatch(t, reg, `{"requests":[
		{"service":"orders","expectedRevision":0,"instances":[
			{"id":"old","address":"old:1"},{"id":"keep","address":"keep:2"}
		]},
		{"service":"billing","expectedRevision":0,"instances":[{"id":"b1","address":"b:3"}]},
		{"service":"orders","expectedRevision":1,"instances":[{"id":"keep","address":"keep:2"}]}
	]}`)
	services := reg.Services()
	if len(services) != 2 {
		t.Fatalf("expected two services, got %+v", services)
	}
	orders := services[1] // billing < orders
	if orders.Service != "orders" || orders.Revision != 2 {
		t.Fatalf("unexpected orders record: %+v", orders)
	}
	if len(orders.Instances) != 1 || orders.Instances[0].ID != "keep" {
		t.Fatalf("unsubmitted instance not removed: %+v", orders.Instances)
	}
	billing := services[0]
	if billing.Revision != 1 || len(billing.Instances) != 1 || billing.Instances[0].ID != "b1" {
		t.Fatalf("other service changed unexpectedly: %+v", billing)
	}
}

func TestRegisterSameContentDifferentOrderIsUnchanged(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h:1"},{"id":"b","address":"h:2"}
		]},
		{"service":"svc","expectedRevision":1,"instances":[
			{"id":"b","address":"h:2"},{"id":"a","address":"h:1"}
		]}
	]}`)
	if results[1].Changed || results[1].Revision != 1 {
		t.Fatalf("reorder counted as a change: %+v", results[1])
	}
}

func TestContentChangesIncrementRevisionOnlyOnce(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"},{"id":"b","address":"h:2"}]},
		{"service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"h:9"}]},
		{"service":"svc","expectedRevision":2,"instances":[{"id":"a","address":"h:9"}]}
	]}`)
	if results[1].Revision != 2 || !results[1].Changed {
		t.Fatalf("change did not bump revision: %+v", results[1])
	}
	if results[2].Revision != 2 || results[2].Changed {
		t.Fatalf("identical content should be unchanged: %+v", results[2])
	}
}

func TestFillAndEmptyBothChange(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[]},
		{"service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"h:1"}]},
		{"service":"svc","expectedRevision":2,"instances":[]}
	]}`)
	for i, wantRevision := range []int{1, 2, 3} {
		if results[i].Revision != wantRevision || !results[i].Changed {
			t.Fatalf("request %d unexpected: %+v", i, results[i])
		}
	}
	if len(reg.Services()[0].Instances) != 0 || reg.Services()[0].Revision != 3 {
		t.Fatalf("service record not retained after emptying: %+v", reg.Services()[0])
	}
}

func TestRevisionConflictPreservesList(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"service":"svc","expectedRevision":5,"instances":[{"id":"z","address":"z:9"}]}
	]}`)
	r := results[1]
	if r.Kind != "conflict" || r.Expected != 5 || r.Revision != 1 {
		t.Fatalf("bad conflict result: %+v", r)
	}
	if !strings.Contains(r.Reason, "5") || !strings.Contains(r.Reason, "1") {
		t.Fatalf("reason should name expected and actual revision: %q", r.Reason)
	}
	services := reg.Services()
	if services[0].Revision != 1 || len(services[0].Instances) != 1 ||
		services[0].Instances[0].ID != "a" {
		t.Fatalf("conflict mutated the stored list: %+v", services)
	}
}

func TestNewServiceRejectsNonzeroExpectedRevision(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"ghost","expectedRevision":3,"instances":[]}
	]}`)
	r := results[0]
	if r.Kind != "conflict" || r.Expected != 3 || r.Revision != 0 {
		t.Fatalf("unexpected result: %+v", r)
	}
	if len(reg.Services()) != 0 {
		t.Fatalf("conflicting request created the service: %+v", reg.Services())
	}
}

func TestInvalidRequestLeavesNoTraceAndProcessingContinues(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"orders","expectedRevision":0,"instances":[
			{"id":"a","address":"h:1"},{"id":"a","address":"h:2"}
		]},
		{"service":"after","expectedRevision":0,"instances":[]}
	]}`)
	if results[0].Kind != "invalid" || !strings.Contains(results[0].Reason, "duplicate") {
		t.Fatalf("expected duplicate-id invalid failure, got %+v", results[0])
	}
	if len(reg.Services()) != 1 || reg.Services()[0].Service != "after" {
		t.Fatalf("failed request left state or later request skipped: %+v", reg.Services())
	}
	if HasFailure(results) != true {
		t.Fatal("HasFailure must be true")
	}
}

func TestInvalidInstanceHasNoPartialInstances(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"orders","expectedRevision":0,"instances":[
			{"id":"a","address":"h:1"},{"id":"b","address":"bad"}
		]}
	]}`)
	if results[0].Kind != "invalid" {
		t.Fatalf("expected invalid, got %+v", results[0])
	}
	if len(reg.Services()) != 0 {
		t.Fatalf("partial instances survived: %+v", reg.Services())
	}
}

func TestContentValidityCheckedBeforeRevisionConflict(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"service":"svc","expectedRevision":99,"instances":[{"id":"","address":"h:2"}]}
	]}`)
	if results[1].Kind != "invalid" {
		t.Fatalf("invalid content must be reported before the version conflict: %+v", results[1])
	}
}

func TestFieldsAreTrimmedAndMustBeNonBlank(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"  orders  ","expectedRevision":0,"instances":[
			{"id":"  a  ","address":"  example.com:80  "}
		]},
		{"service":"   ","expectedRevision":0,"instances":[]}
	]}`)
	if results[0].Kind != "success" || results[0].Service != "orders" {
		t.Fatalf("trimmed service not accepted: %+v", results[0])
	}
	svc := reg.Services()[0]
	if svc.Instances[0].ID != "a" || svc.Instances[0].Address != "example.com:80" {
		t.Fatalf("fields not trimmed before storage: %+v", svc.Instances[0])
	}
	if results[1].Kind != "invalid" || results[1].Revision != 0 {
		t.Fatalf("blank service should be invalid with revision zero: %+v", results[1])
	}
}

func TestDuplicateIDScopedPerService(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"a","expectedRevision":0,"instances":[{"id":"x","address":"h:1"}]},
		{"service":"b","expectedRevision":0,"instances":[{"id":"x","address":"h:2"}]}
	]}`)
	if results[0].Kind != "success" || results[1].Kind != "success" {
		t.Fatalf("same id across services should be allowed: %+v", results)
	}
}

func TestAddressValidation(t *testing.T) {
	valid := []string{
		"example.com:1",
		"svc.internal:8080",
		"127.0.0.1:65535",
		"0.0.0.0:443",
		"[::1]:8080",
		"[2001:db8::1]:443",
		"[::ffff:192.0.2.1]:443",
	}
	for _, addr := range valid {
		reg := NewRegistry()
		input := `{"requests":[{"service":"s","expectedRevision":0,"instances":[{"id":"i","address":"` + addr + `"}]}]}`
		results := mustBatch(t, reg, input)
		if results[0].Kind != "success" {
			t.Errorf("address %q rejected: %s", addr, results[0].Reason)
		}
	}
	invalid := []string{
		":8080",
		"host:",
		"host",
		"host:0",
		"host:65536",
		"host:-1",
		"host:08",
		"host:+1",
		"host:1.0",
		"host:abc",
		"::1:8080",
		"[::1]",
		"[::1]:",
		"[not-ip]:80",
		"[]:80",
		"host :80",
		"ho st:80",
		"host:99999999999999999999",
	}
	for _, addr := range invalid {
		reg := NewRegistry()
		input := `{"requests":[{"service":"s","expectedRevision":0,"instances":[{"id":"i","address":"` + addr + `"}]}]}`
		results := mustBatch(t, reg, input)
		if results[0].Kind != "invalid" {
			t.Errorf("address %q accepted: %+v", addr, results[0])
		}
	}
}

func TestRevisionFieldMustBeNonNegativeInteger(t *testing.T) {
	for _, value := range []string{"-1", "1.5", `"1"`, "true", "null"} {
		reg := NewRegistry()
		input := `{"requests":[{"service":"s","expectedRevision":` + value + `,"instances":[]}]}`
		results := mustBatch(t, reg, input)
		if results[0].Kind != "invalid" {
			t.Errorf("expectedRevision %s accepted: %+v", value, results[0])
		}
	}
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[{"service":"s","instances":[]}]}`)
	if results[0].Kind != "invalid" {
		t.Fatalf("missing expectedRevision accepted: %+v", results[0])
	}
}

func TestInstancesMustBeArrayOfCompleteObjects(t *testing.T) {
	for _, input := range []string{
		`{"requests":[{"service":"s","expectedRevision":0}]}`,
		`{"requests":[{"service":"s","expectedRevision":0,"instances":{}}]}`,
		`{"requests":[{"service":"s","expectedRevision":0,"instances":"x"}]}`,
		`{"requests":[{"service":"s","expectedRevision":0,"instances":[{"id":"a"}]}]}`,
		`{"requests":[{"service":"s","expectedRevision":0,"instances":[{"address":"h:1"}]}]}`,
		`{"requests":[{"service":"s","expectedRevision":0,"instances":[1]}]}`,
	} {
		reg := NewRegistry()
		results := mustBatch(t, reg, input)
		if results[0].Kind != "invalid" {
			t.Errorf("input accepted unexpectedly: %s -> %+v", input, results[0])
		}
		if len(reg.Services()) != 0 {
			t.Errorf("state changed for invalid input: %s", input)
		}
	}
}

func TestBatchRejectsMalformedDocumentEntirely(t *testing.T) {
	for _, input := range []string{
		`not json`,
		`{"requests":"x"}`,
		`{"requests":null}`,
		`{}`,
		`[]`,
		`{"requests":[1,2]}`, // array present but a non-object element is a per-item failure below, not here
	} {
		reg := NewRegistry()
		if input == `{"requests":[1,2]}` {
			results, err := reg.ApplyBatch([]byte(input))
			if err != nil {
				t.Fatalf("non-object element should be a per-item failure: %v", err)
			}
			if len(results) != 2 || results[0].Kind != "invalid" || results[1].Kind != "invalid" {
				t.Fatalf("expected two invalid item results, got %+v", results)
			}
			continue
		}
		_, err := reg.ApplyBatch([]byte(input))
		if err == nil {
			t.Errorf("malformed document accepted: %s", input)
		}
		if len(reg.Services()) != 0 {
			t.Errorf("malformed document changed state: %s", input)
		}
	}
}

func TestEmptyRequestsSucceeds(t *testing.T) {
	reg := NewRegistry()
	results, err := reg.ApplyBatch([]byte(`{"requests":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 || HasFailure(results) {
		t.Fatalf("empty requests should succeed: %+v", results)
	}
}

func TestServicesSortedByNameAndInstancesByID(t *testing.T) {
	reg := NewRegistry()
	mustBatch(t, reg, `{"requests":[
		{"service":"zeta","expectedRevision":0,"instances":[{"id":"z","address":"h:1"},{"id":"a","address":"h:2"}]},
		{"service":"alpha","expectedRevision":0,"instances":[{"id":"q","address":"h:3"},{"id":"m","address":"h:4"}]},
		{"service":"mid","expectedRevision":0,"instances":[{"id":"x","address":"h:5"}]}
	]}`)
	services := reg.Services()
	names := []string{services[0].Service, services[1].Service, services[2].Service}
	if names[0] != "alpha" || names[1] != "mid" || names[2] != "zeta" {
		t.Fatalf("services not sorted: %v", names)
	}
	if services[2].Instances[0].ID != "a" {
		t.Fatalf("instances not sorted by id: %+v", services[2].Instances)
	}
}

func TestResultJSONShapes(t *testing.T) {
	reg := NewRegistry()
	results := mustBatch(t, reg, `{"requests":[
		{"service":"svc","expectedRevision":0,"instances":[]},
		{"service":"svc","expectedRevision":2,"instances":[]}
	]}`)
	success, _ := json.Marshal(results[0])
	if !strings.Contains(string(success), `"status":"success"`) ||
		!strings.Contains(string(success), `"changed":true`) ||
		strings.Contains(string(success), "reason") {
		t.Fatalf("unexpected success JSON: %s", success)
	}
	conflict, _ := json.Marshal(results[1])
	var decoded map[string]any
	if err := json.Unmarshal(conflict, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["status"] != "conflict" || decoded["revision"] != float64(1) ||
		decoded["expectedRevision"] != float64(2) {
		t.Fatalf("unexpected conflict JSON: %s", conflict)
	}
	if _, ok := decoded["changed"]; ok {
		t.Fatalf("conflict result must not carry changed: %s", conflict)
	}
}
