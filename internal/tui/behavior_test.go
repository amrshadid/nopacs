package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// TestRevokeNeedsTheKeyIDTyped guards "nothing destructive by one key" for
// keys: r opens a confirmation, enter on it with nothing or the wrong text
// sends nothing, and only the exact id revokes. It holds from the list and from
// the key's own screen, which shows the key revoked afterwards.
func TestRevokeNeedsTheKeyIDTyped(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) { addKeys(f, 2) })
		r.key("2", "r")
		r.expect("Revoke key k00000000100", "From this moment a request with it is refused", "Type the key id")
		r.key("enter")
		r.expect("not done: type k00000000100 exactly")
		r.typeText("k00000000101")
		r.key("enter")
		r.expect("not done: type k00000000100 exactly")
		r.key("ctrl+u")
		r.typeText("k00000000100")
		r.key("backspace")
		r.key("enter")
		if got := r.calls("RevokeKey"); got != 0 {
			t.Fatalf("RevokeKey was called %d times before the id was typed whole", got)
		}
		r.typeText("0")
		r.key("enter")
		if got := r.calls("RevokeKey"); got != 1 {
			t.Errorf("RevokeKey calls = %d, want 1", got)
		}
		r.expect("Keys  [2 rows]", "key k00000000100 (holder 0) revoked")
		var revoked int
		for _, l := range r.lines() {
			if strings.Contains(l, "k00000000100") && strings.Contains(l, "revoked") {
				revoked++
			}
		}
		if revoked == 0 {
			t.Errorf("the list does not show the key revoked:\n%s", r.screen())
		}

		// From the key's own screen, on the other key.
		r.key("down", "enter", "r")
		r.typeText("k00000000101")
		r.key("enter")
		r.expect("Key k00000000101", "Status:", "revoked")
	})
}

// TestRemoveTenantNeedsTheNameTyped guards the same rule for tenants, from the
// list and from the tenant's screen: the name must be typed whole, the screen
// says nothing is deleted, and afterwards the tenant is gone from the list.
func TestRemoveTenantNeedsTheNameTyped(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addTenants(f, 3) })
	r.key("1", "d")
	r.expect("Remove tenant t000", "Nothing is deleted: its database and files stay where they are", "Type the name")
	r.key("enter")
	r.expect("not done: type t000 exactly")
	r.typeText("t00")
	r.key("enter")
	if got := r.calls("RemoveTenant"); got != 0 {
		t.Fatalf("RemoveTenant was called %d times before the name was typed whole", got)
	}
	r.typeText("0")
	r.key("enter")
	if got := r.calls("RemoveTenant"); got != 1 {
		t.Errorf("RemoveTenant calls = %d, want 1", got)
	}
	r.expect("Tenants  [2 rows]", "tenant t000 unregistered; no data was deleted, it is at /var/lib")
	r.refuse("t000  ")

	// From the tenant's screen it goes back two screens, to the list.
	r.key("enter", "d")
	r.typeText("t001")
	r.key("enter")
	r.expect("Tenants  [1 row]", "t002")
	r.refuse("Tenant t001")
}

// TestFormsTakeLettersAsText guards that q and ? are letters while text is
// being typed, and keep their meaning elsewhere.
func TestFormsTakeLettersAsText(t *testing.T) {
	r := newRig(t, 80, 24, nil)
	r.key("2", "n")
	r.typeText("q?")
	r.expect("q?")
	if r.quit {
		t.Fatal("q quit the program while a holder was being typed")
	}
	r.key("esc")
	r.key("q")
	if !r.quit {
		t.Error("q did not quit from the key list")
	}
}

// TestSlowServerLeavesTheScreenResponsive guards the rule that a call never
// freezes the screen. The server takes two seconds to answer; while the call
// is out, a key press is handled in under 100 ms, the screen says it is
// waiting, and esc cancels the call well before the two seconds are up, with
// "canceled" and not an error.
func TestSlowServerLeavesTheScreenResponsive(t *testing.T) {
	r := newRigWith(t, 80, 24, rigOptions{lazy: true, setup: func(f *fakeserver.Server) {
		addTenants(f, 3)
		f.Delay = 2 * time.Second
	}})
	// Open the tenant list by hand: the call it starts is run beside this
	// test, as Bubble Tea runs a command, and not waited for.
	next, cmd := r.m.Update(keyMsg("enter"))
	r.m = next.(model)
	next, fetch := r.m.Update(cmd())
	r.m = next.(model)
	answers := make(chan tea.Msg, 1)
	started := time.Now()
	go func() { answers <- fetch() }()

	for _, key := range []string{"down", "j", "/", "x"} {
		begun := time.Now()
		next, _ := r.m.Update(keyMsg(key))
		if took := time.Since(begun); took > 100*time.Millisecond {
			t.Errorf("handling %q took %v while a call was out, want under 100ms", key, took)
		}
		r.m = next.(model)
		if key == "x" {
			r.expect("waiting for the server; esc cancels", "loading")
		}
	}
	// esc, even in the filter, first cancels the call the screen waits for.
	next, _ = r.m.Update(keyMsg("esc"))
	r.m = next.(model)
	select {
	case msg := <-answers:
		if took := time.Since(started); took > time.Second {
			t.Errorf("the call ended %v after it started: esc did not cancel it", took)
		}
		next, _ = r.m.Update(msg)
		r.m = next.(model)
	case <-time.After(1900 * time.Millisecond):
		t.Fatal("esc did not cancel the call: no answer before the server's two seconds")
	}
	r.expect("canceled", "could not load the list; ctrl+r tries again")
	r.refuse("request id", "waiting for the server")
	if r.m.c.waiting() || len(r.m.c.pending) != 0 {
		t.Errorf("a call is still pending after it was canceled: %v", r.m.c.pending)
	}
}

