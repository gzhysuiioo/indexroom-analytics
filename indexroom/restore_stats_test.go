package indexroom

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file guards QueryTimeStats against a snapshot Restore that replaces
// the whole main chain. While the snapshot is still being read the index
// stays queryable and every stats query must answer from the complete old
// chain. Once a valid snapshot has been applied, every new query must answer
// from the complete new chain: no transaction, missing-time count, or height
// bound of the replaced chain may linger. A query racing the replacement
// itself may land on either side, but every field of ONE returned TimeStats —
// per-bucket counts, whole-window totals, the resolved height range, and the
// missing-time-block count — must come from that same complete chain. And a
// snapshot whose invalidity (a negative timestamp) only surfaces in its very
// last block must leave the old chain bit-for-bit in effect.
//
// The scenario (window [0,30), step 10 -> [0,10) [10,20) [20,30); height
// bounds left at zero so the range starts at height 1 and pins the observed
// chain tip; no transaction filter):
//
//	Old main chain, tip 4:
//	  h1 t=0  txs [a, a]  // same id twice in one block/segment
//	  h2 t=10 txs [a, b]  // a crosses segments (also in h1)
//	  h3 t=-  txs [a]     // missing timestamp: enters no segment and no block
//	  h4 t=20 txs [c]
//	Version-2 snapshot replacing it, tip 2:
//	  j1 t=0  txs [b]
//	  j2 t=20 txs [b, d, d] // d twice in one block: two occurrences, one id
//
// Old-chain result (range 1..4, one missing-time block):
//
//	[0,10):  occ 2 (a,a), distinct {a}=1,   blocks 1
//	[10,20): occ 2 (a,b), distinct {a,b}=2, blocks 1
//	[20,30): occ 1 (c),   distinct {c}=1,   blocks 1
//	totals: occ 5, window-distinct {a,b,c}=3, blocks 3, missing 1, to 4
//
// New-chain result (range 1..2, no missing-time block, [10,20) stays zeroed):
//
//	[0,10):  occ 1 (b),     distinct {b}=1,   blocks 1
//	[10,20): occ 0,         distinct 0,       blocks 0
//	[20,30): occ 3 (b,d,d), distinct {b,d}=2, blocks 1
//	totals: occ 4, window-distinct {b,d}=2, blocks 2, missing 0, to 2
//
// The two complete answers differ in every discriminating field, so any
// stitched-together answer — old transaction counts under the new height
// bound, the old missing-time count on the new chain, a dedup result from
// the wrong chain — stands out.

var restoreStatsQuery = TimeStatsQuery{Start: 0, End: 30, StepSeconds: 10}

func restoreOldChainBlocks() []Block {
	return []Block{
		timeBlock(1, "h1", "g", []string{"a", "a"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"a", "b"}, intptr(10)),
		timeBlock(3, "h3", "h2", []string{"a"}, nil),
		timeBlock(4, "h4", "h3", []string{"c"}, intptr(20)),
	}
}

func restoreNewChainBlocks() []Block {
	return []Block{
		timeBlock(1, "j1", "g", []string{"b"}, intptr(0)),
		timeBlock(2, "j2", "j1", []string{"b", "d", "d"}, intptr(20)),
	}
}

// restoreNewSnapshot is the version-2 wire form of restoreNewChainBlocks.
const restoreNewSnapshot = `{"version":2,"tip":2,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["b"],"timestamp":0},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["b","d","d"],"timestamp":20}` +
	`]}`

// restoreNegativeTimeSnapshot is identical to restoreNewSnapshot except that
// the LAST block carries an illegal negative timestamp: the document reads as
// valid until its final block is parsed.
const restoreNegativeTimeSnapshot = `{"version":2,"tip":2,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["b"],"timestamp":0},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["b","d","d"],"timestamp":-1}` +
	`]}`

func buildRestoreOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range restoreOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// literalRestoreOldStats is the hand-derived complete old-chain answer.
func literalRestoreOldStats() TimeStats {
	return TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 2, DistinctTxIDs: 1, Blocks: 1},
			{Start: 10, End: 20, TxCount: 2, DistinctTxIDs: 2, Blocks: 1},
			{Start: 20, End: 30, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 5, DistinctTxIDs: 3, Blocks: 3},
		FromHeight:        1,
		ToHeight:          4,
		MissingTimeBlocks: 1,
	}
}

// literalRestoreNewStats is the hand-derived complete new-chain answer.
func literalRestoreNewStats() TimeStats {
	return TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
			{Start: 10, End: 20, TxCount: 0, DistinctTxIDs: 0, Blocks: 0},
			{Start: 20, End: 30, TxCount: 3, DistinctTxIDs: 2, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 4, DistinctTxIDs: 2, Blocks: 2},
		FromHeight:        1,
		ToHeight:          2,
		MissingTimeBlocks: 0,
	}
}

func TestRestoreTimeStatsReferenceMatchesHandDerived(t *testing.T) {
	if got, want := referenceTimeStats(restoreOldChainBlocks(), restoreStatsQuery), literalRestoreOldStats(); !reflect.DeepEqual(got, want) {
		t.Fatalf("old-chain oracle=%+v\nwant=%+v", got, want)
	}
	if got, want := referenceTimeStats(restoreNewChainBlocks(), restoreStatsQuery), literalRestoreNewStats(); !reflect.DeepEqual(got, want) {
		t.Fatalf("new-chain oracle=%+v\nwant=%+v", got, want)
	}
}

// The counting semantics the regression rests on, asserted directly for each
// chain so the guarantees do not hide inside a DeepEqual.
func TestRestoreTimeStatsCountingSemanticsOnBothChains(t *testing.T) {
	old := literalRestoreOldStats()
	if old.Buckets[0].TxCount != 2 || old.Buckets[0].DistinctTxIDs != 1 {
		t.Fatalf("old [0,10): duplicate a must count twice but as one id: %+v", old.Buckets[0])
	}
	if old.Totals.DistinctTxIDs != 3 { // a in two segments counts once over the window
		t.Fatalf("old window distinct must dedup a across segments: %+v", old.Totals)
	}
	if old.MissingTimeBlocks != 1 || old.Totals.Blocks != 3 {
		t.Fatalf("time-less h3 counts toward missing but never toward blocks: %+v", old)
	}
	if old.ToHeight != 4 {
		t.Fatalf("old upper height must pin tip 4: %+v", old)
	}

	newStats := literalRestoreNewStats()
	if len(newStats.Buckets) != 3 || newStats.Buckets[1] != (TimeBucket{Start: 10, End: 20}) {
		t.Fatalf("empty [10,20) segment must still be returned zeroed: %+v", newStats.Buckets)
	}
	if newStats.Buckets[2].TxCount != 3 || newStats.Buckets[2].DistinctTxIDs != 2 {
		t.Fatalf("new [20,30): d twice counts twice but as one id: %+v", newStats.Buckets[2])
	}
	if newStats.Totals.TxCount != 4 || newStats.Totals.DistinctTxIDs != 2 {
		t.Fatalf("new totals: %+v", newStats.Totals)
	}
	if newStats.MissingTimeBlocks != 0 || newStats.ToHeight != 2 {
		t.Fatalf("new chain has no missing time and tip 2: %+v", newStats)
	}
}

// A valid restore replaces the whole chain: after it returns, a new query
// reflects only the snapshot chain and no old height survives anywhere.
func TestQueryTimeStatsBeforeAndAfterRestore(t *testing.T) {
	index := buildRestoreOldChain(t)

	before, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, literalRestoreOldStats()) {
		t.Fatalf("pre-restore stats=%+v\nwant=%+v", before, literalRestoreOldStats())
	}

	if err := index.Restore(strings.NewReader(restoreNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip != 2 {
		t.Fatalf("tip=%d, want restored tip 2", index.Tip)
	}

	after, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, literalRestoreNewStats()) {
		t.Fatalf("post-restore stats not wholly on the new chain:\n%+v", after)
	}
	// No residue of the replaced chain: heights 3 and 4 and their hashes are
	// gone, and none of the old transactions leak into any count.
	for _, height := range []int64{3, 4} {
		if _, ok := index.Blocks[height]; ok {
			t.Fatalf("replaced height %d still present", height)
		}
	}
	for _, gone := range []string{"h1", "h2", "h3", "h4"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("replaced hash %s still indexed", gone)
		}
	}
	if after.ToHeight != 2 || after.MissingTimeBlocks != 0 {
		t.Fatalf("old tip or missing-time count leaked into post-restore stats: %+v", after)
	}
	if after.Totals.TxCount != 4 || after.Totals.DistinctTxIDs != 2 {
		t.Fatalf("old-chain transactions leaked into post-restore totals: %+v", after.Totals)
	}
}

