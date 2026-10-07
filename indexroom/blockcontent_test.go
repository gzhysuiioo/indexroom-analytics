package indexroom

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// legacyRangeFingerprint is the exact range serialization query.go carried
// before the block-content rule was consolidated. It is kept verbatim as an
// oracle so the refactored hashRangeBlockContent cannot silently change
// cursor fingerprint bytes; it is test-only and deliberately duplicates the
// historical code.
func legacyRangeFingerprint(index *Index, from, to int64) []byte {
	h := sha256.New()
	var lenBuf [8]byte
	writeString := func(s string) {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		binary.BigEndian.PutUint64(lenBuf[:], uint64(block.Height))
		h.Write(lenBuf[:])
		writeString(block.Hash)
		writeString(block.Parent)
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(block.Txs)))
		h.Write(lenBuf[:])
		for _, tx := range block.Txs {
			writeString(tx)
		}
		if block.Time == nil {
			h.Write([]byte{0})
		} else {
			h.Write([]byte{1})
			binary.BigEndian.PutUint64(lenBuf[:], uint64(*block.Time))
			h.Write(lenBuf[:])
		}
	}
	return h.Sum(nil)
}

// TestRangeFingerprintIsByteStable pins the consolidated range hash to both a
// golden constant and the pre-refactor serialization, so existing cursors
// keep validating.
func TestRangeFingerprintIsByteStable(t *testing.T) {
	idx := New()
	t0 := int64(0)
	t7 := int64(1700000007)
	idx.storeLocked(Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"ab", "c"}})
	idx.storeLocked(Block{Height: 2, Hash: "h2", Parent: "h1", Txs: nil, Time: &t0})
	idx.storeLocked(Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "bc", "", "a", " x "}, Time: &t7})

	got := hex.EncodeToString(idx.hashRangeBlockContent(1, 3))
	want := hex.EncodeToString(legacyRangeFingerprint(idx, 1, 3))
	if got != want {
		t.Fatalf("range fingerprint drifted from the historical cursor bytes:\n got %s\nwant %s", got, want)
	}

	// Rehashing an unchanged range is deterministic — cursors minted on one
	// call must match the check on the next.
	if again := hex.EncodeToString(idx.hashRangeBlockContent(1, 3)); again != got {
		t.Fatalf("range fingerprint is not deterministic: %s vs %s", again, got)
	}

	// Changing only one timestamp must change the range fingerprint even
	// though hashes and transactions are untouched.
	before := idx.hashRangeBlockContent(1, 3)
	changed := idx.Blocks[2]
	t99 := int64(99)
	changed.Time = &t99
	idx.Blocks[2] = changed
	if hex.EncodeToString(idx.hashRangeBlockContent(1, 3)) == hex.EncodeToString(before) {
		t.Fatal("timestamp-only change must change the range fingerprint")
	}
}

// TestEqualBlockContent covers the single equality rule shared by Append
// resubmission, Reorg dropped-height reporting, and (via the range hash)
// cursor invalidation.
func TestEqualBlockContent(t *testing.T) {
	sec := func(v int64) *int64 { return &v }
	base := func() Block {
		return Block{Height: 5, Hash: "h", Parent: "p", Txs: []string{"ab", "c"}, Time: nil}
	}

	same := []struct {
		name string
		a, b Block
	}{
		{"identical", base(), base()},
		{"nil txs equals empty txs",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: nil, Time: sec(3)},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{}, Time: sec(3)}},
		{"same timestamp seconds",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: nil, Time: sec(42)},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{}, Time: sec(42)}},
	}
	for _, tc := range same {
		t.Run("same/"+tc.name, func(t *testing.T) {
			if !equalBlockContent(tc.a, tc.b) {
				t.Fatalf("expected equal: %+v vs %+v", tc.a, tc.b)
			}
		})
	}

	diff := []struct {
		name string
		a, b Block
	}{
		{"height",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: nil},
			Block{Height: 2, Hash: "h", Parent: "p", Txs: nil}},
		{"hash",
			Block{Height: 1, Hash: "h1", Parent: "p", Txs: nil},
			Block{Height: 1, Hash: "h2", Parent: "p", Txs: nil}},
		{"parent",
			Block{Height: 1, Hash: "h", Parent: "p1", Txs: nil},
			Block{Height: 1, Hash: "h", Parent: "p2", Txs: nil}},
		{"missing time vs real zero",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: nil, Time: nil},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: nil, Time: sec(0)}},
		{"changed seconds",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: nil, Time: sec(7)},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: nil, Time: sec(8)}},
		{"swapped identifiers",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab", "c"}},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"c", "ab"}}},
		{"re-split identifiers",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab", "c"}},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"a", "bc"}}},
		{"added duplicate",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab", "c"}},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab", "c", "ab"}}},
		{"removed identifier",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab", "c"}},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab"}}},
		{"identifier case",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab", "c"}},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"AB", "c"}}},
		{"identifier whitespace",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab", "c"}},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{" ab ", "c"}}},
		{"empty identifier inserted",
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"ab", "c"}},
			Block{Height: 1, Hash: "h", Parent: "p", Txs: []string{"", "ab", "c"}}},
	}
	for _, tc := range diff {
		t.Run("diff/"+tc.name, func(t *testing.T) {
			if equalBlockContent(tc.a, tc.b) {
				t.Fatalf("expected different: %+v vs %+v", tc.a, tc.b)
			}
		})
	}

	// The whole-chain examples in the task description: ["ab","c"] and
	// ["a","bc"] are different content even though they have the same hash
	// field, count, and concatenated bytes.
	if equalBlockContent(base(), func() Block {
		b := base()
		b.Txs = []string{"a", "bc"}
		return b
	}()) {
		t.Fatal(`["ab","c"] must not equal ["a","bc"]`)
	}
}

// TestEqualityAgreesWithSingleBlockFingerprint ties the boolean rule to the
// bytes behind the range fingerprint: equal blocks share a digest and
// different blocks do not, and the range hash equals streaming those digests'
// inputs through one hasher.
func TestEqualityAgreesWithSingleBlockFingerprint(t *testing.T) {
	cases := []Block{
		{Height: 1, Hash: "h", Parent: "p", Txs: nil},
		{Height: 1, Hash: "h", Parent: "p", Txs: []string{}},
		{Height: 1, Hash: "h", Parent: "p", Txs: []string{"a"}},
		{Height: 1, Hash: "h", Parent: "p", Txs: nil, Time: func() *int64 { z := int64(0); return &z }()},
	}
	for i := range cases {
		for j := range cases {
			got := equalBlockContent(cases[i], cases[j])
			want := blockContentDigest(cases[i]) == blockContentDigest(cases[j])
			if got != want {
				t.Fatalf("equality/digest disagreement for cases %d,%d: equal=%v", i, j, got)
			}
		}
	}
}
