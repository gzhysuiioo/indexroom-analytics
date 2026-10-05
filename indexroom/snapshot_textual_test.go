package indexroom

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This file is the regression guard for the relationship between a
// hand-written snapshot and the bytes Export produces: they are two textual
// spellings of one document, not two formats. Restore may accept a document
// whose spelling differs from the canonical export — top-level fields in a
// different order (blocks before version), block-object fields in any order,
// arbitrary layout whitespace, and the same identifier written either as a
// literal character or as an equivalent \uXXXX escape — but those are spelling
// differences only. They must not change block heights, hashes, parent links,
// or transaction order, and Restore performs no text normalization on the
// decoded values: case and whitespace inside an identifier stay significant,
// and duplicate/empty identifiers keep every occurrence. Re-export always
// emits one canonical spelling; it is not required to reproduce a hand-written
// document's indentation, field order, or escape style.
//
// Accepting textual variation must not relax validation: an object field
// written twice is rejected even when the two names decode to the same key
// through different escape spellings, and reversing the blocks array is a
// content error, never "whitespace" that Restore may rearrange. Either
// rejection leaves the existing main chain exactly as it was, valid input
// prefix included.
//
// Fixtures that need Unicode escapes are package-level vars assembled with
// the esc helper (see snapshot_string_test.go) rather than consts: a const
// expression cannot call a function, and composing them at runtime keeps the
// six escape bytes explicit while the document under test stays text.

// textCanonicalV1 is the canonical Export spelling of a three-block version-1
// chain carrying every identifier case the regression queries exercise: a
// duplicated identifier, an empty identifier, trailing and interior
// whitespace, a lowercase twin, and a non-ASCII hash linked by a later block.
const textCanonicalV1 = `{"version":1,"tip":3,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":["Ab","x","Ab","","Ab ","ab","A b","Ab"]},{"height":2,"hash":"链","parent":"h1","txs":["","Ab","Ab"]},{"height":3,"hash":"h3","parent":"链","txs":["ab","Ab"]}]}`

// textRewrittenV1 describes exactly the same chain as textCanonicalV1 with a
// different text spelling: blocks precedes version, every block object lists
// its fields in a different order, layout whitespace is inserted everywhere,
// trailing whitespace follows the object, and the SAME identifiers appear
// under both direct characters and equivalent Unicode escapes:
//   - block 1 txs: "Ab" literal at positions 0 and 7 but escaped at
//     position 2; "x" escaped at position 1; "ab" escaped at position 5;
//   - block 2 parent names block 1's literal "h1" hash via ASCII escapes;
//   - block 2 carries the 链 hash as direct characters; block 3 links to it
//     via the single escape that decodes to 链;
//   - "Ab" is half-escaped inside blocks 2 and 3, again alongside literals.
var textRewrittenV1 = `
  {
    "blocks" : [
      { "txs" : ["Ab", "` + esc("0078") + `", "` + esc("0041") + esc("0062") + `", "", "Ab ", "` + esc("0061") + esc("0062") + `", "A b", "Ab"],
        "parent" : "g",
        "height" : 1 ,
        "hash" : "h1" } ,
      { "parent": "` + esc("0068") + esc("0031") + `",
        "txs": ["", "` + esc("0041") + `b", "Ab"],
        "height": 2,
        "hash": "链" },
      { "hash": "h3",
        "txs": ["ab", "` + esc("0041") + `b"],
        "parent": "` + esc("94fe") + `",
        "height": 3 }
    ] ,
    "tip" : 3,
    "version" : 1
  }
` + " \t"

// textualV1Blocks is the chain both documents above decode to.
func textualV1Blocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"Ab", "x", "Ab", "", "Ab ", "ab", "A b", "Ab"}},
		{Height: 2, Hash: "链", Parent: "h1", Txs: []string{"", "Ab", "Ab"}},
		{Height: 3, Hash: "h3", Parent: "链", Txs: []string{"ab", "Ab"}},
	}
}