// gatedReader delivers the first prefix bytes of data, then parks until
// release is closed, then delivers the rest. It lets a test hold Restore
// mid-parse — after whole blocks have already been consumed but before the
// replacement is applied — without any sleeps or scheduling luck.
type gatedReader struct {
	data    []byte
	prefix  int
	off     int
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedReader(t *testing.T, data, cutMarker string) *gatedReader {
	t.Helper()
	prefix := strings.Index(data, cutMarker)
	if prefix <= 0 {
		t.Fatalf("test setup: cut marker %q not found in %q", cutMarker, data)
	}
	return &gatedReader{
		data:    []byte(data),
		prefix:  prefix,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.off < r.prefix {
		n := copy(p, r.data[r.off:r.prefix])
		r.off += n
		return n, nil
	}
	r.once.Do(func() { close(r.entered) })
	<-r.release
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// While the snapshot is still being read, the restore holds no lock: a stats
// query issued at that moment completes and answers from the complete old
// chain. Once the read finishes and the replacement lands, new queries answer
// from the complete new chain.
func TestQueryTimeStatsWhileRestoreStillReading(t *testing.T) {
	index := buildRestoreOldChain(t)
	// Park the read just before the second block object: the first block has
	// already been consumed, the replacement is nowhere near applied.
	reader := newGatedReader(t, restoreNewSnapshot, `{"height":2`)

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(reader)
	}()
	<-reader.entered // restore is parked mid-stream, holding no lock

	stats, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatalf("query during snapshot read failed: %v", err)
	}
	if !reflect.DeepEqual(stats, literalRestoreOldStats()) {
		t.Fatalf("query during snapshot read must see the complete old chain:\n%+v", stats)
	}
	select {
	case err := <-restoreDone:
		t.Fatalf("restore returned while its input was still gated: %v", err)
	default:
	}

	close(reader.release)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore after release failed: %v", err)
	}
	stats, err = index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stats, literalRestoreNewStats()) {
		t.Fatalf("query after completed restore must see the complete new chain:\n%+v", stats)
	}
}

// A query that has pinned the OLD tip (4) and still holds the lock when the
// restore's replacement is ready must finish on the complete old chain; the
// restore applies only afterwards.
func TestQueryTimeStatsInFlightObservesCompleteOldChainBeforeRestore(t *testing.T) {
	index := buildRestoreOldChain(t)

	queryParked := make(chan struct{})
	releaseQuery := make(chan struct{})
	var hookOnce sync.Once
	statsQueryHookLocked = func(to int64) {
		if to != 4 {
			t.Errorf("stats hook resolved to=%d, want old tip 4", to)
		}
		hookOnce.Do(func() { close(queryParked) })
		<-releaseQuery
	}
	t.Cleanup(func() { statsQueryHookLocked = nil })

	result := make(chan TimeStats, 1)
	errCh := make(chan error, 1)
	go func() {
		stats, err := index.QueryTimeStats(restoreStatsQuery)
		if err != nil {
			errCh <- err
			return
		}
		result <- stats
	}()
	<-queryParked // query holds index.mu and has pinned tip 4

	restoreDone := make(chan error, 1)
	go func() {
		// strings.Reader is fully readable, so this restore parses the whole
		// snapshot and then blocks on index.mu behind the parked query.
		restoreDone <- index.Restore(strings.NewReader(restoreNewSnapshot))
	}()

	close(releaseQuery) // the query scans and finishes on the old chain first
	var got TimeStats
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent query failed: %v", err)
	}
	if !reflect.DeepEqual(got, literalRestoreOldStats()) {
		t.Fatalf("query holding the lock across restore commit must see the old chain:\n%+v", got)
	}
	if err := classifyTimeStats(got, literalRestoreOldStats(), literalRestoreNewStats()); err != nil {
		t.Fatalf("in-flight query did not return one complete chain:\n%v", err)
	}
	statsQueryHookLocked = nil // the rendezvous is spent; later queries run free
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	stats, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stats, literalRestoreNewStats()) {
		t.Fatalf("query after restore must see the complete new chain:\n%+v", stats)
	}
}

