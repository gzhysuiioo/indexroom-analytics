// Package indexroom implements chain ingestion with reorg handling.
package indexroom

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// InstanceInput is one submitted service instance, decoded from JSON.
type InstanceInput struct {
	ID      json.RawMessage `json:"id"`
	Address json.RawMessage `json:"address"`
}

// RegisterRequest is one registration entry decoded from JSON.
type RegisterRequest struct {
	Service          json.RawMessage `json:"service"`
	ExpectedRevision json.RawMessage `json:"expectedRevision"`
	Instances        json.RawMessage `json:"instances"`
}

// Instance is a validated service instance stored in the registry.
type Instance struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}

// ServiceRecord is the current state of one registered service.
type ServiceRecord struct {
	Service   string     `json:"service"`
	Revision  int        `json:"revision"`
	Instances []Instance `json:"instances"`
}

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

func conflictReason(expected, actual int) string {
	return fmt.Sprintf("expected revision %d but service is at revision %d", expected, actual)
}

// Registry keeps service instance lists for one offline registration run.
// Each run starts from an empty registry.
type Registry struct {
	services map[string]*ServiceRecord
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{services: map[string]*ServiceRecord{}}
}

// RegisterResult is the per-request outcome, encoded with type-specific fields.
type RegisterResult struct {
	Kind     string // "success" | "invalid" | "conflict"
	Service  string
	Revision int    // revision still in effect after the attempt (0 when the service is absent)
	Changed  bool   // success only: whether the stored list changed
	Reason   string // failure only: human-readable explanation
	Expected int    // conflict only: revision the request asked for
}

// MarshalJSON renders a discriminated result. Success reports the current
// revision and whether the list changed; failures report a human-readable
// reason and the revision still in effect, with the stale expectation on a
// revision conflict.
func (r RegisterResult) MarshalJSON() ([]byte, error) {
	if r.Kind == "success" {
		return json.Marshal(struct {
			Status   string `json:"status"`
			Service  string `json:"service"`
			Revision int    `json:"revision"`
			Changed  bool   `json:"changed"`
		}{r.Kind, r.Service, r.Revision, r.Changed})
	}
	out := struct {
		Status           string `json:"status"`
		Service          string `json:"service"`
		Reason           string `json:"reason"`
		Revision         int    `json:"revision"`
		ExpectedRevision *int   `json:"expectedRevision,omitempty"`
	}{Status: r.Kind, Service: r.Service, Reason: r.Reason, Revision: r.Revision}
	if r.Kind == "conflict" {
		out.ExpectedRevision = &r.Expected
	}
	return json.Marshal(out)
}

// Apply validates one request and, when the revision matches, replaces the
// service's whole instance list. Invalid input is rejected before the
// revision is examined, so a failed request never creates a service, leaves
// partial instances, or consumes a revision.
func (reg *Registry) Apply(req RegisterRequest) RegisterResult {
	service, expected, instances, err := validateRequest(req)
	result := RegisterResult{Service: service}
	if err != nil {
		result.Kind = "invalid"
		result.Reason = err.Error()
		// Empty service names are useless in the per-item report and could
		// belong to any service; surface what was submitted otherwise.
		result.Revision = reg.revisionOf(service)
		return result
	}
	current, exists := reg.services[service]
	if !exists {
		if expected != 0 {
			result.Kind = "conflict"
			result.Expected, result.Revision = expected, 0
			result.Reason = conflictReason(expected, 0)
			return result
		}
		reg.services[service] = &ServiceRecord{Service: service, Revision: 1, Instances: instances}
		return RegisterResult{Kind: "success", Service: service, Revision: 1, Changed: true}
	}
	if expected != current.Revision {
		result.Kind = "conflict"
		result.Expected, result.Revision = expected, current.Revision
		result.Reason = conflictReason(expected, current.Revision)
		return result
	}
	changed := !sameInstanceSet(current.Instances, instances)
	if changed {
		current.Instances = instances
		current.Revision++
	}
	return RegisterResult{Kind: "success", Service: service, Revision: current.Revision, Changed: changed}
}

func (reg *Registry) revisionOf(service string) int {
	if service == "" {
		return 0
	}
	if current, ok := reg.services[service]; ok {
		return current.Revision
	}
	return 0
}

// Services returns all records sorted by service name, instances sorted by id.
func (reg *Registry) Services() []ServiceRecord {
	names := make([]string, 0, len(reg.services))
	for name := range reg.services {
		names = append(names, name)
	}
	sort.Strings(names)
	records := make([]ServiceRecord, 0, len(names))
	for _, name := range names {
		record := reg.services[name]
		instances := make([]Instance, len(record.Instances))
		copy(instances, record.Instances)
		sort.Slice(instances, func(i, j int) bool { return instances[i].ID < instances[j].ID })
		records = append(records, ServiceRecord{Service: name, Revision: record.Revision, Instances: instances})
	}
	return records
}

