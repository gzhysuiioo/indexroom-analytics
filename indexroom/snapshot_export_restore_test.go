package indexroom

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file guards Export against a snapshot Restore that replaces the whole
// main chain while the export's output is still being written. Export copies
// one complete chain state under the lock and serializes it afterwards, so an
// export that started on the old chain must finish with exactly the old
// chain's bytes even when a restore replaces the index underneath it
// mid-write. The restore is not delayed by the in-flight export, the index
// afterwards holds only the restored chain, and an export failure that
// surfaces after the restore — a writer error or a short write — is reported
// without undoing the replacement.
//
// The scenario:
//
//	Old main chain, tip 3 (version 2 on the wire):
//	  h1 t=-  txs [t1, "", t1]  // missing time, duplicate and empty ids
//	  h2 t=0  txs ["", t2]      // a real zero timestamp
//	  h3 t=42 txs [t3, t3]
//	Version-1 snapshot replacing it, tip 2 (no timestamps at all):
//	  n1 txs [u1]
//	  n2 txs [u2, ""]
//
// The two chains share no hash and no transaction identifier, so any old
// block leaking into the post-restore index — or any restored block leaking
// into the in-flight export's bytes — is visible in an exact comparison.

// exportRaceOldChainBlocks is the three-block chain the racing export starts
// from: timestamps missing, 0, and 42, with duplicated and empty transaction
// identifiers.
func exportRaceOldChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "", "t1"}, Time: nil},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"", "t2"}, Time: intptr(0)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3", "t3"}, Time: intptr(42)},
	}
}

// exportRaceOldSnapshot is the exact version-2 document an export of
// exportRaceOldChainBlocks produces: null for the missing time, the real 0
// and 42 as integers, duplicates and empty identifiers verbatim.
const exportRaceOldSnapshot = `{"version":2,"tip":3,"blocks":[` +
	`{"height":1,"hash":"h1","parent":"genesis","txs":["t1","","t1"],"timestamp":null},` +
	`{"height":2,"hash":"h2","parent":"h1","txs":["","t2"],"timestamp":0},` +
	`{"height":3,"hash":"h3","parent":"h2","txs":["t3","t3"],"timestamp":42}` +
	`]}`

// exportRaceNewSnapshot is the version-1 snapshot that replaces the old
// chain: two blocks, no timestamps anywhere, disjoint hashes and tx ids.
const exportRaceNewSnapshot = `{"version":1,"tip":2,"blocks":[` +
	`{"height":1,"hash":"n1","parent":"seed","txs":["u1"]},` +
	`{"height":2,"hash":"n2","parent":"n1","txs":["u2",""]}` +
	`]}`

func buildExportRaceOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range exportRaceOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	// Pin the exact bytes the in-flight export must deliver.
	if raw := exportString(t, index); raw != exportRaceOldSnapshot {
		t.Fatalf("reference export=%s\nwant=%s", raw, exportRaceOldSnapshot)
	}
	return index
}

// requireRestoredChain verifies the index holds exactly the two-block
// snapshot chain: no height, hash, or transaction of the replaced chain
// survives, and a fresh export reproduces the version-1 document byte for
// byte (no timestamp fields, since no restored block carries a time).
func requireRestoredChain(t *testing.T, index *Index) {
	t.Helper()
	if index.Tip != 2 || len(index.Blocks) != 2 || len(index.ByHash) != 2 {
		t.Fatalf("restored shape: tip=%d blocks=%d byHash=%d", index.Tip, len(index.Blocks), len(index.ByHash))
	}
	if _, ok := index.Blocks[3]; ok {
		t.Fatal("replaced height 3 still present")
	}
	for _, gone := range []string{"h1", "h2", "h3"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("replaced hash %s still indexed", gone)
		}
	}
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatalf("query on restored chain failed: %v", err)
	}
	wantHits := []TxHit{
		{Height: 1, BlockHash: "n1", TxID: "u1", Position: 0},
		{Height: 2, BlockHash: "n2", TxID: "u2", Position: 0},
		{Height: 2, BlockHash: "n2", TxID: "", Position: 1},
	}
	if !reflect.DeepEqual(page.Hits, wantHits) ||
		page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 2 {
		t.Fatalf("query on restored chain=%+v, want hits %v on tip 2", page, wantHits)
	}
	if raw := exportString(t, index); raw != exportRaceNewSnapshot {
		t.Fatalf("re-export of restored chain=%s\nwant=%s", raw, exportRaceNewSnapshot)
	}
	if strings.Contains(exportString(t, index), "timestamp") {
		t.Fatal("restored all-timeless chain must export as version 1 without timestamp fields")
	}
}

