package indexroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func exportString(t *testing.T, index *Index) string {
	t.Helper()
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	return buf.String()
}

// genericSnapshot is a schema-free view of exported bytes for invariant
// checks that should not depend on the production struct tags.
type genericSnapshot struct {
	Version int64 `json:"version"`
	Tip     int64 `json:"tip"`
	Blocks  []struct {
		Height int64    `json:"height"`
		Hash   string   `json:"hash"`
		Parent string   `json:"parent"`
		Txs    []string `json:"txs"`
	} `json:"blocks"`
}

// requireWellFormedSnapshot checks the exported bytes describe one complete
// contiguous chain. It reports rather than fails fatally so it is safe to
// call from helper goroutines.
func requireWellFormedSnapshot(t *testing.T, raw string) {
	t.Helper()
	var doc genericSnapshot
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Errorf("exported snapshot does not parse: %v", err)
		return
	}
	if doc.Version != 1 {
		t.Errorf("exported version=%d, want 1", doc.Version)
	}
	for i, block := range doc.Blocks {
		if want := int64(i + 1); block.Height != want {
			t.Errorf("exported block %d has height %d, want %d", i, block.Height, want)
			return
		}
		if i > 0 && block.Parent != doc.Blocks[i-1].Hash {
			t.Errorf("exported block at height %d breaks the parent link", block.Height)
			return
		}
	}
	last := int64(0)
	if len(doc.Blocks) > 0 {
		last = doc.Blocks[len(doc.Blocks)-1].Height
	}
	if doc.Tip != last {
		t.Errorf("exported tip=%d, last height=%d", doc.Tip, last)
	}
}

func TestExportEmptyIndex(t *testing.T) {
	raw := exportString(t, New())
	if raw != `{"version":1,"tip":0,"blocks":[]}` {
		t.Fatalf("empty export=%s", raw)
	}
}

func TestExportExactBytes(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	want := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"genesis","txs":["t1","t2"]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[]}` +
		`]}`
	if raw := exportString(t, index); raw != want {
		t.Fatalf("export=%s\nwant=%s", raw, want)
	}
}

func TestExportDeterministicAcrossHistory(t *testing.T) {
	// indexA reaches h1,h2b,h3b through a reorg; indexB appends it directly.
	indexA := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"gone"}},
		Block{Height: 3, Hash: "h3", Parent: "h2"},
	)
	if _, err := indexA.Reorg([]Block{
		{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"t2"}},
		{Height: 3, Hash: "h3b", Parent: "h2b"},
	}); err != nil {
		t.Fatal(err)
	}
	indexB := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"t2"}},
		Block{Height: 3, Hash: "h3b", Parent: "h2b"},
	)
	rawA, rawB := exportString(t, indexA), exportString(t, indexB)
	if rawA != rawB {
		t.Fatalf("same main chain exported different bytes:\n%s\n%s", rawA, rawB)
	}
	// Dropped blocks and txs must not appear.
	for _, gone := range []string{`"h2"`, `"h3"`, `"gone"`} {
		if strings.Contains(rawA, gone) {
			t.Fatalf("export contains reorged-away data %s: %s", gone, rawA)
		}
	}
	// Queries in between do not disturb the exported bytes.
	if _, err := indexA.QueryTxs(TxQuery{}); err != nil {
		t.Fatal(err)
	}
	if again := exportString(t, indexA); again != rawA {
		t.Fatalf("export changed after a query:\n%s\n%s", again, rawA)
	}
	// A restore round-trip lands on the same bytes as well.
	restored := New()
	if err := restored.Restore(strings.NewReader(rawA)); err != nil {
		t.Fatal(err)
	}
	if raw := exportString(t, restored); raw != rawA {
		t.Fatalf("export changed after restore:\n%s\n%s", raw, rawA)
	}
}

func TestExportPreservesDuplicateAndEmptyTxIDs(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "", "t1", ""}},
	)
	raw := exportString(t, index)
	want := `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":["t1","","t1",""]}]}`
	if raw != want {
		t.Fatalf("export=%s\nwant=%s", raw, want)
	}
	restored := New()
	if err := restored.Restore(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if got := restored.Blocks[1].Txs; !reflect.DeepEqual(got, []string{"t1", "", "t1", ""}) {
		t.Fatalf("restored txs=%q", got)
	}
}

