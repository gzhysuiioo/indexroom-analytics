package indexroom

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// badUTF8 is an otherwise ordinary string carrying one invalid byte.
const badUTF8 = "\xff"

// wtf8Surrogate is the WTF-8 encoding of a lone high surrogate
// (U+D800 -> ED A0 80): three bytes that look like UTF-8 but encode a
// surrogate, so utf8.ValidString rejects them.
var wtf8Surrogate = string([]byte{0xed, 0xa0, 0x80})

// literalEscape is plain text that looks like a JSON unicode escape: the
// eight characters \ u 0 0 4 1 s h, which must never be decoded so that it
// collides with the ordinary identifier "Ash".
const literalEscape = "\\u0041sh"

func TestAppendRejectsInvalidUTF8Identifiers(t *testing.T) {
	cases := map[string]struct {
		block Block
		want  []string // substrings the error must carry
	}{
		"bad hash on first block": {
			Block{Height: 1, Hash: "h" + badUTF8, Parent: "g"},
			[]string{"height 1", `"hash"`},
		},
		// The first block's parent is exempt from indexing, not from the
		// encoding requirement.
		"bad parent on first block": {
			Block{Height: 1, Hash: "h1", Parent: "g" + badUTF8},
			[]string{"height 1", `"parent"`},
		},
		"bad hash extending the tip": {
			Block{Height: 3, Hash: "h" + badUTF8 + "3", Parent: "h2"},
			[]string{"height 3", `"hash"`},
		},
		"bad parent extending the tip": {
			Block{Height: 3, Hash: "h3", Parent: "h2" + badUTF8},
			[]string{"height 3", `"parent"`},
		},
		"bad first tx identifier": {
			Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{badUTF8}},
			[]string{"height 3", `"txs"`, "element 0"},
		},
		"bad later tx identifier": {
			Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"ok", "fine", "t" + badUTF8}},
			[]string{"height 3", `"txs"`, "element 2"},
		},
		"wtf-8 lone surrogate in hash": {
			Block{Height: 3, Hash: "h" + wtf8Surrogate, Parent: "h2"},
			[]string{"height 3", `"hash"`},
		},
		"wtf-8 lone surrogate in tx id": {
			Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", wtf8Surrogate}},
			[]string{"height 3", `"txs"`, "element 1"},
		},
		"wtf-8 lone surrogate in first parent": {
			Block{Height: 1, Hash: "h1", Parent: wtf8Surrogate},
			[]string{"height 1", `"parent"`},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := chain(t,
				Block{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"keep"}},
				Block{Height: 2, Hash: "h2", Parent: "h1"},
			)
			blocks, byHash, tip := snapshot(index)
			err := index.Append(tc.block)
			if err == nil {
				t.Fatal("expected refusal of invalid UTF-8")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%q, want it to mention %s", err.Error(), want)
				}
			}
			requireUnchanged(t, index, blocks, byHash, tip)

			// Existing exact-match queries are untouched by the rejection.
			page, err := index.QueryTxs(TxQuery{TxIDs: []string{"keep"}})
			if err != nil {
				t.Fatalf("query after rejected append failed: %v", err)
			}
			if page.TotalMatches != 1 {
				t.Fatalf("matches=%d, want 1 after rejected append", page.TotalMatches)
			}
		})
	}
}

