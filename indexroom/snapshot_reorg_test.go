package indexroom

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedWriter blocks inside the first Write until the test releases it, so a
// reorg can run and complete while an export is still streaming the chain
// state it selected before the write began.
type gatedWriter struct {
	entered chan struct{} // closed when the first Write begins
	release chan struct{} // closed to let the blocked write return
	once    sync.Once
	err     error // returned by the blocked write once released
	short   bool  // the blocked write reports a short count without an error
	buf     bytes.Buffer
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	switch {
	case w.err != nil:
		return 0, w.err
	case w.short:
		w.buf.Write(p[:len(p)-1])
		return len(p) - 1, nil
	default:
		w.buf.Write(p)
		return len(p), nil
	}
}

// awaitEntered fails unless the output's first Write begins promptly.
func (w *gatedWriter) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-w.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("export did not start writing to the output")
	}
}

// exportResult carries one finished export's bytes and error back to the
// test goroutine.
type exportResult struct {
	raw string
	err error
}

// startExport launches an export to w in the background.
func startExport(index *Index, w *gatedWriter) chan exportResult {
	done := make(chan exportResult, 1)
	go func() {
		err := index.Export(w)
		done <- exportResult{raw: w.buf.String(), err: err}
	}()
	return done
}

// requireExportPending fails if the export has already finished: the
// not-yet-finished state must be observable while a reorg completes.
func requireExportPending(t *testing.T, done chan exportResult) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("export finished before the output was released: %+v", result)
	default:
	}
}

// awaitExport fails unless the export finishes promptly once released.
func awaitExport(t *testing.T, done chan exportResult) exportResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("export did not finish after the output was released")
		return exportResult{}
	}
}

// oldTimedChain is the pre-reorg main chain: a missing timestamp, a zero
// timestamp, and one real timestamp, so its export is a version-2 document.
// Transaction identifiers carry duplicates and empty strings on purpose.
func oldTimedChain(t *testing.T) *Index {
	t.Helper()
	return chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "", "t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}, Time: intptr(0)},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3", "t3"}, Time: intptr(1700000000)},
	)
}

const oldTimedChainSnapshot = `{"version":2,"tip":3,"blocks":[` +
	`{"height":1,"hash":"h1","parent":"genesis","txs":["t1","","t1"],"timestamp":null},` +
	`{"height":2,"hash":"h2","parent":"h1","txs":["t2"],"timestamp":0},` +
	`{"height":3,"hash":"h3","parent":"h2","txs":["t3","t3"],"timestamp":1700000000}` +
	`]}`

// newTimelessBranch replaces heights 2 and 3 — dropping the zero-timestamp
// block and the only real-timestamp block — so the post-reorg chain holds no
// timestamps at all and exports as a version-1 document.
func newTimelessBranch() []Block {
	return []Block{
		{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"n2", "", "n2"}},
		{Height: 3, Hash: "h3b", Parent: "h2b", Txs: []string{"n3"}},
	}
}

const newTimelessChainSnapshot = `{"version":1,"tip":3,"blocks":[` +
	`{"height":1,"hash":"h1","parent":"genesis","txs":["t1","","t1"]},` +
	`{"height":2,"hash":"h2b","parent":"h1","txs":["n2","","n2"]},` +
	`{"height":3,"hash":"h3b","parent":"h2b","txs":["n3"]}` +
	`]}`

// reorgMidExport replaces the timed suffix while an export is blocked on the
// output, and confirms both observable states at once: the reorg is complete
// (queries see the replacement blocks and transactions) and the export is
// still unfinished.
func reorgMidExport(t *testing.T, index *Index, done chan exportResult) {
	t.Helper()
	dropped, err := index.Reorg(newTimelessBranch())
	if err != nil {
		t.Fatalf("reorg during export refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{2, 3}) {
		t.Fatalf("dropped=%v, want [2 3]", dropped)
	}
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatalf("query after reorg failed: %v", err)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 2},
		{Height: 2, BlockHash: "h2b", TxID: "n2", Position: 0},
		{Height: 2, BlockHash: "h2b", TxID: "", Position: 1},
		{Height: 2, BlockHash: "h2b", TxID: "n2", Position: 2},
		{Height: 3, BlockHash: "h3b", TxID: "n3", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits after reorg=%v, want %v", page.Hits, want)
	}
	requireExportPending(t, done)
}