// failWriter fails every write after a successful prefix of n bytes.
type failWriter struct {
	n   int
	err error
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.n >= len(p) {
		w.n -= len(p)
		return len(p), nil
	}
	return 0, w.err
}

func TestExportWriteFailureLeavesIndexUntouched(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	blocks, byHash, tip := snapshot(index)
	boom := errors.New("disk full")
	if err := index.Export(&failWriter{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the write failure", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestRestoreIntoEmptyIndexMatchesSource(t *testing.T) {
	source := txChain(t,
		[]string{"a", "b"},
		[]string{},
		[]string{"b", "c", "a"},
		[]string{"c"},
	)
	raw := exportString(t, source)

	restored := New()
	if err := restored.Restore(strings.NewReader(raw)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if restored.Tip != source.Tip || len(restored.Blocks) != len(source.Blocks) {
		t.Fatalf("restored shape: tip=%d blocks=%d", restored.Tip, len(restored.Blocks))
	}

	// The same queries return the same hits, positions, order, totals, and
	// matched-block counts on both indexes; cursor strings may differ.
	queries := []TxQuery{
		{},
		{From: 2, To: 3},
		{TxIDs: []string{"a", "c"}},
		{TxIDs: []string{""}},
		{From: 3, TxIDs: []string{"b"}},
	}
	for _, query := range queries {
		wantPages := collectPages(t, source, query)
		gotPages := collectPages(t, restored, query)
		if len(gotPages) != len(wantPages) {
			t.Fatalf("query %+v: %d pages, want %d", query, len(gotPages), len(wantPages))
		}
		for i := range wantPages {
			want, got := wantPages[i], gotPages[i]
			if !reflect.DeepEqual(got.Hits, want.Hits) ||
				got.TotalMatches != want.TotalMatches ||
				got.MatchedBlocks != want.MatchedBlocks ||
				got.ToHeight != want.ToHeight ||
				(got.NextCursor == "") != (want.NextCursor == "") {
				t.Fatalf("query %+v page %d differs:\ngot  %+v\nwant %+v", query, i, got, want)
			}
		}
	}
	// A cursor minted by the source index is a foreign cursor on the target.
	page, err := source.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.QueryTxs(TxQuery{PageSize: 1, Cursor: page.NextCursor}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("foreign cursor: err=%v, want ErrInvalidArgument", err)
	}
}

func TestRestoreReplacesExistingChainCompletely(t *testing.T) {
	target := chain(t,
		Block{Height: 1, Hash: "old1", Parent: "genesis", Txs: []string{"old"}},
		Block{Height: 2, Hash: "old2", Parent: "old1"},
		Block{Height: 3, Hash: "old3", Parent: "old2"},
		Block{Height: 4, Hash: "old4", Parent: "old3"},
	)
	snap := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"start","txs":["x"]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[]}` +
		`]}`
	if err := target.Restore(strings.NewReader(snap)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if target.Tip != 2 || len(target.Blocks) != 2 || len(target.ByHash) != 2 {
		t.Fatalf("leftover state: tip=%d blocks=%d byHash=%d", target.Tip, len(target.Blocks), len(target.ByHash))
	}
	for _, hash := range []string{"old1", "old2", "old3", "old4"} {
		if _, ok := target.ByHash[hash]; ok {
			t.Fatalf("old hash %s still indexed", hash)
		}
	}
	// Appending, reorging, and paginating all work on the restored chain.
	if err := target.Append(Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"y"}}); err != nil {
		t.Fatalf("append after restore refused: %v", err)
	}
	dropped, err := target.Reorg([]Block{{Height: 3, Hash: "h3b", Parent: "h2", Txs: []string{"z"}}})
	if err != nil || !reflect.DeepEqual(dropped, []int64{3}) {
		t.Fatalf("reorg after restore: dropped=%v err=%v", dropped, err)
	}
	pages := collectPages(t, target, TxQuery{PageSize: 1})
	var all []TxHit
	for _, page := range pages {
		all = append(all, page.Hits...)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "x", Position: 0},
		{Height: 3, BlockHash: "h3b", TxID: "z", Position: 0},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("hits after restore=%v, want %v", all, want)
	}
}

func TestRestoreEmptySnapshot(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	if err := index.Restore(strings.NewReader(`{"version":1,"tip":0,"blocks":[]}`)); err != nil {
		t.Fatalf("empty restore failed: %v", err)
	}
	if index.Tip != 0 || len(index.Blocks) != 0 || len(index.ByHash) != 0 {
		t.Fatalf("not empty after restore: tip=%d blocks=%d", index.Tip, len(index.Blocks))
	}
	// Ingestion restarts at height 1.
	if err := index.Append(Block{Height: 1, Hash: "n1", Parent: "fresh", Txs: []string{"t"}}); err != nil {
		t.Fatalf("append at height 1 after empty restore refused: %v", err)
	}
	page, err := index.QueryTxs(TxQuery{})
	if err != nil || page.TotalMatches != 1 || page.ToHeight != 1 {
		t.Fatalf("query after re-ingest: page=%+v err=%v", page, err)
	}
}

func TestRestoreAcceptsTrailingWhitespaceAndArbitraryFirstParent(t *testing.T) {
	index := New()
	snap := "  {\"version\":1,\"tip\":1,\"blocks\":[" +
		"{\"height\":1,\"hash\":\"h1\",\"parent\":\"any-start-marker\",\"txs\":[]}" +
		"]} \n\t"
	if err := index.Restore(strings.NewReader(snap)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if index.Blocks[1].Parent != "any-start-marker" {
		t.Fatalf("first parent not preserved: %+v", index.Blocks[1])
	}
}

func TestRestoreRejectsInvalidSnapshots(t *testing.T) {
	valid := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"genesis","txs":["t1"]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[]}` +
		`]}`
	cases := map[string]string{
		"empty input":          ``,
		"whitespace only":      "  \n",
		"not an object":        `[1,2]`,
		"trailing second doc":  valid + ` {"version":1,"tip":0,"blocks":[]}`,
		"trailing garbage":     valid + ` x`,
		"unknown version":      `{"version":3,"tip":0,"blocks":[]}`,
		"missing version":      `{"tip":0,"blocks":[]}`,
		"missing tip":          `{"version":1,"blocks":[]}`,
		"missing blocks":       `{"version":1,"tip":0}`,
		"version wrong type":   `{"version":"1","tip":0,"blocks":[]}`,
		"tip wrong type":       `{"version":1,"tip":"0","blocks":[]}`,
		"blocks not an array":  `{"version":1,"tip":0,"blocks":{}}`,
		"blocks null":          `{"version":1,"tip":0,"blocks":null}`,
		"block not an object":  `{"version":1,"tip":1,"blocks":[1]}`,
		"block missing height": `{"version":1,"tip":1,"blocks":[{"hash":"h1","parent":"g","txs":[]}]}`,
		"block missing hash":   `{"version":1,"tip":1,"blocks":[{"height":1,"parent":"g","txs":[]}]}`,
		"block missing parent": `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","txs":[]}]}`,
		"block missing txs":    `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g"}]}`,
		"duplicate top field":  `{"version":1,"version":1,"tip":0,"blocks":[]}`,
		"duplicate block field": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"height":1,"hash":"h1","parent":"g","txs":[]}` +
			`]}`,
		"unknown top field": `{"version":1,"tip":0,"blocks":[],"extra":1}`,
		"unknown block field": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":[],"extra":1}` +
			`]}`,
		"first height not one": `{"version":1,"tip":2,"blocks":[` +
			`{"height":2,"hash":"h2","parent":"g","txs":[]}` +
			`]}`,
		"height gap": `{"version":1,"tip":3,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":[]},` +
			`{"height":3,"hash":"h3","parent":"h1","txs":[]}` +
			`]}`,
		"height not integer": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1.5,"hash":"h1","parent":"g","txs":[]}` +
			`]}`,
		"empty hash": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"hash":"","parent":"g","txs":[]}` +
			`]}`,
		"duplicate hash": `{"version":1,"tip":2,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":[]},` +
			`{"height":2,"hash":"h1","parent":"h1","txs":[]}` +
			`]}`,
		"broken parent link": `{"version":1,"tip":2,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":[]},` +
			`{"height":2,"hash":"h2","parent":"nope","txs":[]}` +
			`]}`,
		"tip above last height": `{"version":1,"tip":5,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":[]}` +
			`]}`,
		"tip below last height": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":[]},` +
			`{"height":2,"hash":"h2","parent":"h1","txs":[]}` +
			`]}`,
		"tip on empty blocks": `{"version":1,"tip":1,"blocks":[]}`,
		"txs not an array": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":"t1"}` +
			`]}`,
		"txs null": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":null}` +
			`]}`,
		"txs element not string": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"g","txs":["t1",2]}` +
			`]}`,
		"hash wrong type": `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"hash":7,"parent":"g","txs":[]}` +
			`]}`,
		"truncated": `{"version":1,"tip":`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(strings.NewReader(input))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
			}
			if err == ErrInvalidSnapshot || err.Error() == ErrInvalidSnapshot.Error() {
				t.Fatalf("error carries no specific reason: %v", err)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

func TestRestoreRejectsNullFields(t *testing.T) {
	v1Block := func(field string) string {
		fields := map[string]string{
			"height": "1", "hash": `"h1"`, "parent": `"g"`, "txs": `[]`,
		}
		fields[field] = "null"
		return `{"version":1,"tip":1,"blocks":[{"height":` + fields["height"] +
			`,"hash":` + fields["hash"] + `,"parent":` + fields["parent"] +
			`,"txs":` + fields["txs"] + `}]}`
	}
	cases := map[string]struct {
		input string
		field string // must appear in the error message
	}{
		"version null v1": {`{"version":null,"tip":0,"blocks":[]}`, `"version"`},
		"version null v2": {`{"version":null,"tip":0,"blocks":[]}`, `"version"`},
		"tip null v1":     {`{"version":1,"tip":null,"blocks":[]}`, `"tip"`},
		"tip null v2":     {`{"version":2,"tip":null,"blocks":[]}`, `"tip"`},
		"height null":     {v1Block("height"), `"height"`},
		"hash null":       {v1Block("hash"), `"hash"`},
		"parent null":     {v1Block("parent"), `"parent"`},
		"height null v2": {`{"version":2,"tip":1,"blocks":[` +
			`{"height":null,"hash":"h1","parent":"g","txs":[],"timestamp":5}` +
			`]}`, `"height"`},
		"hash null v2": {`{"version":2,"tip":1,"blocks":[` +
			`{"height":1,"hash":null,"parent":"g","txs":[],"timestamp":5}` +
			`]}`, `"hash"`},
		"parent null v2": {`{"version":2,"tip":1,"blocks":[` +
			`{"height":1,"hash":"h1","parent":null,"txs":[],"timestamp":5}` +
			`]}`, `"parent"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(strings.NewReader(tc.input))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err=%v, want it to name field %s", err, tc.field)
			}
			if !strings.Contains(err.Error(), "null") {
				t.Fatalf("err=%v, want it to mention null", err)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

func TestRestoreRejectsNullTxElement(t *testing.T) {
	// The null sits in the last transaction of the last block: everything
	// before it is valid, yet the whole restore must be refused.
	input := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":["t1"]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":["a",null,"b"]}` +
		`]}`
	index := txChain(t, []string{"x"}, []string{"y"}, []string{"z"})
	blocks, byHash, tip := snapshot(index)
	before := exportString(t, index)
	err := index.Restore(strings.NewReader(input))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
	}
	for _, want := range []string{"height 2", "element 1", "null"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want it to mention %q", err, want)
		}
	}
	requireUnchanged(t, index, blocks, byHash, tip)
	if after := exportString(t, index); after != before {
		t.Fatalf("export changed after rejected restore:\n%s\n%s", before, after)
	}
	page, err := index.QueryTxs(TxQuery{})
	if err != nil || page.TotalMatches != 3 {
		t.Fatalf("query after rejected restore: page=%+v err=%v", page, err)
	}

	// The height is named even when txs precedes height in the object.
	shuffled := `{"version":1,"tip":1,"blocks":[` +
		`{"txs":[null],"hash":"h1","parent":"g","height":1}` +
		`]}`
	err = index.Restore(strings.NewReader(shuffled))
	if !errors.Is(err, ErrInvalidSnapshot) || !strings.Contains(err.Error(), "height 1") {
		t.Fatalf("err=%v, want ErrInvalidSnapshot naming height 1", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestRestorePreservesTxIdentifiersVerbatim(t *testing.T) {
	// Empty and duplicated identifiers and their order survive a restore,
	// and a first-block parent of "" is a valid chain-start marker.
	input := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"","txs":["","a",""]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":["a","a"]}` +
		`]}`
	index := New()
	if err := index.Restore(strings.NewReader(input)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if got := index.Blocks[1].Txs; !reflect.DeepEqual(got, []string{"", "a", ""}) {
		t.Fatalf("height 1 txs=%q", got)
	}
	if got := index.Blocks[2].Txs; !reflect.DeepEqual(got, []string{"a", "a"}) {
		t.Fatalf("height 2 txs=%q", got)
	}
	if raw := exportString(t, index); raw != input {
		t.Fatalf("re-export differs:\n%s\n%s", raw, input)
	}
}