// A query issued while a successful restore has installed the new chain but
// not yet returned must wait and then observe the complete NEW chain,
// including the shortened upper height.
func TestQueryTimeStatsIssuedAtRestoreCommitSeesNewChain(t *testing.T) {
	index := buildRestoreOldChain(t)

	restoreParked := make(chan struct{})
	releaseRestore := make(chan struct{})
	var hookOnce sync.Once
	restoreAppliedHookLocked = func(newTip int64) {
		if newTip != 2 {
			t.Errorf("restore hook saw newTip=%d, want 2", newTip)
		}
		hookOnce.Do(func() { close(restoreParked) })
		<-releaseRestore
	}
	t.Cleanup(func() { restoreAppliedHookLocked = nil })

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(strings.NewReader(restoreNewSnapshot))
	}()
	<-restoreParked // new chain installed (tip 2), Restore still holds the lock

	result := make(chan TimeStats, 1)
	go func() {
		stats, err := index.QueryTimeStats(restoreStatsQuery)
		if err != nil {
			t.Errorf("query during restore commit failed: %v", err)
			return
		}
		result <- stats
	}()
	select {
	case got := <-result:
		t.Fatalf("query returned while the restore still held the lock: %+v", got)
	default:
	}

	close(releaseRestore)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	got := <-result
	if err := classifyTimeStats(got, literalRestoreOldStats(), literalRestoreNewStats()); err != nil {
		t.Fatalf("query across restore commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(got, literalRestoreNewStats()) {
		t.Fatalf("query after restore commit must see the complete new chain:\n%+v", got)
	}
}

// A snapshot whose LAST block carries an illegal negative timestamp is
// rejected with ErrInvalidSnapshot only after the earlier blocks were already
// read. Nothing of them may remain: the old chain stays bit-for-bit in
// effect, before and after the failed restore.
func TestRestoreInvalidNegativeTimeAtLastBlockKeepsOldChain(t *testing.T) {
	index := buildRestoreOldChain(t)
	// Park the read just before the second (poisoned) block: the first block
	// of the snapshot has been fully consumed when the restore stalls.
	reader := newGatedReader(t, restoreNegativeTimeSnapshot, `{"height":2`)

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(reader)
	}()
	<-reader.entered

	stats, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatalf("query during gated read failed: %v", err)
	}
	if !reflect.DeepEqual(stats, literalRestoreOldStats()) {
		t.Fatalf("query while the invalid snapshot is still reading must see the old chain:\n%+v", stats)
	}

	close(reader.release)
	if err := <-restoreDone; !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("restore err=%v, want ErrInvalidSnapshot", err)
	}

	// The rejected restore changed nothing: stats still match the old chain
	// exactly and no partially read snapshot block survived.
	stats, err = index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stats, literalRestoreOldStats()) {
		t.Fatalf("stats after rejected restore differ from the old chain:\n%+v", stats)
	}
	if index.Tip != 4 || len(index.Blocks) != 4 {
		t.Fatalf("rejected restore left tip=%d blocks=%d, want tip 4 with 4 blocks", index.Tip, len(index.Blocks))
	}
	if index.Blocks[3].Time != nil {
		t.Fatalf("time-less h3 gained a timestamp from the rejected snapshot: %+v", index.Blocks[3])
	}
	for _, hash := range []string{"h1", "h2", "h3", "h4"} {
		if _, ok := index.ByHash[hash]; !ok {
			t.Fatalf("old hash %s lost after rejected restore", hash)
		}
	}
	for _, leaked := range []string{"j1", "j2"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("hash %s from the rejected snapshot was left indexed", leaked)
		}
	}
	if stats.MissingTimeBlocks != 1 || stats.ToHeight != 4 {
		t.Fatalf("rejected snapshot altered the missing-time count or upper height: %+v", stats)
	}
}

