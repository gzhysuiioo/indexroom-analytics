package indexroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file is the regression guard for using Export and Restore against the
// same index at the same time. Export copies one complete main chain under
// index.mu and serializes and streams it only after the lock is released;
// Restore applies its validated snapshot as one whole replacement under the
// same lock. When an export's stream is still open while a restore replaces
// the chain, the public behavior is therefore fixed as follows:
//
//   - the restore completes while the old export is still writing, and
//     afterwards queries see only the snapshot chain;
//   - the parked export, once its output drains, still finishes successfully
//     with the complete OLD chain snapshot it copied — same version, tip,
//     block order, parent links, transaction order (duplicates and empty
//     identifiers included), and timestamp literals; no restored block is
//     spliced in and the version is not recomputed against the new chain;
//   - a later export of the same index describes the restored chain only;
//   - an output failure that arrives after the restore succeeded (the
//     writer's concrete error, or a short write reported without one) is
//     returned wrapped so errors.Is recognizes the original cause, and never
//     rolls the restore back.
//
// Scenario:
//
//	Old main chain, tip 3 (version 2 once exported):
//	  old1 parent g    txs [dup, "", dup]  time missing
//	  old2 parent old1 txs [alpha]         time 0
//	  old3 parent old2 txs [gamma]         time 42
//	Restore snapshot, tip 2, version 1, no block carries a time:
//	  new1 parent s    txs [p]
//	  new2 parent new1 txs [q, q]
//
// The two chains share neither a block hash nor a transaction identifier, so
// a query answer or a byte in either snapshot is attributable to exactly one
// of the chains.

// exportReplaceOldChainBlocks builds the three-block original chain: times
// missing, zero, and 42 in that order, with a duplicated and an empty
// transaction identifier in the first block.
func exportReplaceOldChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "old1", Parent: "g", Txs: []string{"dup", "", "dup"}, Time: nil},
		{Height: 2, Hash: "old2", Parent: "old1", Txs: []string{"alpha"}, Time: intptr(0)},
		{Height: 3, Hash: "old3", Parent: "old2", Txs: []string{"gamma"}, Time: intptr(42)},
	}
}

// exportReplaceSnapshot is the valid version-1 wire form of the two-block
// chain (new1, new2) that restores over the original. Neither block carries a
// timestamp, and neither its hashes nor its transaction identifiers occur in
// the original chain.
const exportReplaceSnapshot = `{"version":1,"tip":2,"blocks":[` +
	`{"height":1,"hash":"new1","parent":"s","txs":["p"]},` +
	`{"height":2,"hash":"new2","parent":"new1","txs":["q","q"]}` +
	`]}`

// exportReplaceHeaderPrefix is the document prefix the output end receives
// before it is parked: the version and tip header, before any block bytes.
const exportReplaceHeaderPrefix = `{"version":2,"tip":3,`

func buildExportReplaceChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range exportReplaceOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at height %d: %v", b.Height, err)
		}
	}
	return index
}

