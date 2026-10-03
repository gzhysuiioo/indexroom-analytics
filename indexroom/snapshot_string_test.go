package indexroom

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

// snapV1 builds a version-1 snapshot around raw block objects.
func snapV1(blocks ...string) string {
	return `{"version":1,"tip":` + strconv.Itoa(len(blocks)) + `,"blocks":[` +
		strings.Join(blocks, ",") + `]}`
}

// byteReader delivers its data one byte per Read call, so a multi-byte
// character arrives split across several reads.
type byteReader struct {
	data []byte
}

func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestRestoreRejectsInvalidStringEncoding(t *testing.T) {
	// Each case corrupts one identifier in an otherwise valid snapshot.
	cases := map[string]struct {
		input string
		want  []string // substrings the error must carry
	}{
		"invalid utf8 in hash": {
			snapV1(`{"height":1,"hash":"h` + "\xff" + `1","parent":"g","txs":[]}`),
			[]string{`"hash"`, "height 1"},
		},
		"invalid utf8 in parent": {
			snapV1(`{"height":1,"hash":"h1","parent":"` + "\xc0\xaf" + `","txs":[]}`),
			[]string{`"parent"`, "height 1"},
		},
		"invalid utf8 in tx id": {
			snapV1(`{"height":1,"hash":"h1","parent":"g","txs":["ok","` + "\xed\xa0\x80" + `"]}`),
			[]string{"height 1", "element 1"},
		},
		"lone high surrogate in hash": {
			snapV1(`{"height":1,"hash":"h\uD835","parent":"g","txs":[]}`),
			[]string{`"hash"`, "height 1"},
		},
		"lone low surrogate in parent": {
			snapV1(`{"height":1,"hash":"h1","parent":"\uDC00","txs":[]}`),
			[]string{`"parent"`, "height 1"},
		},
		"reversed surrogate pair in tx id": {
			snapV1(`{"height":1,"hash":"h1","parent":"g","txs":["\uDE00\uD83D"]}`),
			[]string{"height 1", "element 0"},
		},
		"high surrogate at end of tx id": {
			snapV1(`{"height":1,"hash":"h1","parent":"g","txs":["tx\uD83D"]}`),
			[]string{"height 1", "element 0"},
		},
		"invalid utf8 in v2 hash": {
			`{"version":2,"tip":1,"blocks":[` +
				`{"height":1,"hash":"h` + "\xff" + `1","parent":"g","txs":[],"timestamp":5}` +
				`]}`,
			[]string{`"hash"`, "height 1"},
		},
		"lone surrogate in v2 tx id": {
			`{"version":2,"tip":1,"blocks":[` +
				`{"height":1,"hash":"h1","parent":"g","txs":["a","\uD83Dx","b"],"timestamp":null}` +
				`]}`,
			[]string{"height 1", "element 1"},
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
					t.Fatalf("err=%v, want it to mention %q", err, want)
				}
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

func TestRestoreInvalidStringInLastBlockLeavesChainIntact(t *testing.T) {
	// The corrupt identifier sits in the last block and its txs field
	// precedes height in the object: the height must still be named, and
	// nothing of the snapshot may be applied.
	input := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"h1","parent":"g","txs":["ok"]},` +
		`{"txs":["ok","` + "\xff" + `"],"hash":"h2","parent":"h1","height":2}` +
		`]}`
	index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
	blocks, byHash, tip := snapshot(index)
	before := exportString(t, index)
	err := index.Restore(strings.NewReader(input))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
	}
	for _, want := range []string{"height 2", "element 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%v, want it to mention %q", err, want)
		}
	}
	requireUnchanged(t, index, blocks, byHash, tip)
	if after := exportString(t, index); after != before {
		t.Fatalf("export changed after rejected restore:\n%s\n%s", before, after)
	}
}

func TestRestoreAcceptsValidMultibyteIdentifiers(t *testing.T) {
	// Direct and escaped spellings of the same character restore to the same
	// identifier; a valid surrogate pair and a literal U+FFFD are accepted.
	direct := New()
	escaped := New()
	directSnap := snapV1(`{"height":1,"hash":"中文𝜜","parent":"g","txs":["中�𝜜"]}`)
	escapedSnap := snapV1(`{"height":1,"hash":"\u4e2d\u6587\ud835\udf1c","parent":"g","txs":["\u4e2d\ufffd\ud835\udf1c"]}`)
	if err := direct.Restore(strings.NewReader(directSnap)); err != nil {
		t.Fatalf("direct restore failed: %v", err)
	}
	if err := escaped.Restore(strings.NewReader(escapedSnap)); err != nil {
		t.Fatalf("escaped restore failed: %v", err)
	}
	if direct.Blocks[1].Hash != escaped.Blocks[1].Hash {
		t.Fatalf("direct vs escaped hash differ: %q vs %q",
			direct.Blocks[1].Hash, escaped.Blocks[1].Hash)
	}
	if direct.Blocks[1].Txs[0] != escaped.Blocks[1].Txs[0] {
		t.Fatalf("direct vs escaped tx differ: %q vs %q",
			direct.Blocks[1].Txs[0], escaped.Blocks[1].Txs[0])
	}
	if !strings.ContainsRune(direct.Blocks[1].Txs[0], '�') {
		t.Fatalf("literal U+FFFD not preserved: %q", direct.Blocks[1].Txs[0])
	}

	// Text that merely looks like an escape sequence stays ordinary text;
	// no normalization is applied to valid identifiers.
	index := New()
	snap := snapV1(`{"height":1,"hash":"h\\u0041  X","parent":"g","txs":[]}`)
	if err := index.Restore(strings.NewReader(snap)); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if want := `h\u0041  X`; index.Blocks[1].Hash != want {
		t.Fatalf("hash=%q, want the verbatim text %q", index.Blocks[1].Hash, want)
	}
}

func TestRestoreMultibyteSplitAcrossReads(t *testing.T) {
	// A valid multi-byte character delivered one byte per Read call is not
	// an encoding error.
	snap := snapV1(`{"height":1,"hash":"中","parent":"g","txs":["𝜜"]}`)
	index := New()
	if err := index.Restore(&byteReader{data: []byte(snap)}); err != nil {
		t.Fatalf("restore from byte-at-a-time reader failed: %v", err)
	}
	if index.Blocks[1].Hash != "中" || index.Blocks[1].Txs[0] != "𝜜" {
		t.Fatalf("restored identifiers: %q %q", index.Blocks[1].Hash, index.Blocks[1].Txs[0])
	}
}
