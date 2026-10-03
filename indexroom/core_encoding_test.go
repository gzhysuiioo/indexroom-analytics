package indexroom

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestAppendRejectsInvalidUTF8Identifiers(t *testing.T) {
	bad := "\xff"
	cases := map[string]struct {
		block Block
		want  []string // substrings the error must carry
	}{
		"invalid utf8 in hash": {
			Block{Height: 3, Hash: "h" + bad, Parent: "h2", Txs: []string{"t1"}},
			[]string{`"hash"`, "height 3"},
		},
		"invalid utf8 in parent": {
			Block{Height: 3, Hash: "h3", Parent: "h2" + bad, Txs: []string{"t1"}},
			[]string{`"parent"`, "height 3"},
		},
		"invalid utf8 in tx id": {
			Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"ok", "t" + bad}},
			[]string{`"txs"`, "height 3", "element 1"},
		},
		"invalid utf8 in first tx position zero": {
			Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{bad}},
			[]string{`"txs"`, "height 3", "element 0"},
		},
		"first block parent is checked even though it is not indexed": {
			Block{Height: 1, Hash: "h1", Parent: "genesis" + bad},
			[]string{`"parent"`, "height 1"},
		},
		"encoding rule also applies to timestamped blocks": {
			Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"ok", bad}, Time: intptr(7)},
			[]string{`"txs"`, "height 3", "element 1"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := chain(t,
				Block{Height: 1, Hash: "h1", Parent: "genesis"},
				Block{Height: 2, Hash: "h2", Parent: "h1"},
			)
			blocks, byHash, tip := snapshot(index)
			err := index.Append(tc.block)
			if err == nil {
				t.Fatal("expected refusal of invalid UTF-8")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%v, want it to mention %s", err, want)
				}
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

func TestReorgRejectsInvalidUTF8Identifiers(t *testing.T) {
	bad := "\xff"
	build := func(t *testing.T) *Index {
		return chain(t,
			Block{Height: 1, Hash: "h1", Parent: "genesis"},
			Block{Height: 2, Hash: "h2", Parent: "h1"},
			Block{Height: 3, Hash: "h3", Parent: "h2"},
		)
	}
	cases := map[string]struct {
		branch []Block
		want   []string
	}{
		"invalid utf8 in first parent": {
			// Unknown to the index as well: encoding must be reported first.
			[]Block{{Height: 2, Hash: "h2b", Parent: "h1" + bad}},
			[]string{`"parent"`, "height 2"},
		},
		"invalid utf8 in hash of last block": {
			[]Block{
				{Height: 2, Hash: "h2b", Parent: "h1"},
				{Height: 3, Hash: "h3b", Parent: "h2b"},
				{Height: 4, Hash: "h4" + bad, Parent: "h3b"},
			},
			[]string{`"hash"`, "height 4"},
		},
		"invalid utf8 in tx id names zero-based position": {
			[]Block{
				{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"a", "b"}},
				{Height: 3, Hash: "h3b", Parent: "h2b", Txs: []string{"x", "y" + bad, "z"}},
			},
			[]string{`"txs"`, "height 3", "element 1"},
		},
		"invalid utf8 in last block tx forbids earlier applies": {
			[]Block{
				{Height: 2, Hash: "h2b", Parent: "h1"},
				{Height: 3, Hash: "h3b", Parent: "h2b", Txs: []string{bad}},
			},
			[]string{`"txs"`, "height 3", "element 0"},
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
					t.Fatalf("err=%v, want it to mention %s", err, want)
				}
			}
			if len(dropped) != 0 {
				t.Fatalf("dropped=%v, want empty on rejected reorg", dropped)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

func TestRejectedIngestionKeepsQueriesAndCursors(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t1"}},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatalf("first page failed: %v", err)
	}
	if first.NextCursor == "" || first.TotalMatches != 3 {
		t.Fatalf("unexpected first page: %+v", first)
	}

	if err := index.Append(Block{Height: 3, Hash: "h3" + "\xff", Parent: "h2"}); err == nil {
		t.Fatal("expected append refusal")
	}
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b" + "\xff", Parent: "h1"}}); err == nil {
		t.Fatal("expected reorg refusal")
	}

	next, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("cursor expired after rejected calls: %v", err)
	}
	if len(next.Hits) != 1 || next.Hits[0].TxID != "t2" {
		t.Fatalf("unexpected continuation: %+v", next.Hits)
	}

	// Exact queries still return the original occurrences.
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"t1"}})
	if err != nil || page.TotalMatches != 2 {
		t.Fatalf("query changed after rejected calls: matches=%d err=%v", page.TotalMatches, err)
	}
}