// The complete-chain validator must catch exactly the mixtures this
// regression targets: old-chain transaction counts paired with the new
// chain's height bound, missing-time count, or dedup result (and vice versa).
func TestClassifyTimeStatsRejectsRestoreMixtures(t *testing.T) {
	oldRef, newRef := literalRestoreOldStats(), literalRestoreNewStats()
	if err := classifyTimeStats(oldRef, oldRef, newRef); err != nil {
		t.Fatalf("pure old answer rejected: %v", err)
	}
	if err := classifyTimeStats(newRef, oldRef, newRef); err != nil {
		t.Fatalf("pure new answer rejected: %v", err)
	}

	// Old transaction counts and dedup under the NEW upper height.
	mixed := oldRef
	mixed.ToHeight = newRef.ToHeight
	if err := classifyTimeStats(mixed, oldRef, newRef); err == nil {
		t.Fatal("old counts under the new tip height were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("old-counts/new-height mixture not reported as MIXED:\n%v", err)
	}

	// New transaction counts under the OLD upper height and missing count.
	mixed = newRef
	mixed.ToHeight = oldRef.ToHeight
	mixed.MissingTimeBlocks = oldRef.MissingTimeBlocks
	if err := classifyTimeStats(mixed, oldRef, newRef); err == nil {
		t.Fatal("new counts under the old tip and missing count were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("new-counts/old-height mixture not reported as MIXED:\n%v", err)
	}

	// Old per-bucket content with the new chain's dedup result in the totals.
	mixed = oldRef
	mixed.Totals.DistinctTxIDs = newRef.Totals.DistinctTxIDs
	if err := classifyTimeStats(mixed, oldRef, newRef); err == nil {
		t.Fatal("old buckets with the new dedup total were accepted")
	}
}

// Sustained overlap: restores alternate between the old-chain snapshot and
// the new-chain snapshot while readers query continuously. Every answer must
// classify as one complete chain; success never depends on which side of a
// restore a query happened to land. This is a supplement to the deterministic
// rendezvous tests above, not a substitute: those pin the interleavings, this
// one sweeps for anything they missed.
func TestQueryTimeStatsRepeatedRestoresStayConsistent(t *testing.T) {
	index := buildRestoreOldChain(t)
	oldSnapshot := exportString(t, index) // version 2: the chain has timestamps
	oldRef, newRef := literalRestoreOldStats(), literalRestoreNewStats()

	const restorers = 2
	const rounds = 40
	const readers = 4
	const reads = 80

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < restorers; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				select {
				case <-stop:
					return
				default:
				}
				snapshot := restoreNewSnapshot
				if (i+seed)%2 == 0 {
					snapshot = oldSnapshot
				}
				if err := index.Restore(strings.NewReader(snapshot)); err != nil {
					t.Errorf("restore failed: %v", err)
					return
				}
			}
		}(r)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < reads; i++ {
				select {
				case <-stop:
					return
				default:
				}
				stats, err := index.QueryTimeStats(restoreStatsQuery)
				if err != nil {
					t.Errorf("query failed: %v", err)
					return
				}
				if err := classifyTimeStats(stats, oldRef, newRef); err != nil {
					t.Errorf("%v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)

	// Leave the index deterministically on the new chain and re-verify.
	if err := index.Restore(strings.NewReader(restoreNewSnapshot)); err != nil {
		t.Fatalf("final restore failed: %v", err)
	}
	final, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, newRef) {
		t.Fatalf("final state is not the complete new chain:\n%+v", final)
	}
}
