// Package indexroom implements chain ingestion with reorg handling.
package indexroom

import (
	"fmt"
	"math"
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
//
// RevisionMismatch marks the conflict as a registration-revision mismatch:
// only that conflict carries the submitted expectedRevision and the service's
// actual revision as a comparable pair. Zero is a real value on both sides
// (an unknown service is at revision 0 and a new-service request submits 0),
// so callers serialize both fields whenever this is set and omit them on every
// other outcome, including other conflicts such as a reused health sequence.
type Outcome struct {
	Service          string
	OK               bool
	Changed          bool
	Revision         int
	Kind             OutcomeKind
	Reason           string
	Expected         int
	Actual           int
	RevisionMismatch bool
}

// HealthOutcome is the result of applying one health update against the
// registry. RevisionMismatch marks a registration-revision conflict: only
// that conflict reports the submitted expectedRevision and the service's
// actual revision. A same-sequence/different-content health conflict is a
// conflict too but leaves it false, so the two revision comparison fields
// stay omitted and the failure is not misread as a stale registration.
type HealthOutcome struct {
	Service          string
	InstanceID       string
	OK               bool
	Changed          bool
	Kind             OutcomeKind
	Reason           string
	Revision         int
	Expected         int
	Actual           int
	RevisionMismatch bool
	Sequence         int64
}

// Selection is a validated request that chooses one healthy target instance.
//
// SessionKey is the trimmed session identifier, empty when the request carries
// no session. A non-empty key binds the request to a per-service session: the
// first successful selection remembers the chosen instance for the key and
// later requests with the same key reuse that instance while it stays
// registered and healthy.
//
// ExcludeIDs is the normalized (trimmed, deduplicated) excludeInstanceIds
// list, empty when the request carries none. It scopes only this one request:
// the listed ids are skipped as targets but stay registered with their health
// records untouched, so a later request without the list may choose them
// again. Ids that are not registered are simply ignored.
type Selection struct {
	Service    string
	Revision   int
	SessionKey string
	ExcludeIDs []string
}

// SelectOutcome is the result of choosing one healthy instance.
//
// On success the chosen instance carries its id, address and current health
// sequence. A selection never mutates registrations, health records, the
// per-service rotation cursor or the session bindings on failure; on success
// it advances that cursor to the chosen instance's id — except when reusing a
// session binding, which leaves the cursor alone — and records the session
// binding when the request carried a session key that went through rotation.
type SelectOutcome struct {
	Service          string
	OK               bool
	Kind             OutcomeKind
	Reason           string
	Revision         int
	Expected         int
	Actual           int
	RevisionMismatch bool
	InstanceID       string
	Address          string
	Sequence         int64
}

// SessionRelease is a validated request that drops one session binding.
//
// SessionKey is the trimmed session identifier. The release is scoped to the
// named service only: the same key in another service keeps its binding.
type SessionRelease struct {
	Service    string
	Revision   int
	SessionKey string
}

// ReleaseOutcome is the result of releasing one session binding.
//
// A release never selects a target, so it carries no instance id, address or
// health sequence even on success. Changed is set only when a binding actually
// existed for the key; releasing an unbound key still succeeds with Changed
// left false. The release moves neither the per-service rotation cursor nor
// any binding other than the named one, and it never alters registrations,
// revisions or health records — whether the bound instance was removed, never
// observed healthy or is currently unhealthy does not block removing the
// binding itself.
type ReleaseOutcome struct {
	Service          string
	OK               bool
	Changed          bool
	Kind             OutcomeKind
	Reason           string
	Revision         int
	Expected         int
	Actual           int
	RevisionMismatch bool
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

	// sessions binds a session key to the instance id chosen for it by the
	// key's first successful selection. Bindings live per service, so the same
	// key in two services binds independently. A binding is consulted on every
	// selection carrying its key and is reused only while the instance is still
	// registered and healthy at selection time; otherwise the selection falls
	// back to the normal rotation and the binding is replaced only when that
	// rotation succeeds. Failed selections never create or rewrite a binding.
	sessions map[string]string
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

// validateServiceName is the service-identity field shared by every request:
// the trimmed name must not be blank.
func validateServiceName(service string) (string, error) {
	name := strings.TrimSpace(service)
	if name == "" {
		return "", errInvalid("service name must not be empty")
	}
	return name, nil
}

// maxRevision is the largest expectedRevision accepted on the running
// architecture. Revisions are carried by int, so a 32-bit build accepts
// 0..2147483647 and a 64-bit build 0..9223372036854775807. A value outside
// that range must be rejected from its raw submitted form; otherwise it wraps
// when narrowed to int (e.g. 4294967297 becoming 1 on a 32-bit build) and
// could match a revision it never equalled.
const maxRevision = math.MaxInt

// expectedRevisionRangeReason is the invalid reason shared by every request
// kind when a submitted expectedRevision falls outside the architecture's
// integer range. raw carries the submitted digits so the message reports the
// actual value even when it cannot be represented as int or int64.
func expectedRevisionRangeReason(raw string) string {
	return fmt.Sprintf("expectedRevision must be an integer between 0 and %d, got %s", maxRevision, raw)
}

// validateExpectedRevision is the revision field shared by every request. It
// judges the raw submitted value before it is narrowed to int: an int64 still
// holds values outside int's range on a 32-bit build, and converting
// 4294967297 or -4294967295 first would wrap either one into 1, letting a
// mismatching revision look current. A negative value or one above the
// platform's maximum is invalid with a reason stating the expectedRevision
// range. Callers check their remaining content fields around this one, so
// content validity is fully established before any revision comparison
// against the registry.
func validateExpectedRevision(revision int64) error {
	if revision < 0 || revision > maxRevision {
		return errInvalid(expectedRevisionRangeReason(strconv.FormatInt(revision, 10)))
	}
	return nil
}

// isDecimalIntegerText reports whether text is written as a decimal integer:
// an optional sign followed by one or more decimal digits, with no decimal
// point, exponent part or any other character. JSON number tokens are the
// expected input (whitespace never appears inside a raw token), but the check
// does not assume JSON, so a quoted string or a boolean literal fails it too.
//
// The shape must be established before strconv.ParseInt: ParseInt reports
// ErrRange as soon as its digit run overflows, which happens before it looks
// at a trailing '.' or exponent marker. A token such as
// 18446744073709551616.0 or 18446744073709551616e-20 would therefore surface
// as a range error even though it is a non-integer notation regardless of
// its numeric value; it must be reported as an integer-format problem instead.
func isDecimalIntegerText(text string) bool {
	i := 0
	if len(text) > 0 && (text[0] == '+' || text[0] == '-') {
		i = 1
	}
	if i == len(text) {
		return false
	}
	for ; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// parseIntegerToken converts the raw text of a field that must be written as a
// decimal integer, establishing notation first and int64 representability
// second. A non-integer token — a number with a decimal point or exponent
// part (whatever its value, including one that happens to equal an integer),
// a string, a boolean or any other text — fails with the field's integer-type
// reason, quoting the token exactly as submitted. Only a token actually
// written in integer notation whose magnitude cannot be carried by int64 gets
// the range reason; the callers supply both reason builders.
func parseIntegerToken(text, typeReason, rangeReason string) (int64, error) {
	if !isDecimalIntegerText(text) {
		return 0, errInvalid(typeReason)
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, errInvalid(rangeReason)
	}
	return n, nil
}

// ParseExpectedRevision converts the raw decimal text of a submitted
// expectedRevision into an int64 that still carries the submitted value. It
// establishes only the token's integer type: floats, strings, booleans and
// similar text get the integer-type error, while a decimal integer is returned
// without platform-specific narrowing. The architecture range (the 32-bit
// upper bound in particular) is enforced afterwards by
// validateExpectedRevision inside each Validate* method, so the long-standing
// field order (service name, then revision, then the remaining fields) is
// preserved. A token carrying a decimal point or exponent is a notation
// problem even when its integer part overflows (18446744073709551616.0 parses
// as a JSON number but is not integer notation) or when its value is tiny
// (18446744073709551616e-20 is below 1): neither becomes a range rejection or
// an accepted conversion. The one value category that gets the range reason is
// an integer-notation magnitude too large even for int64, reported from its
// raw text rather than as an overflowed number.
func ParseExpectedRevision(text string) (int64, error) {
	return parseIntegerToken(
		text,
		fmt.Sprintf("expectedRevision must be an integer, got %s", text),
		expectedRevisionRangeReason(text),
	)
}

// sequenceRangeReason is the invalid reason when a submitted sequence cannot
// be carried by int64. raw carries the submitted digits so the message reports
// the actual value even when it cannot be represented, which matters for the
// boundary 9223372036854775808 (one past the maximum) and larger magnitudes.
func sequenceRangeReason(raw string) string {
	return fmt.Sprintf("sequence must be an integer between 1 and %d, got %s", int64(math.MaxInt64), raw)
}

// ParseSequence converts the raw decimal text of a health observation's
// sequence into an int64 that carries the submitted value exactly. It
// establishes only the token's integer type and representability, mirroring
// ParseExpectedRevision: a number with a decimal point or exponent part is a
// notation problem whatever its value — 18446744073709551616.0 has an
// overflowed integer part and 18446744073709551616e-20 is below 1, but both
// are "sequence must be an integer" with the submitted text, never a range
// rejection or an accepted conversion — and strings, booleans and similar
// text get the same integer-type error. Only integer notation with a
// magnitude too large even for int64 — such as 9223372036854775808, one past
// the signed range — is reported from its raw text as a sequence range error.
// Parsing the raw token also keeps a non-integer token from failing the whole
// item's JSON decode: it arrives here as text and becomes that item's own
// invalid result, so later items in the batch still get their per-item
// outcome. The positive-value rule (sequence is per-instance and starts at 1)
// stays with ValidateHealth alongside the other content checks.
func ParseSequence(text string) (int64, error) {
	return parseIntegerToken(
		text,
		fmt.Sprintf("sequence must be an integer, got %s", text),
		sequenceRangeReason(text),
	)
}

// revisionFailure describes the common revision-gate result shared by health
// observations and selections. It carries no operation-specific fields: each
// operation maps it onto its own result so stale health sequences, missing
// instances and missing healthy targets never get mixed together.
//
// mismatch is true only for the revision-mismatch conflict, whose expected and
// actual revisions are both real values (each side may legitimately be 0). It
// is false for the matching-revision not_found, which reports no comparison.
type revisionFailure struct {
	kind     OutcomeKind
	reason   string
	revision int // current revision (0 for an unknown service)
	expected int
	actual   int
	mismatch bool
}

// checkServiceRevision applies the gate the health and select operations share.
// Content validation has already happened by the time it runs, so an invalid
// field never reaches it. It first looks the service up (an unknown service is
// at revision 0) and compares revisions: a mismatch is a conflict carrying the
// request's expectedRevision and the actual revision, even when the service or
// the targeted instance is missing — both values are flagged so callers emit
// them even when one side is 0. With matching revisions an unknown service is
// not_found and carries no comparison; a registered service with an empty
// instance list is still returned, so it compares by its real revision rather
// than being treated as absent. Returning nil means the gate passed and the
// caller owns the rest.
func (r *Registry) checkServiceRevision(service string, expected int) (*serviceState, *revisionFailure) {
	st, exists := r.services[service]
	actual := 0
	if exists {
		actual = st.revision
	}
	if expected != actual {
		return nil, &revisionFailure{
			kind:     OutcomeConflict,
			reason:   fmt.Sprintf("service %q is at revision %d, not %d", service, actual, expected),
			revision: actual,
			expected: expected,
			actual:   actual,
			mismatch: true,
		}
	}
	if !exists {
		return nil, &revisionFailure{
			kind:     OutcomeNotFound,
			reason:   fmt.Sprintf("service %q does not exist", service),
			revision: 0,
		}
	}
	return st, nil
}

// asHealth maps the shared revision gate failure onto a health result. The
// operation-specific fields (instance id and the current sequence) stay zero so
// they remain omitted exactly as health's own revision failures always have.
func (f *revisionFailure) asHealth(service string) HealthOutcome {
	return HealthOutcome{
		Service:          service,
		OK:               false,
		Kind:             f.kind,
		Reason:           f.reason,
		Revision:         f.revision,
		Expected:         f.expected,
		Actual:           f.actual,
		RevisionMismatch: f.mismatch,
	}
}

// asSelect maps the shared revision gate failure onto a selection result, which
// never fabricates an instance id, address or sequence on rejection.
func (f *revisionFailure) asSelect(service string) SelectOutcome {
	return SelectOutcome{
		Service:          service,
		OK:               false,
		Kind:             f.kind,
		Reason:           f.reason,
		Revision:         f.revision,
		Expected:         f.expected,
		Actual:           f.actual,
		RevisionMismatch: f.mismatch,
	}
}

// asRelease maps the shared revision gate failure onto a session-release
// result. It carries no target fields: a release never selects an instance.
func (f *revisionFailure) asRelease(service string) ReleaseOutcome {
	return ReleaseOutcome{
		Service:          service,
		OK:               false,
		Kind:             f.kind,
		Reason:           f.reason,
		Revision:         f.revision,
		Expected:         f.expected,
		Actual:           f.actual,
		RevisionMismatch: f.mismatch,
	}
}

// ValidateRegistration trims and validates one request without mutating the registry.
// revision is the raw submitted expectedRevision and is range-checked before it
// is narrowed to int. Content validity is established before any revision check.
func (r *Registry) ValidateRegistration(service string, revision int64, instances []Instance) (Registration, error) {
	name, err := validateServiceName(service)
	if err != nil {
		return Registration{}, err
	}
	if err := validateExpectedRevision(revision); err != nil {
		return Registration{}, err
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
	return Registration{Service: name, Revision: int(revision), Instances: valid}, nil
}

// Apply checks the revision and, on match, replaces the service's instance list.
// A new service is created at revision 1 only when expectedRevision is 0.
// A matching registration with identical content succeeds without bumping revision.
func (r *Registry) Apply(reg Registration) Outcome {
	st, exists := r.services[reg.Service]
	if !exists {
		if reg.Revision != 0 {
			return Outcome{
				Service:          reg.Service,
				OK:               false,
				Kind:             OutcomeConflict,
				Reason:           fmt.Sprintf("service %q does not exist yet; expected revision must be 0, got %d", reg.Service, reg.Revision),
				Expected:         reg.Revision,
				Actual:           0,
				Revision:         0,
				RevisionMismatch: true,
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
			Service:          reg.Service,
			OK:               false,
			Kind:             OutcomeConflict,
			Reason:           fmt.Sprintf("service %q is at revision %d, not %d", reg.Service, st.revision, reg.Revision),
			Expected:         reg.Revision,
			Actual:           st.revision,
			Revision:         st.revision,
			RevisionMismatch: true,
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
// the registry. revision is the raw submitted expectedRevision, range-checked
// before it is narrowed to int. Content validity is established before any
// revision check.
func (r *Registry) ValidateHealth(service, instanceID string, revision int64, sequence int64, healthy bool, reason string) (HealthUpdate, error) {
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
		Revision:   int(revision),
		Sequence:   sequence,
		Healthy:    healthy,
		Reason:     normalizedReason,
	}, nil
}

// ApplyHealth records an offline observation once the shared revision gate has
// passed.
//
// The gate is shared with Select (see checkServiceRevision): a revision
// mismatch is a conflict carrying the request and current revisions even when
// the service or the targeted instance is missing, and a matching request for
// an unknown service is not_found. Only after it does health apply its own
// rules:
//   - a missing instance is not_found;
//   - a sequence below the accepted one is stale and reports the current sequence;
//   - the same sequence with identical normalized status and reason succeeds
//     without changing state;
//   - the same sequence with different content is a conflict;
//   - a newer sequence writes the status and reason.
func (r *Registry) ApplyHealth(upd HealthUpdate) HealthOutcome {
	st, fail := r.checkServiceRevision(upd.Service, upd.Revision)
	if fail != nil {
		return fail.asHealth(upd.Service)
	}
	cur, ok := st.instances[upd.InstanceID]
	if !ok {
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

// validateSessionRequest is the content validation shared by select and
// release_session. It trims the service name and range-checks the raw
// expectedRevision before it is narrowed to int, then normalizes the session
// key:
//
//   - a nil key means the field was absent: an ordinary rotating selection is
//     legal for select (keyRequired == false), while release_session rejects
//     it with missingKeyReason because its key is mandatory;
//   - a present key is trimmed; keys equal after trimming name one session
//     within a service, and a blank-after-trim key is invalid with a reason
//     naming the sessionKey problem.
//
// The order mirrors every other request kind — service name, then the
// revision range, then the remaining content field — so content validity is
// fully established before any revision comparison against the registry.
// Null and non-string key tokens are a JSON concern handled by callers before
// this helper runs and never arrive here as a *string.
func validateSessionRequest(service string, revision int64, sessionKey *string, keyRequired bool, missingKeyReason string) (name string, rev int, key string, err error) {
	name, err = validateServiceName(service)
	if err != nil {
		return "", 0, "", err
	}
	if err = validateExpectedRevision(revision); err != nil {
		return "", 0, "", err
	}
	if sessionKey == nil {
		if keyRequired {
			return "", 0, "", errInvalid(missingKeyReason)
		}
		return name, int(revision), "", nil
	}
	key = strings.TrimSpace(*sessionKey)
	if key == "" {
		return "", 0, "", errInvalid("sessionKey must not be empty")
	}
	return name, int(revision), key, nil
}

// ValidateSelection trims and validates one select request without touching
// the registry. Content validity is established before any revision check.
func (r *Registry) ValidateSelection(service string, revision int64) (Selection, error) {
	return r.ValidateSelectionWithExclusions(service, revision, nil, nil)
}

// ValidateSelectionWithSession is ValidateSelection with an optional session
// key. A nil key means an ordinary rotating selection. A non-nil key is
// trimmed and must not be blank: keys that differ only in surrounding
// whitespace name the same session. revision is the raw submitted
// expectedRevision, range-checked before it is narrowed to int. Content
// validity is established before any revision check.
func (r *Registry) ValidateSelectionWithSession(service string, revision int64, sessionKey *string) (Selection, error) {
	return r.ValidateSelectionWithExclusions(service, revision, sessionKey, nil)
}

// ValidateSelectionWithExclusions is ValidateSelectionWithSession with an
// optional excludeInstanceIds list. A nil or empty list means an ordinary
// selection with no exclusions. Each id is trimmed before use and duplicate
// ids count as one; an id that is blank after trimming is invalid with a
// reason naming the excludeInstanceIds problem. Ids unknown to the registry
// are not an error — they simply exclude nothing. revision is the raw
// submitted expectedRevision, range-checked before it is narrowed to int.
// Content validity is fully established before any revision check, so a
// malformed list fails the item as invalid even when its revision would also
// conflict, and no valid id in the list can make the failed item partially
// take effect.
func (r *Registry) ValidateSelectionWithExclusions(service string, revision int64, sessionKey *string, excludeIDs []string) (Selection, error) {
	name, rev, key, err := validateSessionRequest(service, revision, sessionKey, false, "")
	if err != nil {
		return Selection{}, err
	}
	excludes, err := normalizeExcludeIDs(excludeIDs)
	if err != nil {
		return Selection{}, err
	}
	return Selection{Service: name, Revision: rev, SessionKey: key, ExcludeIDs: excludes}, nil
}

// normalizeExcludeIDs trims and deduplicates one excludeInstanceIds list.
// Each id is trimmed of surrounding whitespace before it names an instance;
// ids equal after trimming name the same instance and are kept once. An id
// that is blank after trimming is invalid with a reason naming the
// excludeInstanceIds problem. A nil or empty list normalizes to nil, which
// selects exactly as if the field had never been submitted.
func normalizeExcludeIDs(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(raw))
	ids := make([]string, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errInvalid("excludeInstanceIds must not contain an empty instance id")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// ValidateSessionRelease trims and validates one release_session request
// without touching the registry. Unlike a selection the sessionKey is
// mandatory: a nil (absent) key or a blank-after-trim key is invalid with a
// reason naming the sessionKey problem. revision is the raw submitted
// expectedRevision, range-checked before it is narrowed to int. The check
// order mirrors ValidateSelectionWithSession — service name, then the
// revision range, then the key — and content validity is fully established
// before any revision comparison against the registry.
func (r *Registry) ValidateSessionRelease(service string, revision int64, sessionKey *string) (SessionRelease, error) {
	const missingKeyReason = "sessionKey is required and must be a non-empty string"
	name, rev, key, err := validateSessionRequest(service, revision, sessionKey, true, missingKeyReason)
	if err != nil {
		return SessionRelease{}, err
	}
	return SessionRelease{Service: name, Revision: rev, SessionKey: key}, nil
}

// Select chooses one healthy instance for the service once the shared revision
// gate has passed.
//
// The gate is shared with ApplyHealth (see checkServiceRevision): a mismatch is
// a conflict carrying the request and current revisions (an unknown service is
// at revision 0), and a matching request for an unknown service is not_found.
// Only after it does select apply its own rule: an existing service with no
// healthy instance is no_healthy and no address is fabricated. A registered
// service with an empty instance list still compares by its real revision; the
// gate hands it through so the answer is no_healthy, not not_found.
//
// Healthy instances rotate per service by ascending instance id: the first
// success takes the smallest id and each later success continues just after
// the previously chosen id, wrapping to the smallest id past the end. With a
// single healthy instance it may be chosen repeatedly. Registrations and
// health changes alter the candidate set immediately but never reset the
// rotation; failed selections leave the cursor where it was.
//
// A request carrying a session key is sticky: while the key's bound instance
// is still registered and healthy at selection time, it is returned with its
// current address and latest accepted health sequence, and the rotation
// cursor does not move. A binding that points at a removed, unknown or
// unhealthy instance is ignored — an instance whose id survived an address
// change is unknown until a fresh observation at the new address, so its old
// health record never carries over — and the request falls back to the normal
// rotation, which on success advances the cursor and replaces the binding.
// The first selection for a key behaves exactly like an ordinary rotation
// success and additionally records the binding. Failed selections (including
// no_healthy) create no binding, rewrite none and move no cursor.
//
// A request carrying excludeInstanceIds skips the listed ids for this one
// request only: an excluded instance keeps its registration and health record
// and a later request without the list may choose it again. The exclusion
// filters the healthy candidate set before the rotation runs, so the rotation
// still continues just after the last actually rotated id — excluding that id
// does not reset the position, the next non-excluded healthy id after it is
// chosen, and only when none follows does the rotation wrap to the smallest
// candidate. A bound session whose instance is excluded falls back to this
// filtered rotation exactly as if the instance were unhealthy, rebinding only
// that key on success while every other session keeps its binding. When the
// service has healthy instances but every one of them is excluded, the
// no_healthy reason says the exclusion removed them all; when there is no
// healthy instance at all, the reason is the ordinary one. Either failure
// fabricates no target and moves nothing.
func (r *Registry) Select(sel Selection) SelectOutcome {
	st, fail := r.checkServiceRevision(sel.Service, sel.Revision)
	if fail != nil {
		return fail.asSelect(sel.Service)
	}

	excluded := make(map[string]bool, len(sel.ExcludeIDs))
	for _, id := range sel.ExcludeIDs {
		excluded[id] = true
	}

	// A bound session reuses its instance while it is registered, healthy and
	// not excluded by this request; the reuse reflects the instance's current
	// address and sequence and does not advance the rotation.
	if sel.SessionKey != "" {
		if id, bound := st.sessions[sel.SessionKey]; bound {
			if cur, ok := st.instances[id]; ok && cur.health == HealthHealthy && !excluded[id] {
				return SelectOutcome{
					Service:    sel.Service,
					OK:         true,
					Revision:   st.revision,
					InstanceID: id,
					Address:    cur.address,
					Sequence:   cur.sequence,
				}
			}
		}
	}

	healthy := make([]string, 0, len(st.instances))
	for id, cur := range st.instances {
		if cur.health == HealthHealthy {
			healthy = append(healthy, id)
		}
	}
	sort.Strings(healthy)

	candidates := healthy[:0]
	for _, id := range healthy {
		if !excluded[id] {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		reason := fmt.Sprintf("service %q has no healthy instance available", sel.Service)
		if len(healthy) > 0 {
			reason = fmt.Sprintf("service %q has no healthy instance available: all healthy instances are excluded by excludeInstanceIds", sel.Service)
		}
		return SelectOutcome{
			Service:  sel.Service,
			OK:       false,
			Kind:     OutcomeNoHealthy,
			Reason:   reason,
			Revision: st.revision,
		}
	}

	chosen := candidates[0]
	if st.cursorSet {
		chosen = candidates[0]
		for _, id := range candidates {
			if id > st.cursor {
				chosen = id
				break
			}
		}
	}
	cur := st.instances[chosen]
	st.cursor = chosen
	st.cursorSet = true
	if sel.SessionKey != "" {
		if st.sessions == nil {
			st.sessions = make(map[string]string)
		}
		st.sessions[sel.SessionKey] = chosen
	}
	return SelectOutcome{
		Service:    sel.Service,
		OK:         true,
		Revision:   st.revision,
		InstanceID: chosen,
		Address:    cur.address,
		Sequence:   cur.sequence,
	}
}

// ReleaseSession drops one session binding once the shared revision gate has
// passed.
//
// The gate is the same one health and select use (see
// checkServiceRevision): a revision mismatch is a conflict carrying the
// request and current revisions (an unknown service is at revision 0), and a
// matching request for an unknown service is not_found — including when
// expectedRevision is 0. Only after it does release do its own work.
//
// Releasing deletes at most the named key's binding in the named service. It
// never consults the bound instance's state: whether that instance was
// removed, never observed healthy, is currently unknown/unhealthy, or the
// service's instance list is empty cannot keep the binding in place, since
// the binding is only a remembered id. Releasing a key that has no binding
// still succeeds with Changed left false. No target is chosen, so the result
// carries no instance id, address or health sequence.
//
// The release moves and resets neither the rotation cursor nor any other
// session's binding (including another session bound to the same instance and
// the same key in another service), and it never alters the instance list,
// the registration revision or a health record. The next selection with the
// released key follows the first-binding rule: it rotates from just after the
// last actually rotated position and records a fresh binding only on success.
func (r *Registry) ReleaseSession(rel SessionRelease) ReleaseOutcome {
	st, fail := r.checkServiceRevision(rel.Service, rel.Revision)
	if fail != nil {
		return fail.asRelease(rel.Service)
	}
	_, bound := st.sessions[rel.SessionKey]
	if bound {
		delete(st.sessions, rel.SessionKey)
	}
	return ReleaseOutcome{
		Service:  rel.Service,
		OK:       true,
		Changed:  bound,
		Revision: st.revision,
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
// A domain host may end in exactly one root dot (e.g. "api.example.:8080"),
// which is retained in the returned text. No network I/O is performed.
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
// A domain may be written in its fully qualified form ending in exactly one
// root dot (e.g. "api.example."): the trailing dot marks the DNS root rather
// than separating two labels, so it is not itself a label, is not counted in
// the 253-character body limit, and no second trailing dot is allowed. An
// empty body (".") or any empty label in the body ("a..b.", "a.b..") is
// rejected just like any other malformed label. No name resolution occurs.
func isDomainName(host string) bool {
	body := host
	if strings.HasSuffix(body, ".") {
		body = body[:len(body)-1]
	}
	if body == "" || len(body) > 253 {
		return false
	}
	for _, label := range strings.Split(body, ".") {
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
