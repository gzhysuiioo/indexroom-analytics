package indexroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// This file pins the behavior that the snapshot's JSON text is a
// serialization, not a wire format with one fixed layout. The same legal
// snapshot written with reordered object fields, added layout whitespace, or
// equivalent string escapes must restore the very same chain — same heights,
// hashes, parent links, and transaction order — while structural changes (a
// field written twice, the blocks array reversed) stay invalid and a rejected
// restore never touches the existing main chain.

// uescape renders the code point r as it appears in JSON text: a \uXXXX
// escape inside the BMP, or the surrogate-pair form above it. It lets the
// hand-written snapshots spell an identifier differently from the literal
// form while denoting the same decoded string.
func uescape(r rune) string {
	if r <= 0xFFFF {
		return fmt.Sprintf("\\u%04x", r)
	}
	r -= 0x10000
	high := 0xD800 + int(r>>10)
	low := 0xDC00 + int(r&0x3FF)
	return fmt.Sprintf("\\u%04x\\u%04x", high, low)
}

// prettyPrint re-renders canonical snapshot text with two-space indentation
// and a trailing newline, using the standard library so no production code
// helps manufacture the "reshaped" input.
func prettyPrint(t *testing.T, canonical string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(canonical), "", "  "); err != nil {
		t.Fatalf("setup: indent snapshot failed: %v", err)
	}
	buf.WriteByte('\n')
	return buf.String()
}

// shuffledV1Snapshot is a hand-written version-1 snapshot whose text departs
// from an export in every tolerated way at once:
//   - blocks precedes version at the top level (tip comes in between);
//   - the fields inside each block object appear in an unusual order;
//   - the same identifiers are written partly as equivalent \uXXXX escapes
//     instead of literal characters (g, h, 1, 中);
//   - whitespace is added throughout (newlines, spaces around ':' and inside
//     the empty txs array).
//
// Decoded, it is a two-block chain: h1/genesis with transactions
// ["tx-中","other","tx-中",""] at height 1, and h2/h1 with one more "tx-中"
// occurrence at height 2.
func shuffledV1Snapshot() string {
	return "{\n  \"blocks\" : [\n" +
		"    { \"txs\" : [ \"tx-" + uescape('中') + "\", \"other\", \"tx-中\", \"\" ],\n" +
		"      \"parent\" : \"" + uescape('g') + "enesis\", \"hash\" : \"" + uescape('h') + "1\", \"height\" : 1 },\n" +
		"    { \"parent\" : \"h" + uescape('1') + "\", \"txs\" : [ \"tx-" + uescape('中') + "\" ], \"height\" : 2, \"hash\" : \"h2\" }\n" +
		"  ],\n  \"tip\" : 2,\n  \"version\" : 1\n}\n"
}

// shuffledV2Snapshot is the version-2 counterpart: tip precedes blocks, the
// block fields are reordered (timestamp first in block 1, last in block 2),
// the block-1 time is null while block 2 carries a real zero, and escapes are
// mixed into the identifiers.
func shuffledV2Snapshot() string {
	return "{\n  \"tip\" : 2,\n" +
		"  \"blocks\" : [\n" +
		"    { \"timestamp\" : null, \"hash\" : \"" + uescape('h') + "1\", \"parent\" : \"g\", \"height\" : 1, \"txs\" : [ \"a\", \"\" ] },\n" +
		"    { \"height\" : 2, \"hash\" : \"h2\", \"parent\" : \"h" + uescape('1') + "\", \"txs\" : [\"b\"], \"timestamp\" : 0 }\n" +
		"  ],\n  \"version\" : 2\n}\n"
}

// buildReferenceChain builds through the ordinary Append API the chain the
// hand-written reshaped version-1 snapshot is meant to decode to. Building it
// independently keeps the parser under test from being the oracle for
// itself.
func buildReferenceChain() *Index {
	index := New()
	appendRef := func(height int64, hash, parent string, txs []string, time *int64) {
		if err := index.Append(Block{Height: height, Hash: hash, Parent: parent, Txs: txs, Time: time}); err != nil {
			panic(fmt.Sprintf("setup append at height %d: %v", height, err))
		}
	}
	appendRef(1, "h1", "genesis", []string{"tx-中", "other", "tx-中", ""}, nil)
	appendRef(2, "h2", "h1", []string{"tx-中"}, nil)
	return index
}

