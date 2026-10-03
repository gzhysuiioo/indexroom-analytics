// Package indexroom implements chain ingestion with reorg handling.
package indexroom

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Instance is one discoverable service instance.
type Instance struct {
	ID      string
	Address string
}

// HealthState is the offline health observation of one instance.
// Health is driven entirely by submitted observations; no probing occurs.
type HealthState string

// Health states.
const (
	HealthUnknown   HealthState = "unknown"
	HealthHealthy   HealthState = "healthy"
	HealthUnhealthy HealthState = "unhealthy"
)

// InstanceView is a snapshot of one instance together with its health observation.
type InstanceView struct {
	ID       string
	Address  string
	Health   HealthState
	Sequence int64
	Reason   string
}

// Registration is a validated request that replaces a service's instance list.
type Registration struct {
	Service   string
	Revision  int
	Instances []Instance
}

// HealthUpdate is a validated health observation request.
type HealthUpdate struct {
	Service    string
	InstanceID string
	Revision   int
	Sequence   int64
	Healthy    bool
	Reason     string
}

// ServiceView is an immutable snapshot of one registered service.
type ServiceView struct {
	Service   string
	Revision  int
	Instances []InstanceView
}

// OutcomeKind classifies a failed request.
type OutcomeKind string

// Outcome kinds.
const (
	OutcomeInvalid   OutcomeKind = "invalid"
	OutcomeConflict  OutcomeKind = "conflict"
	OutcomeNotFound  OutcomeKind = "not_found"
	OutcomeStale     OutcomeKind = "stale"
	OutcomeNoHealthy OutcomeKind = "no_healthy"
)

// Outcome is the result of applying one registration against the registry.
type Outcome struct {
	Service  string
	OK       bool
	Changed  bool
	Revision int
	Kind     OutcomeKind
	Reason   string
	Expected int
	Actual   int
}

// HealthOutcome is the result of applying one health update against the registry.
type HealthOutcome struct {
	Service    string
	InstanceID string
	OK         bool
	Changed    bool
	Kind       OutcomeKind
	Reason     string
	Revision   int
	Expected   int
	Actual     int
	Sequence   int64
}

// Selection is a validated request that chooses one healthy target instance.
type Selection struct {
	Service  string
	Revision int
}

// SelectOutcome is the result of choosing one healthy instance.
//
// On success the chosen instance carries its id, address and current health
// sequence. A selection never mutates registrations, health records or the
// per-service rotation cursor on failure; on success it only advances that
// cursor to the chosen instance's id.
type SelectOutcome struct {
	Service    string
	OK         bool
	Kind       OutcomeKind
	Reason     string
	Revision   int
	Expected   int
	Actual     int
	InstanceID string
	Address    string
	Sequence   int64
}

// Registry is an in-memory service instance registry.
type Registry struct {
	services map[string]*serviceState
}

type serviceState struct {
	revision  int
	instances map[string]*instanceState // instance id -> state

	// cursor is the id returned by the last successful selection. Selection
	// starts at the smallest healthy id before any success (cursorSet == false)
	// and otherwise continues just after cursor. The cursor intentionally
	// survives registration replacements and health changes: removed instances
	// and recovered or newly joined instances are located by id in the current
	// healthy set without restarting the rotation.
	cursor    string
	cursorSet bool
}

type instanceState struct {
	address  string
	health   HealthState
	sequence int64
	reason   string
}

// newInstanceState returns a fresh observation: unknown with sequence 0.
func newInstanceState(address string) *instanceState {
	return &instanceState{address: address, health: HealthUnknown}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{services: map[string]*serviceState{}}
}

// RevisionOf returns the service's current revision, or 0 when it is unknown.
func (r *Registry) RevisionOf(service string) int {
	if st, ok := r.services[service]; ok {
		return st.revision
	}
	return 0
}

// validateServiceName trims and requires a non-empty service name. It is the
// field check shared by health observations and target selections; each
// operation still validates its own additional fields around it.
func validateServiceName(service string) (string, error) {
	name := strings.TrimSpace(service)
	if name == "" {
		return "", errInvalid("service name must not be empty")
	}
	return name, nil
}

// validateExpectedRevision requires the requested revision to be non-negative.
// It is the expectedRevision field check shared by health and select.
func validateExpectedRevision(revision int) error {
	if revision < 0 {
		return errInvalid("expectedRevision must be a non-negative integer")
	}
	return nil
}

