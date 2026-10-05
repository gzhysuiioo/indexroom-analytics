package indexroom

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// This file is the regression guard for one Restore with a concurrent
// APPEND while the snapshot is still being read. The replacement rule is the
// existing whole-chain one:
//
//   - On success the main chain is decided entirely by the snapshot; any block
//     ingested during the read is discarded exactly like the old chain it
//     extended, and later ingestion continues from the snapshot tip.
//   - On failure the main chain keeps everything that was already ingested,
//     including the block appended mid-read; the snapshot's already-read
//     (valid) prefix leaves nothing behind.
//
// Both outcomes are presented through existing public features (Append,
// QueryTxs, Tip/Blocks/ByHash inspection, Export) so the chain state during
// ingestion-while-restore and after restore completion has a reliable
// business basis.
//
// Determinism comes from gating the Restore on its input reader (it then
// holds no lock) rather than from sleeps or scheduling luck: the append and
// the query run while the restore is blocked on the gate, and each is
// required to return while the gate is still closed.
//
// The fixture keeps every block timestamp-less, so all exports are the
// canonical version-1 compact text and re-export comparisons can be exact.
//
//	Original main chain (already ingested), tip 2:
//	  h1 parent genesis txs [old-1]
//	  h2 parent h1      txs [old-2]
//	Mid-read append:
//	  h3 parent h2      txs [mid-3]          // succeeds while reading
//	Version-1 snapshot, tip 2, content differs at every height:
//	  s1 parent genesis txs [snap-1]
//	  s2 parent s1      txs [snap-2a, snap-2b]
//
// The old transactions old-1/old-2, the mid-read transaction mid-3, and the
// snapshot transactions snap-1/snap-2a/snap-2b are disjoint sets, so any
// stitched query — old or mid-read hits under the snapshot tip, snapshot
// blocks appearing early, old+new mixed in one answer — is distinguishable
// record by record.

var overlapOldChainBlocks = []Block{
	{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"old-1"}},
	{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"old-2"}},
}

var overlapMidBlock = Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"mid-3"}}

var overlapSnapshotBlocks = []Block{
	{Height: 1, Hash: "s1", Parent: "genesis", Txs: []string{"snap-1"}},
	{Height: 2, Hash: "s2", Parent: "s1", Txs: []string{"snap-2a", "snap-2b"}},
}

// overlapSnapshot is the canonical version-1 wire form of
// overlapSnapshotBlocks.
const overlapSnapshot = `{"version":1,"tip":2,"blocks":[` +
	`{"height":1,"hash":"s1","parent":"genesis","txs":["snap-1"]},` +
	`{"height":2,"hash":"s2","parent":"s1","txs":["snap-2a","snap-2b"]}` +
	`]}`

// overlapInvalidTailSnapshot is identical to overlapSnapshot except that the
// LAST block has a broken parent link: its valid prefix is the whole first
// snapshot block.
const overlapInvalidTailSnapshot = `{"version":1,"tip":2,"blocks":[` +
	`{"height":1,"hash":"s1","parent":"genesis","txs":["snap-1"]},` +
	`{"height":2,"hash":"s2","parent":"WRONG","txs":["snap-2a","snap-2b"]}` +
	`]}`

// overlapAllQueryTxIDs spans original + mid-read + snapshot transactions so
// one first page (To left at zero) exposes any mixture.
var overlapAllQueryTxIDs = []string{"old-1", "old-2", "mid-3", "snap-1", "snap-2a", "snap-2b"}

func buildOverlapChain(t *testing.T) *Index {
	t.Helper()
	return chain(t, overlapOldChainBlocks...)
}

// startGatedRestore parks Restore after the first snapshot block has been
// consumed: it holds no index lock and is waiting for the rest of the input.
func startGatedRestore(t *testing.T, index *Index, data string) (*gatedReader, <-chan error) {
	t.Helper()
	reader := newGatedReader(t, data, `{"height":2`)
	done := make(chan error, 1)
	go func() { done <- index.Restore(reader) }()
	<-reader.entered
	return reader, done
}