// requireSameChainData fails unless got holds exactly want's main chain: tip,
// per-height block content under sameBlock semantics (an empty tx list equals
// a missing one, but a missing timestamp never equals zero), and the by-hash
// index.
func requireSameChainData(t *testing.T, got, want *Index) {
	t.Helper()
	if got.Tip != want.Tip {
		t.Fatalf("tip=%d, want %d", got.Tip, want.Tip)
	}
	if len(got.Blocks) != len(want.Blocks) || len(got.ByHash) != len(want.ByHash) {
		t.Fatalf("index sizes differ: got blocks=%d byHash=%d, want blocks=%d byHash=%d",
			len(got.Blocks), len(got.ByHash), len(want.Blocks), len(want.ByHash))
	}
	for height := int64(1); height <= want.Tip; height++ {
		if !sameBlock(got.Blocks[height], want.Blocks[height]) {
			t.Fatalf("block at height %d differs:\ngot  %+v\nwant %+v", height, got.Blocks[height], want.Blocks[height])
		}
	}
	if !reflect.DeepEqual(got.ByHash, want.ByHash) {
		t.Fatalf("by-hash indexes differ:\ngot  %v\nwant %v", got.ByHash, want.ByHash)
	}
}

// requireSameTxAnswers fails unless a and b return the same ordered hits,
// totals, matched-block counts, pinned upper heights, and cursor endings for
// the query (followed through every page).
func requireSameTxAnswers(t *testing.T, a, b *Index, query TxQuery) {
	t.Helper()
	pagesA := collectPages(t, a, query)
	pagesB := collectPages(t, b, query)
	if len(pagesA) != len(pagesB) {
		t.Fatalf("query %+v: %d pages vs %d", query, len(pagesA), len(pagesB))
	}
	for i := range pagesA {
		pa, pb := pagesA[i], pagesB[i]
		if !reflect.DeepEqual(pa.Hits, pb.Hits) ||
			pa.TotalMatches != pb.TotalMatches ||
			pa.MatchedBlocks != pb.MatchedBlocks ||
			pa.ToHeight != pb.ToHeight ||
			(pa.NextCursor == "") != (pb.NextCursor == "") {
			t.Fatalf("query %+v page %d differs:\ngot  %+v\nwant %+v", query, i, pb, pa)
		}
	}
}

// requireExactTxAnswer fails unless a first page for query carries exactly the
// given hits, total, and matched-block count. The fixture chains are short
// enough for one page to hold every hit.
func requireExactTxAnswer(t *testing.T, index *Index, query TxQuery, wantHits []TxHit, wantTotal, wantBlocks int64) {
	t.Helper()
	page, err := index.QueryTxs(query)
	if err != nil {
		t.Fatalf("query %+v failed: %v", query, err)
	}
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("query %+v hits=%v, want %v", query, page.Hits, wantHits)
	}
	if page.TotalMatches != wantTotal || page.MatchedBlocks != wantBlocks {
		t.Fatalf("query %+v total=%d blocks=%d, want total=%d blocks=%d",
			query, page.TotalMatches, page.MatchedBlocks, wantTotal, wantBlocks)
	}
}

// The rewritten spelling must really differ from the canonical spelling in the
// ways this regression cares about; otherwise the fixture would prove nothing.
func TestTextualSnapshotFixturesAreGenuinelyDifferentSpellings(t *testing.T) {
	if textRewrittenV1 == textCanonicalV1 {
		t.Fatal("rewritten fixture must not be byte-identical to the canonical fixture")
	}
	for _, marker := range []string{esc("0041"), esc("0068"), esc("94fe")} {
		if !strings.Contains(textRewrittenV1, marker) {
			t.Fatalf("rewritten fixture must use a Unicode escape (%s): %s", marker, textRewrittenV1)
		}
	}
	if !strings.ContainsAny(textRewrittenV1, "\n\t") {
		t.Fatal("rewritten fixture must carry layout whitespace")
	}
	blocksAt := strings.Index(textRewrittenV1, `"blocks"`)
	versionAt := strings.Index(textRewrittenV1, `"version"`)
	if blocksAt < 0 || versionAt < 0 || blocksAt > versionAt {
		t.Fatal("rewritten fixture must place blocks before version")
	}
	for _, doc := range []string{textCanonicalV1, textRewrittenV1} {
		tip, blocks, err := parseSnapshot(strings.NewReader(doc))
		if err != nil {
			t.Fatalf("fixture must parse: %v", err)
		}
		if tip != 3 || len(blocks) != 3 {
			t.Fatalf("fixture parsed tip=%d blocks=%d, want 3/3", tip, len(blocks))
		}
	}
}