func TestAppendRejectsEncodingBeforeStructuralRules(t *testing.T) {
	// Encoding is reported even when the block would fail structural checks
	// as well; it is checked before the index is consulted.
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "g"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	blocks, byHash, tip := snapshot(index)
	err := index.Append(Block{Height: 9, Hash: "h" + badUTF8, Parent: "wrong"})
	if err == nil || !strings.Contains(err.Error(), `"hash"`) {
		t.Fatalf("err=%v, want the encoding error for hash", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestReorgRejectsInvalidUTF8Atomically(t *testing.T) {
	build := func(t *testing.T) *Index {
		return chain(t,
			Block{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"keep"}},
			Block{Height: 2, Hash: "h2", Parent: "h1"},
			Block{Height: 3, Hash: "h3", Parent: "h2"},
		)
	}
	cases := map[string]struct {
		branch []Block
		want   []string
	}{
		"bad hash at start": {
			[]Block{{Height: 2, Hash: "h" + badUTF8, Parent: "h1"}},
			[]string{"height 2", `"hash"`},
		},
		// Valid prefix, invalid only in the branch's last block: nothing
		// may be applied and the old branch must survive wholesale.
		"bad tx in last block": {
			[]Block{
				{Height: 2, Hash: "h2b", Parent: "h1"},
				{Height: 3, Hash: "h3b", Parent: "h2b", Txs: []string{"ok", "t" + badUTF8}},
			},
			[]string{"height 3", `"txs"`, "element 1"},
		},
		"bad parent between blocks": {
			[]Block{
				{Height: 2, Hash: "h2b", Parent: "h1"},
				{Height: 3, Hash: "h3b", Parent: "h2" + badUTF8},
			},
			[]string{"height 3", `"parent"`},
		},
		// The branch root parent is not required to be indexed, but it is
		// still required to be valid UTF-8.
		"bad unindexed branch parent": {
			[]Block{{Height: 4, Hash: "h4b", Parent: "g" + wtf8Surrogate}},
			[]string{"height 4", `"parent"`},
		},
		"wtf-8 surrogate tx at start": {
			[]Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{wtf8Surrogate}}},
			[]string{"height 2", `"txs"`, "element 0"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := build(t)
			blocks, byHash, tip := snapshot(index)
			dropped, err := index.Reorg(tc.branch)
			if err == nil {
				t.Fatal("expected reorg refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%q, want it to mention %s", err.Error(), want)
				}
			}
			if len(dropped) != 0 {
				t.Fatalf("dropped=%v, want empty on rejection", dropped)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

func TestRejectedIngestionKeepsQueriesAndCursors(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"keep", "keep"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"keep"}},
	)
	first, err := index.QueryTxs(TxQuery{TxIDs: []string{"keep"}, PageSize: 1})
	if err != nil {
		t.Fatalf("first page failed: %v", err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}

	if err := index.Append(Block{Height: 3, Hash: "h" + badUTF8, Parent: "h2"}); err == nil {
		t.Fatal("bad append accepted")
	}
	if _, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2b", Parent: "h1"},
		{Height: 3, Hash: "h" + badUTF8, Parent: "h2b"},
	}); err == nil {
		t.Fatal("bad reorg accepted")
	}

	// The cursor pinned before the rejected calls still pages the old chain.
	query := TxQuery{TxIDs: []string{"keep"}, PageSize: 1, Cursor: first.NextCursor}
	page, err := index.QueryTxs(query)
	if err != nil {
		t.Fatalf("cursor continuation failed after rejected ingestion: %v", err)
	}
	pages := []TxPage{first, page}
	for page.NextCursor != "" {
		query.Cursor = page.NextCursor
		page, err = index.QueryTxs(query)
		if err != nil {
			t.Fatalf("cursor continuation failed: %v", err)
		}
		pages = append(pages, page)
	}
	var hits []TxHit
	for _, p := range pages {
		hits = append(hits, p.Hits...)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "keep", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "keep", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "keep", Position: 0},
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits=%v, want %v", hits, want)
	}
}

// unicodeChain builds a chain exercising every legitimate identifier shape:
// Chinese, emoji, a genuine U+FFFD, the empty string, duplicates,
// surrounding whitespace, case-sensitive and backslash-literal text.
func unicodeChain(t *testing.T, withTime bool) *Index {
	t.Helper()
	index := New()
	blocks := []Block{
		{
			Height: 1,
			Hash:   "中文",
			Parent: "g",
			Txs: []string{
				" tx ",        // surrounding whitespace kept
				"",            // empty identifier allowed
				"a", "A", "a", // case-sensitive, duplicates kept
				"😀",
				"�",           // a genuine U+FFFD, not a replacement artifact
				literalEscape, // stays six literal characters, never "A"
			},
		},
		{
			Height: 2,
			Hash:   "😀",
			Parent: "中文",
			Txs:    []string{"�", "😀"},
		},
		{Height: 3, Hash: "h3", Parent: "😀"}, // nil txs, nil time
	}
	if withTime {
		blocks[1].Time = intptr(5)
	}
	for _, block := range blocks {
		if err := index.Append(block); err != nil {
			t.Fatalf("append at height %d refused: %v", block.Height, err)
		}
	}
	return index
}

