package indexroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// errWriter fails on every write.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

// errReader fails on every read with a non-EOF error.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read refused") }

// exportBytes serializes the index and fails the test on error.
func exportBytes(t *testing.T, index *Index) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := index.ExportSnapshot(&buf); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	return buf.Bytes()
}

// restoreBytes restores from raw JSON and fails the test on error.
func restoreBytes(t *testing.T, index *Index, data []byte) {
	t.Helper()
	if err := index.RestoreSnapshot(bytes.NewReader(data)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
}

func TestExportSnapshotEmpty(t *testing.T) {
	index := New()
	data := exportBytes(t, index)

	var snap map[string]interface{}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}
	if snap["version"] != float64(1) {
		t.Fatalf("version=%v, want 1", snap["version"])
	}
	if snap["tip"] != float64(0) {
		t.Fatalf("tip=%v, want 0", snap["tip"])
	}
	blocks, ok := snap["blocks"].([]interface{})
	if !ok {
		t.Fatalf("blocks=%v, want an array", snap["blocks"])
	}
	if len(blocks) != 0 {
		t.Fatalf("blocks=%v, want empty", blocks)
	}
	if !bytes.Contains(data, []byte(`"blocks":[]`)) {
		t.Fatalf("empty snapshot must encode blocks as []: %s", data)
	}
}

