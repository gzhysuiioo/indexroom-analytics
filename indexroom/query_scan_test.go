package indexroom

import (
	"fmt"
	"reflect"
	"testing"
)

// referenceScan independently computes the full filtered occurrence list and
// matched-block count for [from, to], the same way the old collectLocked did,
// so the page-sized scan can be compared against an oracle that keeps every
// record.
func referenceScan(index *Index, from, to int64, txIDs []string) ([]TxHit, int64) {
	filter := map[string]struct{}{}
	for _, id := range txIDs {
		filter[id] = struct{}{}
	}
	var all []TxHit
	var blocks int64
	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		matched := false
		for position, tx := range block.Txs {
			if len(txIDs) > 0 {
				if _, ok := filter[tx]; !ok {
					continue
				}
			}
			all = append(all, TxHit{Height: height, BlockHash: block.Hash, TxID: tx, Position: position})
			matched = true
		}
		if matched {
			blocks++
		}
	}
	return all, blocks
}

// TestScanPageLockedRetainsOnlyPageWindow is the memory contract behind the
// QueryTxs refactor: a scan may keep at most pageSize TxHit records no matter
// how many matches the pinned range holds, while totals still describe the
// whole range and every offset slices the same ordered occurrence list the
// old full-materialization scan would have produced.
func TestScanPageLockedRetainsOnlyPageWindow(t *testing.T) {
	// 60 blocks, three txs each with repeated identifiers: 180 occurrences.
	var blocks [][]string
	for h := 0; h < 60; h++ {
		blocks = append(blocks, []string{
			fmt.Sprintf("t%d", h%5), // five ids cycling: selective filter
			"keep",
			"keep", // same id twice in one block: both occurrences survive
		})
	}
	index := txChain(t, blocks...)

	const from, to = int64(1), int64(60)
	all, matchedBlocks := referenceScan(index, from, to, nil)
	if len(all) != 180 || matchedBlocks != 60 {
		t.Fatalf("oracle setup: hits=%d blocks=%d, want 180/60", len(all), matchedBlocks)
	}

	for _, pageSize := range []int{1, 2, 7, DefaultPageSize, MaxPageSize} {
		var windowed []TxHit
		for offset := int64(0); ; {
			hits, total, gotBlocks := index.scanPageLocked(from, to, newTxFilter(nil), offset, pageSize)
			if total != int64(len(all)) {
				t.Fatalf("pageSize=%d offset=%d: total=%d, want whole-range %d", pageSize, offset, total, len(all))
			}
			if gotBlocks != matchedBlocks {
				t.Fatalf("pageSize=%d offset=%d: blocks=%d, want %d", pageSize, offset, gotBlocks, matchedBlocks)
			}
			if len(hits) > pageSize || cap(hits) > pageSize {
				t.Fatalf("pageSize=%d offset=%d: scan retained len=%d cap=%d records, want at most %d",
					pageSize, offset, len(hits), cap(hits), pageSize)
			}
			end := offset + int64(len(hits))
			if end > int64(len(all)) || !reflect.DeepEqual(hits, all[offset:end]) {
				t.Fatalf("pageSize=%d offset=%d: window does not match the full occurrence slice", pageSize, offset)
			}
			windowed = append(windowed, hits...)
			if len(hits) < pageSize {
				break // last, possibly partial page
			}
			offset = end
		}
		if !reflect.DeepEqual(windowed, all) {
			t.Fatalf("pageSize=%d: walking windows lost or duplicated occurrences", pageSize)
		}
	}

	// A selective filter keeps the same contract on the filtered answer.
	filtered, filteredBlocks := referenceScan(index, from, to, []string{"keep", "t1"})
	hits, total, gotBlocks := index.scanPageLocked(from, to, newTxFilter([]string{"t1", "keep", "keep"}), 3, 5)
	if total != int64(len(filtered)) || gotBlocks != filteredBlocks {
		t.Fatalf("filtered totals=%d/%d, want %d/%d", total, gotBlocks, len(filtered), filteredBlocks)
	}
	if !reflect.DeepEqual(hits, filtered[3:8]) {
		t.Fatalf("filtered window=%v, want %v", hits, filtered[3:8])
	}
	if cap(hits) > 5 {
		t.Fatalf("filtered scan retained cap=%d, want at most 5", cap(hits))
	}

	// An offset beyond the matches keeps whole-range statistics but returns no
	// records; the continuation caller turns this into ErrQueryChanged.
	hits, total, gotBlocks = index.scanPageLocked(from, to, newTxFilter(nil), int64(len(all))+10, 4)
	if len(hits) != 0 || total != int64(len(all)) || gotBlocks != matchedBlocks {
		t.Fatalf("beyond-end scan: hits=%d total=%d blocks=%d", len(hits), total, gotBlocks)
	}

	// An empty range retains nothing and reports zero statistics.
	hits, total, gotBlocks = index.scanPageLocked(100, 200, newTxFilter(nil), 0, 10)
	if len(hits) != 0 || cap(hits) > 10 || total != 0 || gotBlocks != 0 {
		t.Fatalf("empty-range scan: hits=%v total=%d blocks=%d", hits, total, gotBlocks)
	}
}

// TestQueryTxsTemporaryRecordsDoNotScaleWithTotal is an end-to-end check that
// paging through a range with a tiny page size and a huge hit total works
// identically whether the answer is read in one page or many: the temporary
// footprint shrinks with the page size while all visible behavior is fixed.
func TestQueryTxsTemporaryRecordsDoNotScaleWithTotal(t *testing.T) {
	var blocks [][]string
	for h := 0; h < 40; h++ {
		blocks = append(blocks, []string{"a", "b", "c", "d", "e"})
	}
	index := txChain(t, blocks...) // 200 matches

	full, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if full.TotalMatches != 200 || full.MatchedBlocks != 40 {
		t.Fatalf("unexpected totals: %+v", full)
	}

	// Page size 1: 200 round trips, each retaining one hit while scanning.
	pages := collectPages(t, index, TxQuery{PageSize: 1})
	if len(pages) != 200 {
		t.Fatalf("pages=%d, want 200", len(pages))
	}
	var all []TxHit
	for i, page := range pages {
		if page.TotalMatches != 200 || page.MatchedBlocks != 40 || page.ToHeight != 40 {
			t.Fatalf("page %d statistics describe the page instead of the range: %+v", i, page)
		}
		if i < len(pages)-1 && len(page.Hits) != 1 {
			t.Fatalf("page %d holds %d hits, want exactly one", i, len(page.Hits))
		}
		if i < len(pages)-1 && page.NextCursor == "" {
			t.Fatalf("page %d unexpectedly the last", i)
		}
		all = append(all, page.Hits...)
	}
	if pages[len(pages)-1].NextCursor != "" {
		t.Fatalf("last page must carry an empty cursor, got %q", pages[len(pages)-1].NextCursor)
	}
	if len(all) != 200 {
		t.Fatalf("collected %d hits, want 200", len(all))
	}
	// The single large-page answer spans the same ordered occurrences.
	if full.NextCursor != "" || len(full.Hits) != 200 {
		t.Fatalf("unexpected large page: %d hits cursor=%q", len(full.Hits), full.NextCursor)
	}
	if !reflect.DeepEqual(all, full.Hits) {
		t.Fatalf("page-size-1 walk differs from the full answer")
	}
}