// assertRestoreStillReading fails if Restore has returned while its input is
// still gated, which would make the "during the read" observations vacuous.
func assertRestoreStillReading(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("restore returned while its input was still gated: %v", err)
	default:
	}
}

// requireAppendedChain verifies the complete state of original chain + the
// mid-read append: tip 3, the h3 block content, and every hash mapping.
func requireAppendedChain(t *testing.T, index *Index) {
	t.Helper()
	if index.Tip != 3 {
		t.Fatalf("tip=%d, want 3 (original 2 + mid-read append)", index.Tip)
	}
	if got := index.Blocks[3]; !sameBlock(got, overlapMidBlock) {
		t.Fatalf("height 3=%+v, want the mid-read appended block %+v", got, overlapMidBlock)
	}
	for h, want := range map[int64]Block{1: overlapOldChainBlocks[0], 2: overlapOldChainBlocks[1], 3: overlapMidBlock} {
		if got := index.Blocks[h]; !sameBlock(got, want) {
			t.Fatalf("height %d=%+v, want %+v", h, got, want)
		}
	}
	for hash, height := range map[string]int64{"h1": 1, "h2": 2, "h3": 3} {
		if index.ByHash[hash] != height {
			t.Fatalf("ByHash[%q]=%d, want %d (state=%v)", hash, index.ByHash[hash], height, index.ByHash)
		}
	}
}

// requireNoSnapshotResidue verifies neither snapshot hash has leaked into the
// index (used after a failed or still-reading restore).
func requireNoSnapshotResidue(t *testing.T, index *Index) {
	t.Helper()
	for _, leaked := range []string{"s1", "s2"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("already-read snapshot hash %q is visible before restore completes", leaked)
		}
	}
}

// appendedChainHits is the exact ordered answer (height, then in-block
// position) of the all-ids query over original chain + mid-read append.
func appendedChainHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "old-1", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "old-2", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "mid-3", Position: 0},
	}
}

// snapshotChainHits is the exact ordered answer over the snapshot chain.
func snapshotChainHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "s1", TxID: "snap-1", Position: 0},
		{Height: 2, BlockHash: "s2", TxID: "snap-2a", Position: 0},
		{Height: 2, BlockHash: "s2", TxID: "snap-2b", Position: 1},
	}
}