func TestExportSnapshotContentsAndOrder(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2", "t1", ""}},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"", "t3"}},
	)
	data := exportBytes(t, index)

	var snap struct {
		Version int64 `json:"version"`
		Tip     int64 `json:"tip"`
		Blocks  []struct {
			Height int64    `json:"height"`
			Hash   string   `json:"hash"`
			Parent string   `json:"parent"`
			Txs    []string `json:"txs"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != 1 || snap.Tip != 3 {
		t.Fatalf("unexpected header: %+v", snap)
	}
	if len(snap.Blocks) != 3 {
		t.Fatalf("blocks=%d, want 3", len(snap.Blocks))
	}
	want := []struct {
		height int64
		hash   string
		parent string
		txs    []string
	}{
		{1, "h1", "genesis", []string{"t1", "t2", "t1", ""}},
		{2, "h2", "h1", []string{}},
		{3, "h3", "h2", []string{"", "t3"}},
	}
	for i, w := range want {
		b := snap.Blocks[i]
		if b.Height != w.height || b.Hash != w.hash || b.Parent != w.parent {
			t.Fatalf("block %d mismatch: %+v", i, b)
		}
		if !reflect.DeepEqual(b.Txs, w.txs) {
			t.Fatalf("block %d txs=%v, want %v (duplicates and empty strings preserved)", i, b.Txs, w.txs)
		}
	}
}

func TestExportSnapshotDeterministic(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2", "t2"}},
	)
	first := exportBytes(t, index)
	second := exportBytes(t, index)
	if !bytes.Equal(first, second) {
		t.Fatal("repeated exports of the same chain differ")
	}

	// History must not matter: a reorged-away branch followed by a restore
	// yields the same main-chain bytes.
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"x"}}}); err != nil {
		t.Fatal(err)
	}
	restoreBytes(t, index, first)
	third := exportBytes(t, index)
	if !bytes.Equal(first, third) {
		t.Fatalf("export after reorg+restore differs:\n%s\n%s", first, third)
	}

	// Querying does not change the export either.
	if _, err := index.QueryTxs(TxQuery{PageSize: 1}); err != nil {
		t.Fatal(err)
	}
	if fourth := exportBytes(t, index); !bytes.Equal(first, fourth) {
		t.Fatal("export changed after queries")
	}
}

func TestExportSnapshotDoesNotMutateIndex(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	blocks, byHash, tip := snapshot(index)
	_ = exportBytes(t, index)
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestExportSnapshotWriteError(t *testing.T) {
	index := chain(t, Block{Height: 1, Hash: "h1", Parent: "genesis"})
	blocks, byHash, tip := snapshot(index)
	if err := index.ExportSnapshot(errWriter{}); err == nil {
		t.Fatal("expected write error")
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestRestoreSnapshotRoundTrip(t *testing.T) {
	source := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t1"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{}},
	)
	// Give the source some reorg history; the snapshot must contain only the
	// blocks that remain on the main chain.
	if _, err := source.Reorg([]Block{{Height: 3, Hash: "h3b", Parent: "h2", Txs: []string{"t3"}}}); err != nil {
		t.Fatal(err)
	}
	data := exportBytes(t, source)

	restored := New()
	restoreBytes(t, restored, data)

	if restored.Tip != source.Tip {
		t.Fatalf("tip=%d, want %d", restored.Tip, source.Tip)
	}
	if !reflect.DeepEqual(restored.Blocks, source.Blocks) {
		t.Fatalf("blocks differ:\n%+v\n%+v", restored.Blocks, source.Blocks)
	}
	if !reflect.DeepEqual(restored.ByHash, source.ByHash) {
		t.Fatalf("by-hash differs:\n%+v\n%+v", restored.ByHash, source.ByHash)
	}
	if _, ok := restored.ByHash["h3"]; ok {
		t.Fatal("reorged-away hash h3 must not appear in the snapshot")
	}
	if restored.Blocks[3].Hash != "h3b" {
		t.Fatalf("height 3 must be the reorg block: %+v", restored.Blocks[3])
	}
}

func TestRestoreSnapshotReplacesIndex(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
		Block{Height: 3, Hash: "h3", Parent: "h2"},
		Block{Height: 4, Hash: "h4", Parent: "h3"},
	)
	snap := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"genesis","txs":[]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[]}]}`
	if err := index.RestoreSnapshot(strings.NewReader(snap)); err != nil {
		t.Fatal(err)
	}
	if index.Tip != 2 {
		t.Fatalf("tip=%d, want 2", index.Tip)
	}
	if len(index.Blocks) != 2 {
		t.Fatalf("stale heights remain: %+v", index.Blocks)
	}
	for _, stale := range []string{"h3", "h4"} {
		if _, ok := index.ByHash[stale]; ok {
			t.Fatalf("stale hash %s lingers after restore", stale)
		}
	}
	if _, ok := index.Blocks[3]; ok {
		t.Fatal("stale height 3 lingers after restore")
	}
	// The index keeps working: appends extend the restored chain.
	if err := index.Append(Block{Height: 3, Hash: "h3n", Parent: "h2"}); err != nil {
		t.Fatalf("append after restore failed: %v", err)
	}
	if index.Tip != 3 || index.ByHash["h3n"] != 3 {
		t.Fatalf("append after restore misbehaved: tip=%d", index.Tip)
	}
	// Reorgs also keep working against the restored chain.
	if dropped, err := index.Reorg([]Block{{Height: 3, Hash: "h3r", Parent: "h2"}}); err != nil {
		t.Fatalf("reorg after restore failed: %v", err)
	} else if !reflect.DeepEqual(dropped, []int64{3}) {
		t.Fatalf("reorg after restore dropped=%v, want [3]", dropped)
	}
	if index.Tip != 3 || index.Blocks[3].Hash != "h3r" {
		t.Fatalf("reorg after restore misbehaved: tip=%d block=%+v", index.Tip, index.Blocks[3])
	}
}

func TestRestoreSnapshotEmptyThenIngest(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "old1", Parent: "genesis"},
		Block{Height: 2, Hash: "old2", Parent: "old1"},
	)
	restoreBytes(t, index, []byte(`{"version":1,"tip":0,"blocks":[]}`))
	if index.Tip != 0 || len(index.Blocks) != 0 || len(index.ByHash) != 0 {
		t.Fatalf("empty restore did not clear the index: tip=%d blocks=%v", index.Tip, index.Blocks)
	}
	// After an empty restore the chain starts again at height 1.
	if err := index.Append(Block{Height: 1, Hash: "new1", Parent: "any-start-id"}); err != nil {
		t.Fatalf("append after empty restore failed: %v", err)
	}
	if index.Tip != 1 || index.ByHash["new1"] != 1 {
		t.Fatalf("unexpected state after fresh ingest: tip=%d", index.Tip)
	}
}

