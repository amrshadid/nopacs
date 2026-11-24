package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// testSettings is the list of settings of the tests' fake: the server's own,
// with one a tenant may override, which the server's list does not have yet.
// It is made here, in the tests, and never ships.
func testSettings() *settings.Table {
	period := func(v string) error {
		_, err := settings.ParseDuration(v)
		return err
	}
	t, err := settings.New(append(settings.Builtin(),
		settings.Definition{Name: "test.retention", Default: "365d", TenantOverride: true, Validate: period})...)
	if err != nil {
		panic(err)
	}
	return t
}

// fixedNow is the clock of every test, so times in a view are the same on every
// run.
var fixedNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// zone is the zone times are shown in: not UTC, so a test that forgot to
// convert would show a different hour.
var zone = time.FixedZone("EAT", 3*60*60)

// testSecret is the secret the fake makes for the first key it creates; the
// fake builds it from the key's id, and the tests look for it everywhere.
const testSecret = "nopacs_k00000000001_SSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSSS"

// rig drives the root model with no terminal: key presses and the answers of a
// real fake server go in, frames come out. Every command a screen returns is
// run to the end, as Bubble Tea would, except that it happens inside send, so a
// test reads the screen after the whole chain has settled.
type rig struct {
	t      *testing.T
	fake   *fakeserver.Server
	m      model
	w, h   int
	quit   bool
	frames []string
	// tops records, for each frame, the type of the screen that was on top.
	tops []string
}

// rigOptions are the few things a test changes about the rig.
type rigOptions struct {
	setup   func(*fakeserver.Server)
	timeout time.Duration
	// lazy leaves the model's first command unrun, for a test that runs it
	// itself.
	lazy bool
}

// newRig serves a fake on a unix socket, makes a client for it and a model of
// the given size, runs the model's first command, and checks the first frame.
func newRig(t *testing.T, w, h int, setup func(*fakeserver.Server)) *rig {
	t.Helper()
	return newRigWith(t, w, h, rigOptions{setup: setup})
}

// newRigWith is newRig with options.
func newRigWith(t *testing.T, w, h int, o rigOptions) *rig {
	t.Helper()
	fake := fakeserver.New()
	fake.Now = func() time.Time { return fixedNow }
	fake.Settings = testSettings()
	if o.setup != nil {
		o.setup(fake)
	}
	dir := fake.ServeUnix(t)
	cl, err := client.New(client.Config{DataDir: dir, Timeout: o.timeout})
	if err != nil {
		t.Fatalf("making a client: %v", err)
	}
	t.Cleanup(cl.Close)
	m := newModel(context.Background(), Options{
		Client: cl, Where: client.SocketPath(dir), Now: func() time.Time { return fixedNow },
		Location: zone, NoRefresh: true,
	})
	r := &rig{t: t, fake: fake, m: m, w: w, h: h}
	r.send(tea.WindowSizeMsg{Width: w, Height: h})
	if !o.lazy {
		r.exec(m.Init())
	}
	r.record()
	return r
}

// send gives the model a message, runs what it asks for until nothing is left,
// and checks the frame it ends on.
func (r *rig) send(msg tea.Msg) {
	r.t.Helper()
	var cmd tea.Cmd
	var next tea.Model
	next, cmd = r.m.Update(msg)
	r.m = next.(model)
	r.exec(cmd)
	r.record()
}

// exec runs a command and feeds its message back, depth first.
func (r *rig) exec(cmd tea.Cmd) {
	r.t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			r.exec(c)
		}
	case tea.QuitMsg:
		r.quit = true
	default:
		var c tea.Cmd
		var next tea.Model
		next, c = r.m.Update(msg)
		r.m = next.(model)
		r.exec(c)
	}
}

// record keeps the current frame and checks that it fits the terminal: no more
// lines than rows, no line wider than the columns.
func (r *rig) record() {
	r.t.Helper()
	frame := r.m.View().Content
	checkFrame(r.t, frame, r.m.c.w, r.m.c.h)
	r.frames = append(r.frames, frame)
	r.tops = append(r.tops, fmt.Sprintf("%T", r.m.top()))
}