func TestExportInFlightSnapshotSurvivesReorg(t *testing.T) {
	index := oldTimedChain(t)
	writer := newGatedWriter()
	done := startExport(index, writer)
	writer.awaitEntered(t)

	// The export is streaming the pre-reorg chain; the reorg completes and
	// is queryable while the output is still blocked.
	reorgMidExport(t, index, done)

	// Releasing the output lets the export finish with the chain state it
	// selected before the reorg: the version-2 document with a null
	// timestamp, a zero timestamp, and the dropped branch's hashes and
	// transactions in their original order.
	close(writer.release)
	result := awaitExport(t, done)
	if result.err != nil {
		t.Fatalf("export failed: %v", result.err)
	}
	if result.raw != oldTimedChainSnapshot {
		t.Fatalf("export=%s\nwant=%s", result.raw, oldTimedChainSnapshot)
	}

	// The old blocks appearing in that snapshot did not leak back into the
	// main chain: the reorged chain is still in effect, and a fresh export
	// is the version-1 document of the new chain with no timestamp fields.
	if index.Tip != 3 || index.ByHash["h2b"] != 2 || index.ByHash["h3b"] != 3 {
		t.Fatalf("unexpected chain after export: tip=%d byHash=%v", index.Tip, index.ByHash)
	}
	for _, hash := range []string{"h2", "h3"} {
		if _, ok := index.ByHash[hash]; ok {
			t.Fatalf("reorged-away hash %s is indexed again", hash)
		}
	}
	raw := exportString(t, index)
	if raw != newTimelessChainSnapshot {
		t.Fatalf("post-reorg export=%s\nwant=%s", raw, newTimelessChainSnapshot)
	}
	if strings.Contains(raw, "timestamp") {
		t.Fatalf("version-1 export carries a timestamp field: %s", raw)
	}

	// The earlier snapshot restores into another index, where the old
	// chain's transactions are queryable, while the original index keeps
	// serving the reorged chain.
	restored := New()
	if err := restored.Restore(strings.NewReader(oldTimedChainSnapshot)); err != nil {
		t.Fatalf("restore of the in-flight snapshot failed: %v", err)
	}
	oldPage, err := restored.QueryTxs(TxQuery{TxIDs: []string{"t3"}})
	if err != nil {
		t.Fatal(err)
	}
	wantOld := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "t3", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "t3", Position: 1},
	}
	if !reflect.DeepEqual(oldPage.Hits, wantOld) {
		t.Fatalf("restored hits=%v, want %v", oldPage.Hits, wantOld)
	}
	gonePage, err := index.QueryTxs(TxQuery{TxIDs: []string{"t2", "t3"}})
	if err != nil {
		t.Fatal(err)
	}
	if gonePage.TotalMatches != 0 {
		t.Fatalf("reorged-away transactions still visible: %+v", gonePage)
	}
	newPage, err := index.QueryTxs(TxQuery{TxIDs: []string{"n2"}})
	if err != nil {
		t.Fatal(err)
	}
	wantNew := []TxHit{
		{Height: 2, BlockHash: "h2b", TxID: "n2", Position: 0},
		{Height: 2, BlockHash: "h2b", TxID: "n2", Position: 2},
	}
	if !reflect.DeepEqual(newPage.Hits, wantNew) {
		t.Fatalf("post-reorg hits=%v, want %v", newPage.Hits, wantNew)
	}
}

func TestExportFailureMidReorgKeepsReorgedChain(t *testing.T) {
	boom := errors.New("disk full")
	cases := map[string]struct {
		prepare func(w *gatedWriter)
		wantErr error
	}{
		"write error": {func(w *gatedWriter) { w.err = boom }, boom},
		"short write": {func(w *gatedWriter) { w.short = true }, io.ErrShortWrite},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := oldTimedChain(t)
			writer := newGatedWriter()
			tc.prepare(writer)
			done := startExport(index, writer)
			writer.awaitEntered(t)

			// The reorg completes while the export is still blocked.
			reorgMidExport(t, index, done)
			blocks, byHash, tip := snapshot(index)

			close(writer.release)
			result := awaitExport(t, done)
			if !errors.Is(result.err, tc.wantErr) {
				t.Fatalf("err=%v, want errors.Is(_, %v)", result.err, tc.wantErr)
			}
			if result.err == nil {
				t.Fatal("export succeeded despite the failing output")
			}

			// The failed export neither undid the reorg nor changed the
			// chain in any other way.
			requireUnchanged(t, index, blocks, byHash, tip)
			if raw := exportString(t, index); raw != newTimelessChainSnapshot {
				t.Fatalf("post-failure export=%s\nwant=%s", raw, newTimelessChainSnapshot)
			}
		})
	}
}