// Both spellings of the same legal snapshot restore to the same chain and the
// same query answers as ingesting the blocks directly: heights, hashes, parent
// links, and transaction order are identical even though block 2's parent and
// block 3's parent arrived as Unicode escapes. Version-1 blocks restore with
// no timestamp at all.
func TestRestoreCanonicalAndRewrittenV1SnapshotYieldSameChain(t *testing.T) {
	source := chain(t, textualV1Blocks()...)

	instances := map[string]*Index{"ingested": source}
	for name, doc := range map[string]string{
		"canonical layout": textCanonicalV1,
		"rewritten layout": textRewrittenV1,
	} {
		index := New()
		if err := index.Restore(strings.NewReader(doc)); err != nil {
			t.Fatalf("%s restore failed: %v", name, err)
		}
		instances[name] = index
	}

	for name, index := range instances {
		t.Run(name, func(t *testing.T) {
			requireSameChainData(t, index, source)
			// Every block of a version-1 restore carries a missing time.
			for height := int64(1); height <= 3; height++ {
				if index.Blocks[height].Time != nil {
					t.Fatalf("height %d time=%v, want missing after v1 restore", height, index.Blocks[height].Time)
				}
			}
			// The escaped parent links decoded to the literal hashes.
			if index.Blocks[2].Hash != "链" || index.Blocks[2].Parent != "h1" ||
				index.Blocks[3].Parent != "链" {
				t.Fatalf("escaped identifiers changed block linkage: %+v %+v", index.Blocks[2], index.Blocks[3])
			}
		})
	}

	// Queries use the decoded identifier: "Ab" matches both the literal
	// entries and the escaped ones, keeping every occurrence, its in-block
	// position, and the matched-block count.
	abHits := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 7},
		{Height: 2, BlockHash: "链", TxID: "Ab", Position: 1},
		{Height: 2, BlockHash: "链", TxID: "Ab", Position: 2},
		{Height: 3, BlockHash: "h3", TxID: "Ab", Position: 1},
	}
	emptyHits := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "", Position: 3},
		{Height: 2, BlockHash: "链", TxID: "", Position: 0},
	}
	allHits := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "x", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "", Position: 3},
		{Height: 1, BlockHash: "h1", TxID: "Ab ", Position: 4},
		{Height: 1, BlockHash: "h1", TxID: "ab", Position: 5},
		{Height: 1, BlockHash: "h1", TxID: "A b", Position: 6},
		{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 7},
		{Height: 2, BlockHash: "链", TxID: "", Position: 0},
		{Height: 2, BlockHash: "链", TxID: "Ab", Position: 1},
		{Height: 2, BlockHash: "链", TxID: "Ab", Position: 2},
		{Height: 3, BlockHash: "h3", TxID: "ab", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "Ab", Position: 1},
	}
	for _, name := range []string{"canonical layout", "rewritten layout"} {
		index := instances[name]
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{"Ab"}}, abHits, 6, 3)
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{""}}, emptyHits, 2, 2)
		requireExactTxAnswer(t, index, TxQuery{}, allHits, 13, 3)
		// Case and interior/trailing whitespace are part of the identifier:
		// textual tolerance must not merge the distinct spellings. "ab" is hit
		// twice: written escaped in block 1 and as direct characters in block 3.
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{"ab"}}, []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "ab", Position: 5},
			{Height: 3, BlockHash: "h3", TxID: "ab", Position: 0},
		}, 2, 2)
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{"Ab "}}, []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "Ab ", Position: 4},
		}, 1, 1)
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{"A b"}}, []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "A b", Position: 6},
		}, 1, 1)
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{"AB"}}, []TxHit{}, 0, 0)
		// A filtered query likewise sees the decoded, order-preserving data.
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{"Ab", "ab"}}, []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 0},
			{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 2},
			{Height: 1, BlockHash: "h1", TxID: "ab", Position: 5},
			{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 7},
			{Height: 2, BlockHash: "链", TxID: "Ab", Position: 1},
			{Height: 2, BlockHash: "链", TxID: "Ab", Position: 2},
			{Height: 3, BlockHash: "h3", TxID: "ab", Position: 0},
			{Height: 3, BlockHash: "h3", TxID: "Ab", Position: 1},
		}, 8, 3)
		// The rewritten spelling gives exactly the query behavior of the
		// canonical spelling and of direct ingestion.
		requireSameTxAnswers(t, index, source, TxQuery{})
		requireSameTxAnswers(t, index, instances["canonical layout"], TxQuery{TxIDs: []string{"Ab", "", "ab"}})
	}
}