// checkRevision is the revision gate shared by ApplyHealth and Select, applied
// after field validation and before either operation's own business rule. It
// resolves the service and its current revision (an unknown service is at
// revision 0) and reports whether expected matches it.
//
// Results the caller must honor:
//   - a non-nil state with ok == true: the gate passes, business logic runs;
//   - ok == false: a conflict carrying actual (and expected, held by the
//     caller), regardless of the service's instances or the request payload;
//   - a nil state with ok == true: the service is unknown and expected was 0,
//     so the caller reports its own not_found result.
//
// A registered service whose instance list is empty is still a present state
// at its real revision and is never treated as unknown here.
func (r *Registry) checkRevision(service string, expected int) (st *serviceState, actual int, ok bool) {
	st, exists := r.services[service]
	if !exists {
		return nil, 0, expected == 0
	}
	return st, st.revision, expected == st.revision
}

// revisionConflictReason is the shared wording for a health/select revision
// mismatch, naming the service, its current revision and the requested one.
func revisionConflictReason(service string, expected, actual int) string {
	return fmt.Sprintf("service %q is at revision %d, not %d", service, actual, expected)
}

// ValidateRegistration trims and validates one request without mutating the registry.
// Content validity is established before any revision check.
func (r *Registry) ValidateRegistration(service string, revision int, instances []Instance) (Registration, error) {
	name := strings.TrimSpace(service)
	if name == "" {
		return Registration{}, errInvalid("service name must not be empty")
	}
	if revision < 0 {
		return Registration{}, errInvalid("expectedRevision must be a non-negative integer")
	}
	seen := make(map[string]bool, len(instances))
	valid := make([]Instance, 0, len(instances))
	for _, inst := range instances {
		id := strings.TrimSpace(inst.ID)
		if id == "" {
			return Registration{}, errInvalid("instance id must not be empty")
		}
		if seen[id] {
			return Registration{}, errInvalid(fmt.Sprintf("duplicate instance id %q", id))
		}
		addr, err := parseAddress(inst.Address)
		if err != nil {
			return Registration{}, errInvalid(err.Error())
		}
		seen[id] = true
		valid = append(valid, Instance{ID: id, Address: addr})
	}
	return Registration{Service: name, Revision: revision, Instances: valid}, nil
}

// Apply checks the revision and, on match, replaces the service's instance list.
// A new service is created at revision 1 only when expectedRevision is 0.
// A matching registration with identical content succeeds without bumping revision.
func (r *Registry) Apply(reg Registration) Outcome {
	st, exists := r.services[reg.Service]
	if !exists {
		if reg.Revision != 0 {
			return Outcome{
				Service:  reg.Service,
				OK:       false,
				Kind:     OutcomeConflict,
				Reason:   fmt.Sprintf("service %q does not exist yet; expected revision must be 0, got %d", reg.Service, reg.Revision),
				Expected: reg.Revision,
				Actual:   0,
				Revision: 0,
			}
		}
		st = &serviceState{revision: 1, instances: make(map[string]*instanceState, len(reg.Instances))}
		r.services[reg.Service] = st
		for _, inst := range reg.Instances {
			st.instances[inst.ID] = newInstanceState(inst.Address)
		}
		return Outcome{Service: reg.Service, OK: true, Changed: true, Revision: 1}
	}
	if reg.Revision != st.revision {
		return Outcome{
			Service:  reg.Service,
			OK:       false,
			Kind:     OutcomeConflict,
			Reason:   fmt.Sprintf("service %q is at revision %d, not %d", reg.Service, st.revision, reg.Revision),
			Expected: reg.Revision,
			Actual:   st.revision,
			Revision: st.revision,
		}
	}
	changed := !sameInstances(st.instances, reg.Instances)
	if changed {
		// A replacement fully rebuilds the list, but an instance that keeps both
		// its id and its address retains its health observation. Anything new,
		// address-changed, or removed-then-readded starts from unknown at sequence 0.
		next := make(map[string]*instanceState, len(reg.Instances))
		for _, inst := range reg.Instances {
			if old, ok := st.instances[inst.ID]; ok && old.address == inst.Address {
				next[inst.ID] = old
			} else {
				next[inst.ID] = newInstanceState(inst.Address)
			}
		}
		st.instances = next
		st.revision++
	}
	return Outcome{Service: reg.Service, OK: true, Changed: changed, Revision: st.revision}
}

// ValidateHealth trims and validates one health observation without mutating
// the registry. Content validity is established before any revision check.
func (r *Registry) ValidateHealth(service, instanceID string, revision int, sequence int64, healthy bool, reason string) (HealthUpdate, error) {
	name, err := validateServiceName(service)
	if err != nil {
		return HealthUpdate{}, err
	}
	id := strings.TrimSpace(instanceID)
	if id == "" {
		return HealthUpdate{}, errInvalid("instance id must not be empty")
	}
	if err := validateExpectedRevision(revision); err != nil {
		return HealthUpdate{}, err
	}
	if sequence <= 0 {
		return HealthUpdate{}, errInvalid("sequence must be a positive integer")
	}
	normalizedReason := strings.TrimSpace(reason)
	if healthy {
		// A healthy observation carries no reason.
		normalizedReason = ""
	} else if normalizedReason == "" {
		return HealthUpdate{}, errInvalid("reason must not be empty when unhealthy")
	}
	return HealthUpdate{
		Service:    name,
		InstanceID: id,
		Revision:   revision,
		Sequence:   sequence,
		Healthy:    healthy,
		Reason:     normalizedReason,
	}, nil
}

