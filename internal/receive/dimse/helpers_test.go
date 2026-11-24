package dimse_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/dimse"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// pythonEnv names the Python with pynetdicom the outside tests use; when it
// is not set, python3 on the PATH is tried.
const pythonEnv = "NOPACS_PYTHON"

// t0 is the time the rows of a test's receivers are made at.
var t0 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// row returns a valid DIMSE receiver that admits the caller SCANNER from
// anywhere, changed by each mod.
func row(mods ...func(*tenant.Receiver)) tenant.Receiver {
	r := tenant.Receiver{
		Name: "dimse", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 34,
		DIMSE:   tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", Callers: []tenant.CallerRule{{CallingAETitle: "SCANNER"}}},
		Created: t0, Updated: t0,
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

// kept is one file a fakeSink took.
type kept struct {
	arrival receive.Arrival
	body    []byte
	err     error
}

// fakeSink is a Sink that reads each body as the pipeline does (to its end
// or past the limit) and answers what it is told: it keeps a file only when
// its body ended cleanly and within the limit, as the pipeline's landing
// does.
type fakeSink struct {
	mu    sync.Mutex
	files []kept
	// answer, when set, is the outcome of every file, whatever its body.
	answer *receive.Outcome
	limit  int64
	// read is told the bytes read so far after each read, for a test that
	// acts in the middle of a body.
	read func(n int64)
}

// Accept reads a's body and answers.
func (s *fakeSink) Accept(_ context.Context, a receive.Arrival) receive.Outcome {
	if s.answer != nil {
		return *s.answer
	}
	limit := s.limit
	if limit == 0 {
		limit = 1 << 34
	}
	var buf bytes.Buffer
	var err error
	var n int64
	chunk := make([]byte, 64<<10)
	for n <= limit {
		m, rerr := a.Body.Read(chunk)
		buf.Write(chunk[:m])
		n += int64(m)
		if s.read != nil && m > 0 {
			s.read(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			err = rerr
			break
		}
	}
	a.Body = nil
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err != nil:
		s.files = append(s.files, kept{arrival: a, err: err})
		return receive.Outcome{Status: receive.Refused, Reason: "not kept"}
	case n > limit:
		s.files = append(s.files, kept{arrival: a, err: io.ErrShortBuffer})
		return receive.Outcome{Status: receive.Failed, Reason: "too large"}
	}
	s.files = append(s.files, kept{arrival: a, body: buf.Bytes()})
	return receive.Outcome{Status: receive.Received}
}

// whole returns the files the sink kept whole.
func (s *fakeSink) whole() []kept {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []kept
	for _, f := range s.files {
		if f.err == nil && f.body != nil {
			out = append(out, f)
		}
	}
	return out
}

// all returns every file the sink was handed.
func (s *fakeSink) all() []kept {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]kept(nil), s.files...)
}

// failure is one caller turned away, as Report was told.
type failure struct {
	failure tenant.AssociationFailure
	caller  tenant.GroupCaller
}

// reports records what a listener reports.
type reports struct {
	mu   sync.Mutex
	list []failure
}

func (r *reports) report(f tenant.AssociationFailure, c tenant.GroupCaller) {
	r.mu.Lock()
	r.list = append(r.list, failure{f, c})
	r.mu.Unlock()
}

func (r *reports) get() []failure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]failure(nil), r.list...)
}

// waitFor waits until cond holds, failing the test after a generous real
// deadline; it polls, never sleeping for a result.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// started is a listener a test runs.
type started struct {
	l       *dimse.Listener
	host    string
	port    string
	reports *reports
}

// listen starts a listener for r on a port the system picks, with sink and
// refusal (nil for none), and stops it at the test's end. It is skipped
// under -short, since it opens a port.
func listen(t *testing.T, r tenant.Receiver, sink receive.Sink, refusal dimse.Refusal, change ...func(*dimse.Config)) *started {
	t.Helper()
	return listenSeams(t, r, sink, refusal, nil, change...)
}

// listenSeams is listen with seams set on the listener before it starts,
// so the listener's goroutines never race the test that sets them.
func listenSeams(t *testing.T, r tenant.Receiver, sink receive.Sink, refusal dimse.Refusal, seams func(*dimse.Listener),
	change ...func(*dimse.Config)) *started {
	t.Helper()
	if testing.Short() {
		t.Skip("opens a port; left out under -short")
	}
	rep := &reports{}
	cfg := dimse.Config{Receiver: r, BindAddress: "127.0.0.1", Sink: sink, Refusal: refusal, Report: rep.report, Drain: 20 * time.Second}
	for _, c := range change {
		c(&cfg)
	}
	l, err := dimse.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dimse.SetAnyPort(l)
	if seams != nil {
		seams(l)
	}
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Stop(context.Background()) })
	host, port, err := net.SplitHostPort(l.Addr())
	if err != nil {
		t.Fatal(err)
	}
	return &started{l: l, host: host, port: port, reports: rep}
}

// dcmtkEnv names a directory holding dcmtk's programs, looked in before the
// PATH: another program of the same name (pynetdicom installs an echoscu
// and a storescu of its own) may come first on the PATH.
const dcmtkEnv = "NOPACS_DCMTK"