// Re-export normalizes the text spelling: data-equal inputs produce the same
// canonical bytes, without reproducing the hand-written indentation, field
// order, or escapes.
func TestReexportCanonicalizesRewrittenV1Spelling(t *testing.T) {
	canonical := New()
	if err := canonical.Restore(strings.NewReader(textCanonicalV1)); err != nil {
		t.Fatal(err)
	}
	rewritten := New()
	if err := rewritten.Restore(strings.NewReader(textRewrittenV1)); err != nil {
		t.Fatal(err)
	}
	got := exportString(t, rewritten)
	if got != textCanonicalV1 {
		t.Fatalf("rewritten spelling re-exported as:\n%s\nwant canonical:\n%s", got, textCanonicalV1)
	}
	if other := exportString(t, canonical); other != got {
		t.Fatalf("data-equal inputs exported different bytes:\n%s\n%s", other, got)
	}
	// The canonical spelling carries the decoded characters and none of the
	// hand-written escape/whitespace spellings.
	for _, marker := range []string{esc("0041"), esc("94fe"), "\n", `"blocks" :`} {
		if strings.Contains(got, marker) {
			t.Fatalf("canonical export retained hand-written spelling %q: %s", marker, got)
		}
	}
	if !strings.Contains(got, `"链"`) {
		t.Fatalf("canonical export must carry the decoded non-ASCII hash: %s", got)
	}
}

// textCanonicalV2 is the canonical version-2 spelling: a missing time as null
// on block 1, a real zero second on block 2, and a positive time on block 3.
const textCanonicalV2 = `{"version":2,"tip":3,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":["Ab","Ab",""],"timestamp":null},{"height":2,"hash":"h2","parent":"h1","txs":[],"timestamp":0},{"height":3,"hash":"h3","parent":"h2","txs":[],"timestamp":1700000000}]}`

// textRewrittenV2 is the same version-2 chain with blocks first, fields
// reordered (timestamp moves within each object), whitespace, block 2's
// parent written as ASCII escapes, and block 1's duplicate "Ab" written once
// literally and once as an escape.
var textRewrittenV2 = `{
  "blocks" : [
    { "timestamp": null,
      "txs": ["Ab", "` + esc("0041") + `b", ""],
      "parent": "g",
      "hash": "h1",
      "height": 1 },
    { "height": 2,
      "timestamp": 0,
      "parent": "` + esc("0068") + `1",
      "hash": "h2",
      "txs": [ ] },
    { "hash": "h3",
      "height": 3,
      "txs": [],
      "parent": "h2",
      "timestamp": 1700000000 }
  ],
  "tip": 3 , "version": 2
}`

