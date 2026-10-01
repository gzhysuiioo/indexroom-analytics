package indexroom

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// timeChain builds a chain whose blocks carry the given per-height timestamps
// (nil entry means a missing timestamp).
func timeChain(t *testing.T, times ...*int64) *Index {
	t.Helper()
	index := New()
	for i, when := range times {
		height := int64(i + 1)
		parent := "genesis"
		if height > 1 {
			parent = fmt.Sprintf("h%d", height-1)
		}
		block := Block{
			Height: height,
			Hash:   fmt.Sprintf("h%d", height),
			Parent: parent,
			Txs:    []string{"a"},
			Time:   when,
		}
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at %d: %v", height, err)
		}
	}
	return index
}

func TestQueryTxsCursorDiesOnInRangeTimestampChange(t *testing.T) {
	t.Run("reorg changes timestamp in range", func(t *testing.T) {
		idx := timeChain(t, intptr(10), intptr(20), intptr(30))
		first, _ := idx.QueryTxs(TxQuery{PageSize: 1})
		if _, err := idx.Reorg([]Block{
			{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}, Time: intptr(999)},
			{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: intptr(30)},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := idx.QueryTxs(TxQuery{PageSize: 1, Cursor: first.NextCursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("missing becomes known in range", func(t *testing.T) {
		idx := timeChain(t, intptr(10), nil, intptr(30))
		first, _ := idx.QueryTxs(TxQuery{PageSize: 1})
		if _, err := idx.Reorg([]Block{
			{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}, Time: intptr(20)},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := idx.QueryTxs(TxQuery{PageSize: 1, Cursor: first.NextCursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("restore changes timestamp in range", func(t *testing.T) {
		idx := timeChain(t, intptr(10), intptr(20), intptr(30))
		first, _ := idx.QueryTxs(TxQuery{PageSize: 1})
		snap := `{"version":2,"tip":3,"blocks":[` +
			`{"height":1,"hash":"h1","parent":"genesis","txs":["a"],"timestamp":10},` +
			`{"height":2,"hash":"h2","parent":"h1","txs":["a"],"timestamp":21},` +
			`{"height":3,"hash":"h3","parent":"h2","txs":["a"],"timestamp":30}` +
			`]}`
		if err := idx.Restore(strings.NewReader(snap)); err != nil {
			t.Fatal(err)
		}
		if _, err := idx.QueryTxs(TxQuery{PageSize: 1, Cursor: first.NextCursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})
}

func TestQueryTxsCursorSurvivesTimestampChangeOutsideRange(t *testing.T) {
	index := timeChain(t, intptr(10), intptr(20), intptr(30), intptr(40))
	// Pin the query to heights 1..2.
	first, err := index.QueryTxs(TxQuery{From: 1, To: 2, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Change only the timestamp of height 4, outside the pinned range.
	if _, err := index.Reorg([]Block{
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: intptr(30)},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"a"}, Time: intptr(400)},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := index.QueryTxs(TxQuery{From: 1, To: 2, PageSize: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("out-of-range time change killed the cursor: %v", err)
	}
	if page.TotalMatches != 2 || page.ToHeight != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestQueryTxsCursorSurvivesIdenticalTimestamps(t *testing.T) {
	index := timeChain(t, intptr(10), intptr(20), intptr(30))
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Reorg that replays the branch with identical hashes, txs, and times.
	if _, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}, Time: intptr(20)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: intptr(30)},
	}); err != nil {
		t.Fatal(err)
	}
	pages := collectPages(t, index, TxQuery{PageSize: 1, Cursor: first.NextCursor})
	var hits int
	for _, page := range pages {
		if page.TotalMatches != 3 || page.ToHeight != 3 {
			t.Fatalf("stats changed after identical-time replay: %+v", page)
		}
		hits += len(page.Hits)
	}
	if hits != 2 { // the first page already consumed height 1
		t.Fatalf("continuation yielded %d hits, want 2", hits)
	}
}
