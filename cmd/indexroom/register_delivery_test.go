package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// The tests in this file guard the register command's result-delivery
// contract at the process level: the command is built once and launched
// through its public entry point, so the assertions cover the writes the
// command process actually performs on standard output, its exit status and
// its standard-error diagnostic — the signals a caller piping the result
// relies on to tell a rejected request apart from an undelivered result.
// Everything runs offline against the in-memory registry; the instance
// addresses are never connected to.

var (
	registerBinOnce sync.Once
	registerBinPath string
	registerBinErr  error
)

// registerCommandBinary builds the command once per test run and returns the
// path of the executable the delivery tests launch as a real process.
func registerCommandBinary(t *testing.T) string {
	t.Helper()
	registerBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "indexroom-register-bin")
		if err != nil {
			registerBinErr = err
			return
		}
		registerBinPath = filepath.Join(dir, "indexroom")
		out, err := exec.Command("go", "build", "-o", registerBinPath, ".").CombinedOutput()
		if err != nil {
			registerBinErr = fmt.Errorf("building the register command: %v\n%s", err, out)
		}
	})
	if registerBinErr != nil {
		t.Fatal(registerBinErr)
	}
	return registerBinPath
}

// runRegisterProcess launches the register command with input on standard
// input, captures standard output and standard error, and reports the process
// exit code.
func runRegisterProcess(t *testing.T, bin, input string) (stdout, stderr string, exitCode int) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(bin, "register")
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), exitCodeOf(t, err)
}

// exitCodeOf converts a finished command's error into its exit code. A
// command terminated by a signal — on SIGPIPE platforms that is exactly the
// failure these tests guard against, since a SIGPIPE kill would deny the
// caller the promised exit status and stderr diagnostic — fails the test
// instead of masquerading as an exit status.
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("running the register command: %v", err)
	}
	code := exitErr.ExitCode()
	if code < 0 {
		t.Fatalf("register command was terminated by a signal instead of reporting via its exit status: %v", err)
	}
	return code
}

// assertDeliveryFailureDiagnostic checks the standard-error diagnostic
// promised when the JSON result cannot be delivered: it names the failed
// delivery and preserves the underlying write error, and it is never
// phrased as a request-level invalid or conflict rejection.
func assertDeliveryFailureDiagnostic(t *testing.T, stderr string) {
	t.Helper()
	if !strings.Contains(stderr, "failed to write request results to standard output") {
		t.Fatalf("stderr should diagnose the undelivered result, got %q", stderr)
	}
	// The underlying write error travels with the diagnostic so the caller
	// can tell a broken pipe from any other delivery problem.
	if runtime.GOOS != "windows" && !strings.Contains(stderr, "broken pipe") {
		t.Fatalf("diagnostic should carry the underlying pipe write error, got %q", stderr)
	}
	if strings.Contains(stderr, `"error":"invalid"`) || strings.Contains(stderr, `"error":"conflict"`) {
		t.Fatalf("delivery failure must not be misreported as a request rejection, got %q", stderr)
	}
}

// allSuccessDeliveryBatch is a batch in which every request succeeds, shared
// by the delivery tests so a non-zero exit or a stderr diagnostic can only
// come from result delivery, never from a rejected request.
const allSuccessDeliveryBatch = `{"requests":[
	{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i2","address":"h2:8080"},{"id":"i1","address":"h1:8080"}]},
	{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
	{"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":1,"healthy":true},
	{"type":"select","service":"svc","expectedRevision":1}
]}`

// TestRegisterProcessDeliversCompleteResult covers normal delivery through
// the public command entry: an all-success batch yields one complete JSON
// document with the per-item results and the final service list, exit status
// 0 and no delivery-failure diagnostic on standard error.
func TestRegisterProcessDeliversCompleteResult(t *testing.T) {
	bin := registerCommandBinary(t)
	out, stderr, code := runRegisterProcess(t, bin, allSuccessDeliveryBatch)
	if code != 0 {
		t.Fatalf("exit code: %d, stderr: %s", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("no delivery-failure diagnostic expected on standard error, got %q", stderr)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not one complete JSON document: %v\n%s", err, out)
	}
	if len(got.Results) != 4 {
		t.Fatalf("results: %+v", got.Results)
	}
	for i, r := range got.Results {
		if !r.OK {
			t.Fatalf("result %d should succeed: %+v", i, r)
		}
	}
	// The selection rotates to the smallest healthy instance id and reports
	// its address and accepted health sequence.
	if r := got.Results[3]; r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("select result: %+v", r)
	}
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 2 ||
		insts[0].ID != "i1" || insts[0].Health != "healthy" || insts[0].Sequence != 1 ||
		insts[1].ID != "i2" || insts[1].Health != "healthy" || insts[1].Sequence != 1 {
		t.Fatalf("instances: %+v", insts)
	}
}