// errReader fails after delivering a valid snapshot prefix.
type errReader struct {
	data []byte
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestRestoreReadFailureLeavesIndexUntouched(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	blocks, byHash, tip := snapshot(index)
	boom := errors.New("connection reset")
	raw := exportString(t, index)
	err := index.Restore(&errReader{data: []byte(raw[:len(raw)/2]), err: boom})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the read failure", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

// readStep scripts one scriptedSnapshotReader.Read: at most n bytes (all
// remaining data when n < 0), delivered together with err exactly as a
// failing transport may return usable bytes and an error in one call.
type readStep struct {
	n   int
	err error
}

// scriptedSnapshotReader serves data following a per-read script. Once the
// script is exhausted every further read returns io.EOF.
type scriptedSnapshotReader struct {
	data  []byte
	off   int
	steps []readStep
}

func (r *scriptedSnapshotReader) Read(p []byte) (int, error) {
	n := len(r.data) - r.off
	err := io.EOF
	if len(r.steps) > 0 {
		step := r.steps[0]
		r.steps = r.steps[1:]
		if step.n >= 0 && n > step.n {
			n = step.n
		}
		err = step.err
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, err
}

// A half snapshot that ends on a clean io.EOF is a truncated, invalid
// snapshot; the same half bytes cut off by an abnormal read failure are a
// read failure. The identical input bytes must yield different error
// categories depending solely on how the stream ended.
func TestRestoreTruncatedEOFVsReadFailureOnSameBytes(t *testing.T) {
	half := `{"version":1,"tip":`

	t.Run("clean EOF is ErrInvalidSnapshot", func(t *testing.T) {
		index := txChain(t, []string{"a"}, []string{"b"})
		blocks, byHash, tip := snapshot(index)
		err := index.Restore(strings.NewReader(half))
		if !errors.Is(err, ErrInvalidSnapshot) {
			t.Fatalf("err=%v, want ErrInvalidSnapshot for a cleanly ended half snapshot", err)
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("clean truncation must not match io.ErrUnexpectedEOF: %v", err)
		}
		requireUnchanged(t, index, blocks, byHash, tip)
	})

	cases := map[string][]readStep{
		"fault on the next read": {
			{-1, nil},
			{0, io.ErrUnexpectedEOF},
			{0, io.ErrUnexpectedEOF},
		},
		"bytes and fault in one read": {
			{len(half), io.ErrUnexpectedEOF},
			{0, io.ErrUnexpectedEOF},
		},
		"wrapped fault on the next read": {
			{-1, nil},
			{0, fmt.Errorf("storage layer: %w", io.ErrUnexpectedEOF)},
			{0, fmt.Errorf("storage layer: %w", io.ErrUnexpectedEOF)},
		},
		"fault then a later clean EOF": {
			{len(half), io.ErrUnexpectedEOF},
			{0, io.EOF},
		},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(&scriptedSnapshotReader{data: []byte(half), steps: steps})
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("err=%v, want errors.Is io.ErrUnexpectedEOF", err)
			}
			if errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("a reader-reported failure must not match ErrInvalidSnapshot: %v", err)
			}
			if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
				t.Fatalf("err=%v, want the read snapshot prefix", err)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

// Empty or whitespace-only input cut off by an abnormal read failure is a
// read failure too, not an empty/half document.
func TestRestoreUnexpectedEOFOnEmptyInputIsReadFailure(t *testing.T) {
	for name, input := range map[string]string{"empty": ``, "whitespace": "  \n"} {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(&scriptedSnapshotReader{
				data:  []byte(input),
				steps: []readStep{{len(input), io.ErrUnexpectedEOF}, {0, io.ErrUnexpectedEOF}},
			})
			if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, want a read failure matching io.ErrUnexpectedEOF", err)
			}
			requireUnchanged(t, index, blocks, byHash, tip)

			// The same bytes with a clean EOF stay an invalid snapshot.
			err = index.Restore(strings.NewReader(input))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("cleanly ended %q input: err=%v, want ErrInvalidSnapshot", name, err)
			}
		})
	}
}