// requireSameChain fails unless got and want describe the same main chain:
// tip, per-height content (height, hash, parent, transactions in order,
// timestamp presence and value), and the hash-to-height index.
func requireSameChain(t *testing.T, got, want *Index) {
	t.Helper()
	if got.Tip != want.Tip {
		t.Fatalf("tip=%d, want %d", got.Tip, want.Tip)
	}
	if len(got.Blocks) != len(want.Blocks) || len(got.ByHash) != len(want.ByHash) {
		t.Fatalf("chain size: blocks=%d byHash=%d, want blocks=%d byHash=%d",
			len(got.Blocks), len(got.ByHash), len(want.Blocks), len(want.ByHash))
	}
	for h := int64(1); h <= want.Tip; h++ {
		g, ok := got.Blocks[h]
		if !ok {
			t.Fatalf("missing block at height %d after restore", h)
		}
		w := want.Blocks[h]
		if g.Height != w.Height || g.Hash != w.Hash || g.Parent != w.Parent {
			t.Fatalf("height %d: got %+v, want %+v", h, g, w)
		}
		if !reflect.DeepEqual(g.Txs, w.Txs) {
			t.Fatalf("height %d txs=%q, want %q", h, g.Txs, w.Txs)
		}
		if (g.Time == nil) != (w.Time == nil) || (g.Time != nil && *g.Time != *w.Time) {
			t.Fatalf("height %d time=%v, want %v", h, g.Time, w.Time)
		}
		if got.ByHash[w.Hash] != h {
			t.Fatalf("by-hash lookup for %q = %d, want %d", w.Hash, got.ByHash[w.Hash], h)
		}
	}
}

// requireTxQuery runs an exact-identifier query and checks the hit positions,
// total occurrence count, and count of blocks holding a match.
func requireTxQuery(t *testing.T, index *Index, id string, wantHits []TxHit, total, matchedBlocks int64) {
	t.Helper()
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{id}})
	if err != nil {
		t.Fatalf("query %q failed: %v", id, err)
	}
	// A nil expectation means "no hits"; the scanner returns an empty
	// non-nil slice, so compare emptiness rather than slice identity.
	if (wantHits == nil && len(page.Hits) != 0) ||
		(wantHits != nil && !reflect.DeepEqual(page.Hits, wantHits)) {
		t.Fatalf("query %q hits=%+v, want %+v", id, page.Hits, wantHits)
	}
	if page.TotalMatches != total || page.MatchedBlocks != matchedBlocks {
		t.Fatalf("query %q: total=%d blocks=%d, want total=%d blocks=%d",
			id, page.TotalMatches, page.MatchedBlocks, total, matchedBlocks)
	}
}

func TestRestoreReshapedV1TextMatchesCanonicalChain(t *testing.T) {
	index := New()
	if err := index.Restore(strings.NewReader(shuffledV1Snapshot())); err != nil {
		t.Fatalf("reshaped snapshot rejected: %v", err)
	}
	requireSameChain(t, index, buildReferenceChain())

	// Querying by the decoded identifier finds every occurrence at its
	// original position: "tx-中" appears twice in block 1 and once in block 2,
	// the empty identifier at position 3, and "other" at position 1.
	requireTxQuery(t, index, "tx-中", []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "tx-中", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "tx-中", Position: 2},
		{Height: 2, BlockHash: "h2", TxID: "tx-中", Position: 0},
	}, 3, 2)
	requireTxQuery(t, index, "", []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "", Position: 3},
	}, 1, 1)
	requireTxQuery(t, index, "other", []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "other", Position: 1},
	}, 1, 1)

	// Case and whitespace inside an identifier are part of the identifier;
	// accepting text variants must not merge originally distinct ids.
	for _, other := range []string{" tx-中", "tx-中 ", "TX-中", "tx-中x"} {
		requireTxQuery(t, index, other, nil, 0, 0)
	}

	// Without a filter the transactions come back in the decoded order.
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	wantAll := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "tx-中", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "other", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "tx-中", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "", Position: 3},
		{Height: 2, BlockHash: "h2", TxID: "tx-中", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, wantAll) {
		t.Fatalf("unfiltered hits=%+v, want %+v", page.Hits, wantAll)
	}
	if page.TotalMatches != 5 || page.MatchedBlocks != 2 {
		t.Fatalf("unfiltered totals=%d/%d, want 5/2", page.TotalMatches, page.MatchedBlocks)
	}
}