// readExact reads exactly len(buf) bytes from r, failing the test on any
// shortfall. It never consumes beyond len(buf), so a caller can take a precise
// prefix off a pipe and leave the rest for later.
func readExact(t *testing.T, r io.Reader, buf []byte) {
	t.Helper()
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("reading export output: %v", err)
	}
}

// An export whose output stalls after the document prefix must not hold the
// restore back: the restore succeeds while the export is still parked, new
// queries see only the restored chain, and once the output side drains the
// rest, the export completes successfully with the complete OLD chain —
// version 2, tip 3, timestamps null/0/42, duplicates and empty ids intact,
// and nothing of the restored chain mixed in.
func TestExportInFlightFinishesOldChainWhileRestoreReplacesIndex(t *testing.T) {
	index := buildExportRaceOldChain(t)

	pr, pw := io.Pipe()
	exportDone := make(chan error, 1)
	go func() {
		exportDone <- index.Export(pw)
	}()

	// Receive only the document prefix — everything before the second block
	// object. The export's single Write stays blocked inside the pipe,
	// holding the remainder.
	cut := strings.Index(exportRaceOldSnapshot, `{"height":2`)
	if cut <= 0 {
		t.Fatal("test setup: cut marker not found in the expected document")
	}
	received := make([]byte, len(exportRaceOldSnapshot))
	readExact(t, pr, received[:cut])

	// The restore applies to the same index while the export is parked and
	// must succeed before the export ends.
	if err := index.Restore(strings.NewReader(exportRaceNewSnapshot)); err != nil {
		t.Fatalf("restore during in-flight export failed: %v", err)
	}
	select {
	case err := <-exportDone:
		t.Fatalf("export finished while its output was still gated: %v", err)
	default:
	}

	// A transaction query issued now sees only the restored two-block chain:
	// the replaced transactions and the extra third block are gone.
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatalf("query after restore failed: %v", err)
	}
	if page.ToHeight != 2 || page.TotalMatches != 3 {
		t.Fatalf("query after restore sees old chain residue: %+v", page)
	}
	for _, hit := range page.Hits {
		if hit.Height > 2 || strings.HasPrefix(hit.BlockHash, "h") || strings.HasPrefix(hit.TxID, "t") {
			t.Fatalf("old-chain hit leaked into post-restore query: %+v in %+v", hit, page.Hits)
		}
	}

	// The output side receives the rest; the earlier export must end
	// successfully and the delivered bytes must be the complete original
	// chain snapshot, unaffected by the restore that landed in between.
	readExact(t, pr, received[cut:])
	if err := <-exportDone; err != nil {
		t.Fatalf("export after drain failed: %v", err)
	}
	if got := string(received); got != exportRaceOldSnapshot {
		t.Fatalf("in-flight export delivered %s\nwant the complete old chain %s", got, exportRaceOldSnapshot)
	}

	// The index keeps the restored chain afterwards.
	requireRestoredChain(t, index)
}

// gatedWriter parks inside Write until release is closed, then reports n
// bytes consumed and err. It lets a test run a restore while an export is
// suspended mid-write and then decide how the write ends.
type gatedWriter struct {
	n       int
	err     error
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedWriter(n int, err error) *gatedWriter {
	return &gatedWriter{
		n:       n,
		err:     err,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.n, w.err
}

// runExportAcrossRestore starts an export that parks inside the writer,
// completes the restore while the export is suspended, then releases the
// writer and returns the export's result.
func runExportAcrossRestore(t *testing.T, index *Index, w *gatedWriter) error {
	t.Helper()
	exportDone := make(chan error, 1)
	go func() {
		exportDone <- index.Export(w)
	}()
	<-w.entered // the export is parked inside the write
	if err := index.Restore(strings.NewReader(exportRaceNewSnapshot)); err != nil {
		t.Fatalf("restore during in-flight export failed: %v", err)
	}
	close(w.release)
	return <-exportDone
}

// A writer error surfacing after the restore must reach the caller
// identifiably, and the failed export must not roll the index back to the
// old chain.
func TestExportWriteFailureAfterRestoreKeepsRestoredChain(t *testing.T) {
	index := buildExportRaceOldChain(t)
	boom := errors.New("disk full")
	err := runExportAcrossRestore(t, index, newGatedWriter(0, boom))
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the writer's failure", err)
	}
	requireRestoredChain(t, index)
}

// A writer that silently consumes only part of the document must surface as
// io.ErrShortWrite; the completed restore is equally untouched.
func TestExportShortWriteAfterRestoreKeepsRestoredChain(t *testing.T) {
	index := buildExportRaceOldChain(t)
	short := len(exportRaceOldSnapshot) / 2
	err := runExportAcrossRestore(t, index, newGatedWriter(short, nil))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("err=%v, want io.ErrShortWrite", err)
	}
	requireRestoredChain(t, index)
}
