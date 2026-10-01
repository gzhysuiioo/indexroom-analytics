package indexroom

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestExportV2ExactBytes(t *testing.T) {
	index := New()
	blocks := []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"t1"}, Time: nil},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{}, Time: intptr(0)},
		{Height: 3, Hash: "h3", Parent: "h2", Time: intptr(1700000000)},
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	want := `{"version":2,"tip":3,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":["t1"],"timestamp":null},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[],"timestamp":0},` +
		`{"height":3,"hash":"h3","parent":"h2","txs":[],"timestamp":1700000000}` +
		`]}`
	if raw := exportString(t, index); raw != want {
		t.Fatalf("export=%s\nwant=%s", raw, want)
	}
}

func TestExportStaysV1WhenAllTimesMissing(t *testing.T) {
	// Chains restored from version 1 and chains appended without times both
	// export the original version-1 bytes, including the empty index.
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	want := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":["t1"]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":[]}` +
		`]}`
	if raw := exportString(t, index); raw != want {
		t.Fatalf("export=%s\nwant=%s", raw, want)
	}

	v1 := `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[]}]}`
	restored := New()
	if err := restored.Restore(strings.NewReader(v1)); err != nil {
		t.Fatal(err)
	}
	if raw := exportString(t, restored); raw != v1 {
		t.Fatalf("restored v1 re-exported as %s", raw)
	}
}

func TestRestoreV1TreatsTimestampsAsMissing(t *testing.T) {
	v1 := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":["a"]},` +
		`{"height":2,"hash":"h2","parent":"h1","txs":["b"]}` +
		`]}`
	index := New()
	if err := index.Restore(strings.NewReader(v1)); err != nil {
		t.Fatal(err)
	}
	for h := int64(1); h <= 2; h++ {
		if index.Blocks[h].Time != nil {
			t.Fatalf("height %d got time %v, want missing", h, index.Blocks[h].Time)
		}
	}
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 100, StepSeconds: 100})
	if err != nil {
		t.Fatal(err)
	}
	if stats.MissingTimeBlocks != 2 || stats.Totals.TxCount != 0 {
		t.Fatalf("v1 blocks entered buckets: %+v", stats)
	}
}

func TestSnapshotV2RoundTripPreservesTimes(t *testing.T) {
	source := New()
	blocks := []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"a", ""}, Time: nil},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"b", "b"}, Time: intptr(0)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"c"}, Time: intptr(42)},
	}
	for _, b := range blocks {
		if err := source.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	raw := exportString(t, source)

	target := New()
	if err := target.Restore(strings.NewReader(raw)); err != nil {
		t.Fatalf("restore v2 failed: %v", err)
	}
	for h := int64(1); h <= 3; h++ {
		got, want := target.Blocks[h].Time, source.Blocks[h].Time
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Fatalf("height %d time=%v, want %v", h, got, want)
		}
	}
	// Export after restore is byte-identical, and stats agree.
	if again := exportString(t, target); again != raw {
		t.Fatalf("re-export differs:\n%s\n%s", again, raw)
	}
	query := TimeStatsQuery{From: 1, TxIDs: []string{"b", "c", ""}, Start: 0, End: 100, StepSeconds: 25}
	gotStats, err := target.QueryTimeStats(query)
	if err != nil {
		t.Fatal(err)
	}
	wantStats, err := source.QueryTimeStats(query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotStats, wantStats) {
		t.Fatalf("stats differ:\ngot  %+v\nwant %+v", gotStats, wantStats)
	}
}

func TestExportV2DeterministicAcrossHistory(t *testing.T) {
	indexA := chain(t, Block{Height: 1, Hash: "h1", Parent: "g", Time: intptr(1)})
	if _, err := indexA.Reorg([]Block{
		{Height: 2, Hash: "old", Parent: "h1", Time: intptr(2)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := indexA.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(20)},
		{Height: 3, Hash: "h3", Parent: "h2", Time: nil},
	}); err != nil {
		t.Fatal(err)
	}
	indexB := New()
	for _, b := range []Block{
		{Height: 1, Hash: "h1", Parent: "g", Time: intptr(1)},
		{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(20)},
		{Height: 3, Hash: "h3", Parent: "h2", Time: nil},
	} {
		if err := indexB.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	if rawA, rawB := exportString(t, indexA), exportString(t, indexB); rawA != rawB {
		t.Fatalf("same chain exported differently:\n%s\n%s", rawA, rawB)
	}
}

func TestRestoreV2RejectsInvalidTimestamps(t *testing.T) {
	good := `{"version":2,"tip":1,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":5}` +
		`]}`
	cases := map[string]string{
		"missing timestamp":          `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[]}]}`,
		"negative timestamp":         `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":-1}]}`,
		"string timestamp":           `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":"5"}]}`,
		"float timestamp":            `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":1.5}]}`,
		"bool timestamp":             `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":true}]}`,
		"duplicate timestamp":        `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":1,"timestamp":2}]}`,
		"unknown block field":        `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":1,"extra":2}]}`,
		"v1 block carries timestamp": `{"version":1,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":1}]}`,
		"truncated timestamp":        `{"version":2,"tip":1,"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(strings.NewReader(input))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
	// The well-formed document restores fine, and null is a valid missing time.
	index := New()
	if err := index.Restore(strings.NewReader(good)); err != nil {
		t.Fatalf("valid v2 refused: %v", err)
	}
	nullDoc := `{"version":2,"tip":1,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":[],"timestamp":null}]}`
	if err := index.Restore(strings.NewReader(nullDoc)); err != nil {
		t.Fatalf("null timestamp refused: %v", err)
	}
	if index.Blocks[1].Time != nil {
		t.Fatalf("null restored as %v, want nil", index.Blocks[1].Time)
	}
}

func TestRestoreV2FailureLeavesTimesUntouched(t *testing.T) {
	index := New()
	for _, b := range []Block{
		{Height: 1, Hash: "h1", Parent: "g", Time: intptr(1)},
		{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(2)},
	} {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	blocks, byHash, tip := snapshot(index)
	bad := `{"version":2,"tip":2,"blocks":[` +
		`{"height":1,"hash":"x1","parent":"g","txs":[],"timestamp":10},` +
		`{"height":2,"hash":"x2","parent":"x1","txs":[],"timestamp":-5}` +
		`]}`
	if err := index.Restore(strings.NewReader(bad)); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}