// tool returns the path of one of dcmtk's programs, or skips the test. A
// program of that name that is not dcmtk's is passed over.
func tool(t *testing.T, name string) string {
	t.Helper()
	var candidates []string
	if d := os.Getenv(dcmtkEnv); d != "" {
		candidates = append(candidates, filepath.Join(d, name))
	}
	if p, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), toolBound)
		out, _ := exec.CommandContext(ctx, p, "--version").CombinedOutput() // #nosec G204 G702 -- a test's own tool, from its PATH or its environment.
		cancel()
		if bytes.Contains(out, []byte("dcmtk")) {
			return p
		}
	}
	t.Skip("dcmtk's " + name + " is not installed; put it on the PATH or set " + dcmtkEnv)
	return ""
}

// pythonFound holds the answer of the first look for a Python with
// pynetdicom, so a package's tests ask the interpreter once.
var pythonFound struct {
	once sync.Once
	path string
	why  string
}

// python returns a Python that imports pynetdicom, or skips the test. A
// test calls it first, before it starts a listener or a goroutine: a skip
// from inside a goroutine does not stop the test, which then waits on a
// sender that never ran.
func python(t *testing.T) string {
	t.Helper()
	pythonFound.once.Do(func() {
		p := os.Getenv(pythonEnv)
		if p == "" {
			var err error
			if p, err = exec.LookPath("python3"); err != nil {
				pythonFound.why = "no python3, and " + pythonEnv + " is not set"
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), toolBound)
		defer cancel()
		if err := exec.CommandContext(ctx, p, "-c", "import pynetdicom, pydicom").Run(); err != nil { // #nosec G204 G702 -- the test's own interpreter, from its environment.
			pythonFound.why = "the Python at " + p + " has no pynetdicom; set " + pythonEnv
			return
		}
		pythonFound.path = p
	})
	if pythonFound.path == "" {
		t.Skip(pythonFound.why)
	}
	return pythonFound.path
}

// toolBound is the longest an outside program a test runs may take; one
// that hangs is killed and fails the test instead of holding it to the
// package's timeout.
const toolBound = 2 * time.Minute

// command is exec.Command bounded by toolBound: a program that hangs is
// killed and its test fails on the error instead of waiting forever.
func command(t testing.TB, name string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), toolBound)
	t.Cleanup(cancel)
	return exec.CommandContext(ctx, name, args...) // #nosec G204 -- a test's own tool and arguments.
}

// waitBound is the longest a test waits on a channel for something it
// started.
const waitBound = time.Minute

// within returns the value ch gives, failing the test with what it waited
// for when none comes within waitBound. It is called from the test's own
// goroutine only.
func within[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitBound):
		t.Fatalf("timed out after %v waiting for %s", waitBound, what)
	}
	var zero T
	return zero
}

// scuResult is what testdata/scu.py prints.
type scuResult struct {
	Assoc    string   `json:"assoc"`
	RJ       []int    `json:"rj"`
	Statuses []*int   `json:"statuses"`
	Accepted []string `json:"accepted_contexts"`
	Rejected []string `json:"rejected_contexts"`
	// After is what became of an association after garbage was sent on it:
	// aborted, open or closed.
	After string `json:"after"`
}

// scu runs testdata/scu.py with py (from python, called before the test
// started anything) and an action and its arguments against s.
func scu(t *testing.T, py string, s *started, action, calling, called string, args ...string) scuResult {
	t.Helper()
	r, err := runSCU(py, s, action, calling, called, args...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// runSCU is scu for a goroutine: it never touches the test, so a failure
// is returned for the test's goroutine to report.
func runSCU(py string, s *started, action, calling, called string, args ...string) (scuResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), toolBound)
	defer cancel()
	argv := append([]string{"-I", filepath.Join("testdata", "scu.py"), action, s.host, s.port, calling, called}, args...)
	out, err := exec.CommandContext(ctx, py, argv...).CombinedOutput() // #nosec G204 -- the test's own script and arguments.
	if err != nil {
		return scuResult{}, fmt.Errorf("scu.py %s: %w\n%s", action, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var r scuResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
		return scuResult{}, fmt.Errorf("scu.py %s printed %q", action, out)
	}
	return r, nil
}

// isStatus reports whether an answered status is want; a store answered
// nothing (its association aborted) is nil.
func isStatus(st *int, want int) bool { return st != nil && *st == want }

// corpusPath returns the path of a public test file of the pipeline.
func corpusPath(name string) string { return filepath.Join("..", "testdata", name) }

// corpusBytes reads a public test file.
func corpusBytes(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(corpusPath(name)) // #nosec G304 -- a test's own file names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dataSet returns the data set of a Part 10 file: what follows its meta
// group, whose length its first element gives (PS3.10 7.1).
func dataSet(t testing.TB, file []byte) []byte {
	t.Helper()
	const at = 128 + 4
	if len(file) < at+12 || string(file[128:132]) != "DICM" {
		t.Fatal("not a Part 10 file")
	}
	// (0002,0000) UL, explicit VR little endian: tag 4, VR 2, length 2,
	// value 4.
	n := int(file[at+8]) | int(file[at+9])<<8 | int(file[at+10])<<16 | int(file[at+11])<<24
	return file[at+12+n:]
}

// sum returns the SHA-256 of b.
func sum(b []byte) [32]byte { return sha256.Sum256(b) }