// Null and a real zero second stay distinct after either spelling restores,
// and both spellings re-export to the same canonical version-2 bytes.
func TestRestoreCanonicalAndRewrittenV2SnapshotKeepNullAndZero(t *testing.T) {
	source := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"Ab", "Ab", ""}, Time: nil},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{}, Time: intptr(0)},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: nil, Time: intptr(1700000000)},
	)
	instances := map[string]*Index{"ingested": source}
	for name, doc := range map[string]string{
		"canonical layout": textCanonicalV2,
		"rewritten layout": textRewrittenV2,
	} {
		index := New()
		if err := index.Restore(strings.NewReader(doc)); err != nil {
			t.Fatalf("%s restore failed: %v", name, err)
		}
		instances[name] = index
	}

	for name, index := range instances {
		t.Run(name, func(t *testing.T) {
			requireSameChainData(t, index, source)
			if index.Blocks[1].Time != nil {
				t.Fatalf("block 1 time=%v, want nil (null)", index.Blocks[1].Time)
			}
			t0 := index.Blocks[2].Time
			if t0 == nil || *t0 != 0 {
				t.Fatalf("block 2 time=%v, want pointer to real zero, distinct from null", t0)
			}
			t1 := index.Blocks[3].Time
			if t1 == nil || *t1 != 1700000000 {
				t.Fatalf("block 3 time=%v, want 1700000000", t1)
			}
		})
	}

	for _, name := range []string{"canonical layout", "rewritten layout"} {
		index := instances[name]
		if raw := exportString(t, index); raw != textCanonicalV2 {
			t.Fatalf("%s re-exported as:\n%s\nwant:\n%s", name, raw, textCanonicalV2)
		}
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{"Ab"}}, []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 0},
			{Height: 1, BlockHash: "h1", TxID: "Ab", Position: 1},
		}, 2, 1)
		requireExactTxAnswer(t, index, TxQuery{TxIDs: []string{""}}, []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "", Position: 2},
		}, 1, 1)
		requireSameTxAnswers(t, index, source, TxQuery{})
	}
}

const (
	// textNullV2Compact is the all-null-timestamp version-2 chain in the
	// canonical field order.
	textNullV2Compact = `{"version":2,"tip":2,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":["Ab"],"timestamp":null},{"height":2,"hash":"h2","parent":"h1","txs":[],"timestamp":null}]}`
	// textNullV1Export is the canonical version-1 spelling of the same chain:
	// an all-null version-2 snapshot downgrades on export.
	textNullV1Export = `{"version":1,"tip":2,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":["Ab"]},{"height":2,"hash":"h2","parent":"h1","txs":[]}]}`
	// textZeroV2 has one real zero second alongside a missing time: the zero
	// alone forces version 2 and the missing time must stay null.
	textZeroV2 = `{"version":2,"tip":2,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":null},{"height":2,"hash":"h2","parent":"h1","txs":[],"timestamp":0}]}`
)

// textNullV2Rewritten is textNullV2Compact with blocks first, reordered
// fields, whitespace, and its one transaction written as an ASCII escape.
var textNullV2Rewritten = `{"blocks" : [
  { "timestamp": null, "txs": ["` + esc("0041") + `b"], "parent": "g", "hash": "h1", "height": 1 },
  { "height": 2, "hash": "h2", "parent": "h1", "txs": [], "timestamp": null }
], "tip": 2, "version": 2}`

// Re-export picks version 1 only when every block lacks a time (version-1
// input or an all-null version-2 input, either spelling). A single real zero
// second — or any other valid time — forces version 2 with null preserved for
// the missing times.
func TestReexportVersionSelectionAcrossSpellings(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"version 1 canonical", textCanonicalV1, textCanonicalV1},
		{"version 1 rewritten", textRewrittenV1, textCanonicalV1},
		{"version 2 all null canonical", textNullV2Compact, textNullV1Export},
		{"version 2 all null rewritten", textNullV2Rewritten, textNullV1Export},
		{"version 2 real zero keeps version 2", textZeroV2, textZeroV2},
		{"version 2 mixed times canonical", textCanonicalV2, textCanonicalV2},
		{"version 2 mixed times rewritten", textRewrittenV2, textCanonicalV2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := New()
			if err := index.Restore(strings.NewReader(tc.input)); err != nil {
				t.Fatalf("restore failed: %v", err)
			}
			if raw := exportString(t, index); raw != tc.want {
				t.Fatalf("export=%s\nwant=%s", raw, tc.want)
			}
		})
	}

	// The all-null restore loses no block: it only downgrades the layout.
	index := New()
	if err := index.Restore(strings.NewReader(textNullV2Rewritten)); err != nil {
		t.Fatal(err)
	}
	if index.Tip != 2 || index.Blocks[1].Txs[0] != "Ab" || index.Blocks[2].Parent != "h1" {
		t.Fatalf("all-null v2 restore lost data: %+v", index.Blocks)
	}

	// The zero-vs-null distinction is visible on the index, not just in bytes.
	zero := New()
	if err := zero.Restore(strings.NewReader(textZeroV2)); err != nil {
		t.Fatal(err)
	}
	if zero.Blocks[1].Time != nil {
		t.Fatalf("block 1 time=%v, want nil", zero.Blocks[1].Time)
	}
	if t0 := zero.Blocks[2].Time; t0 == nil || *t0 != 0 {
		t.Fatalf("block 2 time=%v, want real zero", t0)
	}
}