// The fixture the regression rests on, checked directly: the old chain
// exports version 2 with the missing/zero/42 timestamp literals, the restore
// snapshot parses into two timestamp-less blocks, and the two chains share
// neither a hash nor a transaction identifier (the empty identifier included).
func TestExportReplaceFixtureSemantics(t *testing.T) {
	index := buildExportReplaceChain(t)
	raw := exportString(t, index)
	doc := decodeExportSnapshot(t, raw)
	if doc.Version != 2 || doc.Tip != 3 || len(doc.Blocks) != 3 {
		t.Fatalf("old chain export=%+v, want version 2 tip 3 with three blocks", doc)
	}
	wantTimes := []string{"null", "0", "42"}
	for i, want := range wantTimes {
		got, ok := doc.Blocks[i]["timestamp"]
		if !ok {
			t.Fatalf("old block %d is missing its timestamp field", i+1)
		}
		if string(got) != want {
			t.Fatalf("old block %d timestamp=%s, want %s", i+1, got, want)
		}
	}

	tip, blocks, err := parseSnapshot(strings.NewReader(exportReplaceSnapshot))
	if err != nil {
		t.Fatalf("restore snapshot must parse: %v", err)
	}
	if tip != 2 || len(blocks) != 2 {
		t.Fatalf("parsed restore snapshot tip=%d blocks=%d, want tip 2 with 2 blocks", tip, len(blocks))
	}
	wantNew := []Block{
		{Height: 1, Hash: "new1", Parent: "s", Txs: []string{"p"}},
		{Height: 2, Hash: "new2", Parent: "new1", Txs: []string{"q", "q"}},
	}
	for i := range wantNew {
		if !reflect.DeepEqual(blocks[i], wantNew[i]) {
			t.Fatalf("parsed new block %d=%+v, want %+v", i+1, blocks[i], wantNew[i])
		}
		if blocks[i].Time != nil {
			t.Fatalf("new block %d carries time %v, want a missing time", i+1, blocks[i].Time)
		}
	}

	oldHashes := map[string]bool{"old1": true, "old2": true, "old3": true}
	for _, hash := range []string{"new1", "new2"} {
		if oldHashes[hash] {
			t.Fatalf("fixture hash %q is shared between the chains", hash)
		}
	}
	oldTxIDs := map[string]bool{"dup": true, "": true, "alpha": true, "gamma": true}
	for _, id := range []string{"p", "q"} {
		if oldTxIDs[id] {
			t.Fatalf("fixture tx %q is shared between the chains", id)
		}
	}
}

// gatedExportWriter accepts a prefix of the first write verbatim, then parks
// the writer until release is closed. On release it either fails with fail,
// reports a short successful write (short: accepting the prefix only with no
// error), or drains the rest normally. It makes "the export has started and
// only the prefix is out" a deterministic rendezvous without sleeps.
type gatedExportWriter struct {
	prefix  int
	mu      sync.Mutex
	buf     bytes.Buffer
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	fail    error
	short   bool
}

func newGatedExportWriter(prefix int, fail error, short bool) *gatedExportWriter {
	return &gatedExportWriter{
		prefix:  prefix,
		reached: make(chan struct{}),
		release: make(chan struct{}),
		fail:    fail,
		short:   short,
	}
}

func (w *gatedExportWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.buf.Len() < w.prefix {
		take := w.prefix - w.buf.Len()
		if take > len(p) {
			take = len(p)
		}
		w.buf.Write(p[:take])
	}
	parked := w.buf.Len() >= w.prefix
	w.mu.Unlock()

	if parked {
		w.once.Do(func() { close(w.reached) })
		<-w.release
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	accepted := w.buf.Len()
	switch {
	case w.fail != nil:
		return accepted, w.fail
	case w.short:
		// Partial acceptance reported as a clean write: Export must translate
		// this into io.ErrShortWrite.
		return accepted, nil
	default:
		if len(p) > accepted {
			w.buf.Write(p[accepted:])
		}
		return len(p), nil
	}
}

func (w *gatedExportWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// exportWireDoc decodes exported bytes with every block kept as a raw field
// map, so the presence of a "timestamp" key is observable independently of
// its value (version 1 must omit it entirely).
type exportWireDoc struct {
	Version int64                        `json:"version"`
	Tip     int64                        `json:"tip"`
	Blocks  []map[string]json.RawMessage `json:"blocks"`
}

func decodeExportSnapshot(t *testing.T, raw string) exportWireDoc {
	t.Helper()
	var doc exportWireDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("exported snapshot does not parse: %v\n%s", err, raw)
	}
	return doc
}

func requireRawString(t *testing.T, raw json.RawMessage, field string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("field %q is not a JSON string: %v", field, err)
	}
	return s
}

func requireRawNumber(t *testing.T, raw json.RawMessage, field string) string {
	t.Helper()
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("field %q is not a JSON integer: %v", field, err)
	}
	return jsonNumber(n)
}

func requireRawStrings(t *testing.T, raw json.RawMessage, field string) []string {
	t.Helper()
	var ss []string
	if err := json.Unmarshal(raw, &ss); err != nil {
		t.Fatalf("field %q is not a JSON string array: %v", field, err)
	}
	return ss
}

