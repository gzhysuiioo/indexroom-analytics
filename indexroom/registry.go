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

// Registration is a validated request that replaces a service's instance list.
type Registration struct {
	Service   string
	Revision  int
	Instances []Instance
}

// ServiceView is an immutable snapshot of one registered service.
type ServiceView struct {
	Service   string
	Revision  int
	Instances []Instance
}

// OutcomeKind classifies a failed registration.
type OutcomeKind string

// Outcome kinds.
const (
	OutcomeInvalid  OutcomeKind = "invalid"
	OutcomeConflict OutcomeKind = "conflict"
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

// Registry is an in-memory service instance registry.
type Registry struct {
	services map[string]*serviceState
}

type serviceState struct {
	revision  int
	instances map[string]string // instance id -> address
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
		st = &serviceState{revision: 1, instances: make(map[string]string, len(reg.Instances))}
		r.services[reg.Service] = st
		for _, inst := range reg.Instances {
			st.instances[inst.ID] = inst.Address
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
		st.instances = make(map[string]string, len(reg.Instances))
		for _, inst := range reg.Instances {
			st.instances[inst.ID] = inst.Address
		}
		st.revision++
	}
	return Outcome{Service: reg.Service, OK: true, Changed: changed, Revision: st.revision}
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
		instances := make([]Instance, 0, len(ids))
		for _, id := range ids {
			instances = append(instances, Instance{ID: id, Address: st.instances[id]})
		}
		views = append(views, ServiceView{Service: name, Revision: st.revision, Instances: instances})
	}
	return views
}

func sameInstances(current map[string]string, incoming []Instance) bool {
	if len(current) != len(incoming) {
		return false
	}
	for _, inst := range incoming {
		if addr, ok := current[inst.ID]; !ok || addr != inst.Address {
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
