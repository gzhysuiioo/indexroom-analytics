package indexroom

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file is the regression guard for the ordering between a Restore that
// is still reading its input and block ingestion (Append) that succeeds while
// that read is incomplete. Restore consumes and validates the whole stream
// before taking index.mu, so ingestion and querying stay open for the whole
// read window; the two deterministic outcomes then rest on the existing
// whole-replacement rules:
//
//   - restore success: the main chain is decided by the snapshot alone. A
//     block appended while the restore was reading, at a height above the
//     snapshot tip, disappears together with the old chain; appends made
//     afterwards extend the snapshot chain from its own tip and hash;
//   - restore failure (an invalid block found only at the end, or a read
//     fault the underlying reader reports at the end): the old chain and the
//     block ingested during the read both stay in effect, and Export
//     describes that extended chain. Nothing of the snapshot's valid prefix
//     survives.
//
// Scenario:
//
//	Old main chain already ingested, tip 2:
//	  h1 parent g  txs [a]
//	  h2 parent h1 txs [b]
//	Block appended while the restore is parked mid-read:
//	  h3 parent h2 txs [c]           // height 3, above the snapshot tip
//	Version-1 snapshot, tip 2, two blocks with different content:
//	  j1 parent g  txs [x, z, x]     // x twice in one block: both occurrences
//	  j2 parent j1 txs [y]           // keep their original in-block positions
//
// The two chains share neither a hash nor a transaction id, so a query answer
// can never be explained by both, and the appended h3 is distinguishable from
// every other height. During the read window the chain must answer
// {a, b, c} with upper height 3 — never an already-read j1 record mixed in;
// after a successful restore it must answer {x, z, x, y} with upper height 2
// and no trace of h1, h2, or h3; after either failure it must still answer
// {a, b, c} with upper height 3.

// restoreIngestSnapshot is the valid version-1 wire form of the two-block
// snapshot chain (j1, j2).
const restoreIngestSnapshot = `{"version":1,"tip":2,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["x","z","x"]},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["y"]}` +
	`]}`

// restoreIngestBrokenSnapshot is identical to restoreIngestSnapshot except
// that the LAST block carries a wrong parent link, so its invalidity is
// discovered only at the end of the document, after the whole valid prefix
// (including j1) has been read.
const restoreIngestBrokenSnapshot = `{"version":1,"tip":2,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["x","z","x"]},` +
	`{"height":2,"hash":"j2","parent":"j1-broken","txs":["y"]}` +
	`]}`

// restoreIngestCutMarker parks the read between the two block objects: j1 has
// already been fully consumed, j2 has not arrived, and Restore holds no lock.
const restoreIngestCutMarker = `{"height":2`

func restoreIngestOldChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"a"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"b"}},
	}
}

// restoreIngestAppendedBlock is the legal append that lands while the restore
// is still waiting for the rest of its input.
func restoreIngestAppendedBlock() Block {
	return Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"c"}}
}

func buildRestoreIngestChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range restoreIngestOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at height %d: %v", b.Height, err)
		}
	}
	return index
}

// restoreIngestExportDoc is the minimal schema used to inspect Export output.
type restoreIngestExportDoc struct {
	Version int64 `json:"version"`
	Tip     int64 `json:"tip"`
	Blocks  []struct {
		Height int64    `json:"height"`
		Hash   string   `json:"hash"`
		Parent string   `json:"parent"`
		Txs    []string `json:"txs"`
	} `json:"blocks"`
}

func mustRestoreIngestExport(t *testing.T, index *Index) restoreIngestExportDoc {
	t.Helper()
	raw := exportString(t, index)
	var doc restoreIngestExportDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("exported snapshot does not parse: %v\n%s", err, raw)
	}
	return doc
}

func restoreIngestQuery(t *testing.T, index *Index, ids ...string) TxPage {
	t.Helper()
	page, err := index.QueryTxs(TxQuery{TxIDs: ids})
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	return page
}