func TestRestoreCanonicalAndReshapedTextProduceSameReexport(t *testing.T) {
	reference := buildReferenceChain()
	canonical := exportString(t, reference)

	fromCanonical := New()
	if err := fromCanonical.Restore(strings.NewReader(canonical)); err != nil {
		t.Fatal(err)
	}
	fromShuffled := New()
	if err := fromShuffled.Restore(strings.NewReader(shuffledV1Snapshot())); err != nil {
		t.Fatalf("reshaped snapshot rejected: %v", err)
	}
	againCanonical := exportString(t, fromCanonical)
	againShuffled := exportString(t, fromShuffled)

	// Same data → identical canonical text, regardless of the input layout.
	// The target is the reference export, not a byte-for-byte copy of the
	// hand-written indentation, field order, or escapes.
	if againCanonical != canonical || againShuffled != canonical {
		t.Fatalf("re-exports disagree on normalization:\nreference:  %s\ncanonical:  %s\nreshaped:   %s",
			canonical, againCanonical, againShuffled)
	}
	if strings.ContainsAny(againShuffled, "\n") {
		t.Fatalf("re-export kept hand-written whitespace: %s", againShuffled)
	}
	if strings.Contains(againShuffled, `\u`) {
		t.Fatalf("re-export carries input escape notation: %s", againShuffled)
	}
}

func TestRestorePrettyPrintedExportIsCanonicalAgain(t *testing.T) {
	reference := buildReferenceChain()
	canonical := exportString(t, reference)
	pretty := prettyPrint(t, canonical)
	if pretty == canonical {
		t.Fatal("setup: indented snapshot equals the canonical bytes")
	}
	index := New()
	if err := index.Restore(strings.NewReader(pretty)); err != nil {
		t.Fatalf("indented snapshot rejected: %v", err)
	}
	requireSameChain(t, index, reference)
	if got := exportString(t, index); got != canonical {
		t.Fatalf("re-export after indented restore differs:\n%s\nwant %s", got, canonical)
	}
}

func TestRestoreV1BlocksHaveNoTimeAndReexportStaysV1(t *testing.T) {
	index := New()
	if err := index.Restore(strings.NewReader(shuffledV1Snapshot())); err != nil {
		t.Fatalf("reshaped v1 snapshot rejected: %v", err)
	}
	for h := int64(1); h <= index.Tip; h++ {
		if index.Blocks[h].Time != nil {
			t.Fatalf("v1 height %d has time %v, want nil", h, index.Blocks[h].Time)
		}
	}
	raw := exportString(t, index)
	if !strings.HasPrefix(raw, `{"version":1,`) {
		t.Fatalf("v1 chain re-exported with another version: %s", raw)
	}
	if strings.Contains(raw, "timestamp") {
		t.Fatalf("v1 export carries a timestamp field: %s", raw)
	}
}

func TestRestoreV2NullDistinctFromZeroAndNormalizes(t *testing.T) {
	index := New()
	if err := index.Restore(strings.NewReader(shuffledV2Snapshot())); err != nil {
		t.Fatalf("reshaped v2 snapshot rejected: %v", err)
	}
	if got := index.Blocks[1].Time; got != nil {
		t.Fatalf("null timestamp restored as %v, want nil", got)
	}
	zero := index.Blocks[2].Time
	if zero == nil || *zero != 0 {
		t.Fatalf("zero timestamp restored as %v, want a pointer to 0", zero)
	}

	// Reordered fields and added whitespace are normalized away, but null and
	// the real zero are exported as distinct literals.
	want := `{"version":2,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":["a",""],"timestamp":null},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":["b"],"timestamp":0}` +
		`]}`
	if got := exportString(t, index); got != want {
		t.Fatalf("v2 re-export=%s\nwant %s", got, want)
	}
}

func TestRestoreV2AllNullReexportsAsV1(t *testing.T) {
	input := "{\n \"version\" : 2,\n \"blocks\" : [\n" +
		"  { \"timestamp\" : null, \"txs\" : [], \"parent\" : \"g\", \"hash\" : \"h1\", \"height\" : 1 },\n" +
		"  { \"height\" : 2, \"timestamp\" : null, \"hash\" : \"h2\", \"parent\" : \"h1\", \"txs\" : [] }\n" +
		" ],\n \"tip\" : 2\n}\n"
	index := New()
	if err := index.Restore(strings.NewReader(input)); err != nil {
		t.Fatalf("all-null v2 snapshot rejected: %v", err)
	}
	for h := int64(1); h <= 2; h++ {
		if index.Blocks[h].Time != nil {
			t.Fatalf("height %d time=%v, want nil", h, index.Blocks[h].Time)
		}
	}
	want := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":[]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[]}` +
		`]}`
	if got := exportString(t, index); got != want {
		t.Fatalf("all-null v2 re-exported as %s\nwant %s", got, want)
	}
}