// ApplyHealth checks the revision and records an offline observation.
//
// The shared revision gate (checkRevision) runs first: a mismatch is a conflict
// carrying the request and current revisions, even when the instance exists or
// the request carries a larger sequence. With a matching revision, a missing
// service or instance is not_found. Sequence handling:
//   - a sequence below the accepted one is stale and reports the current sequence;
//   - the same sequence with identical normalized status and reason succeeds
//     without changing state;
//   - the same sequence with different content is a conflict;
//   - a newer sequence writes the status and reason.
func (r *Registry) ApplyHealth(upd HealthUpdate) HealthOutcome {
	st, actual, ok := r.checkRevision(upd.Service, upd.Revision)
	if !ok {
		return HealthOutcome{
			Service:  upd.Service,
			OK:       false,
			Kind:     OutcomeConflict,
			Reason:   revisionConflictReason(upd.Service, upd.Revision, actual),
			Expected: upd.Revision,
			Actual:   actual,
			Revision: actual,
		}
	}
	if st == nil {
		return HealthOutcome{
			Service:  upd.Service,
			OK:       false,
			Kind:     OutcomeNotFound,
			Reason:   fmt.Sprintf("service %q does not exist", upd.Service),
			Revision: 0,
		}
	}
	cur, found := st.instances[upd.InstanceID]
	if !found {
		return HealthOutcome{
			Service:    upd.Service,
			InstanceID: upd.InstanceID,
			OK:         false,
			Kind:       OutcomeNotFound,
			Reason:     fmt.Sprintf("instance %q does not exist in service %q", upd.InstanceID, upd.Service),
			Revision:   st.revision,
		}
	}
	if upd.Sequence < cur.sequence {
		return HealthOutcome{
			Service:    upd.Service,
			InstanceID: upd.InstanceID,
			OK:         false,
			Kind:       OutcomeStale,
			Reason:     fmt.Sprintf("sequence %d is older than the current sequence %d", upd.Sequence, cur.sequence),
			Revision:   st.revision,
			Sequence:   cur.sequence,
		}
	}
	if upd.Sequence == cur.sequence {
		want := HealthUnhealthy
		if upd.Healthy {
			want = HealthHealthy
		}
		if cur.health == want && cur.reason == upd.Reason {
			return HealthOutcome{
				Service:    upd.Service,
				InstanceID: upd.InstanceID,
				OK:         true,
				Changed:    false,
				Revision:   st.revision,
				Sequence:   cur.sequence,
			}
		}
		return HealthOutcome{
			Service:    upd.Service,
			InstanceID: upd.InstanceID,
			OK:         false,
			Kind:       OutcomeConflict,
			Reason:     fmt.Sprintf("sequence %d already used with different health content", upd.Sequence),
			Revision:   st.revision,
			Sequence:   cur.sequence,
		}
	}
	// Newer sequence: record the observation. Health updates never bump the
	// registration revision.
	if upd.Healthy {
		cur.health = HealthHealthy
		cur.reason = ""
	} else {
		cur.health = HealthUnhealthy
		cur.reason = upd.Reason
	}
	cur.sequence = upd.Sequence
	return HealthOutcome{
		Service:    upd.Service,
		InstanceID: upd.InstanceID,
		OK:         true,
		Changed:    true,
		Revision:   st.revision,
		Sequence:   cur.sequence,
	}
}

// ValidateSelection trims and validates one select request without touching
// the registry. Content validity is established before any revision check.
func (r *Registry) ValidateSelection(service string, revision int) (Selection, error) {
	name, err := validateServiceName(service)
	if err != nil {
		return Selection{}, err
	}
	if err := validateExpectedRevision(revision); err != nil {
		return Selection{}, err
	}
	return Selection{Service: name, Revision: revision}, nil
}

