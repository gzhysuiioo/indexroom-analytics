package indexroom

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
)

// txFilter is the single normalized form of a TxIDs filter list, shared by
// the paginated query, the time-window statistics, and cursor validation so
// that exact matching, duplicate handling, and set equivalence are defined
// exactly once.
//
// Identifiers match by exact string comparison: case, surrounding
// whitespace, and string boundaries are all significant. Order and
// duplicates inside the list are irrelevant — ["a","b","a"] and ["b","a"]
// normalize to the same filter. A nil set means "no filter" and matches
// every identifier; it is what both a missing and an explicitly empty TxIDs
// list produce, and it stays distinct from the one-element set [""], which
// matches empty identifiers only. Construction copies what it needs and
// never mutates or retains the caller's slice.
type txFilter struct {
	// set is nil for an unrestricted filter, otherwise the deduplicated
	// identifier set.
	set map[string]struct{}
	// sum is the canonical digest of the set: identical for every spelling
	// of the same set, different across sets. It is the value cursors pin.
	sum string
}

// newTxFilter normalizes a TxIDs list into a txFilter.
func newTxFilter(txIDs []string) txFilter {
	if len(txIDs) == 0 {
		return txFilter{sum: hashTxIDs(nil)}
	}
	set := make(map[string]struct{}, len(txIDs))
	unique := make([]string, 0, len(txIDs))
	for _, id := range txIDs {
		if _, ok := set[id]; ok {
			continue
		}
		set[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Strings(unique)
	return txFilter{set: set, sum: hashTxIDs(unique)}
}

// matches reports whether an identifier passes the filter. An unrestricted
// filter matches everything, including the empty identifier.
func (f txFilter) matches(id string) bool {
	if f.set == nil {
		return true
	}
	_, ok := f.set[id]
	return ok
}

// hashTxIDs hashes a sorted, deduplicated identifier list so that cursor
// validation is insensitive to duplicates and ordering. Each identifier is
// length-prefixed, keeping string boundaries significant: ["ab","c"] and
// ["a","bc"] hash differently. The empty list hashes the empty input, so an
// unrestricted filter never collides with a set containing "".
func hashTxIDs(sortedUnique []string) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, id := range sortedUnique {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(id)))
		h.Write(lenBuf[:])
		h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil))
}