// ApplyBatch parses and processes requests strictly in their submitted order.
// Malformed JSON or a non-array requests field rejects the whole input
// before the registry is touched; a single element that is not an object is
// reported as one failed registration and the remaining elements still run.
func (reg *Registry) ApplyBatch(data []byte) ([]RegisterResult, error) {
	var input struct {
		Requests json.RawMessage `json:"requests"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("input is not a valid JSON object: %w", err)
	}
	if len(input.Requests) == 0 {
		return nil, fmt.Errorf("input is missing the requests array")
	}
	var rawRequests []json.RawMessage
	if err := json.Unmarshal(input.Requests, &rawRequests); err != nil || rawRequests == nil {
		return nil, fmt.Errorf("requests must be an array")
	}
	results := make([]RegisterResult, 0, len(rawRequests))
	for _, raw := range rawRequests {
		var req RegisterRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			results = append(results, RegisterResult{
				Kind:   "invalid",
				Reason: "request must be a JSON object with service, expectedRevision and instances",
			})
			continue
		}
		results = append(results, reg.Apply(req))
	}
	return results, nil
}

// HasFailure reports whether any per-request result is a failure.
func HasFailure(results []RegisterResult) bool {
	for _, result := range results {
		if result.Kind != "success" {
			return true
		}
	}
	return false
}

func validateRequest(req RegisterRequest) (service string, expected int, instances []Instance, err error) {
	service, err = requireTrimmedString(req.Service, "service")
	if err != nil {
		return "", 0, nil, err
	}
	expected, err = requireRevision(req.ExpectedRevision)
	if err != nil {
		return service, 0, nil, err
	}
	instances, err = requireInstances(req.Instances)
	if err != nil {
		return service, expected, nil, err
	}
	return service, expected, instances, nil
}

func requireTrimmedString(raw json.RawMessage, field string) (string, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return "", &validationError{fmt.Sprintf("%s must be a non-empty string", field)}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", &validationError{fmt.Sprintf("%s must not be blank", field)}
	}
	return value, nil
}

func requireRevision(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, &validationError{"expectedRevision is required"}
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 {
		return 0, &validationError{"expectedRevision must be a non-negative integer"}
	}
	return value, nil
}

func requireInstances(raw json.RawMessage) ([]Instance, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, &validationError{"instances must be an array"}
	}
	var entries []InstanceInput
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, &validationError{"instances must be an array of objects with id and address"}
	}
	seen := map[string]bool{}
	instances := make([]Instance, 0, len(entries))
	for i, entry := range entries {
		id, err := requireTrimmedString(entry.ID, "instance id")
		if err != nil {
			return nil, &validationError{fmt.Sprintf("instances[%d]: %v", i, err)}
		}
		if seen[id] {
			return nil, &validationError{fmt.Sprintf("instances[%d]: duplicate instance id %q", i, id)}
		}
		seen[id] = true
		address, err := requireAddress(entry.Address, i)
		if err != nil {
			return nil, err
		}
		instances = append(instances, Instance{ID: id, Address: address})
	}
	return instances, nil
}

func requireAddress(raw json.RawMessage, index int) (string, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return "", &validationError{fmt.Sprintf("instances[%d]: address must be a host:port string", index)}
	}
	value = strings.TrimSpace(value)
	_, portText, err := splitHostPort(value)
	if err != nil {
		return "", &validationError{fmt.Sprintf("instances[%d]: %v", index, err)}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || !isCanonicalDecimal(portText) {
		return "", &validationError{fmt.Sprintf("instances[%d]: port must be a decimal integer between 1 and 65535", index)}
	}
	return value, nil
}

func isCanonicalDecimal(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	// A decimal integer has no leading zeroes other than the single digit 0,
	// which is itself an out-of-range port.
	return len(text) == 1 || text[0] != '0'
}

// splitHostPort accepts host:port with a domain or IPv4 host, or [IPv6]:port.
// It performs no network access.
func splitHostPort(value string) (string, string, error) {
	if value == "" {
		return "", "", fmt.Errorf("address must be host:port")
	}
	if value[0] == '[' {
		closeBracket := strings.IndexByte(value, ']')
		if closeBracket < 0 || closeBracket == 1 {
			return "", "", fmt.Errorf("address has a malformed bracketed IPv6 host")
		}
		host := value[1:closeBracket]
		rest := value[closeBracket+1:]
		if len(rest) < 2 || rest[0] != ':' {
			return "", "", fmt.Errorf("address must be [ipv6]:port")
		}
		if ip := net.ParseIP(host); ip == nil || !strings.Contains(host, ":") {
			return "", "", fmt.Errorf("bracketed host must be a valid IPv6 address")
		}
		return host, rest[1:], nil
	}
	colon := strings.LastIndexByte(value, ':')
	if colon <= 0 || colon == len(value)-1 {
		return "", "", fmt.Errorf("address must be host:port")
	}
	if strings.Count(value, ":") > 1 {
		return "", "", fmt.Errorf("an IPv6 host must be enclosed in square brackets: [ipv6]:port")
	}
	host := value[:colon]
	port := value[colon+1:]
	if host == "" {
		return "", "", fmt.Errorf("host must not be empty")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return "", "", fmt.Errorf("host must not contain whitespace")
	}
	return host, port, nil
}

func sameInstanceSet(a, b []Instance) bool {
	if len(a) != len(b) {
		return false
	}
	byID := make(map[string]string, len(a))
	for _, inst := range a {
		byID[inst.ID] = inst.Address
	}
	for _, inst := range b {
		if existing, ok := byID[inst.ID]; !ok || existing != inst.Address {
			return false
		}
	}
	return true
}