// checkFrame fails the test when a frame does not fit w columns by h rows.
func checkFrame(t *testing.T, frame string, w, h int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) > h {
		t.Errorf("a frame has %d lines for a terminal of %d rows:\n%s", len(lines), h, frame)
	}
	for _, l := range lines {
		if got := width(l); got > w {
			t.Errorf("a line is %d cells wide for a terminal of %d columns: %q", got, w, l)
		}
	}
}

// key sends one key by name: "enter", "esc", "down", "ctrl+c", "space", or a
// single character.
func (r *rig) key(names ...string) {
	r.t.Helper()
	for _, n := range names {
		r.send(keyMsg(n))
	}
}

// typeText sends each character of s as its own key.
func (r *rig) typeText(s string) {
	r.t.Helper()
	for _, ch := range s {
		r.send(keyMsg(string(ch)))
	}
}

// keyMsg builds the key press a terminal would send for a name.
func keyMsg(name string) tea.KeyPressMsg {
	special := map[string]rune{
		"enter": tea.KeyEnter, "esc": tea.KeyEscape, "up": tea.KeyUp, "down": tea.KeyDown,
		"left": tea.KeyLeft, "right": tea.KeyRight, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown,
		"home": tea.KeyHome, "end": tea.KeyEnd, "tab": tea.KeyTab, "backspace": tea.KeyBackspace,
		"delete": tea.KeyDelete,
	}
	switch {
	case name == "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case name == "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case strings.HasPrefix(name, "ctrl+"):
		return tea.KeyPressMsg{Code: rune(name[len("ctrl+")]), Mod: tea.ModCtrl}
	}
	if code, ok := special[name]; ok {
		return tea.KeyPressMsg{Code: code}
	}
	r := []rune(name)
	return tea.KeyPressMsg{Code: r[0], Text: name}
}

// screen is the current frame.
func (r *rig) screen() string { return r.m.View().Content }

// lines is the current frame split into lines.
func (r *rig) lines() []string { return strings.Split(r.screen(), "\n") }

// expect fails the test unless the current frame holds every text.
func (r *rig) expect(texts ...string) {
	r.t.Helper()
	s := r.screen()
	for _, want := range texts {
		if !strings.Contains(s, want) {
			r.t.Errorf("the screen does not show %q:\n%s", want, s)
		}
	}
}

// refuse fails the test if the current frame holds any of the texts.
func (r *rig) refuse(texts ...string) {
	r.t.Helper()
	s := r.screen()
	for _, bad := range texts {
		if strings.Contains(s, bad) {
			r.t.Errorf("the screen shows %q and must not:\n%s", bad, s)
		}
	}
}

// calls counts the requests the server has seen for a method, by name.
func (r *rig) calls(method string) int {
	n := 0
	for _, s := range r.fake.SeenRequests() {
		if strings.HasSuffix(s.URL, "/"+method) {
			n++
		}
	}
	return n
}

// sizes are the two terminals every screen is tested at.
var sizes = []struct{ w, h int }{{80, 24}, {200, 50}}

// forSizes runs a test body at both sizes.
func forSizes(t *testing.T, body func(t *testing.T, w, h int)) {
	t.Helper()
	for _, s := range sizes {
		t.Run(fmt.Sprintf("%dx%d", s.w, s.h), func(t *testing.T) { body(t, s.w, s.h) })
	}
}

// addTenants puts n ready tenants named t000, t001, ... in the fake.
func addTenants(f *fakeserver.Server, n int) {
	for i := range n {
		f.AddTenant(&v1.Tenant{
			Name: fmt.Sprintf("t%03d", i), DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE,
			State: v1.TenantState_TENANT_STATE_READY, CreateTime: timestamppb.New(fixedNow.Add(-time.Duration(i) * time.Hour)),
		})
	}
}

// addKeys puts n keys with ids k00000000100, ... in the fake.
func addKeys(f *fakeserver.Server, n int) {
	for i := range n {
		f.AddKey(&v1.Key{
			Id: fmt.Sprintf("k%011d", 100+i), Holder: fmt.Sprintf("holder %d", i),
			CreateTime: timestamp(-i), ExpireTime: timestamp(24 * 30),
		})
	}
}

// sizeMsg is the message a terminal sends when it is resized.
func sizeMsg(w, h int) tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} }

// timestamp is the fixed time moved by hours.
func timestamp(hours int) *timestamppb.Timestamp {
	return timestamppb.New(fixedNow.Add(time.Duration(hours) * time.Hour))
}