// Select checks the revision and chooses one healthy instance for the service.
//
// The shared revision gate (checkRevision) runs first: a mismatch is a conflict
// carrying the request and current revisions (an unknown service is at
// revision 0). With a matching revision, a missing service is not_found. When
// the service exists but has no healthy instance the outcome is no_healthy and
// no address is fabricated.
//
// Healthy instances rotate per service by ascending instance id: the first
// success takes the smallest id and each later success continues just after
// the previously chosen id, wrapping to the smallest id past the end. With a
// single healthy instance it may be chosen repeatedly. Registrations and
// health changes alter the candidate set immediately but never reset the
// rotation; failed selections leave the cursor where it was.
func (r *Registry) Select(sel Selection) SelectOutcome {
	st, actual, ok := r.checkRevision(sel.Service, sel.Revision)
	if !ok {
		return SelectOutcome{
			Service:  sel.Service,
			OK:       false,
			Kind:     OutcomeConflict,
			Reason:   revisionConflictReason(sel.Service, sel.Revision, actual),
			Expected: sel.Revision,
			Actual:   actual,
			Revision: actual,
		}
	}
	if st == nil {
		return SelectOutcome{
			Service:  sel.Service,
			OK:       false,
			Kind:     OutcomeNotFound,
			Reason:   fmt.Sprintf("service %q does not exist", sel.Service),
			Revision: 0,
		}
	}

	healthy := make([]string, 0, len(st.instances))
	for id, cur := range st.instances {
		if cur.health == HealthHealthy {
			healthy = append(healthy, id)
		}
	}
	sort.Strings(healthy)
	if len(healthy) == 0 {
		return SelectOutcome{
			Service:  sel.Service,
			OK:       false,
			Kind:     OutcomeNoHealthy,
			Reason:   fmt.Sprintf("service %q has no healthy instance available", sel.Service),
			Revision: st.revision,
		}
	}

	chosen := healthy[0]
	if st.cursorSet {
		chosen = healthy[0]
		for _, id := range healthy {
			if id > st.cursor {
				chosen = id
				break
			}
		}
	}
	cur := st.instances[chosen]
	st.cursor = chosen
	st.cursorSet = true
	return SelectOutcome{
		Service:    sel.Service,
		OK:         true,
		Revision:   st.revision,
		InstanceID: chosen,
		Address:    cur.address,
		Sequence:   cur.sequence,
	}
}

// Snapshot returns all services sorted by name, with instances sorted by id.
func (r *Registry) Snapshot() []ServiceView {
	names := make([]string, 0, len(r.services))
	for name := range r.services {
		names = append(names, name)
	}
	sort.Strings(names)
	views := make([]ServiceView, 0, len(names))
	for _, name := range names {
		st := r.services[name]
		ids := make([]string, 0, len(st.instances))
		for id := range st.instances {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		instances := make([]InstanceView, 0, len(ids))
		for _, id := range ids {
			cur := st.instances[id]
			instances = append(instances, InstanceView{
				ID:       id,
				Address:  cur.address,
				Health:   cur.health,
				Sequence: cur.sequence,
				Reason:   cur.reason,
			})
		}
		views = append(views, ServiceView{Service: name, Revision: st.revision, Instances: instances})
	}
	return views
}

func sameInstances(current map[string]*instanceState, incoming []Instance) bool {
	if len(current) != len(incoming) {
		return false
	}
	for _, inst := range incoming {
		if old, ok := current[inst.ID]; !ok || old.address != inst.Address {
			return false
		}
	}
	return true
}

// parseAddress validates a host:port address: domains, IPv4 and bracketed IPv6.
// No network I/O is performed.
func parseAddress(raw string) (string, error) {
	addr := strings.TrimSpace(raw)
	if addr == "" {
		return "", fmt.Errorf("instance address must not be empty")
	}
	for _, r := range addr {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("instance address %q must not contain whitespace or control characters", addr)
		}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("instance address %q is not a host:port address: %v", addr, err)
	}
	if host == "" {
		return "", fmt.Errorf("instance address %q has an empty host", addr)
	}
	if !allDigits(port) {
		return "", fmt.Errorf("instance address %q port %q must be a decimal integer", addr, port)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return "", fmt.Errorf("instance address %q port %q must be a decimal integer", addr, port)
	}
	if n < 1 || n > 65535 {
		return "", fmt.Errorf("instance address %q port %d is out of range (1-65535)", addr, n)
	}
	if strings.HasPrefix(addr, "[") {
		ip := net.ParseIP(host)
		if ip == nil || !strings.Contains(host, ":") {
			return "", fmt.Errorf("instance address %q bracketed host is not a valid IPv6 address", addr)
		}
	} else if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return "", fmt.Errorf("instance address %q IPv6 addresses must be bracketed", addr)
		}
	} else if !isDomainName(host) {
		return "", fmt.Errorf("instance address %q host %q is not a valid domain, IPv4 or bracketed IPv6", addr, host)
	}
	return addr, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isDomainName validates a dotted hostname with alphanumeric/hyphen labels.
func isDomainName(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		n := len(label)
		if n == 0 || n > 63 {
			return false
		}
		if label[0] == '-' || label[n-1] == '-' {
			return false
		}
		for _, r := range label {
			if r == '-' ||
				(r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') {
				continue
			}
			return false
		}
	}
	return true
}