// A read fault after some complete, valid blocks have been delivered neither
// applies those blocks nor changes the error category because the tail is
// missing.
func TestRestoreReadFailureAfterValidBlocksAppliesNothing(t *testing.T) {
	prefix := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"x1","parent":"g","txs":["t1"]},` +
		`{"height":2,"hash":"x2"`
	index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
	blocks, byHash, tip := snapshot(index)
	before := exportString(t, index)

	err := index.Restore(&scriptedSnapshotReader{
		data:  []byte(prefix),
		steps: []readStep{{len(prefix), io.ErrUnexpectedEOF}, {0, io.ErrUnexpectedEOF}},
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want the read failure", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
	for _, leaked := range []string{"x1", "x2"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("snapshot hash %q was applied before the read failure", leaked)
		}
	}
	if after := exportString(t, index); after != before {
		t.Fatalf("chain changed despite the read failure:\n%s\n%s", before, after)
	}
}

// A genuine content defect in the delivered bytes is an invalid snapshot
// even when the same read also reports a transport fault.
func TestRestoreContentErrorWithSimultaneousReadFaultIsInvalid(t *testing.T) {
	// 'x' where the blocks array must start: a hard syntax error in hand.
	input := `{"version":1,"tip":0,"blocks":x`
	index := txChain(t, []string{"a"}, []string{"b"})
	blocks, byHash, tip := snapshot(index)
	boom := errors.New("connection reset")
	err := index.Restore(&scriptedSnapshotReader{
		data:  []byte(input),
		steps: []readStep{{len(input), boom}, {0, boom}},
	})
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want ErrInvalidSnapshot for malformed delivered bytes", err)
	}
	if errors.Is(err, boom) {
		t.Fatalf("content errors must not be swallowed into the read failure: %v", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

// A read failure, like a rejected snapshot, leaves previously issued cursors
// paging over the old chain; it must not surface as ErrQueryChanged.
func TestRestoreReadFailureKeepsCursorsAlive(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}
	half := `{"version":1,"tip":3,"blocks":[` +
		`{"height":1,"hash":"x1","parent":"g","txs":[]},`
	if err := index.Restore(&scriptedSnapshotReader{
		data: []byte(half),
		steps: []readStep{
			{len(half), io.ErrUnexpectedEOF},
			{0, io.ErrUnexpectedEOF},
		},
	}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("restore err=%v, want the read failure", err)
	}
	pages := collectPages(t, index, TxQuery{PageSize: 1, Cursor: first.NextCursor})
	var all []TxHit
	for _, page := range pages {
		all = append(all, page.Hits...)
	}
	want := []TxHit{
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 0},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("cursor after read failure hits=%v, want %v", all, want)
	}
}

func TestRestoreFailureKeepsCursorsAlive(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"b"},
		[]string{"c"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.NextCursor
	if cursor == "" {
		t.Fatal("expected a continuation cursor")
	}
	// The invalid block sits at the very end of the snapshot.
	bad := `{"version":1,"tip":3,"blocks":[` +
		`{"height":1,"hash":"x1","parent":"g","txs":[]},` +
		`{"height":2,"hash":"x2","parent":"x1","txs":[]},` +
		`{"height":3,"hash":"","parent":"x2","txs":[]}` +
		`]}`
	if err := index.Restore(strings.NewReader(bad)); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
	}
	// The cursor issued before the failed restore still pages to the end.
	pages := collectPages(t, index, TxQuery{PageSize: 1, Cursor: cursor})
	var all []TxHit
	for _, page := range pages {
		all = append(all, page.Hits...)
	}
	want := []TxHit{
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 0},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("hits=%v, want %v", all, want)
	}
}

func TestRestoreSameContentKeepsCursorDifferentContentKills(t *testing.T) {
	build := func(t *testing.T) (*Index, string) {
		t.Helper()
		index := txChain(t,
			[]string{"a"},
			[]string{"b"},
			[]string{"c"},
		)
		first, err := index.QueryTxs(TxQuery{PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		return index, first.NextCursor
	}

	t.Run("identical restore keeps the cursor", func(t *testing.T) {
		index, cursor := build(t)
		raw := exportString(t, index)
		if err := index.Restore(strings.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
		page, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("continuation after identical restore failed: %v", err)
		}
		want := []TxHit{{Height: 2, BlockHash: "h2", TxID: "b", Position: 0}}
		if !reflect.DeepEqual(page.Hits, want) {
			t.Fatalf("hits=%v, want %v", page.Hits, want)
		}
	})

	t.Run("changed range kills the cursor", func(t *testing.T) {
		index, cursor := build(t)
		snap := `{"version":1,"tip":3,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"genesis","txs":["a"]},` +
			`{"height":2,"hash":"h2","parent":"h1","txs":["CHANGED"]},` +
			`{"height":3,"hash":"h3","parent":"h2","txs":["c"]}` +
			`]}`
		if err := index.Restore(strings.NewReader(snap)); err != nil {
			t.Fatal(err)
		}
		if _, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: cursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("shortened chain kills the cursor", func(t *testing.T) {
		index, cursor := build(t)
		snap := `{"version":1,"tip":1,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"genesis","txs":["a"]}` +
			`]}`
		if err := index.Restore(strings.NewReader(snap)); err != nil {
			t.Fatal(err)
		}
		if _, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: cursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})
}

func TestRestoreWhileStreamOpenIndexStaysUsable(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- index.Restore(pr)
	}()
	// Deliver only a prefix; the restore is now blocked mid-stream.
	if _, err := pw.Write([]byte(`{"version":1,"tip":0,`)); err != nil {
		t.Fatal(err)
	}
	// The index stays queryable and appendable while the stream is open.
	page, err := index.QueryTxs(TxQuery{})
	if err != nil || page.TotalMatches != 2 {
		t.Fatalf("query while restore in flight: page=%+v err=%v", page, err)
	}
	if err := index.Append(Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"c"}}); err != nil {
		t.Fatalf("append while restore in flight: %v", err)
	}
	// Ending the stream early fails the restore without touching the chain.
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("restore err=%v, want ErrInvalidSnapshot", err)
	}
	// Only the append changed the chain; the failed restore left no trace.
	if index.Tip != 3 || len(index.Blocks) != 3 || index.Blocks[3].Hash != "h3" {
		t.Fatalf("unexpected state: tip=%d blocks=%v", index.Tip, index.Blocks)
	}
}

func TestSnapshotConcurrentWithChainOps(t *testing.T) {
	source := txChain(t, []string{"seed"})
	var wg sync.WaitGroup

	// One writer keeps appending; exporters must always observe one complete
	// contiguous chain.
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for height := int64(2); height <= 300; height++ {
			select {
			case <-stop:
				return
			default:
			}
			block := Block{
				Height: height,
				Hash:   fmt.Sprintf("h%d", height),
				Parent: fmt.Sprintf("h%d", height-1),
				Txs:    []string{fmt.Sprintf("tx-%d", height)},
			}
			if err := source.Append(block); err != nil {
				return
			}
		}
	}()

	for exporter := 0; exporter < 3; exporter++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 30; round++ {
				var buf bytes.Buffer
				if err := source.Export(&buf); err != nil {
					t.Errorf("export failed: %v", err)
					return
				}
				requireWellFormedSnapshot(t, buf.String())
			}
		}()
	}

	// Restores and queries race on a second index; every restore applies a
	// complete snapshot, so queries always see a contiguous chain.
	target := New()
	raw := exportString(t, txChain(t,
		[]string{"a", "b"},
		[]string{"c"},
		[]string{"d"},
	))
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				if err := target.Restore(strings.NewReader(raw)); err != nil {
					t.Errorf("restore failed: %v", err)
					return
				}
			}
		}()
	}
	for reader := 0; reader < 2; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 40; round++ {
				page, err := target.QueryTxs(TxQuery{})
				if err != nil {
					t.Errorf("query failed: %v", err)
					return
				}
				lastHeight, lastPosition := int64(0), -1
				for _, hit := range page.Hits {
					if hit.Height < lastHeight || (hit.Height == lastHeight && hit.Position <= lastPosition) {
						t.Errorf("hits out of order: %+v after (%d,%d)", hit, lastHeight, lastPosition)
						return
					}
					lastHeight, lastPosition = hit.Height, hit.Position
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	writer.Wait()
}
