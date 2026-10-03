package indexroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// These tests pin down the observable contract that one Export observes one
// complete main-chain state: once serialization has started, a reorg that
// replaces the branch while the output sink has not yet received the whole
// document neither blocks nor contaminates the in-flight snapshot, and a
// completed reorg is never undone by an output failure discovered afterwards.

// assertExportStillRunning fails if the export goroutine has already
// returned. It is a non-blocking observation point that distinguishes the
// "export not finished yet" state from the "reorg completed" state without
// any sleeps.
func assertExportStillRunning(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("export finished before the sink was released: %v", err)
	default:
	}
}

func TestExportPinsCompleteChainStateAcrossMidStreamReorg(t *testing.T) {
	// The old main chain mixes every timestamp shape: a missing time, a
	// real zero timestamp, and the only non-zero timestamp. Its duplicate
	// identifiers, empty identifier, and their ordering must all survive.
	index := chain(t,
		Block{Height: 1, Hash: "old-a", Parent: "genesis", Txs: []string{"dup", ""}},
		Block{Height: 2, Hash: "old-b", Parent: "old-a", Txs: []string{"dup", "old-tx"}, Time: intptr(0)},
		Block{Height: 3, Hash: "old-c", Parent: "old-b", Txs: []string{"old-tx", "after"}, Time: intptr(1700000000)},
	)
	wantOldSnapshot := `{"version":2,"tip":3,"blocks":[` +
		`{"height":1,"hash":"old-a","parent":"genesis","txs":["dup",""],"timestamp":null},` +
		`{"height":2,"hash":"old-b","parent":"old-a","txs":["dup","old-tx"],"timestamp":0},` +
		`{"height":3,"hash":"old-c","parent":"old-b","txs":["old-tx","after"],"timestamp":1700000000}` +
		`]}`

	// A pipe lets the test hold the export mid-write: Export makes one Write
	// of the whole document, which stays blocked until every byte is drained.
	// Closing the pipe after Export returns gives the drainer its EOF.
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := index.Export(pw)
		pw.Close()
		done <- err
	}()

	// Observable state 1: writing has begun (one byte already in flight) but
	// the export has not finished.
	first := make([]byte, 1)
	if n, err := pr.Read(first); err != nil || n != 1 {
		t.Fatalf("first read: n=%d err=%v", n, err)
	}
	if string(first[0]) != wantOldSnapshot[:1] {
		t.Fatalf("in-flight output starts with %q, want %q", first[0], wantOldSnapshot[0])
	}
	assertExportStillRunning(t, done)

	// While that write is still open, a legal reorg replaces heights 2 and 3
	// and extends the chain with a new block. The replacement branch carries
	// no timestamps at all.
	dropped, err := index.Reorg([]Block{
		{Height: 2, Hash: "new-b", Parent: "old-a", Txs: []string{"new-tx", "", "new-tx"}},
		{Height: 3, Hash: "new-c", Parent: "new-b", Txs: []string{"after"}},
		{Height: 4, Hash: "new-d", Parent: "new-c", Txs: []string{"new-tx"}},
	})
	if err != nil {
		t.Fatalf("reorg while export in flight refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{2, 3}) {
		t.Fatalf("dropped=%v, want [2 3]", dropped)
	}

	// Observable state 2: the reorg has completed and is visible through
	// queries and the index, while the earlier export is still open.
	assertExportStillRunning(t, done)
	if index.Tip != 4 || index.Blocks[2].Hash != "new-b" || index.Blocks[3].Hash != "new-c" {
		t.Fatalf("reorg not observable on the index: %+v", index.Blocks)
	}
	for _, gone := range []string{"old-b", "old-c"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("reorged-away hash %s still indexed", gone)
		}
	}
	if got := index.Blocks[2].Txs; !reflect.DeepEqual(got, []string{"new-tx", "", "new-tx"}) {
		t.Fatalf("new block txs=%q", got)
	}
	page, err := index.QueryTxs(TxQuery{})
	if err != nil || page.TotalMatches != 7 || page.ToHeight != 4 {
		t.Fatalf("query after reorg: page=%+v err=%v", page, err)
	}
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"old-tx"}})
	if err != nil || page.TotalMatches != 0 {
		t.Fatalf("old tx still queryable after reorg: %+v err=%v", page, err)
	}
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"new-tx"}})
	if err != nil {
		t.Fatal(err)
	}
	wantNewHits := []TxHit{
		{Height: 2, BlockHash: "new-b", TxID: "new-tx", Position: 0},
		{Height: 2, BlockHash: "new-b", TxID: "new-tx", Position: 2},
		{Height: 4, BlockHash: "new-d", TxID: "new-tx", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, wantNewHits) {
		t.Fatalf("new-tx hits=%v, want %v", page.Hits, wantNewHits)
	}

	// The sink finishes receiving: the export ends successfully and its
	// bytes describe exactly the old main chain selected when writing began.
	rest, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("draining the export failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("export after reorg failed: %v", err)
	}
	raw := string(first[0]) + string(rest)
	if raw != wantOldSnapshot {
		t.Fatalf("pinned export=%s\nwant=%s", raw, wantOldSnapshot)
	}
	// Explicit leakage guards: the document must name no replacement hash,
	// transaction, or the new tip, and must keep the old height cap.
	for _, leaked := range []string{"new-b", "new-c", "new-d", "new-tx", `"tip":4`} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("old snapshot leaks replacement content %q: %s", leaked, raw)
		}
	}
	var timedDoc struct {
		Version int64 `json:"version"`
		Tip     int64 `json:"tip"`
		Blocks  []struct {
			Height    int64           `json:"height"`
			Hash      string          `json:"hash"`
			Parent    string          `json:"parent"`
			Txs       []string        `json:"txs"`
			Timestamp json.RawMessage `json:"timestamp"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(raw), &timedDoc); err != nil {
		t.Fatalf("pinned snapshot does not parse: %v", err)
	}
	if timedDoc.Version != 2 || timedDoc.Tip != 3 || len(timedDoc.Blocks) != 3 {
		t.Fatalf("pinned snapshot shape: %+v", timedDoc)
	}
	wantTimes := []string{"null", "0", "1700000000"}
	for i, block := range timedDoc.Blocks {
		if got := string(block.Timestamp); got != wantTimes[i] {
			t.Fatalf("block %d timestamp=%s, want %s", i+1, got, wantTimes[i])
		}
		if i > 0 && block.Parent != timedDoc.Blocks[i-1].Hash {
			t.Fatalf("pinned snapshot breaks the parent link at height %d", block.Height)
		}
	}

	// The old snapshot restores into a separate index: old-chain txs are
	// queryable there, including duplicates, the empty id, and their order;
	// missing/zero/real timestamps come back as distinct values.
	restored := New()
	if err := restored.Restore(strings.NewReader(raw)); err != nil {
		t.Fatalf("restore of pinned snapshot failed: %v", err)
	}
	if restored.Blocks[1].Time != nil {
		t.Fatalf("height 1 time=%v, want missing", restored.Blocks[1].Time)
	}
	if restored.Blocks[2].Time == nil || *restored.Blocks[2].Time != 0 {
		t.Fatalf("height 2 time=%v, want 0", restored.Blocks[2].Time)
	}
	if restored.Blocks[3].Time == nil || *restored.Blocks[3].Time != 1700000000 {
		t.Fatalf("height 3 time=%v, want 1700000000", restored.Blocks[3].Time)
	}
	page, err = restored.QueryTxs(TxQuery{})
	if err != nil || page.TotalMatches != 6 || page.ToHeight != 3 {
		t.Fatalf("restored old-chain query: page=%+v err=%v", page, err)
	}
	page, err = restored.QueryTxs(TxQuery{TxIDs: []string{"old-tx"}})
	if err != nil {
		t.Fatal(err)
	}
	wantOldHits := []TxHit{
		{Height: 2, BlockHash: "old-b", TxID: "old-tx", Position: 1},
		{Height: 3, BlockHash: "old-c", TxID: "old-tx", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, wantOldHits) {
		t.Fatalf("old-tx hits on restored index=%v, want %v", page.Hits, wantOldHits)
	}
	page, err = restored.QueryTxs(TxQuery{TxIDs: []string{"new-tx"}})
	if err != nil || page.TotalMatches != 0 {
		t.Fatalf("restored old index sees replacement txs: %+v err=%v", page, err)
	}
	if again := exportString(t, restored); again != raw {
		t.Fatalf("re-export of restored old snapshot differs:\n%s\n%s", again, raw)
	}

	// Restoring the old chain elsewhere leaves the current main chain alone.
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"after"}})
	if err != nil {
		t.Fatal(err)
	}
	wantAfter := []TxHit{{Height: 3, BlockHash: "new-c", TxID: "after", Position: 0}}
	if !reflect.DeepEqual(page.Hits, wantAfter) {
		t.Fatalf("after-tx on current index=%v, want %v", page.Hits, wantAfter)
	}

	// A second export after the reorg reflects only the new, fully untimed
	// chain: version 1, no timestamp field anywhere, tip and hashes new.
	wantNewSnapshot := `{"version":1,"tip":4,"blocks":[` +
		`{"height":1,"hash":"old-a","parent":"genesis","txs":["dup",""]},` +
		`{"height":2,"hash":"new-b","parent":"old-a","txs":["new-tx","","new-tx"]},` +
		`{"height":3,"hash":"new-c","parent":"new-b","txs":["after"]},` +
		`{"height":4,"hash":"new-d","parent":"new-c","txs":["new-tx"]}` +
		`]}`
	if rawNew := exportString(t, index); rawNew != wantNewSnapshot {
		t.Fatalf("post-reorg export=%s\nwant=%s", rawNew, wantNewSnapshot)
	}
	if strings.Contains(exportString(t, index), "timestamp") {
		t.Fatalf("post-reorg snapshot still carries a timestamp field")
	}
}

// gateWriteOutcome is what a blockingGateWriter returns from its one blocked
// Write call.
type gateWriteOutcome struct {
	accepted int
	err      error
}

// blockingGateWriter silently accepts up to prefix bytes of the first write,
// records them, signals entered, and then blocks until the test releases it
// with a fixed return value. Export performs exactly one Write, which is all
// this writer needs to support.
type blockingGateWriter struct {
	prefix   int
	entered  chan struct{}
	release  chan gateWriteOutcome
	received bytes.Buffer
}

func (w *blockingGateWriter) Write(p []byte) (int, error) {
	n := w.prefix
	if n > len(p) {
		n = len(p)
	}
	w.received.Write(p[:n])
	close(w.entered)
	out := <-w.release
	if out.accepted > len(p) {
		out.accepted = len(p)
	}
	return out.accepted, out.err
}

// startTimedExportReorgedMidStream builds an old chain whose only non-missing
// timestamp sits on the block about to be reorged away, starts an export into
// a gate blocked after 16 bytes, performs the reorg while the export is open,
// and verifies every observable state along the way. The post-reorg chain is
// entirely untimed, so a fresh export switches from version 2 to version 1.
func startTimedExportReorgedMidStream(t *testing.T) (*Index, string, *blockingGateWriter, <-chan error) {
	t.Helper()
	index := chain(t,
		Block{Height: 1, Hash: "old-a", Parent: "genesis", Txs: []string{"old-a-tx"}},
		Block{Height: 2, Hash: "old-b", Parent: "old-a", Txs: []string{"old-b-tx"}, Time: intptr(1700000000)},
	)
	wantOld := exportString(t, index)
	if !strings.Contains(wantOld, `"version":2`) || len(wantOld) <= 17 {
		t.Fatalf("test setup: old snapshot too small or not v2: %s", wantOld)
	}

	writer := &blockingGateWriter{
		prefix:  16,
		entered: make(chan struct{}),
		release: make(chan gateWriteOutcome),
	}
	done := make(chan error, 1)
	go func() {
		done <- index.Export(writer)
	}()
	<-writer.entered
	if got := writer.received.String(); got != wantOld[:16] {
		t.Fatalf("in-flight prefix=%q, want old-chain prefix %q", got, wantOld[:16])
	}
	assertExportStillRunning(t, done)

	dropped, err := index.Reorg([]Block{
		{Height: 2, Hash: "new-b", Parent: "old-a", Txs: []string{"new-b-tx", "", "new-b-tx"}},
	})
	if err != nil {
		t.Fatalf("reorg while export blocked refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{2}) {
		t.Fatalf("dropped=%v, want [2]", dropped)
	}
	assertExportStillRunning(t, done)
	return index, wantOld, writer, done
}

// requireCurrentChainIsReplacement verifies the reorg survived and the
// current main chain is the untimed replacement, including through a fresh
// export that must be version 1 with no timestamp residue.
func requireCurrentChainIsReplacement(t *testing.T, index *Index) {
	t.Helper()
	if index.Tip != 2 {
		t.Fatalf("tip=%d, want 2", index.Tip)
	}
	block := index.Blocks[2]
	if block.Hash != "new-b" || block.Parent != "old-a" || block.Time != nil {
		t.Fatalf("height 2=%+v, want new-b without a timestamp", block)
	}
	if !reflect.DeepEqual(block.Txs, []string{"new-b-tx", "", "new-b-tx"}) {
		t.Fatalf("replacement txs=%q", block.Txs)
	}
	if _, ok := index.ByHash["old-b"]; ok {
		t.Fatal("reorged-away hash old-b still indexed")
	}
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"old-b-tx"}})
	if err != nil || page.TotalMatches != 0 {
		t.Fatalf("old tx still queryable: %+v err=%v", page, err)
	}
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"new-b-tx"}})
	if err != nil || page.TotalMatches != 2 {
		t.Fatalf("new tx matches=%d err=%v, want 2", page.TotalMatches, err)
	}
	raw := exportString(t, index)
	want := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"old-a","parent":"genesis","txs":["old-a-tx"]},` +
		`{"height":2,"hash":"new-b","parent":"old-a","txs":["new-b-tx","","new-b-tx"]}` +
		`]}`
	if raw != want {
		t.Fatalf("post-reorg export=%s\nwant=%s", raw, want)
	}
	if strings.Contains(raw, "timestamp") {
		t.Fatalf("post-reorg snapshot still carries a timestamp field: %s", raw)
	}
}

func TestExportSinkFailureAfterReorgKeepsReorg(t *testing.T) {
	t.Run("sink returns concrete error", func(t *testing.T) {
		index, _, writer, done := startTimedExportReorgedMidStream(t)
		boom := errors.New("sink disk full")
		writer.release <- gateWriteOutcome{accepted: 16, err: boom}
		if err := <-done; !errors.Is(err, boom) {
			t.Fatalf("export err=%v, want the sink error", err)
		}
		// The completed reorg stands; the failed export changed nothing.
		requireCurrentChainIsReplacement(t, index)
	})

	t.Run("sink accepts only some bytes with no error", func(t *testing.T) {
		index, wantOld, writer, done := startTimedExportReorgedMidStream(t)
		// Honest short write: the sink keeps only the 16 buffered bytes of
		// the document and returns nil instead of an error.
		writer.release <- gateWriteOutcome{accepted: 16, err: nil}
		if 16 >= len(wantOld) {
			t.Fatalf("test setup: accepted bytes must be below %d", len(wantOld))
		}
		if err := <-done; !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("export err=%v, want io.ErrShortWrite", err)
		}
		requireCurrentChainIsReplacement(t, index)
	})
}
