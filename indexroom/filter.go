package indexroom

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
)

// txFilter is the single canonical form of a query's TxIDs argument, shared by
// the paginated scan, time-window statistics, and continuation-cursor
// validation so that exact matching, duplicate handling, and set equivalence
// live in one place.
//
// Identifiers match by exact string comparison: case, leading and trailing
// whitespace, and string boundaries all matter, so the sets {"ab","c"} and
// {"a","bc"} differ. Ordering and repetitions in the caller's list do not:
// ["a","b","a"] and ["b","a"] canonicalize to one set. A filter built from no
// identifiers (a nil or empty list) is unrestricted and matches every
// occurrence, including empty identifiers; that is distinct from the
// one-element set {""}, which matches only empty-identifier occurrences.
//
// A txFilter is read-only after construction. Construction only reads the
// caller's slice, never reorders, deduplicates, or otherwise mutates it.
type txFilter struct {
	// members holds each distinct identifier once. A nil map means the
	// filter is unrestricted; a populated list never canonicalizes to an
	// empty map, so {""} (one entry) cannot collapse into the nil case.
	members map[string]struct{}
	// digest summarizes the deduplicated, sorted set and is byte-identical
	// to the value historically stored in the v1 cursor's set field, so
	// existing cursors keep validating. It is set even for the unrestricted
	// filter, whose digest is the hash of the empty entry sequence.
	digest []byte
}

// newTxFilter canonicalizes an identifier list. The passed slice is never
// modified.
func newTxFilter(txIDs []string) txFilter {
	if len(txIDs) == 0 {
		// No entries at all: hash of the empty sequence, matching the value
		// cursors carried for an absent filter.
		return txFilter{digest: hashTxIDs(nil)}
	}
	members := make(map[string]struct{}, len(txIDs))
	unique := make([]string, 0, len(txIDs))
	for _, id := range txIDs {
		if _, ok := members[id]; ok {
			continue
		}
		members[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Strings(unique)
	return txFilter{members: members, digest: hashTxIDs(unique)}
}

// matches reports whether one on-chain occurrence belongs in the answer.
func (f txFilter) matches(id string) bool {
	if f.members == nil {
		return true
	}
	_, ok := f.members[id]
	return ok
}

// hexDigest is the cursor encoding of the set summary; two filters produce the
// same value exactly when their identifier sets are equal, so it is both the
// value minted into a first-page cursor and the continuation equivalence key.
func (f txFilter) hexDigest() string {
	return hex.EncodeToString(f.digest)
}

// hashTxIDs hashes the already deduplicated, sorted identifiers with an
// 8-byte length prefix each, so differently concatenated spellings cannot
// collide: {"ab","c"} serializes differently from {"a","bc"}.
func hashTxIDs(sorted []string) []byte {
	h := sha256.New()
	var lenBuf [8]byte
	for _, id := range sorted {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(id)))
		h.Write(lenBuf[:])
		h.Write([]byte(id))
	}
	return h.Sum(nil)
}