// The fixture the regression rests on, checked directly: the wire documents
// parse to the intended chains, the broken document is invalid only in its
// last block, h3 extends the old chain, and the two chains share neither a
// hash nor a transaction id.
func TestRestoreIngestFixtureSemantics(t *testing.T) {
	tip, blocks, err := parseSnapshot(strings.NewReader(restoreIngestSnapshot))
	if err != nil {
		t.Fatalf("valid snapshot must parse: %v", err)
	}
	if tip != 2 || len(blocks) != 2 {
		t.Fatalf("parsed snapshot tip=%d blocks=%d, want tip 2 with 2 blocks", tip, len(blocks))
	}
	wantJ1 := Block{Height: 1, Hash: "j1", Parent: "g", Txs: []string{"x", "z", "x"}}
	wantJ2 := Block{Height: 2, Hash: "j2", Parent: "j1", Txs: []string{"y"}}
	if !reflect.DeepEqual(blocks[0], wantJ1) || !reflect.DeepEqual(blocks[1], wantJ2) {
		t.Fatalf("parsed blocks=%v, want [%+v %+v]", blocks, wantJ1, wantJ2)
	}

	// The broken document is well-formed right up to its last block's parent
	// linkage, which is exactly the "invalidity discovered at the end" case.
	if _, _, err := parseSnapshot(strings.NewReader(restoreIngestBrokenSnapshot)); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("broken snapshot err=%v, want ErrInvalidSnapshot", err)
	} else if !strings.Contains(err.Error(), "does not link to its parent") {
		t.Fatalf("broken snapshot must fail on the last block's parent link: %v", err)
	}

	index := buildRestoreIngestChain(t)
	if err := index.Append(restoreIngestAppendedBlock()); err != nil {
		t.Fatalf("h3 must extend the old chain: %v", err)
	}
	if index.Tip != 3 || index.Blocks[3].Hash != "h3" || index.Blocks[3].Parent != "h2" {
		t.Fatalf("chain after h3 append: tip=%d block3=%+v", index.Tip, index.Blocks[3])
	}

	// No hash and no transaction identifier is explainable by both chains,
	// and h3 is outside the snapshot's height range.
	for _, hash := range []string{"h1", "h2", "h3"} {
		if hash == "j1" || hash == "j2" {
			t.Fatalf("fixture hash %q is shared between the chains", hash)
		}
	}
	oldIDs := map[string]bool{"a": true, "b": true, "c": true}
	for _, id := range []string{"x", "y", "z"} {
		if oldIDs[id] {
			t.Fatalf("fixture tx %q is shared between the chains", id)
		}
	}
}

// While the restore is still waiting for the remainder of its input it holds
// no lock: a legal append completes at once, the new transaction is
// immediately queryable, and the query's upper height reflects the appended
// tip. The snapshot block already read must not appear early, and old-chain
// and snapshot transactions must never share one answer. Both the append and
// the query finish before the restore does.
func TestAppendAndQueryWhileRestoreWaitsForInput(t *testing.T) {
	index := buildRestoreIngestChain(t)
	reader := newGatedReader(t, restoreIngestSnapshot, restoreIngestCutMarker)

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(reader)
	}()
	<-reader.entered // j1 consumed, j2 not yet arrived; Restore holds no lock

	// Existing append rules are still enforced during the read window: a
	// block not extending the current tip is rejected and changes nothing.
	blocks, byHash, tip := snapshot(index)
	if err := index.Append(Block{Height: 3, Hash: "r3", Parent: "h1", Txs: []string{"r"}}); err == nil {
		t.Fatal("append with a non-tip parent must be rejected while the restore reads")
	}
	requireUnchanged(t, index, blocks, byHash, tip)

	// The legal append extends the OLD chain tip and returns before the
	// restore ends.
	if err := index.Append(restoreIngestAppendedBlock()); err != nil {
		t.Fatalf("append while restore is reading: %v", err)
	}
	select {
	case err := <-restoreDone:
		t.Fatalf("restore returned while its input was still gated: %v", err)
	default:
	}

	// The appended transaction is immediately visible, at its original
	// position, and the pinned upper height follows the appended tip.
	page := restoreIngestQuery(t, index)
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("query during the read window=%v, want old chain plus h3", page.Hits)
	}
	if page.ToHeight != 3 || page.TotalMatches != 3 || page.MatchedBlocks != 3 {
		t.Fatalf("upper height and statistics must reflect appended tip 3: %+v", page)
	}

	// Already-read snapshot content must not surface early: filtering for
	// snapshot transactions returns an empty successful page pinned at tip 3.
	snapshotOnly := restoreIngestQuery(t, index, "x", "y", "z")
	if len(snapshotOnly.Hits) != 0 || snapshotOnly.ToHeight != 3 {
		t.Fatalf("snapshot txs visible before the restore applied: %+v", snapshotOnly)
	}

	// One filtered answer must never stitch both chains together: the old id
	// is present, the snapshot id is absent, even though j1 was already read.
	mixed := restoreIngestQuery(t, index, "a", "x")
	if !reflect.DeepEqual(mixed.Hits, []TxHit{{Height: 1, BlockHash: "h1", TxID: "a", Position: 0}}) {
		t.Fatalf("old and snapshot txs mixed into one answer: %+v", mixed.Hits)
	}

	// The already-read snapshot block has no presence in the index, and the
	// query above completed while the restore was still blocked.
	for _, leaked := range []string{"j1", "j2"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("snapshot hash %s appeared before the restore applied", leaked)
		}
	}
	select {
	case err := <-restoreDone:
		t.Fatalf("restore returned before its input was released: %v", err)
	default:
	}

	close(reader.release)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore after release failed: %v", err)
	}
}

