package indexroom

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// oneShotReader delivers all of data together with err on the first Read and
// clean io.EOF forever after, modelling a Reader whose single read returns
// valid bytes and a fault at once.
type oneShotReader struct {
	done bool
	data []byte
	err  error
}

func (r *oneShotReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	return n, r.err
}

// halfSnapshot is a syntactically valid prefix that cannot be a complete
// snapshot; the same bytes end normally in one case and with a fault in the
// other.
const halfSnapshot = `{"version":1,"tip":2,"blocks":[` +
	`{"height":1,"hash":"h1","parent":"g","txs":[]},` +
	`{"height":2,"hash":"h2","parent":"h1","txs":`

// errStorageOffline is a non-EOF read fault unrelated to JSON shape.
var errStorageOffline = errors.New("storage offline")

func TestRestoreUnexpectedEOFIsReadFailure(t *testing.T) {
	cases := map[string]struct {
		r       io.Reader
		wantErr error
	}{
		"bare ErrUnexpectedEOF after prefix": {
			&errReader{data: []byte(halfSnapshot), err: io.ErrUnexpectedEOF},
			io.ErrUnexpectedEOF,
		},
		"wrapped ErrUnexpectedEOF after prefix": {
			&errReader{data: []byte(halfSnapshot), err: fmt.Errorf("storage cut: %w", io.ErrUnexpectedEOF)},
			io.ErrUnexpectedEOF,
		},
		"bytes and bare ErrUnexpectedEOF in one read": {
			&oneShotReader{data: []byte(halfSnapshot), err: io.ErrUnexpectedEOF},
			io.ErrUnexpectedEOF,
		},
		"bytes and wrapped ErrUnexpectedEOF in one read": {
			&oneShotReader{data: []byte(halfSnapshot), err: fmt.Errorf("storage cut: %w", io.ErrUnexpectedEOF)},
			io.ErrUnexpectedEOF,
		},
		"fault in one read at a complete token boundary": {
			&oneShotReader{data: []byte(`{"version":1,`), err: io.ErrUnexpectedEOF},
			io.ErrUnexpectedEOF,
		},
		"empty read returning ErrUnexpectedEOF": {
			&oneShotReader{err: io.ErrUnexpectedEOF},
			io.ErrUnexpectedEOF,
		},
		"bytes and a generic fault in one read": {
			&oneShotReader{data: []byte(halfSnapshot), err: errStorageOffline},
			errStorageOffline,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(tc.r)
			if errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, must not match ErrInvalidSnapshot", err)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v, want errors.Is %v", err, tc.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
				t.Fatalf("err=%q, want the read-snapshot prefix", err.Error())
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

// A pipe cut off with an error is a read failure, unlike a pipe closed
// normally after a prefix (which is an honestly truncated snapshot).
func TestRestorePipeClosedWithErrorIsReadFailure(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	blocks, byHash, tip := snapshot(index)
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- index.Restore(pr) }()
	if _, err := pw.Write([]byte(halfSnapshot)); err != nil {
		t.Fatal(err)
	}
	if err := pw.CloseWithError(io.ErrUnexpectedEOF); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, must not be ErrInvalidSnapshot", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v, want errors.Is ErrUnexpectedEOF", err)
	}
	if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
		t.Fatalf("err=%q, want the read-snapshot prefix", err.Error())
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestRestoreSameHalfBytesCleanEOFVsFault(t *testing.T) {
	// Normal end after the same half document: truncated content, not a read
	// failure.
	index := txChain(t, []string{"a"}, []string{"b"})
	err := index.Restore(strings.NewReader(halfSnapshot))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("clean EOF: err=%v, want ErrInvalidSnapshot", err)
	}

	// Read failure on the same bytes: a different error category.
	index = txChain(t, []string{"a"}, []string{"b"})
	err = index.Restore(&oneShotReader{data: []byte(halfSnapshot), err: io.ErrUnexpectedEOF})
	if errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("fault: err=%v, must not be ErrInvalidSnapshot", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("fault: err=%v, want errors.Is ErrUnexpectedEOF", err)
	}
}

func TestRestoreCompleteDocArrivingWithFaultIsReadFailure(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	blocks, byHash, tip := snapshot(index)
	raw := exportString(t, index)
	// The single read hands over a whole, valid snapshot together with the
	// fault and never signals a clean EOF: the stream failed, so Restore must
	// not apply it.
	err := index.Restore(&oneShotReader{data: []byte(raw), err: io.ErrUnexpectedEOF})
	if errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, must not be ErrInvalidSnapshot", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err=%v, want errors.Is ErrUnexpectedEOF", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestRestoreReadFailureKeepsChainAndCursor(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.NextCursor
	if cursor == "" {
		t.Fatal("expected a continuation cursor")
	}
	blocks, byHash, tip := snapshot(index)

	err = index.Restore(&oneShotReader{data: []byte(halfSnapshot), err: io.ErrUnexpectedEOF})
	if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want a read failure distinct from ErrInvalidSnapshot", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)

	// The pre-failure cursor keeps paging the original chain; the failed read
	// must not be reported as ErrQueryChanged.
	pages := collectPages(t, index, TxQuery{PageSize: 1, Cursor: cursor})
	var all []TxHit
	for _, page := range pages {
		all = append(all, page.Hits...)
	}
	want := []TxHit{
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 0},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("hits after read failure=%v, want %v", all, want)
	}
}