func TestRestoreV2RealTimeAnywhereKeepsNullsForRest(t *testing.T) {
	// Height 2 carries a real zero; the blocks around it are null and must
	// stay null in the version-2 export rather than being zeroed.
	input := `{"version":2,"tip":3,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":null},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[],"timestamp":0},` +
		`{"height":3,"hash":"h3","parent":"h2","txs":[],"timestamp":42}` +
		`]}`
	index := New()
	if err := index.Restore(strings.NewReader(input)); err != nil {
		t.Fatalf("v2 snapshot rejected: %v", err)
	}
	if got := index.Blocks[1].Time; got != nil {
		t.Fatalf("height 1 time=%v, want nil", got)
	}
	want := `{"version":2,"tip":3,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":null},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[],"timestamp":0},` +
		`{"height":3,"hash":"h3","parent":"h2","txs":[],"timestamp":42}` +
		`]}`
	if got := exportString(t, index); got != want {
		t.Fatalf("re-export=%s\nwant %s", got, want)
	}
}

// existingChainForRejection is a populated chain every rejection test starts
// from, so it can prove not only that the error is ErrInvalidSnapshot but
// that the old chain survives intact — including any valid prefix the input
// document contained.
func existingChainForRejection(t *testing.T) *Index {
	t.Helper()
	return txChain(t, []string{"keep"}, []string{"also-keep"})
}

func TestRestoreRejectsDuplicateFieldHiddenByEscape(t *testing.T) {
	index := existingChainForRejection(t)
	blocks, byHash, tip := snapshot(index)
	before := exportString(t, index)

	// Block 1 is a fully valid legal prefix; block 2 writes one field twice,
	// with names whose text differs ("height" vs "height") but which
	// decode to the same key. Tolerating text variants must not hide the
	// duplication, and block 1 must not be left applied.
	input := `{"blocks":[` +
		`{"height":1,"hash":"n1","parent":"g","txs":["new1"]},` +
		`{"height":2,"heigh` + uescape('t') + `":2,"hash":"n2","parent":"n1","txs":[]}` +
		`],"tip":2,"version":1}`
	err := index.Restore(strings.NewReader(input))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("block-level duplicate: err=%v, want ErrInvalidSnapshot", err)
	}
	if !strings.Contains(err.Error(), "duplicate block field") {
		t.Fatalf("block-level duplicate: err=%v, want a duplicate-block-field reason", err)
	}

	// Same trick at the top level: "version" vs "version".
	topInput := `{"blocks":[],"tip":0,"version":1,"` + uescape('v') + `ersion":1}`
	err = index.Restore(strings.NewReader(topInput))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("top-level duplicate: err=%v, want ErrInvalidSnapshot", err)
	}
	if !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("top-level duplicate: err=%v, want a duplicate-field reason", err)
	}

	requireUnchanged(t, index, blocks, byHash, tip)
	if after := exportString(t, index); after != before {
		t.Fatalf("export differs after rejected restores:\n%s\n%s", before, after)
	}
	// The valid first block of the rejected document never entered the index.
	if _, leaked := index.ByHash["n1"]; leaked {
		t.Fatal("valid prefix block n1 was applied despite the rejected duplicate field")
	}
	if got := index.Blocks[1]; got.Hash != "h1" || got.Parent != "genesis" || !reflect.DeepEqual(got.Txs, []string{"keep"}) {
		t.Fatalf("height 1 no longer holds the old chain: %+v", got)
	}
}

func TestRestoreRejectsReversedBlocksArray(t *testing.T) {
	index := existingChainForRejection(t)
	blocks, byHash, tip := snapshot(index)
	before := exportString(t, index)

	// Array order is data, not typography: the consecutive blocks reversed
	// must be rejected even though every block object is individually valid.
	input := `{"version":1,"tip":2,"blocks":[` +
		`{"height":2,"hash":"r2","parent":"r1","txs":[]},` +
		`{"height":1,"hash":"r1","parent":"g","txs":[]}` +
		`]}`
	err := index.Restore(strings.NewReader(input))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
	}
	if !strings.Contains(err.Error(), "out of order") {
		t.Fatalf("err=%v, want it to report the block out of order", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
	if after := exportString(t, index); after != before {
		t.Fatalf("export differs after rejected restore:\n%s\n%s", before, after)
	}
	for _, leaked := range []string{"r1", "r2"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("input hash %q entered the index despite the rejection", leaked)
		}
	}
	// The old main chain is still the old main chain, in the old order.
	if index.Blocks[1].Hash != "h1" || index.Blocks[2].Parent != "h1" {
		t.Fatalf("old chain links were disturbed: %+v %+v", index.Blocks[1], index.Blocks[2])
	}
}