// TestCallHasTheCommandsDeadline guards that a call gives up at the client's
// deadline, as a command does, and says so in the command's own words on the
// error line, with the request id.
func TestCallHasTheCommandsDeadline(t *testing.T) {
	r := newRigWith(t, 200, 24, rigOptions{lazy: true, timeout: 200 * time.Millisecond, setup: func(f *fakeserver.Server) {
		addTenants(f, 1)
		f.Delay = 3 * time.Second
	}})
	r.key("enter")
	lines := r.lines()
	status := lines[len(lines)-2]
	// The request id is there when the server's answer headers arrived before
	// the deadline and absent when they did not; which one happens depends on
	// the system, so both are the command's own line.
	if !deadlineLine.MatchString(status) {
		t.Errorf("the deadline error line is %q", status)
	}
}

// deadlineLine is the line a call that ran out of time gives: the command's
// own words, with the request id when there is one.
var deadlineLine = regexp.MustCompile(`^tenant list: no answer from \S+ within 200ms \(see --timeout\)( \(request id req-\d+\))?$`)

// TestLeavingAScreenCancelsItsCalls guards that going back does not leave a
// call running for a screen that is gone, and that its late answer is dropped
// rather than given to the screen now on top.
func TestLeavingAScreenCancelsItsCalls(t *testing.T) {
	r := newRigWith(t, 80, 24, rigOptions{lazy: true, setup: func(f *fakeserver.Server) {
		addTenants(f, 1)
		f.Delay = time.Second
	}})
	next, cmd := r.m.Update(keyMsg("enter"))
	r.m = next.(model)
	next, fetch := r.m.Update(cmd())
	r.m = next.(model)
	gone := r.m.top().sid()
	answers := make(chan tea.Msg, 1)
	go func() { answers <- fetch() }()
	next, _ = r.m.Update(backMsg{})
	r.m = next.(model)
	msg := <-answers
	next, _ = r.m.Update(msg)
	r.m = next.(model)
	if r.m.top().sid() == gone || len(r.m.c.pending) != 0 {
		t.Errorf("the screen is still there or a call is pending: top %d, pending %v", r.m.top().sid(), r.m.c.pending)
	}
	r.expect("Menu")
	r.refuse("request id", "could not load")
}

// TestTickRefreshesOnlyTheTopList guards the timer of the tenant list: a tick
// fetches the first page again, and a tick that arrives while another screen is
// on top fetches nothing.
func TestTickRefreshesOnlyTheTopList(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addTenants(f, 3) })
	r.key("1")
	id := r.m.top().sid()
	if got := r.calls("ListTenants"); got != 1 {
		t.Fatalf("calls after opening = %d", got)
	}
	r.fake.AddTenant(&v1.Tenant{Name: "t999", DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE, State: v1.TenantState_TENANT_STATE_FAILED, StateReason: "disk full", CreateTime: timestamp(0)})
	// The tick re-arms itself; the test counts the timers and never waits.
	armed := 0
	r.m.c.refresh = time.Second
	r.m.c.after = func(int) tea.Cmd { armed++; return nil }
	r.send(tickMsg{to: id})
	if got := r.calls("ListTenants"); got != 2 {
		t.Errorf("calls after a tick = %d, want 2", got)
	}
	r.expect("Tenants  [4 rows]", "t999", "failed: disk full")
	if armed != 1 {
		t.Errorf("the tick armed %d timers, want 1", armed)
	}

	r.key("enter")
	r.send(tickMsg{to: id})
	if got := r.calls("ListTenants"); got != 2 {
		t.Errorf("a tick with another screen on top fetched: calls = %d, want 2", got)
	}
}