// TestRegisterProcessRejectedRequestsAreNotDeliveryFailures covers a batch
// containing a rejected request: later requests still run, every result is
// delivered in input order inside the complete JSON document, the exit
// status is 1, and the rejection stays inside that request's result — it is
// not misreported as a standard-output write failure on standard error.
func TestRegisterProcessRejectedRequestsAreNotDeliveryFailures(t *testing.T) {
	bin := registerCommandBinary(t)
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":9,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, stderr, code := runRegisterProcess(t, bin, input)
	if code != 1 {
		t.Fatalf("batch contains a rejected request, exit code: %d, stderr: %s", code, stderr)
	}
	if strings.Contains(stderr, "failed to write") {
		t.Fatalf("a rejected request must not surface as a delivery failure, stderr: %q", stderr)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not one complete JSON document: %v\n%s", err, out)
	}
	// One result per request, in input order; the conflict keeps its slot and
	// the requests after it still ran.
	if len(got.Results) != 4 {
		t.Fatalf("results: %+v", got.Results)
	}
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	if r := got.Results[1]; r.OK || r.Error != "conflict" || !hasRevisionPair(r, 9, 1) || r.Revision != 1 {
		t.Fatalf("result 1 should carry the rejection in its own result: %+v", r)
	}
	if r := got.Results[2]; !r.OK || !r.Changed || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 2: %+v", r)
	}
	if r := got.Results[3]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 1 {
		t.Fatalf("result 3: %+v", r)
	}
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	if insts := got.Services[0].Instances; len(insts) != 1 || insts[0].ID != "i1" || insts[0].Health != "healthy" || insts[0].Sequence != 1 {
		t.Fatalf("instances: %+v", insts)
	}
}

// TestRegisterProcessClosedStdoutReportsDeliveryFailure closes the receiving
// end of the output pipe before the command can write: even though every
// request succeeds, the command must exit 1 and diagnose on standard error
// that the request results could not be written to standard output,
// preserving the underlying pipe error. On SIGPIPE platforms the process
// must not be killed by the signal — exitCodeOf rejects a signal death.
func TestRegisterProcessClosedStdoutReportsDeliveryFailure(t *testing.T) {
	bin := registerCommandBinary(t)

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	// The receiver is already gone before the command starts, so the result
	// write fails deterministically no matter how small the document is.
	readEnd.Close()

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "register")
	cmd.Stdin = strings.NewReader(allSuccessDeliveryBatch)
	cmd.Stdout = writeEnd
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	writeEnd.Close()

	if code := exitCodeOf(t, runErr); code != 1 {
		t.Fatalf("exit code with a closed stdout pipe: %d, stderr: %s", code, stderr.String())
	}
	assertDeliveryFailureDiagnostic(t, stderr.String())
}

// largeDeliveryBatch registers enough instances that the result document is
// far larger than any pipe buffer, so a receiver that closes the pipe after
// reading only the head leaves the command with a genuinely partial
// delivery: the write is still in progress when the pipe breaks.
func largeDeliveryBatch() string {
	var b strings.Builder
	b.WriteString(`{"requests":[{"type":"register","service":"svc","expectedRevision":0,"instances":[`)
	for i := 0; i < 20000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":"inst-%06d","address":"host-%06d.example:8080"}`, i, i)
	}
	b.WriteString(`]}]}`)
	return b.String()
}

// TestRegisterProcessPartialDeliveryReportsFailure has the receiver consume
// the head of the result document and then close the pipe mid-delivery. The
// command must report the delivery failure on standard error and exit 1;
// everything already sent must belong to the original result document — a
// strict prefix of the complete document with no diagnostic text mixed into
// standard output and no second error JSON or success trailer appended after
// the incomplete document.
func TestRegisterProcessPartialDeliveryReportsFailure(t *testing.T) {
	bin := registerCommandBinary(t)
	input := largeDeliveryBatch()

	// Reference run with an intact receiver: the complete result document the
	// partial delivery is compared against.
	full, fullStderr, code := runRegisterProcess(t, bin, input)
	if code != 0 {
		t.Fatalf("reference run: exit code %d, stderr %s", code, fullStderr)
	}
	if !json.Valid([]byte(full)) {
		t.Fatalf("reference output is not one complete JSON document")
	}
	if len(full) < 256*1024 {
		t.Fatalf("result document too small to exercise partial delivery: %d bytes", len(full))
	}

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "register")
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = writeEnd
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the register command: %v", err)
	}
	writeEnd.Close()

	// The receiver takes only the head of the document and closes the pipe
	// while the command is still writing the rest.
	received := make([]byte, 4096)
	if _, err := io.ReadFull(readEnd, received); err != nil {
		t.Fatalf("reading the delivered prefix: %v", err)
	}
	readEnd.Close()

	if code := exitCodeOf(t, cmd.Wait()); code != 1 {
		t.Fatalf("exit code after a partial delivery: %d, stderr: %s", code, stderr.String())
	}
	assertDeliveryFailureDiagnostic(t, stderr.String())

	// The delivered head is a strict prefix of the original result document:
	// no diagnostic text leaked into standard output, and nothing — no error
	// JSON, no success trailer — was appended after the incomplete document.
	if !strings.HasPrefix(full, string(received)) {
		t.Fatalf("delivered content is not a prefix of the result document:\n%q", received)
	}
	if strings.Contains(string(received), "failed to write") {
		t.Fatalf("diagnostic text leaked into standard output:\n%q", received)
	}
	if json.Valid(received) {
		t.Fatalf("the delivered prefix should be an incomplete JSON document")
	}
}