// Once the complete, valid snapshot arrives the whole-replacement rule wins:
// the final tip is the snapshot tip (2) even though height 3 was appended
// during the read, every returned transaction (with its original in-block
// position) is a snapshot transaction, and a fresh Export contains only the
// snapshot chain. Appends afterwards continue from the new tip and hash;
// requests still parented by the old tip or by the block appended mid-read
// are rejected by the existing rules and cannot revive the replaced chain.
func TestSuccessfulRestoreAfterIngestionReplacesWholeChain(t *testing.T) {
	index := buildRestoreIngestChain(t)
	reader := newGatedReader(t, restoreIngestSnapshot, restoreIngestCutMarker)
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(reader)
	}()
	<-reader.entered
	if err := index.Append(restoreIngestAppendedBlock()); err != nil {
		t.Fatalf("append while restore is reading: %v", err)
	}
	page := restoreIngestQuery(t, index)
	if page.ToHeight != 3 || len(page.Hits) != 3 {
		t.Fatalf("setup: appended block not visible during the read: %+v", page)
	}

	close(reader.release)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore must succeed on complete valid input: %v", err)
	}

	// The snapshot decides the chain completely: tip 2, exactly j1 and j2.
	if index.Tip != 2 {
		t.Fatalf("tip after restore=%d, want snapshot tip 2 (height 3 appended during the read must vanish)", index.Tip)
	}
	wantIndex := chain(t,
		Block{Height: 1, Hash: "j1", Parent: "g", Txs: []string{"x", "z", "x"}},
		Block{Height: 2, Hash: "j2", Parent: "j1", Txs: []string{"y"}},
	)
	if !reflect.DeepEqual(index.Blocks, wantIndex.Blocks) || !reflect.DeepEqual(index.ByHash, wantIndex.ByHash) {
		t.Fatalf("chain after restore is not exactly the snapshot chain:\nblocks=%v\nbyHash=%v", index.Blocks, index.ByHash)
	}
	for _, leaked := range []int64{3} {
		if _, ok := index.Blocks[leaked]; ok {
			t.Fatalf("height %d appended during the read survived the restore", leaked)
		}
	}
	for _, leaked := range []string{"h1", "h2", "h3"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("replaced hash %s survived the restore", leaked)
		}
	}

	// Queries return snapshot transactions at their original block positions;
	// the duplicate x keeps both occurrences, and every old/ingested tx is
	// gone.
	page = restoreIngestQuery(t, index)
	wantHits := []TxHit{
		{Height: 1, BlockHash: "j1", TxID: "x", Position: 0},
		{Height: 1, BlockHash: "j1", TxID: "z", Position: 1},
		{Height: 1, BlockHash: "j1", TxID: "x", Position: 2},
		{Height: 2, BlockHash: "j2", TxID: "y", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("post-restore hits=%v, want snapshot transactions at their original positions", page.Hits)
	}
	if page.ToHeight != 2 || page.TotalMatches != 4 || page.MatchedBlocks != 2 {
		t.Fatalf("post-restore page must describe the snapshot chain: %+v", page)
	}
	oldOnly := restoreIngestQuery(t, index, "a", "b", "c")
	if len(oldOnly.Hits) != 0 {
		t.Fatalf("old and mid-read transactions survived the restore: %+v", oldOnly.Hits)
	}
	dupX := restoreIngestQuery(t, index, "x")
	if !reflect.DeepEqual(dupX.Hits, []TxHit{
		{Height: 1, BlockHash: "j1", TxID: "x", Position: 0},
		{Height: 1, BlockHash: "j1", TxID: "x", Position: 2},
	}) {
		t.Fatalf("duplicate snapshot tx must keep both in-block positions: %+v", dupX.Hits)
	}

	// A fresh export contains exactly the snapshot main chain: no height 3
	// and no replaced hash anywhere in the bytes.
	doc := mustRestoreIngestExport(t, index)
	if doc.Version != 1 || doc.Tip != 2 || len(doc.Blocks) != 2 {
		t.Fatalf("export after restore=%+v, want version 1, tip 2, two blocks", doc)
	}
	for i, want := range []struct {
		height int64
		hash   string
		parent string
		txs    []string
	}{
		{1, "j1", "g", []string{"x", "z", "x"}},
		{2, "j2", "j1", []string{"y"}},
	} {
		got := doc.Blocks[i]
		if got.Height != want.height || got.Hash != want.hash || got.Parent != want.parent ||
			!reflect.DeepEqual(got.Txs, want.txs) {
			t.Fatalf("exported block %d=%+v, want %+v", i+1, got, want)
		}
	}
	raw := exportString(t, index)
	for _, gone := range []string{"h1", "h2", "h3", `"c"`} {
		if strings.Contains(raw, gone) {
			t.Fatalf("export after restore still contains %q:\n%s", gone, raw)
		}
	}

	// Requests still parented by the pre-restore tip (h2) or by the block
	// appended during the read (h3) follow the existing rejection rules and
	// must not revive the replaced chain.
	stateBlocks, stateByHash, stateTip := snapshot(index)
	for _, stale := range []Block{
		{Height: 3, Hash: "r3", Parent: "h2", Txs: []string{"r"}}, // parented by the old tip
		{Height: 4, Hash: "r4", Parent: "h3", Txs: []string{"r"}}, // parented by the mid-read block
	} {
		if err := index.Append(stale); err == nil {
			t.Fatalf("stale append %+v must be rejected after the restore", stale)
		}
		requireUnchanged(t, index, stateBlocks, stateByHash, stateTip)
	}

	// Ingestion continues from the new chain tip and hash.
	continued := Block{Height: 3, Hash: "k3", Parent: "j2", Txs: []string{"w"}}
	if err := index.Append(continued); err != nil {
		t.Fatalf("append continuing the snapshot chain: %v", err)
	}
	if index.Tip != 3 || index.Blocks[3].Hash != "k3" || index.Blocks[3].Parent != "j2" {
		t.Fatalf("chain after continuation: tip=%d block3=%+v", index.Tip, index.Blocks[3])
	}
	gotW := restoreIngestQuery(t, index, "w")
	if !reflect.DeepEqual(gotW.Hits, []TxHit{{Height: 3, BlockHash: "k3", TxID: "w", Position: 0}}) {
		t.Fatalf("continued block transaction=%+v, want w@k3#0", gotW.Hits)
	}
	// A block still citing h3 (the vanished mid-read tip) cannot extend the
	// chain even at the right height.
	if err := index.Append(Block{Height: 4, Hash: "r4", Parent: "h3", Txs: []string{"r"}}); err == nil {
		t.Fatal("append parented by the vanished h3 must be rejected")
	}
	if index.Tip != 3 || index.Blocks[3].Hash != "k3" {
		t.Fatalf("rejected append altered the restored chain: tip=%d block3=%+v", index.Tip, index.Blocks[3])
	}
}