// requireUnicodeHits checks every occurrence of every identifier survives
// with its block and zero-based position, and that no distinct identifiers
// have merged.
func requireUnicodeHits(t *testing.T, index *Index) {
	t.Helper()
	cases := []struct {
		txID string
		want []TxHit
	}{
		{" tx ", []TxHit{{Height: 1, BlockHash: "中文", TxID: " tx ", Position: 0}}},
		{"", []TxHit{{Height: 1, BlockHash: "中文", TxID: "", Position: 1}}},
		{"a", []TxHit{
			{Height: 1, BlockHash: "中文", TxID: "a", Position: 2},
			{Height: 1, BlockHash: "中文", TxID: "a", Position: 4},
		}},
		{"A", []TxHit{{Height: 1, BlockHash: "中文", TxID: "A", Position: 3}}},
		{"😀", []TxHit{
			{Height: 1, BlockHash: "中文", TxID: "😀", Position: 5},
			{Height: 2, BlockHash: "😀", TxID: "😀", Position: 1},
		}},
		{"�", []TxHit{
			{Height: 1, BlockHash: "中文", TxID: "�", Position: 6},
			{Height: 2, BlockHash: "😀", TxID: "�", Position: 0},
		}},
		{literalEscape, []TxHit{{Height: 1, BlockHash: "中文", TxID: literalEscape, Position: 7}}},
	}
	for _, tc := range cases {
		page, err := index.QueryTxs(TxQuery{TxIDs: []string{tc.txID}})
		if err != nil {
			t.Fatalf("query %q failed: %v", tc.txID, err)
		}
		if !reflect.DeepEqual(page.Hits, tc.want) {
			t.Fatalf("query %q hits=%v, want %v", tc.txID, page.Hits, tc.want)
		}
	}
	// The literal A text must not have decoded to A or merged with the
	// real uppercase-A identifier.
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"A"}})
	if err != nil || page.TotalMatches != 1 {
		t.Fatalf("exact A query matches=%d err=%v, want exactly 1", page.TotalMatches, err)
	}
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{literalEscape}})
	if err != nil || page.TotalMatches != 1 {
		t.Fatalf("literal escape query matches=%d err=%v, want exactly 1", page.TotalMatches, err)
	}
}

func TestUnicodeIdentifiersRoundTripThroughSnapshots(t *testing.T) {
	for _, withTime := range []bool{false, true} {
		name := "version1-no-times"
		if withTime {
			name = "version2-with-times"
		}
		t.Run(name, func(t *testing.T) {
			index := unicodeChain(t, withTime)
			requireUnicodeHits(t, index)

			var raw bytes.Buffer
			if err := index.Export(&raw); err != nil {
				t.Fatalf("export failed: %v", err)
			}
			// The genuine replacement character is stored as itself.
			if !strings.Contains(raw.String(), `"�"`) {
				t.Fatalf("export=%s, want the genuine U+FFFD kept verbatim", raw.String())
			}

			restored := New()
			if err := restored.Restore(bytes.NewReader(raw.Bytes())); err != nil {
				t.Fatalf("restore failed: %v", err)
			}
			if restored.Tip != index.Tip {
				t.Fatalf("restored tip=%d, want %d", restored.Tip, index.Tip)
			}
			// Block content, including the nil-vs-empty tx slice at height 3,
			// comes back exactly as it went in.
			for height := int64(1); height <= index.Tip; height++ {
				if !sameBlock(index.Blocks[height], restored.Blocks[height]) {
					t.Fatalf("height %d changed across the snapshot:\nbefore=%+v\nafter =%+v",
						height, index.Blocks[height], restored.Blocks[height])
				}
			}
			requireUnicodeHits(t, restored)

			// Re-export is byte-identical: the snapshot carries the chain
			// losslessly and deterministically.
			var again bytes.Buffer
			if err := restored.Export(&again); err != nil {
				t.Fatalf("second export failed: %v", err)
			}
			if !bytes.Equal(raw.Bytes(), again.Bytes()) {
				t.Fatalf("exports differ:\n%s\n%s", raw.String(), again.String())
			}
		})
	}
}
