package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// registerBin is the compiled command binary shared by the process-level
// delivery tests. These tests exercise the public command entry point: the
// register command must run as a real child process whose standard output is
// a genuine pipe, so that a broken or closing receiver surfaces exactly the
// way a caller driving the command from a shell pipeline would observe it.
// TestMain builds the binary once for the whole package.
var registerBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "indexroom-register-bin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "test binary dir: %v\n", err)
		os.Exit(1)
	}
	registerBin = filepath.Join(dir, "indexroom")
	build := exec.Command("go", "build", "-o", registerBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building test binary: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// deliverySuccessInput is an all-success batch: registration, two offline
// health observations and two rotating selections, none of which can fail.
const deliverySuccessInput = `{"requests":[
	{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"}]},
	{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
	{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
	{"type":"select","service":"svc","expectedRevision":1},
	{"type":"select","service":"svc","expectedRevision":1}
]}`

// runRegisterProcess launches the compiled register command from its public
// entry point with input on stdin and stdout as its standard output (an
// *os.File is handed to the child directly, so the process writes to a real
// pipe). It returns the process exit code and everything the process wrote to
// standard error. A process killed by a signal — in particular SIGPIPE —
// fails the test via assertRegisterNotSignaled instead of reporting an exit
// code, because that would deny the caller the contracted status and
// diagnostic.
func runRegisterProcess(t *testing.T, input string, stdout io.Writer) (int, string) {
	t.Helper()
	cmd := exec.Command(registerBin, "register")
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("running register process: %v", err)
	}
	assertRegisterNotSignaled(t, exitErr.ProcessState)
	return exitErr.ExitCode(), stderr.String()
}

// TestRegisterProcessAllSuccessExitZero locks the normal delivery contract at
// the process level: a well-formed batch whose requests all succeed yields one
// complete JSON document with the per-item results and the final service list,
// exit status 0, and no delivery-failure diagnostic on standard error.
func TestRegisterProcessAllSuccessExitZero(t *testing.T) {
	var stdout bytes.Buffer
	code, stderr := runRegisterProcess(t, deliverySuccessInput, &stdout)
	if code != 0 {
		t.Fatalf("all-success batch should exit 0, got %d (stderr: %q)", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("no delivery diagnostic is expected on standard error, got %q", stderr)
	}
	var got registerOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not one complete JSON document: %v\n%s", err, stdout.String())
	}
	if len(got.Results) != 5 {
		t.Fatalf("results: %+v", got.Results)
	}
	for i, r := range got.Results {
		if !r.OK {
			t.Fatalf("result %d should succeed: %+v", i, r)
		}
	}
	if r := got.Results[3]; r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("first select: %+v", r)
	}
	if r := got.Results[4]; r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 1 {
		t.Fatalf("second select: %+v", r)
	}
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 2 || insts[0].ID != "a" || insts[0].Health != "healthy" || insts[0].Sequence != 1 ||
		insts[1].ID != "b" || insts[1].Health != "healthy" || insts[1].Sequence != 1 {
		t.Fatalf("instances: %+v", insts)
	}
}

// TestRegisterProcessRejectedItemsExitOneCleanStderr locks the distinction
// between a rejected request and an undelivered result: a batch containing
// business-rule rejections still processes every later request, delivers the
// complete result document in input order, and exits 1 — with the failure
// reasons carried by the per-item results and standard error staying free of
// any delivery-failure diagnostic.
func TestRegisterProcessRejectedItemsExitOneCleanStderr(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h1:1"}]},
		{"type":"register","service":"svc","expectedRevision":7,"instances":[{"id":"a","address":"h1:1"}]},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"a","address":"bad-address"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	var stdout bytes.Buffer
	code, stderr := runRegisterProcess(t, input, &stdout)
	if code != 1 {
		t.Fatalf("batch with rejected items should exit 1, got %d (stderr: %q)", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("a rejected request is not a delivery failure; standard error must stay clean, got %q", stderr)
	}
	var got registerOutput
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not one complete JSON document: %v\n%s", err, stdout.String())
	}
	// Every request kept its slot, in input order, including the ones after
	// the rejections.
	if len(got.Results) != 5 {
		t.Fatalf("results: %+v", got.Results)
	}
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	if r := got.Results[1]; r.OK || r.Error != "conflict" || !hasRevisionPair(r, 7, 1) || r.Revision != 1 {
		t.Fatalf("result 1: %+v", r)
	}
	if r := got.Results[2]; r.OK || r.Error != "invalid" || r.Revision != 1 || !omitsRevisionPair(r) {
		t.Fatalf("result 2: %+v", r)
	}
	// The rejections did not abort the batch: the later health report and
	// selection still ran against the committed state.
	if r := got.Results[3]; !r.OK || !r.Changed || r.Sequence != 1 {
		t.Fatalf("result 3: %+v", r)
	}
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("result 4: %+v", r)
	}
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
}

