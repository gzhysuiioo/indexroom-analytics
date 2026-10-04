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

// scriptReader delivers each chunk in order, one Read per chunk, pairing the
// chunk's bytes with its error exactly as an underlying stream might; once
// the chunks run out it ends normally.
type scriptReader struct {
	chunks []scriptChunk
}

type scriptChunk struct {
	data string
	err  error
}

func (r *scriptReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	n := copy(p, chunk.data)
	return n, chunk.err
}

// A content error found in bytes that arrived together with a read fault is
// still a read failure: the stream failed, so the bytes cannot be trusted to
// be the whole document. The same bytes read to a clean end stay content
// errors.
func TestRestoreContentErrorWithFaultIsReadFailure(t *testing.T) {
	cases := map[string]string{
		"unknown top-level field":   `{"version":1,"tip":0,"blocks":[],"extra":0}`,
		"duplicate top-level field": `{"version":1,"version":1,"tip":0,"blocks":[]}`,
		"top level not an object":   `["version",1]`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(&oneShotReader{data: []byte(doc), err: errStorageOffline})
			if errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, must not match ErrInvalidSnapshot", err)
			}
			if !errors.Is(err, errStorageOffline) {
				t.Fatalf("err=%v, want errors.Is errStorageOffline", err)
			}
			if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
				t.Fatalf("err=%q, want the read-snapshot prefix", err.Error())
			}
			requireUnchanged(t, index, blocks, byHash, tip)

			// The same bytes delivered to a clean end report the content
			// error instead.
			index = txChain(t, []string{"a"}, []string{"b"})
			err = index.Restore(strings.NewReader(doc))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("clean EOF: err=%v, want ErrInvalidSnapshot", err)
			}
		})
	}
}

// Trailing data after the object is a read failure when the read that fetched
// those bytes already reported the fault, whether the fault arrived with the
// object's final bytes or only with the trailing ones.
func TestRestoreTrailingDataWithFaultIsReadFailure(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	raw := exportString(t, index)
	cases := map[string]io.Reader{
		"fault with the trailing bytes": &scriptReader{chunks: []scriptChunk{
			{data: raw},
			{data: " 2", err: io.ErrUnexpectedEOF},
		}},
		"fault with the whole stream": &oneShotReader{
			data: []byte(raw + " 2"),
			err:  errStorageOffline,
		},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(r)
			if errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, must not match ErrInvalidSnapshot", err)
			}
			if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
				t.Fatalf("err=%q, want the read-snapshot prefix", err.Error())
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}

	// The same trailing bytes read to a clean end stay a content error.
	index = txChain(t, []string{"a"}, []string{"b"})
	if err := index.Restore(strings.NewReader(raw + " 2")); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("clean EOF: err=%v, want ErrInvalidSnapshot", err)
	}
}

// Only a fault already received wins. A content error found before the reader
// has reported any failure rejects the snapshot as invalid right away, without
// reading ahead to look for a fault.
func TestRestoreContentErrorBeforeFaultStaysInvalid(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	blocks, byHash, tip := snapshot(index)
	err := index.Restore(&scriptReader{chunks: []scriptChunk{
		{data: `{"version":1,"extra":0,"tip":0,"blocks":[]}`},
		{err: errStorageOffline},
	}})
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
	}
	if errors.Is(err, errStorageOffline) {
		t.Fatalf("err=%v, must not read ahead into the fault", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

// Only an io.EOF returned directly by the reader ends the stream normally.
// An io.EOF wrapped with extra detail, or joined together with a storage
// fault, is a read failure — whether it arrives on its own, together with a
// complete valid snapshot, or together with half of one.
func TestRestoreJoinedOrWrappedEOFIsReadFailure(t *testing.T) {
	src := txChain(t, []string{"x"})
	raw := exportString(t, src)
	joined := errors.Join(io.EOF, errStorageOffline)
	cases := map[string]struct {
		r       io.Reader
		wantErr []error
	}{
		"complete snapshot with joined EOF and fault": {
			r:       &oneShotReader{data: []byte(raw), err: joined},
			wantErr: []error{io.EOF, errStorageOffline},
		},
		"half snapshot with joined EOF and fault": {
			r:       &oneShotReader{data: []byte(halfSnapshot), err: joined},
			wantErr: []error{io.EOF, errStorageOffline},
		},
		"joined EOF and fault with no bytes": {
			r:       &oneShotReader{err: joined},
			wantErr: []error{io.EOF, errStorageOffline},
		},
		"complete snapshot with wrapped EOF": {
			r:       &oneShotReader{data: []byte(raw), err: fmt.Errorf("storage cut: %w", io.EOF)},
			wantErr: []error{io.EOF},
		},
		"fault followed by a clean EOF stays a failure": {
			r: &scriptReader{chunks: []scriptChunk{
				{data: raw, err: joined},
			}},
			wantErr: []error{io.EOF, errStorageOffline},
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
			for _, want := range tc.wantErr {
				if !errors.Is(err, want) {
					t.Fatalf("err=%v, want errors.Is %v", err, want)
				}
			}
			if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
				t.Fatalf("err=%q, want the read-snapshot prefix", err.Error())
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

// A failed restore never shortens the chain first: a one-block snapshot whose
// final bytes arrive with a joined read fault leaves the three-block chain
// exactly as it was.
func TestRestoreJoinedFaultDoesNotShortenChain(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
	blocks, byHash, tip := snapshot(index)
	one := txChain(t, []string{"z"})
	raw := exportString(t, one)
	err := index.Restore(&oneShotReader{
		data: []byte(raw),
		err:  errors.Join(io.EOF, errStorageOffline),
	})
	if !errors.Is(err, errStorageOffline) || errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, want a read failure distinct from ErrInvalidSnapshot", err)
	}
	requireUnchanged(t, index, blocks, byHash, tip)
	if index.Tip != 3 {
		t.Fatalf("tip=%d, want the original 3", index.Tip)
	}
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