func TestRestoreSnapshotQueryParity(t *testing.T) {
	source := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"c"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "d"}},
		Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{}},
	)
	// Drop a block so the snapshot proves reorged-away data is invisible.
	if _, err := source.Reorg([]Block{{Height: 4, Hash: "h4b", Parent: "h3", Txs: []string{"e"}}}); err != nil {
		t.Fatal(err)
	}
	restored := New()
	restoreBytes(t, restored, exportBytes(t, source))

	queries := []TxQuery{
		{},
		{From: 2},
		{From: 1, To: 3},
		{TxIDs: []string{"a"}},
		{TxIDs: []string{"a", "b", "e"}},
		{From: 2, To: 4, TxIDs: []string{"a", "d"}},
		{PageSize: 2},
		{From: 1, To: 4, PageSize: 3},
	}
	for _, query := range queries {
		want := collectPages(t, source, query)
		got := collectPages(t, restored, query)
		if len(got) != len(want) {
			t.Fatalf("query %+v: page count differs: %d vs %d", query, len(got), len(want))
		}
		for i := range want {
			// Cursor strings are allowed to differ; everything else must match.
			got[i].NextCursor, want[i].NextCursor = "", ""
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Fatalf("query %+v page %d differs:\n%+v\n%+v", query, i, got[i], want[i])
			}
		}
	}
	// The reorged-away transaction must not be found in either index.
	for _, idx := range []*Index{source, restored} {
		page, err := idx.QueryTxs(TxQuery{TxIDs: []string{"dropped-tx"}})
		if err != nil {
			t.Fatal(err)
		}
		if page.TotalMatches != 0 || len(page.Hits) != 0 {
			t.Fatalf("reorged-away tx surfaced: %+v", page)
		}
	}
}

func TestRestoreSnapshotCursorSurvivesSameChain(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"b"},
		[]string{"c"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}
	cursor := first.NextCursor

	// Restore the very same chain state: the pinned range is unchanged and
	// the tip is not below its upper bound, so the cursor must keep working.
	restoreBytes(t, index, exportBytes(t, index))
	second, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: cursor})
	if err != nil {
		t.Fatalf("valid continuation refused after same-chain restore: %v", err)
	}
	if second.TotalMatches != 3 || second.ToHeight != 3 {
		t.Fatalf("unexpected continuation page: %+v", second)
	}
	pages := collectPages(t, index, TxQuery{PageSize: 1, Cursor: cursor})
	if len(pages) != 2 {
		t.Fatalf("remaining pages=%d, want 2", len(pages))
	}
}