// requireOldChainExport asserts raw is the complete original chain snapshot:
// version 2, tip 3, blocks at heights 1..3 with unchanged parent links and
// transaction order, null/0/42 timestamp literals, and no content from the
// restored two-block chain anywhere.
func requireOldChainExport(t *testing.T, raw string) {
	t.Helper()
	want := []struct {
		height int64
		hash   string
		parent string
		txs    []string
		time   string
	}{
		{1, "old1", "g", []string{"dup", "", "dup"}, "null"},
		{2, "old2", "old1", []string{"alpha"}, "0"},
		{3, "old3", "old2", []string{"gamma"}, "42"},
	}
	doc := decodeExportSnapshot(t, raw)
	if doc.Version != 2 {
		t.Fatalf("parked export version=%d, must stay version 2 even though the current chain has no times", doc.Version)
	}
	if doc.Tip != 3 {
		t.Fatalf("parked export tip=%d, want the original tip 3", doc.Tip)
	}
	if len(doc.Blocks) != 3 {
		t.Fatalf("parked export carries %d blocks, want the original three", len(doc.Blocks))
	}
	for i, wb := range want {
		block := doc.Blocks[i]
		if got := requireRawNumber(t, block["height"], "height"); got != jsonNumber(wb.height) {
			t.Fatalf("block %d height=%s, want %d", i+1, got, wb.height)
		}
		if got := requireRawString(t, block["hash"], "hash"); got != wb.hash {
			t.Fatalf("block %d hash=%q, want %q", i+1, got, wb.hash)
		}
		if got := requireRawString(t, block["parent"], "parent"); got != wb.parent {
			t.Fatalf("block %d parent=%q, want %q", i+1, got, wb.parent)
		}
		if got := requireRawStrings(t, block["txs"], "txs"); !reflect.DeepEqual(got, wb.txs) {
			t.Fatalf("block %d txs=%q, want %q with duplicates and the empty id in order", i+1, got, wb.txs)
		}
		ts, ok := block["timestamp"]
		if !ok {
			t.Fatalf("block %d is missing the timestamp field in a version-2 export", i+1)
		}
		if string(ts) != wb.time {
			t.Fatalf("block %d timestamp=%s, want %s", i+1, ts, wb.time)
		}
	}
	// Nothing of the restored chain may be spliced into the old document.
	if strings.Contains(raw, "new1") || strings.Contains(raw, "new2") {
		t.Fatalf("parked export contains a restored block:\n%s", raw)
	}
	if strings.Contains(raw, `"p"`) || strings.Contains(raw, `"q"`) {
		t.Fatalf("parked export contains a restored transaction:\n%s", raw)
	}
}

// jsonNumber renders an int64 the way encoding/json writes a bare number.
func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// requireNewChainExport asserts raw is exactly the restored chain: version 1,
// tip 2, two blocks, no timestamp field anywhere, and no original-chain
// token (including the replaced transactions and the extra third block).
func requireNewChainExport(t *testing.T, raw string) {
	t.Helper()
	doc := decodeExportSnapshot(t, raw)
	if doc.Version != 1 {
		t.Fatalf("restored chain export version=%d, want version 1", doc.Version)
	}
	if doc.Tip != 2 || len(doc.Blocks) != 2 {
		t.Fatalf("restored chain export tip=%d blocks=%d, want tip 2 with two blocks", doc.Tip, len(doc.Blocks))
	}
	want := []struct {
		height int64
		hash   string
		parent string
		txs    []string
	}{
		{1, "new1", "s", []string{"p"}},
		{2, "new2", "new1", []string{"q", "q"}},
	}
	for i, wb := range want {
		block := doc.Blocks[i]
		if got := requireRawNumber(t, block["height"], "height"); got != jsonNumber(wb.height) {
			t.Fatalf("new block %d height=%s, want %d", i+1, got, wb.height)
		}
		if got := requireRawString(t, block["hash"], "hash"); got != wb.hash {
			t.Fatalf("new block %d hash=%q, want %q", i+1, got, wb.hash)
		}
		if got := requireRawString(t, block["parent"], "parent"); got != wb.parent {
			t.Fatalf("new block %d parent=%q, want %q", i+1, got, wb.parent)
		}
		if got := requireRawStrings(t, block["txs"], "txs"); !reflect.DeepEqual(got, wb.txs) {
			t.Fatalf("new block %d txs=%q, want %q", i+1, got, wb.txs)
		}
		if _, ok := block["timestamp"]; ok {
			t.Fatalf("new block %d must not carry a timestamp field in version 1: %s", i+1, block["timestamp"])
		}
	}
	if strings.Contains(raw, "timestamp") {
		t.Fatalf("version-1 export must contain no timestamp field:\n%s", raw)
	}
	for _, leaked := range []string{"old1", "old2", "old3", `"dup"`, `""`, `"alpha"`, `"gamma"`} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("restored export contains original-chain token %q:\n%s", leaked, raw)
		}
	}
}