// After a legal append during the read window, an invalid block discovered
// only at the end of the input fails the restore with ErrInvalidSnapshot and
// leaves the old chain plus the appended block exactly as they were; the
// snapshot's valid prefix leaves nothing behind.
func TestRestoreRejectedByLastBlockKeepsIngestedChain(t *testing.T) {
	index := buildRestoreIngestChain(t)
	reader := newGatedReader(t, restoreIngestBrokenSnapshot, restoreIngestCutMarker)
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(reader)
	}()
	<-reader.entered
	if err := index.Append(restoreIngestAppendedBlock()); err != nil {
		t.Fatalf("append while restore is reading: %v", err)
	}
	page := restoreIngestQuery(t, index)
	if page.ToHeight != 3 || len(page.Hits) != 3 {
		t.Fatalf("setup: appended block not visible during the read: %+v", page)
	}

	close(reader.release)
	if err := <-restoreDone; !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("restore err=%v, want ErrInvalidSnapshot", err)
	}

	// The old chain and the block ingested during the read both survive; no
	// snapshot block was left behind.
	wantIndex := chain(t, append(restoreIngestOldChainBlocks(), restoreIngestAppendedBlock())...)
	if !reflect.DeepEqual(index.Blocks, wantIndex.Blocks) || !reflect.DeepEqual(index.ByHash, wantIndex.ByHash) || index.Tip != 3 {
		t.Fatalf("chain after rejected restore:\nblocks=%v\nbyHash=%v tip=%d\nwant the old chain plus h3",
			index.Blocks, index.ByHash, index.Tip)
	}
	for _, leaked := range []string{"j1", "j2"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("snapshot hash %s from the valid prefix was left indexed", leaked)
		}
	}

	page = restoreIngestQuery(t, index)
	wantHits := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, wantHits) || page.ToHeight != 3 {
		t.Fatalf("query after rejected restore=%+v, want the complete old chain including h3", page)
	}
	if len(restoreIngestQuery(t, index, "x", "y", "z").Hits) != 0 {
		t.Fatal("snapshot transactions leaked in after the rejected restore")
	}

	// Export reflects the chain as extended by the mid-read append, with no
	// snapshot hash or transaction in the document.
	doc := mustRestoreIngestExport(t, index)
	if doc.Version != 1 || doc.Tip != 3 || len(doc.Blocks) != 3 {
		t.Fatalf("export after rejected restore=%+v, want version 1, tip 3, three blocks", doc)
	}
	for i, want := range []struct {
		height int64
		hash   string
		parent string
		txs    []string
	}{
		{1, "h1", "g", []string{"a"}},
		{2, "h2", "h1", []string{"b"}},
		{3, "h3", "h2", []string{"c"}},
	} {
		got := doc.Blocks[i]
		if got.Height != want.height || got.Hash != want.hash || got.Parent != want.parent ||
			!reflect.DeepEqual(got.Txs, want.txs) {
			t.Fatalf("exported block %d=%+v, want %+v", i+1, got, want)
		}
	}
	if raw := exportString(t, index); strings.Contains(raw, "j1") || strings.Contains(raw, "j2") {
		t.Fatalf("export after rejected restore contains snapshot hashes:\n%s", raw)
	}
}

