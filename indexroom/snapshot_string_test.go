package indexroom

import (
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// v1Chain wraps block JSON fragments into a version-1 snapshot.
func v1Chain(blocks ...string) string {
	return `{"version":1,"tip":` + strconv.Itoa(len(blocks)) + `,"blocks":[` +
		strings.Join(blocks, ",") + `]}`
}

// v2Chain wraps block JSON fragments into a version-2 snapshot; each
// fragment must carry its own timestamp field.
func v2Chain(blocks ...string) string {
	return `{"version":2,"tip":` + strconv.Itoa(len(blocks)) + `,"blocks":[` +
		strings.Join(blocks, ",") + `]}`
}

func TestRestoreRejectsInvalidStringEncoding(t *testing.T) {
	// badByte is an invalid UTF-8 byte inside an otherwise valid string.
	badByte := "\xff"
	cases := map[string]struct {
		input string
		want  []string // substrings the error must carry
	}{
		"invalid utf8 in hash": {
			v1Chain(`{"height":1,"hash":"h` + badByte + `1","parent":"g","txs":[]}`),
			[]string{`"hash"`, "height 1"},
		},
		"invalid utf8 in parent": {
			v1Chain(`{"height":1,"hash":"h1","parent":"g` + badByte + `","txs":[]}`),
			[]string{`"parent"`, "height 1"},
		},
		"invalid utf8 in tx id": {
			v1Chain(`{"height":1,"hash":"h1","parent":"g","txs":["ok","t` + badByte + `"]}`),
			[]string{`"txs"`, "height 1", "element 1"},
		},
		"invalid utf8 in last block": {
			v1Chain(
				`{"height":1,"hash":"h1","parent":"g","txs":[]}`,
				`{"height":2,"hash":"h`+badByte+`2","parent":"h1","txs":[]}`,
			),
			[]string{`"hash"`, "height 2"},
		},
		"invalid utf8 in v2 block": {
			v2Chain(`{"height":1,"hash":"h1","parent":"g` + badByte + `","txs":[],"timestamp":5}`),
			[]string{`"parent"`, "height 1"},
		},
		"two high surrogates in hash": {
			v1Chain(`{"height":1,"hash":"h\ud801\ud802","parent":"g","txs":[]}`),
			[]string{`"hash"`, "height 1"},
		}, "lone low surrogate in parent": {
			v1Chain(`{"height":1,"hash":"h1","parent":"g\ude00","txs":[]}`),
			[]string{`"parent"`, "height 1"},
		},
		"misordered surrogate pair": {
			v1Chain(`{"height":1,"hash":"\ude00\ud800","parent":"g","txs":[]}`),
			[]string{`"hash"`, "height 1"},
		},
		"high surrogate then plain escape": {
			v1Chain(`{"height":1,"hash":"\ud800A","parent":"g","txs":[]}`),
			[]string{`"hash"`, "height 1"},
		},
		"high surrogate then literal text": {
			v1Chain(`{"height":1,"hash":"\ud800x","parent":"g","txs":[]}`),
			[]string{`"hash"`, "height 1"},
		},
		"high surrogate at end of string": {
			v1Chain(`{"height":1,"hash":"h\ud800","parent":"g","txs":[]}`),
			[]string{`"hash"`, "height 1"},
		},
		"lone surrogate in tx id": {
			v1Chain(`{"height":1,"hash":"h1","parent":"g","txs":["a","b\ud83d","c"]}`),
			[]string{`"txs"`, "height 1", "element 1"},
		},
		"lone surrogate in v2 tx id": {
			v2Chain(`{"height":1,"hash":"h1","parent":"g","txs":["\udfff"],"timestamp":null}`),
			[]string{`"txs"`, "height 1", "element 0"},
		},
		"field order does not hide the height": {
			v1Chain(
				`{"height":1,"hash":"h1","parent":"g","txs":[]}`,
				`{"hash":"h`+badByte+`2","txs":[],"parent":"h1","height":2}`,
			),
			[]string{`"hash"`, "height 2"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(strings.NewReader(tc.input))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%v, want it to mention %s", err, want)
				}
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

// esc builds a JSON \uXXXX escape sequence for embedding in a snapshot
// document.
func esc(hex string) string { return "\\" + "u" + hex }

func TestRestoreAcceptsValidUnicodeIdentifiers(t *testing.T) {
	// The same character written literally and as escapes must decode to
	// the same identifier: the escaped emoji in txs[0] equals the literal
	// one in txs[4], the escaped 中文 parent links to the literal hash, and
	// a genuine U+FFFD — literal or escaped — is valid input.
	input := v1Chain(
		`{"height":1,"hash":"中文","parent":"中文","txs":["tx-`+esc("d83d")+esc("de00")+`","�","`+esc("fffd")+`","","tx-😀"]}`,
		`{"height":2,"hash":"`+esc("d83d")+esc("de00")+`","parent":"`+esc("4e2d")+esc("6587")+`","txs":["h\\u0041sh"]}`,
	)
	index := txChain(t, []string{"old"})
	if err := index.Restore(strings.NewReader(input)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	blocks, _, tip := snapshot(index)
	if tip != 2 {
		t.Fatalf("tip=%d, want 2", tip)
	}
	if blocks[1].Hash != "中文" || blocks[1].Parent != "中文" {
		t.Fatalf("block 1 identifiers=%q/%q, want 中文", blocks[1].Hash, blocks[1].Parent)
	}
	wantTxs := []string{"tx-😀", "�", "�", "", "tx-😀"}
	if !reflect.DeepEqual(blocks[1].Txs, wantTxs) {
		t.Fatalf("block 1 txs=%q, want %q", blocks[1].Txs, wantTxs)
	}
	if blocks[2].Hash != "😀" {
		t.Fatalf("block 2 hash=%q, want 😀", blocks[2].Hash)
	}
	// An escaped backslash followed by "u0041" is literal text, not an
	// escape, and must survive verbatim.
	wantLiteral := "h" + string('\\') + "u0041sh"
	if blocks[2].Txs[0] != wantLiteral {
		t.Fatalf("block 2 tx=%q, want literal %q", blocks[2].Txs[0], wantLiteral)
	}

	// Exact-match queries see the restored identifiers as they are.
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"tx-😀"}})
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if page.TotalMatches != 2 {
		t.Fatalf("matches=%d, want 2 for the duplicated identifier", page.TotalMatches)
	}
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"�"}})
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if page.TotalMatches != 2 {
		t.Fatalf("matches=%d, want 2 for the genuine U+FFFD identifiers", page.TotalMatches)
	}

	// Re-export carries the original identifiers, in order, duplicates and
	// the empty string included.
	if raw := exportString(t, index); !strings.Contains(raw, `"tx-😀","�","�","","tx-😀"`) {
		t.Fatalf("export=%s, want the original tx identifiers verbatim", raw)
	}
}

// dripReader delivers its data one byte at a time, so a multi-byte UTF-8
// character is split across several Read calls.
type dripReader struct{ data []byte }

func (r *dripReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestRestoreSplitMultibyteReadsAreNotEncodingErrors(t *testing.T) {
	input := v1Chain(`{"height":1,"hash":"中文字","parent":"g","txs":["😀"]}`)
	index := New()
	if err := index.Restore(&dripReader{data: []byte(input)}); err != nil {
		t.Fatalf("restore over split reads failed: %v", err)
	}
	blocks, _, _ := snapshot(index)
	if blocks[1].Hash != "中文字" || blocks[1].Txs[0] != "😀" {
		t.Fatalf("restored identifiers=%q/%q", blocks[1].Hash, blocks[1].Txs)
	}
}