// While Restore is still reading only part of its input, a legal append
// extending the current tip must succeed and return before the restore ends.
// The caller can immediately query the new block's transactions, the first
// page's pinned upper bound must follow the appended tip (3), the already
// read snapshot blocks must not appear early, and old and snapshot
// transactions must never be mixed in one answer.
func TestAppendSucceedsWhileRestoreStillReading(t *testing.T) {
	index := buildOverlapChain(t)
	reader, done := startGatedRestore(t, index, overlapSnapshot)

	// A legal append extends the current tip (2) and succeeds while the
	// restore is still blocked on the gated reader, holding no lock.
	if err := index.Append(overlapMidBlock); err != nil {
		t.Fatalf("append while restore is reading failed: %v", err)
	}
	requireAppendedChain(t, index)
	requireNoSnapshotResidue(t, index)

	// The query also completes before the restore ends. The first page pins
	// the tip it observes: 3 after the append, not the snapshot tip 2.
	page, err := index.QueryTxs(TxQuery{TxIDs: overlapAllQueryTxIDs})
	if err != nil {
		t.Fatalf("query while restore is reading failed: %v", err)
	}
	assertRestoreStillReading(t, done)

	wantHits := appendedChainHits()
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("query during read hits=%v, want original+appended only %v", page.Hits, wantHits)
	}
	if page.ToHeight != 3 {
		t.Fatalf("query during read ToHeight=%d, want appended tip 3", page.ToHeight)
	}
	if page.TotalMatches != 3 || page.MatchedBlocks != 3 {
		t.Fatalf("query during read stats=%+v, want 3 matches over 3 blocks", page)
	}
	for _, hit := range page.Hits {
		if strings.HasPrefix(hit.BlockHash, "s") {
			t.Fatalf("snapshot block %q visible before restore completes: %+v", hit.BlockHash, hit)
		}
	}
	// Snapshot-only transactions must not be queryable yet.
	for _, id := range []string{"snap-1", "snap-2a", "snap-2b"} {
		p, qerr := index.QueryTxs(TxQuery{TxIDs: []string{id}})
		if qerr != nil || p.TotalMatches != 0 {
			t.Fatalf("snapshot tx %q queryable early: page=%+v err=%v", id, p, qerr)
		}
	}
	// The appended transaction is immediately queryable, by height and by the
	// pinned upper bound reflecting the appended chain top.
	mid, err := index.QueryTxs(TxQuery{TxIDs: []string{"mid-3"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mid.Hits, []TxHit{{Height: 3, BlockHash: "h3", TxID: "mid-3", Position: 0}}) {
		t.Fatalf("mid-3 hits=%v, want the single appended occurrence", mid.Hits)
	}
	if mid.ToHeight != 3 {
		t.Fatalf("mid-3 query ToHeight=%d, want 3", mid.ToHeight)
	}

	close(reader.release)
	if err := <-done; err != nil {
		t.Fatalf("restore after release failed: %v", err)
	}
}

// After the mid-read append, a complete and legal snapshot restores
// successfully: the final tip is the snapshot tip (2), queries return the
// snapshot transactions at their original in-block positions, the original
// and mid-read transactions are gone, and a re-export contains exactly the
// snapshot chain (no height 3, no replaced hashes). Ingestion afterwards
// continues from the new tip; requests still parented on the old tip or the
// mid-read block are rejected by the existing append rules and leave the
// restored chain intact.
func TestSuccessfulRestoreAfterMidReadAppendReplacesWholeChain(t *testing.T) {
	index := buildOverlapChain(t)
	reader, done := startGatedRestore(t, index, overlapSnapshot)
	if err := index.Append(overlapMidBlock); err != nil {
		t.Fatalf("append while restore is reading failed: %v", err)
	}
	requireAppendedChain(t, index)

	close(reader.release)
	if err := <-done; err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	// The snapshot decides the whole main chain: tip 2 even though height 3
	// was ingested during the read.
	if index.Tip != 2 {
		t.Fatalf("tip after restore=%d, want snapshot tip 2", index.Tip)
	}
	if len(index.Blocks) != 2 || len(index.ByHash) != 2 {
		t.Fatalf("leftover state after restore: blocks=%d byHash=%d", len(index.Blocks), len(index.ByHash))
	}
	for h, want := range map[int64]Block{1: overlapSnapshotBlocks[0], 2: overlapSnapshotBlocks[1]} {
		if got := index.Blocks[h]; !sameBlock(got, want) {
			t.Fatalf("height %d=%+v, want snapshot block %+v", h, got, want)
		}
	}
	for _, gone := range []string{"h1", "h2", "h3"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("old/mid-read hash %q survived the restore", gone)
		}
	}
	if _, ok := index.Blocks[3]; ok {
		t.Fatalf("mid-read height 3 survived a successful restore: %+v", index.Blocks[3])
	}

	// Query: snapshot transactions at original in-block positions; original
	// and mid-read transactions have disappeared.
	page, err := index.QueryTxs(TxQuery{TxIDs: overlapAllQueryTxIDs})
	if err != nil {
		t.Fatal(err)
	}
	wantHits := snapshotChainHits()
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("post-restore hits=%v, want snapshot hits %v", page.Hits, wantHits)
	}
	if page.ToHeight != 2 || page.TotalMatches != 3 || page.MatchedBlocks != 2 {
		t.Fatalf("post-restore page stats=%+v, want tip 2, 3 matches over 2 blocks", page)
	}
	for _, id := range []string{"old-1", "old-2", "mid-3"} {
		p, qerr := index.QueryTxs(TxQuery{TxIDs: []string{id}})
		if qerr != nil || p.TotalMatches != 0 {
			t.Fatalf("gone tx %q still queryable after restore: page=%+v err=%v", id, p, qerr)
		}
	}

	// Re-export is exactly the snapshot's main chain: canonical version-1
	// text, no height 3, no replaced hashes.
	if raw := exportString(t, index); raw != overlapSnapshot {
		t.Fatalf("re-export after restore=%s\nwant=%s", raw, overlapSnapshot)
	}

	// Continued ingestion extends the NEW tip by height and hash.
	if err := index.Append(Block{Height: 3, Hash: "s3", Parent: "s2", Txs: []string{"snap-3"}}); err != nil {
		t.Fatalf("append after restore must continue from the new tip s2: %v", err)
	}
	if index.Tip != 3 || index.ByHash["s3"] != 3 || index.Blocks[3].Parent != "s2" {
		t.Fatalf("post-restore append did not extend the snapshot chain: tip=%d byHash=%v", index.Tip, index.ByHash)
	}
	continued, err := index.QueryTxs(TxQuery{TxIDs: []string{"snap-3"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(continued.Hits, []TxHit{{Height: 3, BlockHash: "s3", TxID: "snap-3", Position: 0}}) {
		t.Fatalf("post-restore appended tx hits=%v", continued.Hits)
	}

	// Requests still parented on the original tip (h2) or the mid-read block
	// (h3) are rejected by the existing append rules and change nothing.
	for name, block := range map[string]Block{
		"parented on old tip h2":  {Height: 4, Hash: "stale-a", Parent: "h2", Txs: []string{"x"}},
		"parented on mid-read h3": {Height: 4, Hash: "stale-b", Parent: "h3", Txs: []string{"x"}},
	} {
		if err := index.Append(block); err == nil {
			t.Fatalf("append %s was accepted", name)
		}
		if index.Tip != 3 || index.Blocks[3].Hash != "s3" {
			t.Fatalf("rejected append %s altered the restored chain: tip=%d h3=%+v", name, index.Tip, index.Blocks[3])
		}
	}
}

// After the same legal mid-read append, an illegal block surfacing only at
// the end of the snapshot makes Restore return ErrInvalidSnapshot. The
// original chain AND the just-appended block must both survive: their
// transactions stay queryable, the export reflects the full appended chain,
// and the snapshot's valid prefix leaves nothing behind.
func TestInvalidTailAfterMidReadAppendKeepsIngestedChain(t *testing.T) {
	index := buildOverlapChain(t)
	reader, done := startGatedRestore(t, index, overlapInvalidTailSnapshot)
	if err := index.Append(overlapMidBlock); err != nil {
		t.Fatalf("append while restore is reading failed: %v", err)
	}
	requireAppendedChain(t, index)
	wantExport := exportString(t, index) // original + h3, canonical version 1

	close(reader.release)
	err := <-done
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("restore err=%v, want ErrInvalidSnapshot", err)
	}

	requireAppendedChain(t, index)
	requireNoSnapshotResidue(t, index)

	page, err := index.QueryTxs(TxQuery{TxIDs: overlapAllQueryTxIDs})
	if err != nil {
		t.Fatalf("query after failed restore failed: %v", err)
	}
	wantHits := appendedChainHits()
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("post-failure hits=%v, want original+appended %v", page.Hits, wantHits)
	}
	if page.ToHeight != 3 || page.TotalMatches != 3 || page.MatchedBlocks != 3 {
		t.Fatalf("post-failure stats=%+v, want tip 3, 3 matches over 3 blocks", page)
	}
	for _, id := range []string{"snap-1", "snap-2a", "snap-2b"} {
		p, qerr := index.QueryTxs(TxQuery{TxIDs: []string{id}})
		if qerr != nil || p.TotalMatches != 0 {
			t.Fatalf("snapshot tx %q leaked after a rejected restore: page=%+v err=%v", id, p, qerr)
		}
	}
	if raw := exportString(t, index); raw != wantExport {
		t.Fatalf("export changed after rejected restore:\n%s\nwant:\n%s", raw, wantExport)
	}
	if strings.Contains(wantExport, `"s1"`) || strings.Contains(wantExport, `"s2"`) || strings.Contains(wantExport, `"tip":2`) {
		t.Fatalf("appended-chain export must not carry snapshot content: %s", wantExport)
	}
}