// gatedTailFaultReader delivers the prefix, parks until release, then hands
// over the remaining bytes together with fault on the first read after the
// gate and the fault on every later read. It models a stream whose storage
// fails once the restore is allowed to run to the end, even when the buffered
// tail already completes the document.
type gatedTailFaultReader struct {
	data    []byte
	prefix  int
	off     int
	fault   error
	entered chan struct{}
	release chan struct{}
	parked  bool
	once    sync.Once
}

func newGatedTailFaultReader(t *testing.T, data, cutMarker string, fault error) *gatedTailFaultReader {
	t.Helper()
	prefix := strings.Index(data, cutMarker)
	if prefix <= 0 {
		t.Fatalf("test setup: cut marker %q not found in %q", cutMarker, data)
	}
	if fault == nil {
		t.Fatal("test setup: gated tail fault reader requires a fault")
	}
	return &gatedTailFaultReader{
		data:    []byte(data),
		prefix:  prefix,
		fault:   fault,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *gatedTailFaultReader) Read(p []byte) (int, error) {
	if r.off < r.prefix {
		n := copy(p, r.data[r.off:r.prefix])
		r.off += n
		return n, nil
	}
	if !r.parked {
		r.parked = true
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		// Bytes and fault in one read, exactly as a failing storage read can
		// deliver them; snapshotReader withholds the fault until next time.
		return n, r.fault
	}
	return 0, r.fault
}

// After a legal append during the read window, a read fault reported at the
// end of the input — together with the tail bytes of an otherwise complete
// snapshot — fails the restore as a read failure carrying the recognizable
// original fault (never ErrInvalidSnapshot) and leaves the old chain plus the
// appended block in effect, in queries and in export.
func TestRestoreReadFaultAfterIngestionKeepsChainAndFault(t *testing.T) {
	index := buildRestoreIngestChain(t)
	// The valid snapshot completes in the faulted tail, proving a read
	// failure outranks an already-complete document rather than applying it.
	reader := newGatedTailFaultReader(t, restoreIngestSnapshot, restoreIngestCutMarker, errStorageOffline)
	done := make(chan error, 1)
	go func() {
		done <- index.Restore(reader)
	}()
	<-reader.entered
	if err := index.Append(restoreIngestAppendedBlock()); err != nil {
		t.Fatalf("append while restore is reading: %v", err)
	}
	page := restoreIngestQuery(t, index)
	if page.ToHeight != 3 || len(page.Hits) != 3 {
		t.Fatalf("setup: appended block not visible during the read: %+v", page)
	}

	close(reader.release)
	err := <-done
	if err == nil {
		t.Fatal("restore with a faulted tail must fail")
	}
	if errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, a read fault must not be reported as ErrInvalidSnapshot", err)
	}
	if !errors.Is(err, errStorageOffline) {
		t.Fatalf("err=%v, caller must still errors.Is the original fault", err)
	}
	if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
		t.Fatalf("err=%q, want the read-snapshot prefix", err.Error())
	}

	// The old chain and the block ingested during the read both survive; the
	// complete, valid-looking snapshot is not applied and leaves nothing.
	wantIndex := chain(t, append(restoreIngestOldChainBlocks(), restoreIngestAppendedBlock())...)
	if !reflect.DeepEqual(index.Blocks, wantIndex.Blocks) || !reflect.DeepEqual(index.ByHash, wantIndex.ByHash) || index.Tip != 3 {
		t.Fatalf("chain after read fault:\nblocks=%v\nbyHash=%v tip=%d\nwant the old chain plus h3",
			index.Blocks, index.ByHash, index.Tip)
	}
	for _, leaked := range []string{"j1", "j2"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("snapshot hash %s was left indexed after the read fault", leaked)
		}
	}
	page = restoreIngestQuery(t, index)
	if !reflect.DeepEqual(page.Hits, []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 0},
	}) || page.ToHeight != 3 {
		t.Fatalf("query after read fault=%+v, want the complete old chain including h3", page)
	}
	doc := mustRestoreIngestExport(t, index)
	if doc.Version != 1 || doc.Tip != 3 || len(doc.Blocks) != 3 {
		t.Fatalf("export after read fault=%+v, want version 1, tip 3, three blocks", doc)
	}
	if doc.Blocks[2].Hash != "h3" || doc.Blocks[2].Parent != "h2" {
		t.Fatalf("export after read fault must end at the appended h3: %+v", doc.Blocks[2])
	}
	if raw := exportString(t, index); strings.Contains(raw, "j1") || strings.Contains(raw, "j2") {
		t.Fatalf("export after read fault contains snapshot hashes:\n%s", raw)
	}
}