// textualInvalidPrefixBlocks is a two-block valid prefix shared by the
// rejection fixtures; none of its hashes may survive a failed restore.
const textualInvalidPrefixBlocks = `
    { "height": 1, "hash": "p1", "parent": "g", "txs": ["p"] },
    { "height": 2, "hash": "p2", "parent": "p1", "txs": [] }`

// dupEscapedBlockField writes block 3's "hash" field twice: once literally
// and once with the 'h' written as an ASCII escape — two textual names that
// decode to the same key. The duplicate sits in the LAST block, after a
// complete valid prefix.
var dupEscapedBlockField = `{
  "version": 1, "tip": 3,
  "blocks": [
` + textualInvalidPrefixBlocks + `,
    { "height": 3, "hash": "p3", "` + esc("0068") + `ash": "q3", "parent": "p2", "txs": [] }
  ]
}`

// dupEscapedTopField writes the top-level "version" field twice, the second
// time with its 'v' written as an ASCII escape, so the two key spellings are
// textually different but decode to the same name.
var dupEscapedTopField = `{"blocks":[{"height":1,"hash":"p1","parent":"g","txs":["p"]}],"tip":1,"version":1,"` + esc("0076") + `ersion":1}`

// reversedBlocks spells a legal three-block chain with the blocks array in
// descending order; pretty layout does not make array order typography.
const reversedBlocks = `{
  "version" : 1,
  "tip" : 3,
  "blocks" : [
    { "height": 3, "hash": "p3", "parent": "p2", "txs": [] },
    { "height": 2, "hash": "p2", "parent": "p1", "txs": [] },
    { "height": 1, "hash": "p1", "parent": "g", "txs": ["p"] }
  ]
}`

// Textual tolerance never relaxes validation: a field duplicated under an
// equivalent escaped name is rejected with ErrInvalidSnapshot, and a reversed
// blocks array is rejected as out of order rather than rearranged. Both
// rejections leave the existing main chain untouched, with no valid prefix
// applied from the rejected document.
func TestRestoreRejectsTextuallyHiddenDuplicateFieldAndReversedBlocks(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		reason    string
		newHashes []string
	}{
		{"duplicate block field via escape", dupEscapedBlockField, "duplicate block field", []string{"p1", "p2", "p3", "q3"}},
		{"duplicate top-level field via escape", dupEscapedTopField, "duplicate field", []string{"p1"}},
		{"reversed blocks array", reversedBlocks, "out of order", []string{"p1", "p2", "p3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			before := exportString(t, index)

			err := index.Restore(strings.NewReader(tc.input))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
			}
			if err == ErrInvalidSnapshot || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err=%v, want a specific reason containing %q", err, tc.reason)
			}

			// The preexisting main chain is unchanged and still queryable.
			requireUnchanged(t, index, blocks, byHash, tip)
			if after := exportString(t, index); after != before {
				t.Fatalf("export changed after rejected restore:\n%s\n%s", before, after)
			}
			page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}})
			if err != nil || page.TotalMatches != 1 {
				t.Fatalf("old chain query after rejection: page=%+v err=%v", page, err)
			}

			// Nothing from the rejected document was applied: not even the
			// valid prefix before the defect.
			for _, hash := range tc.newHashes {
				if _, ok := index.ByHash[hash]; ok {
					t.Fatalf("rejected input left hash %q indexed: %v", hash, index.ByHash)
				}
			}
			if leaked, err := index.QueryTxs(TxQuery{TxIDs: []string{"p"}}); err != nil || leaked.TotalMatches != 0 {
				t.Fatalf("rejected input left its prefix transactions: page=%+v err=%v", leaked, err)
			}
		})
	}
}