// TestRestoreKeepsCaseAndWhitespaceDistinctIdentifiers pins the point that
// accepting text variants never normalizes the decoded identifiers: ids that
// differ only in case or in interior/surrounding whitespace stay different
// transactions at different positions, each queryable on its own.
func TestRestoreKeepsCaseAndWhitespaceDistinctIdentifiers(t *testing.T) {
	// The first two ids differ only in case; the last pair differs only in
	// the surrounding whitespace; "" stays its own identifier as well.
	input := v1Chain(
		`{"height":1,"hash":"h1","parent":"g","txs":["Ab","ab","a b"," a b ",""]}`,
		`{"height":2,"hash":"h2","parent":"h1","txs":["AB"]}`,
	)
	index := New()
	if err := index.Restore(strings.NewReader(input)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if got := index.Blocks[1].Txs; !reflect.DeepEqual(got, []string{"Ab", "ab", "a b", " a b ", ""}) {
		t.Fatalf("identifiers were normalized: %q", got)
	}
	cases := []struct {
		id     string
		hits   []TxHit
		total  int64
		blocks int64
	}{
		{"Ab", []TxHit{{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 0}}, 1, 1},
		{"ab", []TxHit{{Height: 1, BlockHash: "h1", TxID: "ab", Position: 1}}, 1, 1},
		{"a b", []TxHit{{Height: 1, BlockHash: "h1", TxID: "a b", Position: 2}}, 1, 1},
		{" a b ", []TxHit{{Height: 1, BlockHash: "h1", TxID: " a b ", Position: 3}}, 1, 1},
		{"AB", []TxHit{{Height: 2, BlockHash: "h2", TxID: "AB", Position: 0}}, 1, 1},
		{"", []TxHit{{Height: 1, BlockHash: "h1", TxID: "", Position: 4}}, 1, 1},
		// Nothing folds into these: a trimmed/lowercased variant is absent.
		{" a b", nil, 0, 0},
		{"a b ", nil, 0, 0},
	}
	for _, tc := range cases {
		requireTxQuery(t, index, tc.id, tc.hits, tc.total, tc.blocks)
	}
	// The canonical export spells every distinct identifier verbatim.
	raw := exportString(t, index)
	for _, want := range []string{`"Ab","ab","a b"," a b ",""`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("export=%s, want it to preserve %s", raw, want)
		}
	}
}

// TestRestoreEscapeVariantsAreTheSameChain closes the equivalence loop with
// two whole documents differing only in whether the second block's hash and
// parent are written literally or as equivalent \uXXXX escapes: both restore
// to the same chain and re-export byte-identically.
func TestRestoreEscapeVariantsAreTheSameChain(t *testing.T) {
	mkInput := func(escaped bool) string {
		hash, parent := "h2", "h1"
		if escaped {
			hash = "h" + uescape('2')
			parent = "h" + uescape('1')
		}
		return v1Chain(
			`{"height":1,"hash":"h1","parent":"g","txs":["id-中","id-`+uescape('中')+`"]}`,
			`{"height":2,"hash":"`+hash+`","parent":"`+parent+`","txs":[]}`,
		)
	}
	var exports [2]string
	for i, escaped := range [2]bool{false, true} {
		index := New()
		if err := index.Restore(strings.NewReader(mkInput(escaped))); err != nil {
			t.Fatalf("restore (escaped=%v) failed: %v", escaped, err)
		}
		// Literal and escaped occurrences in one txs array are the same
		// decoded identifier; duplicates are kept in order.
		requireTxQuery(t, index, "id-中", []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "id-中", Position: 0},
			{Height: 1, BlockHash: "h1", TxID: "id-中", Position: 1},
		}, 2, 1)
		exports[i] = exportString(t, index)
	}
	if exports[0] != exports[1] {
		t.Fatalf("escape variants exported differently:\n%s\n%s", exports[0], exports[1])
	}
}
