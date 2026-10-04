package indexroom

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
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

// trailingFaultReader first streams the complete document prefix without
// error, then delivers the trailing bytes together with a fault on the next
// read, modelling storage failing exactly when the post-object bytes arrive.
type trailingFaultReader struct {
	head      []byte
	tail      []byte
	err       error
	stageRead bool
}

func (r *trailingFaultReader) Read(p []byte) (int, error) {
	if len(r.head) > 0 {
		n := copy(p, r.head)
		r.head = r.head[n:]
		return n, nil
	}
	if !r.stageRead {
		r.stageRead = true
		if len(r.tail) == 0 {
			return 0, r.err
		}
		n := copy(p, r.tail)
		r.tail = r.tail[n:]
		return n, r.err
	}
	return 0, io.EOF
}

// requireReadFailure asserts err is a read failure: it keeps the
// "indexroom: read snapshot:" prefix, identifies the underlying fault, and
// never also identifies as ErrInvalidSnapshot.
func requireReadFailure(t *testing.T, err error, fault error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v, must not match ErrInvalidSnapshot", err)
	}
	if !errors.Is(err, fault) {
		t.Fatalf("err=%v, want errors.Is %v", err, fault)
	}
	if !strings.HasPrefix(err.Error(), "indexroom: read snapshot:") {
		t.Fatalf("err=%q, want the read-snapshot prefix", err.Error())
	}
}

// contentErrorDocs each carry a top-level content error that can be decoded
// without consuming past their bytes: an unknown field, a duplicated field,
// or a non-object top-level value. The same documents end honestly in the
// clean-EOF case and must be rejected as invalid snapshots.
var contentErrorDocs = map[string]string{
	"unknown field": `{"version":1,"tip":1,` +
		`"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[]}],` +
		`"extra":1}`,
	"duplicate field": `{"version":1,"version":2,"tip":1,` +
		`"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[]}]}`,
	"top-level array": `[]`,
}

// TestRestoreContentErrorWithReadFaultReportsReadFailure verifies the
// precedence rule: when one read delivers bytes exposing a top-level content
// error and a read fault together, the already-received read failure ends the
// restore even though the same bytes are invalid content.
func TestRestoreContentErrorWithReadFaultReportsReadFailure(t *testing.T) {
	faults := map[string]error{
		"bare unexpected EOF":    io.ErrUnexpectedEOF,
		"wrapped unexpected EOF": fmt.Errorf("storage cut: %w", io.ErrUnexpectedEOF),
		"generic fault":          errStorageOffline,
	}
	for name, doc := range contentErrorDocs {
		for fname, fault := range faults {
			t.Run(name+" / "+fname, func(t *testing.T) {
				index := txChain(t, []string{"a"}, []string{"b"})
				blocks, byHash, tip := snapshot(index)
				err := index.Restore(&oneShotReader{data: []byte(doc), err: fault})
				requireReadFailure(t, err, fault)
				requireUnchanged(t, index, blocks, byHash, tip)
			})
		}
	}
}

// TestRestoreContentErrorCleanEOFIsInvalidSnapshot is the honest-end
// counterpart: the same invalid bytes read to a normal end are rejected with
// ErrInvalidSnapshot, naming the concrete content problem.
func TestRestoreContentErrorCleanEOFIsInvalidSnapshot(t *testing.T) {
	wantReason := map[string]string{
		"unknown field":   `unknown field "extra"`,
		"duplicate field": `duplicate field "version"`,
		"top-level array": "snapshot must be a single JSON object",
	}
	for name, doc := range contentErrorDocs {
		t.Run(name, func(t *testing.T) {
			index := txChain(t, []string{"a"}, []string{"b"})
			blocks, byHash, tip := snapshot(index)
			err := index.Restore(strings.NewReader(doc))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
			}
			if !strings.Contains(err.Error(), wantReason[name]) {
				t.Fatalf("err=%q, want it to mention %q", err.Error(), wantReason[name])
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

// TestRestoreCompleteSnapshotWithFaultReportedAtEnd tests that a complete,
// valid snapshot arriving in the same read as the fault is not applied: the
// stream failed, so the document cannot be known to be complete.
func TestRestoreCompleteSnapshotWithFaultReportedAtEnd(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	blocks, byHash, tip := snapshot(index)
	raw := exportString(t, index)
	for _, fault := range []error{
		io.ErrUnexpectedEOF,
		fmt.Errorf("storage cut: %w", io.ErrUnexpectedEOF),
		errStorageOffline,
	} {
		err := index.Restore(&oneShotReader{data: []byte(raw), err: fault})
		requireReadFailure(t, err, fault)
		requireUnchanged(t, index, blocks, byHash, tip)
	}
}

// TestRestoreTrailingDataWithFaultReportsReadFailure covers the post-object
// check: bytes after the object read together with a fault are a read
// failure, not merely trailing data.
func TestRestoreTrailingDataWithFaultReportsReadFailure(t *testing.T) {
	doc := `{"version":1,"tip":1,` +
		`"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[]}]}`
	for _, tail := range []string{"X", "123", `"y"`, "true "} {
		for _, fault := range []error{io.ErrUnexpectedEOF, errStorageOffline} {
			t.Run(tail, func(t *testing.T) {
				index := txChain(t, []string{"a"}, []string{"b"})
				blocks, byHash, tip := snapshot(index)
				r := &trailingFaultReader{head: []byte(doc), tail: []byte(tail), err: fault}
				err := index.Restore(r)
				requireReadFailure(t, err, fault)
				requireUnchanged(t, index, blocks, byHash, tip)
			})
		}
	}
}

// TestRestoreTrailingDataCleanEOFIsInvalidSnapshot is the honest-end
// counterpart for the post-object check.
func TestRestoreTrailingDataCleanEOFIsInvalidSnapshot(t *testing.T) {
	doc := `{"version":1,"tip":1,` +
		`"blocks":[{"height":1,"hash":"h1","parent":"g","txs":[]}]}`
	for _, tc := range []struct {
		tail       string
		wantReason string
	}{
		{"X", "invalid character"}, // malformed trailing bytes
		{"123", "trailing data after the snapshot object"},
		{`"y"`, "trailing data after the snapshot object"},
		{"true ", "trailing data after the snapshot object"},
	} {
		t.Run(tc.tail, func(t *testing.T) {
			err := (&Index{}).Restore(strings.NewReader(doc + tc.tail))
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("err=%q, want it to mention %q", err.Error(), tc.wantReason)
			}
		})
	}
}

// blockAfterReader delivers data without error, then blocks forever on every
// later read: there is no fault to discover past the document, so a parser
// that keeps reading to look for one would hang.
type blockAfterReader struct {
	data []byte
}

func (r *blockAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		select {}
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestRestoreContentErrorDoesNotReadAheadForFault checks the precedence is
// limited to faults already received: a content error found on honestly-read
// bytes rejects the snapshot without forcing further reads to hunt for a
// fault.
func TestRestoreContentErrorDoesNotReadAheadForFault(t *testing.T) {
	doc := contentErrorDocs["unknown field"]
	done := make(chan error, 1)
	go func() { done <- (&Index{}).Restore(&blockAfterReader{data: []byte(doc)}) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidSnapshot) {
			t.Fatalf("err=%v, want ErrInvalidSnapshot", err)
		}
		if !strings.Contains(err.Error(), `unknown field "extra"`) {
			t.Fatalf("err=%q, want unknown-field reason", err.Error())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Restore blocked reading ahead for a fault that never arrived")
	}
}