// After the same legal mid-read append, a read fault the underlying reader
// reports before the document completes must surface with the identifiable
// original error (errors.Is), distinct from ErrInvalidSnapshot. Both the
// original chain and the just-appended block survive: queries and the export
// reflect the full appended chain and the snapshot prefix leaves nothing.
func TestReadFailureAfterMidReadAppendKeepsIngestedChain(t *testing.T) {
	readFault := errors.New("simulated snapshot stream failure")

	index := buildOverlapChain(t)
	// Deliver the valid prefix (first snapshot block) and park before block 2,
	// exactly like the gated reader, so the append provably happens while the
	// restore is still reading.
	prefix := overlapSnapshot[:strings.Index(overlapSnapshot, `{"height":2`)]
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- index.Restore(pr) }()
	if _, err := pw.Write([]byte(prefix)); err != nil {
		t.Fatal(err)
	}

	if err := index.Append(overlapMidBlock); err != nil {
		t.Fatalf("append while restore is reading failed: %v", err)
	}
	requireAppendedChain(t, index)
	wantExport := exportString(t, index)

	// The stream then faults before delivering the rest of the document.
	if err := pw.CloseWithError(readFault); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("restore err=%v, must not be ErrInvalidSnapshot", err)
	}
	if !errors.Is(err, readFault) {
		t.Fatalf("restore err=%v, want errors.Is the original read fault", err)
	}
	if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
		t.Fatalf("err=%q, want the read-snapshot prefix", err.Error())
	}

	requireAppendedChain(t, index)
	requireNoSnapshotResidue(t, index)

	page, qerr := index.QueryTxs(TxQuery{TxIDs: overlapAllQueryTxIDs})
	if qerr != nil {
		t.Fatalf("query after read failure failed: %v", qerr)
	}
	wantHits := appendedChainHits()
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("post-fault hits=%v, want original+appended %v", page.Hits, wantHits)
	}
	if page.ToHeight != 3 || page.TotalMatches != 3 || page.MatchedBlocks != 3 {
		t.Fatalf("post-fault stats=%+v, want tip 3, 3 matches over 3 blocks", page)
	}
	for _, id := range []string{"snap-1", "snap-2a", "snap-2b"} {
		p, qerr := index.QueryTxs(TxQuery{TxIDs: []string{id}})
		if qerr != nil || p.TotalMatches != 0 {
			t.Fatalf("snapshot tx %q leaked after read failure: page=%+v err=%v", id, p, qerr)
		}
	}
	if raw := exportString(t, index); raw != wantExport {
		t.Fatalf("export changed after read failure:\n%s\nwant:\n%s", raw, wantExport)
	}
}