// requireIndexOnNewChain asserts the live index holds exactly the restored
// two-block chain: state maps, queries, and the absence of every old token.
func requireIndexOnNewChain(t *testing.T, index *Index) {
	t.Helper()
	if index.Tip != 2 || len(index.Blocks) != 2 || len(index.ByHash) != 2 {
		t.Fatalf("index state tip=%d blocks=%d byHash=%d, want the restored two-block chain",
			index.Tip, len(index.Blocks), len(index.ByHash))
	}
	wantIndex := chain(t,
		Block{Height: 1, Hash: "new1", Parent: "s", Txs: []string{"p"}},
		Block{Height: 2, Hash: "new2", Parent: "new1", Txs: []string{"q", "q"}},
	)
	if !reflect.DeepEqual(index.Blocks, wantIndex.Blocks) || !reflect.DeepEqual(index.ByHash, wantIndex.ByHash) {
		t.Fatalf("index is not exactly the restored chain:\nblocks=%v\nbyHash=%v", index.Blocks, index.ByHash)
	}

	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatalf("query after restore failed: %v", err)
	}
	wantHits := []TxHit{
		{Height: 1, BlockHash: "new1", TxID: "p", Position: 0},
		{Height: 2, BlockHash: "new2", TxID: "q", Position: 0},
		{Height: 2, BlockHash: "new2", TxID: "q", Position: 1},
	}
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("query after restore=%v, want only the new chain's transactions", page.Hits)
	}
	if page.ToHeight != 2 || page.TotalMatches != 3 || page.MatchedBlocks != 2 {
		t.Fatalf("query after restore must describe the two-block chain: %+v", page)
	}

	// The replaced transactions — the duplicate, the empty id, alpha and
	// gamma — and the extra third block are gone from the live index.
	oldPage, err := index.QueryTxs(TxQuery{TxIDs: []string{"dup", "", "alpha", "gamma"}})
	if err != nil {
		t.Fatalf("filtered query after restore failed: %v", err)
	}
	if len(oldPage.Hits) != 0 || oldPage.ToHeight != 2 {
		t.Fatalf("original-chain transactions still queryable after restore: %+v", oldPage)
	}
	for _, gone := range []string{"old1", "old2", "old3"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("replaced hash %q is still indexed", gone)
		}
	}
	if _, ok := index.Blocks[3]; ok {
		t.Fatalf("the original third block is still present")
	}
}