// TestRegisterProcessClosedPipeDeliveryFailure covers the receiver closing
// the result pipe before the command writes: even though every request
// succeeds, the process must exit 1 and explain on standard error that the
// request results could not be written to standard output, preserving the
// underlying pipe error. On platforms with SIGPIPE the process must not be
// killed by the signal (asserted inside runRegisterProcess), and the delivery
// failure must not be dressed up as a business rejection.
func TestRegisterProcessClosedPipeDeliveryFailure(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// The receiver is gone before the command even starts.
	if err := readEnd.Close(); err != nil {
		t.Fatalf("close read end: %v", err)
	}
	code, stderr := runRegisterProcess(t, deliverySuccessInput, writeEnd)
	if err := writeEnd.Close(); err != nil {
		t.Fatalf("close write end: %v", err)
	}
	if code != 1 {
		t.Fatalf("undeliverable all-success batch should exit 1, got %d (stderr: %q)", code, stderr)
	}
	if !strings.Contains(stderr, "failed to write request results to standard output") {
		t.Fatalf("stderr should state that the request results could not be written to standard output, got %q", stderr)
	}
	if sigpipePlatform && !strings.Contains(stderr, "broken pipe") {
		t.Fatalf("stderr should preserve the underlying pipe write error, got %q", stderr)
	}
	// The delivery failure is independent of the request outcomes: it must
	// not masquerade as one of the business rejection kinds.
	for _, kind := range []string{"invalid", "conflict", "not_found", "no_healthy", "stale"} {
		if strings.Contains(stderr, kind) {
			t.Fatalf("delivery diagnostic must not look like a business %q rejection: %q", kind, stderr)
		}
	}
}

// TestRegisterProcessPartialDeliveryThenClosedPipe covers the receiver
// closing the pipe after only part of the result document arrived: the
// command must still report the delivery failure on standard error and exit
// 1, the bytes already sent must be nothing but a prefix of the original
// result document, and the command must neither mix its diagnostic into
// standard output nor append a second JSON document or a success-looking
// trailer after the truncated one.
func TestRegisterProcessPartialDeliveryThenClosedPipe(t *testing.T) {
	// The result document must comfortably exceed any default pipe buffer so
	// the write cannot complete before the receiver closes: 3000 instances
	// produce a services list of roughly 270 KB.
	input := largeRegisterInput(3000)

	// Reference run with an intact receiver: the complete document.
	var reference bytes.Buffer
	code, refStderr := runRegisterProcess(t, input, &reference)
	if code != 0 {
		t.Fatalf("reference run should exit 0, got %d (stderr: %q)", code, refStderr)
	}
	if len(reference.Bytes()) <= 128*1024 {
		t.Fatalf("reference document should exceed the pipe buffer, got %d bytes", reference.Len())
	}

	cmd := exec.Command(registerBin, "register")
	cmd.Stdin = strings.NewReader(input)
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	cmd.Stdout = writeEnd
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start register process: %v", err)
	}
	// The parent drops its copy of the write end; the child keeps its own.
	if err := writeEnd.Close(); err != nil {
		t.Fatalf("close write end: %v", err)
	}
	// Receive a part of the document, then close the pipe while the command
	// is still writing the rest.
	received := make([]byte, 8192)
	if _, err := io.ReadFull(readEnd, received); err != nil {
		t.Fatalf("reading the partial document: %v", err)
	}
	if err := readEnd.Close(); err != nil {
		t.Fatalf("close read end: %v", err)
	}
	waitErr := cmd.Wait()
	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("register process should exit with a delivery failure, got %v", waitErr)
	}
	assertRegisterNotSignaled(t, exitErr.ProcessState)
	if exitErr.ExitCode() != 1 {
		t.Fatalf("delivery failure after a partial write should exit 1, got %d", exitErr.ExitCode())
	}
	stderr := stderrBuf.String()
	if !strings.Contains(stderr, "failed to write request results to standard output") {
		t.Fatalf("stderr should state that the request results could not be written to standard output, got %q", stderr)
	}
	if sigpipePlatform && !strings.Contains(stderr, "broken pipe") {
		t.Fatalf("stderr should preserve the underlying pipe write error, got %q", stderr)
	}
	// What the receiver got is a strict prefix of the original result
	// document — nothing was appended after the truncation.
	if !strings.HasPrefix(reference.String(), string(received)) || len(received) >= reference.Len() {
		t.Fatalf("received bytes must be a strict prefix of the original document (got %d of %d bytes)",
			len(received), reference.Len())
	}
	// The diagnostic stayed on standard error; it never leaked into the
	// result stream.
	if strings.Contains(string(received), "register:") || strings.Contains(string(received), "failed to write") {
		t.Fatalf("delivery diagnostic must not be mixed into standard output: %q", received)
	}
	// The truncated stream is not a complete JSON document, so no receiver
	// can mistake it (or anything appended to it) for the full result.
	if json.Valid(received) {
		t.Fatalf("partial document must not parse as a complete result: %q", received)
	}
}

// largeRegisterInput builds a single-request batch registering n instances,
// whose result document is large enough to exceed a pipe's buffer.
func largeRegisterInput(n int) string {
	var b strings.Builder
	b.WriteString(`{"requests":[{"type":"register","service":"svc","expectedRevision":0,"instances":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"inst-%04d","address":"host-%04d:8080"}`, i, i)
	}
	b.WriteString(`]}]}`)
	return b.String()
}
