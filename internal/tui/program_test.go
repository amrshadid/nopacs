package tui

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// The sequences a terminal is told when the program enters and leaves the
// alternate screen and hides and shows the cursor.
const (
	enterAlt   = "\x1b[?1049h"
	leaveAlt   = "\x1b[?1049l"
	showCursor = "\x1b[?25h"
)

// programWait is how long a test waits for the real program to draw or to end.
// It is long because the whole suite runs under the race detector on shared CI
// machines beside database tests, where a first call can take seconds; a
// program that works never uses it.
const programWait = 30 * time.Second

// syncBuffer is a writer the program writes to from its own goroutines and the
// test reads from.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends to the buffer.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// String is what was written so far.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// bomb is the root model with a timer that panics inside Update a moment after
// the program has drawn, to show that the terminal is restored when the program
// dies. The panic is in the program's own update loop, where a screen's would
// be.
type bomb struct{ model }

// boomMsg is what the timer sends.
type boomMsg struct{}

// Init starts the model and the timer.
func (b bomb) Init() tea.Cmd {
	return tea.Batch(b.model.Init(), tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return boomMsg{} }))
}

// Update panics on the timer's message and otherwise is the model.
func (b bomb) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(boomMsg); ok {
		panic("a screen failed")
	}
	next, cmd := b.model.Update(msg)
	b.model = next.(model)
	return b, cmd
}

// View is the model's.
func (b bomb) View() tea.View { return b.model.View() }

// programRig runs a real Bubble Tea program on a pipe, with a fake server
// behind it, and reads what it writes to its output.
type programRig struct {
	t    *testing.T
	out  *syncBuffer
	in   *os.File
	done chan error
	opts Options
}

// startProgram runs the program on pipes at 80 by 24 and returns when it has
// drawn the first frame.
func startProgram(t *testing.T, panics bool) *programRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Bubble Tea reads a Windows console, not a pipe, in the ways this
		// test needs; the real terminal on Windows is not testable in CI.
		t.Skip("the program is run on pipes only on Linux and macOS")
	}
	fake := fakeserver.New()
	fake.Now = func() time.Time { return fixedNow }
	addTenants(fake, 3)
	dir := fake.ServeUnix(t)
	cl, err := client.New(client.Config{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
	p := &programRig{t: t, out: &syncBuffer{}, in: pw, done: make(chan error, 1)}
	p.opts = Options{Client: cl, Where: client.SocketPath(dir), Input: pr, Output: p.out, Width: 80, Height: 24,
		Now: func() time.Time { return fixedNow }, Location: zone}
	if panics {
		// A program that ends by itself needs no input, and with none there
		// is no input reader to be closed while it is still reading.
		p.opts.noInput = true
	}
	m := newModel(context.Background(), p.opts)
	var run tea.Model = m
	if panics {
		run = bomb{m}
	}
	go func() { p.done <- runProgram(run, m.c, p.opts) }()
	// The first frame is drawn whole, so its words are in the output in one
	// piece; later frames are drawn by differences, and a word that changed
	// may be split by cursor movements, so nothing later is looked for in the
	// raw output. scripts/check/tui.py reads the screen through an emulator.
	p.waitFor("Menu")
	return p
}

// waitFor waits until the output holds text.
func (p *programRig) waitFor(text string) {
	p.t.Helper()
	deadline := time.Now().Add(programWait)
	for !strings.Contains(p.out.String(), text) {
		if time.Now().After(deadline) {
			p.t.Fatalf("the program never drew %q; it wrote %q", text, p.out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// press writes keys to the program's input.
func (p *programRig) press(keys string) {
	p.t.Helper()
	if _, err := p.in.WriteString(keys); err != nil {
		p.t.Fatalf("writing to the program's input: %v", err)
	}
}

// finish waits for the program to end and returns what Run returned.
func (p *programRig) finish() error {
	p.t.Helper()
	select {
	case err := <-p.done:
		return err
	case <-time.After(programWait):
		p.t.Fatalf("the program did not end; it wrote %q", p.out.String())
		return nil
	}
}

// restored checks the terminal was left as it was found: the alternate screen
// entered and then left, and the cursor shown after the last time it was
// hidden.
func (p *programRig) restored() {
	p.t.Helper()
	out := p.out.String()
	if !strings.Contains(out, enterAlt) {
		p.t.Errorf("the program never entered the alternate screen: %q", out)
	}
	if !strings.Contains(out, leaveAlt) || strings.LastIndex(out, leaveAlt) < strings.LastIndex(out, enterAlt) {
		p.t.Errorf("the alternate screen was not left last: %q", out)
	}
	if !strings.Contains(out[strings.LastIndex(out, enterAlt):], showCursor) {
		p.t.Errorf("the cursor was not shown again: %q", out)
	}
}

// TestProgramQuitsAndRestoresTheTerminal guards leaving on q: the real
// program, on a pipe, draws the menu, is given a key that opens a list and then
// q, and has told the terminal to leave the alternate screen and show the
// cursor.
func TestProgramQuitsAndRestoresTheTerminal(t *testing.T) {
	p := startProgram(t, false)
	p.press("1q")
	if err := p.finish(); err != nil {
		t.Fatalf("Run returned %v after q", err)
	}
	p.restored()
}

// TestProgramRestoresTheTerminalOnAPanic guards leaving on a panic: a panic in
// the update loop ends the program with an error, and the terminal has been
// restored by then, so the person's shell is not left in the alternate screen
// with no cursor.
func TestProgramRestoresTheTerminalOnAPanic(t *testing.T) {
	p := startProgram(t, true)
	err := p.finish()
	if err == nil || fault.KindOf(err) != fault.Internal || !strings.Contains(err.Error(), "internal error; the terminal was restored") {
		t.Errorf("Run returned %v after a panic, want an internal error", err)
	}
	p.restored()
}

// TestProgramQuitsOnCtrlC guards leaving on ctrl+c, which in the terminal's
// raw mode arrives as a key; a signal sent to the process is shown by the
// pseudo-terminal check.
func TestProgramQuitsOnCtrlC(t *testing.T) {
	p := startProgram(t, false)
	// ctrl+c in the program's raw mode is a key, and quits.
	p.press("\x03")
	if err := p.finish(); err != nil {
		t.Errorf("Run returned %v after ctrl+c, want nil", err)
	}
	p.restored()
}

// TestIsTerminalIsNotFooledByCharacterDevices guards the test for a terminal:
// /dev/null is a character device and no terminal, and neither is a pipe, a
// buffer or nothing. (A real terminal is shown by the pseudo-terminal check,
// scripts/check/tui.py.)
func TestIsTerminalIsNotFooledByCharacterDevices(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close(); _ = pw.Close() }()
	for name, v := range map[string]any{"/dev/null": null, "a pipe reader": pr, "a pipe writer": pw, "a buffer": &bytes.Buffer{}, "nil": nil} {
		if IsTerminal(v) {
			t.Errorf("IsTerminal(%s) is true", name)
		}
	}
}