// The central scenario: an export that has copied the old chain and only
// delivered the document prefix is parked when Restore replaces the whole
// chain on the same index. The restore finishes while the export is still in
// flight, queries then see only the two-block snapshot chain, and the parked
// export — once its output drains — still completes with the full original
// version-2 snapshot. A later export describes the new chain in version 1.
func TestExportInFlightAcrossWholeChainRestore(t *testing.T) {
	index := buildExportReplaceChain(t)
	wantRaw := exportString(t, index)
	if len(wantRaw) <= len(exportReplaceHeaderPrefix) {
		t.Fatalf("test setup: expected an export longer than its header prefix")
	}

	writer := newGatedExportWriter(len(exportReplaceHeaderPrefix), nil, false)
	exportDone := make(chan error, 1)
	go func() { exportDone <- index.Export(writer) }()
	<-writer.reached

	// Only the document prefix is out; the export is not finished.
	if got := string(writer.bytes()); got != exportReplaceHeaderPrefix {
		t.Fatalf("output received %q, want only the prefix %q", got, exportReplaceHeaderPrefix)
	}
	select {
	case err := <-exportDone:
		t.Fatalf("export ended while its output was still gated: %v", err)
	default:
	}

	// The restore completes on the same index before the old export ends.
	if err := index.Restore(strings.NewReader(exportReplaceSnapshot)); err != nil {
		t.Fatalf("restore while an export is in flight must succeed: %v", err)
	}
	select {
	case err := <-exportDone:
		t.Fatalf("old export ended before its output was released: %v", err)
	default:
	}

	// Queries issued afterwards see only the restored two-block chain; the
	// replaced transactions and the extra third block are gone.
	requireIndexOnNewChain(t, index)

	// Let the old export finish: it must succeed and still carry the complete
	// original chain snapshot.
	close(writer.release)
	if err := <-exportDone; err != nil {
		t.Fatalf("parked export failed after the restore: %v", err)
	}
	gotRaw := string(writer.bytes())
	if gotRaw != wantRaw {
		t.Fatalf("parked export changed after the restore:\ngot  %s\nwant %s", gotRaw, wantRaw)
	}
	if !strings.HasPrefix(gotRaw, exportReplaceHeaderPrefix) {
		t.Fatalf("parked export lost the prefix the output end already received")
	}
	requireOldChainExport(t, gotRaw)

	// The index stays on the restored chain, and another export describes
	// only that chain: two blocks, version 1, no timestamp field.
	requireIndexOnNewChain(t, index)
	requireNewChainExport(t, exportString(t, index))
}

// errExportSinkOffline is the concrete output-side fault, unrelated to EOF or
// short writes.
var errExportSinkOffline = errors.New("export sink offline")

// Output failures that arrive only after the restore has landed are reported
// through Export's existing error model, and neither failure undoes the
// restore: the index stays on the new two-block chain in state, in queries,
// and in a subsequent export.
func TestExportOutputFailureAfterRestoreKeepsRestoredChain(t *testing.T) {
	cases := map[string]struct {
		writer *gatedExportWriter
		want   error
	}{
		"writer reports its concrete error": {
			newGatedExportWriter(len(exportReplaceHeaderPrefix), errExportSinkOffline, false),
			errExportSinkOffline,
		},
		"writer accepts only some bytes without error": {
			newGatedExportWriter(len(exportReplaceHeaderPrefix), nil, true),
			io.ErrShortWrite,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := buildExportReplaceChain(t)
			exportDone := make(chan error, 1)
			go func() { exportDone <- index.Export(tc.writer) }()
			<-tc.writer.reached

			if err := index.Restore(strings.NewReader(exportReplaceSnapshot)); err != nil {
				t.Fatalf("restore while an export is in flight must succeed: %v", err)
			}
			requireIndexOnNewChain(t, index)

			close(tc.writer.release)
			err := <-exportDone
			if err == nil {
				t.Fatal("expected the parked export to fail, got success")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("export err=%v, want errors.Is %v", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "indexroom: export snapshot:") {
				t.Fatalf("export err=%q, want the export-snapshot prefix", err.Error())
			}
			// The output end never received more than the prefix.
			if got := tc.writer.bytes(); len(got) != len(exportReplaceHeaderPrefix) {
				t.Fatalf("output received %d bytes after the failure, want only the %d-byte prefix",
					len(got), len(exportReplaceHeaderPrefix))
			}

			// The completed restore is not rolled back: no old block returns to
			// the index, queries keep seeing the new chain, and a fresh export
			// still describes the two-block version-1 chain.
			requireIndexOnNewChain(t, index)
			requireNewChainExport(t, exportString(t, index))
		})
	}
}