func TestRestoreSnapshotCursorDiesWithChangedRange(t *testing.T) {
	build := func(t *testing.T) (*Index, string) {
		t.Helper()
		index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
		first, err := index.QueryTxs(TxQuery{PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		return index, first.NextCursor
	}

	t.Run("content changed", func(t *testing.T) {
		index, cursor := build(t)
		snap := `{"version":1,"tip":3,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"genesis","txs":["a"]},` +
			`{"height":2,"hash":"h2","parent":"h1","txs":["z"]},` +
			`{"height":3,"hash":"h3","parent":"h2","txs":["c"]}]}`
		if err := index.RestoreSnapshot(strings.NewReader(snap)); err != nil {
			t.Fatal(err)
		}
		if _, err := index.QueryTxs(TxQuery{Cursor: cursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("chain shortened", func(t *testing.T) {
		index, cursor := build(t)
		snap := `{"version":1,"tip":2,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"genesis","txs":["a"]},` +
			`{"height":2,"hash":"h2","parent":"h1","txs":["b"]}]}`
		if err := index.RestoreSnapshot(strings.NewReader(snap)); err != nil {
			t.Fatal(err)
		}
		if _, err := index.QueryTxs(TxQuery{Cursor: cursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("foreign cursor still rejected", func(t *testing.T) {
		index, _ := build(t)
		other := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
		foreign, err := other.QueryTxs(TxQuery{PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		restoreBytes(t, index, exportBytes(t, index))
		if _, err := index.QueryTxs(TxQuery{Cursor: foreign.NextCursor}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("foreign cursor err=%v, want ErrInvalidArgument", err)
		}
	})
}

func TestRestoreSnapshotRejectsInvalidInput(t *testing.T) {
	valid := func() string {
		// Base snapshot with one block at height 1.
		return `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":["t"]}]}`
	}
	cases := map[string]string{
		"unknown version":           `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[]}]}`,
		"version string":            `{"version":"1","tip":0,"blocks":[]}`,
		"version float":             `{"version":1.5,"tip":0,"blocks":[]}`,
		"missing version":           `{"tip":0,"blocks":[]}`,
		"missing tip":               `{"version":1,"blocks":[]}`,
		"missing blocks":            `{"version":1,"tip":0}`,
		"null blocks":               `{"version":1,"tip":0,"blocks":null}`,
		"null txs":                  `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":null}]}`,
		"txs not array":             `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":"t"}]}`,
		"tx element number":         `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[1]}]}`,
		"height string":             `{"version":1,"tip":1,"blocks":[{"height":"1","hash":"h1","parent":"genesis","txs":[]}]}`,
		"tip string":                `{"version":"1","tip":"0","blocks":[]}`,
		"negative tip":              `{"version":1,"tip":-1,"blocks":[]}`,
		"tip above last height":     `{"version":1,"tip":2,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[]}]}`,
		"tip below last height":     `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[]},{"height":2,"hash":"h2","parent":"h1","txs":[]}]}`,
		"starts at height 2":        `{"version":1,"tip":1,"blocks":[{"height":2,"hash":"h2","parent":"h1","txs":[]}]}`,
		"gap in heights":            `{"version":1,"tip":3,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[]},{"height":3,"hash":"h3","parent":"h2","txs":[]}]}`,
		"height zero":               `{"version":1,"tip":0,"blocks":[{"height":0,"hash":"h0","parent":"genesis","txs":[]}]}`,
		"empty hash":                `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"","parent":"genesis","txs":[]}]}`,
		"duplicate hash":            `{"version":1,"tip":2,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[]},{"height":2,"hash":"h1","parent":"h1","txs":[]}]}`,
		"broken parent link":        `{"version":1,"tip":2,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[]},{"height":2,"hash":"h2","parent":"wrong","txs":[]}]}`,
		"parent matches own hash":   `{"version":1,"tip":2,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[]},{"height":2,"hash":"h2","parent":"h2","txs":[]}]}`,
		"missing block height":      `{"version":1,"tip":1,"blocks":[{"hash":"h1","parent":"genesis","txs":[]}]}`,
		"missing block hash":        `{"version":1,"tip":1,"blocks":[{"height":1,"parent":"genesis","txs":[]}]}`,
		"missing block parent":      `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","txs":[]}]}`,
		"missing block txs":         `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis"}]}`,
		"duplicate top-level field": `{"version":1,"version":1,"tip":0,"blocks":[]}`,
		"duplicate block field":     `{"version":1,"tip":1,"blocks":[{"height":1,"height":1,"hash":"h1","parent":"genesis","txs":[]}]}`,
		"unknown top-level field":   `{"version":1,"tip":0,"blocks":[],"extra":1}`,
		"unknown block field":       `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[],"extra":1}]}`,
		"trailing second object":    valid() + ` {}`,
		"trailing second snapshot":  valid() + ` ` + valid(),
		"trailing garbage":          valid() + ` xyz`,
		"trailing closing brace":    valid() + `}`,
		"empty input":               ``,
		"whitespace only":           `   `,
		"top-level array":           `[]`,
		"top-level null":            `null`,
		"blocks element not object": `{"version":1,"tip":1,"blocks":[42]}`,
		"late error at last block":  `{"version":1,"tip":3,"blocks":[{"height":1,"hash":"h1","parent":"genesis","txs":[]},{"height":2,"hash":"h2","parent":"h1","txs":[]},{"height":3,"hash":"h3","parent":"broken","txs":[]}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			index := chain(t,
				Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"keep"}},
				Block{Height: 2, Hash: "h2", Parent: "h1"},
			)
			// A valid cursor that must survive the rejected restore.
			first, err := index.QueryTxs(TxQuery{PageSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			cursor := first.NextCursor
			blocks, byHash, tip := snapshot(index)

			err = index.RestoreSnapshot(strings.NewReader(input))
			if err == nil {
				t.Fatalf("expected rejection, but restore succeeded")
			}
			if !strings.Contains(err.Error(), "invalid snapshot") && !strings.Contains(err.Error(), "snapshot") {
				t.Fatalf("error %q does not give a snapshot-specific reason", err)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
			// The valid cursor must still work.
			if _, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: cursor}); err != nil {
				t.Fatalf("valid cursor invalidated by rejected restore: %v", err)
			}
		})
	}
}

func TestRestoreSnapshotAcceptsWhitespaceAround(t *testing.T) {
	index := New()
	input := "\n\t " + `{"version":1,"tip":0,"blocks":[]}` + " \n\t"
	if err := index.RestoreSnapshot(strings.NewReader(input)); err != nil {
		t.Fatalf("whitespace-padded snapshot refused: %v", err)
	}
	if index.Tip != 0 {
		t.Fatalf("tip=%d, want 0", index.Tip)
	}
}

func TestRestoreSnapshotReadErrorLeavesState(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"keep"}},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	blocks, byHash, tip := snapshot(index)

	if err := index.RestoreSnapshot(errReader{}); err == nil {
		t.Fatal("expected read error")
	}
	requireUnchanged(t, index, blocks, byHash, tip)
	if _, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: first.NextCursor}); err != nil {
		t.Fatalf("valid cursor invalidated by read failure: %v", err)
	}
}

func TestSnapshotConcurrentWithWritersAndReaders(t *testing.T) {
	index := chain(t, Block{Height: 1, Hash: "h1", Parent: "genesis"})
	stop := make(chan struct{})

	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for height := int64(2); height <= 100; height++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = index.Append(Block{
				Height: height,
				Hash:   fmt.Sprintf("h%d", height),
				Parent: fmt.Sprintf("h%d", height-1),
				Txs:    []string{fmt.Sprintf("tx-%d", height)},
			})
		}
	}()

	var restorers sync.WaitGroup
	for r := 0; r < 3; r++ {
		restorers.Add(1)
		go func() {
			defer restorers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var buf bytes.Buffer
				if err := index.ExportSnapshot(&buf); err != nil {
					t.Errorf("export failed: %v", err)
					return
				}
				if err := index.RestoreSnapshot(&buf); err != nil {
					t.Errorf("restore failed: %v", err)
					return
				}
			}
		}()
	}

	var readers sync.WaitGroup
	for r := 0; r < 3; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				pages := collectPages(t, index, TxQuery{PageSize: 7})
				for _, page := range pages {
					if page.TotalMatches < 0 || page.ToHeight < 0 {
						t.Errorf("nonsense page: %+v", page)
						return
					}
				}
			}
		}()
	}

	// Let the writer finish, then stop the rest of the fleet.
	writers.Wait()
	close(stop)
	restorers.Wait()
	readers.Wait()

	// After all concurrent activity the chain must remain contiguous.
	if int64(len(index.Blocks)) != index.Tip || len(index.ByHash) != len(index.Blocks) {
		t.Fatalf("inconsistent index after concurrency: blocks=%d byHash=%d tip=%d",
			len(index.Blocks), len(index.ByHash), index.Tip)
	}
	for height := int64(2); height <= index.Tip; height++ {
		block, ok := index.Blocks[height]
		if !ok || block.Parent != index.Blocks[height-1].Hash {
			t.Fatalf("broken chain at height %d: %+v", height, index.Blocks[height])
		}
	}
}