func TestUnicodeIdentifiersSurviveExportAndRestore(t *testing.T) {
	// literalEscape looks like a Unicode escape but is ordinary text (a
	// backslash followed by u0041); it must stay literal and stay distinct
	// from the letter "A".
	literalEscape := `\u0041`
	// Two distinct identifiers that both contain a genuine U+FFFD.
	replLeft := "�x"
	replRight := "x�"
	index := chain(t,
		Block{Height: 1, Hash: "中文", Parent: "创世", Txs: []string{"tx-😀", "中文交易", "", "", " tx ", "TX", literalEscape}},
		Block{Height: 2, Hash: "h2", Parent: "中文", Txs: []string{"tx-😀", replLeft}},
	)
	// A unicode-bearing reorg branch is accepted and keeps every byte.
	dropped, err := index.Reorg([]Block{
		{Height: 2, Hash: "区块二", Parent: "中文", Txs: []string{"tx-😀", replLeft, replRight}},
	})
	if err != nil {
		t.Fatalf("unicode reorg refused: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != 2 {
		t.Fatalf("dropped=%v, want [2]", dropped)
	}
	// Replaying an identical unicode block is still a no-op.
	if err := index.Append(Block{Height: 2, Hash: "区块二", Parent: "中文", Txs: []string{"tx-😀", replLeft, replRight}}); err != nil {
		t.Fatalf("identical unicode replay refused: %v", err)
	}

	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	raw := buf.String()
	// json.Marshal escapes the literal backslash, so "A" is emitted as
	// "\\u0041" — still literal text, never decoded into the letter "A".
	jsonLiteralEscape := `\\u0041`
	for _, fragment := range []string{
		`"tx-😀"`, `"中文交易"`, `" tx "`, `"TX"`, `"` + jsonLiteralEscape + `"`,
		`"` + replLeft + `"`, `"` + replRight + `"`, `"中文"`, `"区块二"`,
	} {
		if !strings.Contains(raw, fragment) {
			t.Fatalf("export=%s, missing verbatim fragment %s", raw, fragment)
		}
	}

	restored := New()
	if err := restored.Restore(&buf); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	block := restored.Blocks[2]
	wantTxs := []string{"tx-😀", replLeft, replRight}
	if !reflect.DeepEqual(block.Txs, wantTxs) {
		t.Fatalf("restored txs=%q, want %q", block.Txs, wantTxs)
	}
	if block.Hash != "区块二" || block.Parent != "中文" {
		t.Fatalf("restored identifiers=%q/%q", block.Hash, block.Parent)
	}

	// Every occurrence survives with its block and position; distinct
	// identifiers never merge, duplicates never collapse, and whitespace and
	// case stay significant.
	assertMatches := func(txID string, want int64) {
		t.Helper()
		page, err := restored.QueryTxs(TxQuery{TxIDs: []string{txID}})
		if err != nil {
			t.Fatalf("query %q failed: %v", txID, err)
		}
		if page.TotalMatches != want {
			t.Fatalf("query %q matches=%d, want %d", txID, page.TotalMatches, want)
		}
	}
	assertMatches("tx-😀", 2)
	assertMatches("中文交易", 1)
	assertMatches("", 2)
	assertMatches(" tx ", 1)
	assertMatches("TX", 1)
	assertMatches(literalEscape, 1)
	assertMatches(replLeft, 1)
	assertMatches(replRight, 1)

	page, err := restored.QueryTxs(TxQuery{TxIDs: []string{"tx-😀"}})
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	wantHits := []struct {
		height int64
		pos    int
		hash   string
	}{
		{1, 0, "中文"},
		{2, 0, "区块二"},
	}
	for i, want := range wantHits {
		got := page.Hits[i]
		if got.Height != want.height || got.Position != want.pos || got.BlockHash != want.hash {
			t.Fatalf("hit %d=%+v, want %+v", i, got, want)
		}
	}

	// Timed and untimed blocks both keep unicode identifiers after a round
	// trip, and the document stays version 2.
	timed := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"😀"}},
	)
	if err := timed.Append(Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"中文"}, Time: intptr(42)}); err != nil {
		t.Fatalf("timed unicode append refused: %v", err)
	}
	var timedBuf bytes.Buffer
	if err := timed.Export(&timedBuf); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	if !strings.Contains(timedBuf.String(), `"version":2`) {
		t.Fatalf("expected version 2 snapshot: %s", timedBuf.String())
	}
	timedRestored := New()
	if err := timedRestored.Restore(&timedBuf); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if got := timedRestored.Blocks[1].Txs[0]; got != "😀" {
		t.Fatalf("untimed tx=%q, want 😀", got)
	}
	if got := timedRestored.Blocks[2].Txs[0]; got != "中文" {
		t.Fatalf("timed tx=%q, want 中文", got)
	}
}
