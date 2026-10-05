package indexroom

import (
	"reflect"
	"testing"
)

// digest constants computed independently from the pre-refactor hashTxSet
// algorithm; pinning them guarantees the v1 cursor set field stays
// byte-compatible and old cursors continue to validate.
func TestTxFilterDigestMatchesLegacyCursorEncoding(t *testing.T) {
	cases := []struct {
		name  string
		txIDs []string
		want  string
	}{
		{"unrestricted", nil, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"empty slice", []string{}, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"empty identifier only", []string{""}, "af5570f5a1810b7af78caf4bc70a660f0df51e42baf91d4de5b2328de0e83dfc"},
		{"a and b", []string{"a", "b", "a"}, "3c9d591045bc8876f9d0399bbfb05c6a412096e906f73278f98406cd5dca86df"},
		{"ab and c", []string{"ab", "c"}, "601d5476e2ccfe2c87a2bba7a322659734a05749d5b5aa781f513e4912db0d5f"},
		{"a and bc", []string{"a", "bc"}, "3fafa1cf2f19a7c1129beb20cf0983f73a489a221fc0dd2f16d1be292d089205"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newTxFilter(tc.txIDs).hexDigest(); got != tc.want {
				t.Fatalf("digest=%s, want legacy %s", got, tc.want)
			}
		})
	}
}

// TestTxFilterSetEquivalence pins the shared semantics used by both query
// paths and by cursor validation: order and repetitions are irrelevant, but
// exact string boundaries, case, and surrounding whitespace matter, and the
// unrestricted filter is not the same as {""}.
func TestTxFilterSetEquivalence(t *testing.T) {
	groups := [][][]string{
		{{"a", "b", "a"}, {"b", "a"}, {"b", "a", "b", "a"}},
		{nil, {}, []string{}},
		{{""}, {"", ""}},
		{{"ab", "c"}, {"c", "ab", "c"}},
		{{"a", "bc"}},
		{{"A"}},   // case is significant: distinct from group 0's "a"
		{{" a "}}, // surrounding whitespace is significant
	}
	digests := make([]string, len(groups))
	for i, spellings := range groups {
		want := newTxFilter(spellings[0]).hexDigest()
		digests[i] = want
		for _, spelling := range spellings[1:] {
			if got := newTxFilter(spelling).hexDigest(); got != want {
				t.Fatalf("spellings %q and %q differ: %s vs %s", spelling, spellings[0], got, want)
			}
		}
	}
	for i := 0; i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			if digests[i] == digests[j] {
				t.Fatalf("distinct filters collide: %q and %q -> %s", groups[i][0], groups[j][0], digests[i])
			}
		}
	}
}

// TestTxFilterMatching pins exact membership and the three filter modes.
func TestTxFilterMatching(t *testing.T) {
	unrestricted := newTxFilter(nil)
	for _, id := range []string{"", "a", "ab", " A ", "z"} {
		if !unrestricted.matches(id) {
			t.Fatalf("unrestricted filter rejected %q", id)
		}
	}

	onlyEmpty := newTxFilter([]string{"", ""})
	if !onlyEmpty.matches("") {
		t.Fatal(`{""} filter must match the empty identifier`)
	}
	for _, id := range []string{"a", " ", "\t"} {
		if onlyEmpty.matches(id) {
			t.Fatalf(`{""} filter matched %q`, id)
		}
	}

	f := newTxFilter([]string{"ab", "c", "ab"})
	for _, id := range []string{"ab", "c"} {
		if !f.matches(id) {
			t.Fatalf("set filter rejected member %q", id)
		}
	}
	for _, id := range []string{"", "a", "bc", "AB", " c", "ab ", "x"} {
		if f.matches(id) {
			t.Fatalf("set filter matched non-member %q", id)
		}
	}
}

// TestTxFilterDoesNotMutateInput guarantees the caller's list keeps its exact
// contents and order after canonicalization, including repeated entries.
func TestTxFilterDoesNotMutateInput(t *testing.T) {
	in := []string{"b", "", "a", "b", "", " a "}
	want := append([]string{}, in...)
	_ = newTxFilter(in)
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("input list mutated: %q, want %q", in, want)
	}
}

// TestQueriesDoNotMutateTxIDs pins the public contract end to end: neither a
// first page, a continuation, nor a time-stats call reorder, deduplicate, or
// otherwise touch the caller's TxIDs list.
func TestQueriesDoNotMutateTxIDs(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b", "a"},
		[]string{"", "a"},
	)
	in := []string{"b", "a", "", "b", "a"}
	want := append([]string{}, in...)

	first, err := index.QueryTxs(TxQuery{TxIDs: in, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("first page mutated TxIDs: %q, want %q", in, want)
	}
	if _, err := index.QueryTxs(TxQuery{TxIDs: in, PageSize: 1, Cursor: first.NextCursor}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("continuation mutated TxIDs: %q, want %q", in, want)
	}
	if _, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: in, Start: 0, End: 100, StepSeconds: 50,
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, want) {
		t.Fatalf("time stats mutated TxIDs: %q, want %q", in, want)
	}
}