// The query answer used by the during-read assertions must itself be
// internally consistent: original+appended hits under the appended tip, or
// snapshot hits under the snapshot tip — never a stitched page. This pins the
// oracle values the overlap tests compare against, so a regression that
// mixed the two chains could not pass on a coincidental DeepEqual.
func TestOverlapFixtureOracleAnswersAreDistinct(t *testing.T) {
	appended := buildOverlapChain(t)
	if err := appended.Append(overlapMidBlock); err != nil {
		t.Fatal(err)
	}
	page, err := appended.QueryTxs(TxQuery{TxIDs: overlapAllQueryTxIDs})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Hits, appendedChainHits()) || page.ToHeight != 3 ||
		page.TotalMatches != 3 || page.MatchedBlocks != 3 {
		t.Fatalf("appended-chain oracle page=%+v", page)
	}

	snap := New()
	if err := snap.Restore(strings.NewReader(overlapSnapshot)); err != nil {
		t.Fatal(err)
	}
	page, err = snap.QueryTxs(TxQuery{TxIDs: overlapAllQueryTxIDs})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Hits, snapshotChainHits()) || page.ToHeight != 2 ||
		page.TotalMatches != 3 || page.MatchedBlocks != 2 {
		t.Fatalf("snapshot-chain oracle page=%+v", page)
	}

	// No transaction is shared between the two chain answers, and the
	// snapshot tip (2) is strictly below the appended tip (3).
	seen := map[string]bool{}
	for _, h := range appendedChainHits() {
		seen[h.BlockHash+"/"+h.TxID] = true
	}
	for _, h := range snapshotChainHits() {
		if seen[h.BlockHash+"/"+h.TxID] {
			t.Fatalf("fixture tx %q is explainable on both chains", h.TxID)
		}
	}
}
