package tui

import (
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// TestMenuAndHeader guards the first screen: the header says who is connected
// and where to (from WhoAmI and the client's target), and the menu names every
// screen the program has, at both terminal sizes, in a frame that fits.
func TestMenuAndHeader(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, nil)
		r.expect("nopacs | local operator (user 501) | ", "Menu", "Tenants", "Keys", "Settings", "Audit log", "q quit")
		if got := len(r.lines()); got != h {
			t.Errorf("the frame has %d lines, want exactly the terminal's %d", got, h)
		}
	})
}

// TestTenantListShowsStateAndReason guards the tenant list: name, database
// type, state and creation time, with the reason of a tenant that is not
// ready in the row and again, whole, under the table for the selected one.
func TestTenantListShowsStateAndReason(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) {
			addTenants(f, 2)
			f.AddTenant(&v1.Tenant{Name: "t900", DatabaseType: v1.DatabaseType_DATABASE_TYPE_POSTGRES,
				State: v1.TenantState_TENANT_STATE_UNREACHABLE, StateReason: "connection refused by db.example.org",
				CreateTime: timestamp(-5)})
		})
		r.key("enter")
		r.expect("Tenants  [3 rows]", "NAME", "DB", "STATE", "CREATED", "t000", "sqlite", "ready", "2026-11-17 15:00:00", "t900", "postgres", "unreachable")
		r.key("end")
		r.expect("t900 is unreachable: connection refused by db.example.org")
		for _, l := range r.lines() {
			if strings.HasPrefix(l, "> ") && !strings.Contains(l, "t900") {
				t.Errorf("the cursor is not on the last row: %q", l)
			}
		}
	})
}

// TestTenantListIsPagedOnDemand guards the rule that a list never loads
// everything. With 250 tenants the first page of 100 is fetched when the list
// opens, the second only when the cursor comes near the end of the first, and
// the third near the end of the second; the server's own request log is the
// proof, and the title says "100+" until the last page is in.
func TestTenantListIsPagedOnDemand(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addTenants(f, 250) })
	r.key("enter")
	if got := r.calls("ListTenants"); got != 1 {
		t.Fatalf("opening the list made %d ListTenants calls, want 1", got)
	}
	r.expect("Tenants  [100+ rows]")
	for range 85 {
		r.key("down")
	}
	if got := r.calls("ListTenants"); got != 1 {
		t.Errorf("after moving 85 rows there were %d calls, want 1: the end of the page is not near yet", got)
	}
	for range 10 {
		r.key("down")
	}
	if got := r.calls("ListTenants"); got != 2 {
		t.Errorf("near the end of the first page there were %d calls, want 2", got)
	}
	r.expect("Tenants  [200+ rows]")
	for range 100 {
		r.key("down")
	}
	if got := r.calls("ListTenants"); got != 3 {
		t.Errorf("near the end of the second page there were %d calls, want 3", got)
	}
	r.expect("Tenants  [250 rows]")
	for range 20 {
		r.key("down")
	}
	r.key("end")
	if got := r.calls("ListTenants"); got != 3 {
		t.Errorf("at the end of the list there were %d calls, want 3: nothing more to fetch", got)
	}
	r.expect("t249")
}

// TestFilterNarrowsLoadedRows guards "/": it filters what is loaded, as the
// letters are typed, and esc clears it.
func TestFilterNarrowsLoadedRows(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addTenants(f, 30) })
	r.key("enter")
	r.key("/")
	r.typeText("t02")
	r.expect("Tenants  [10 of 30 rows, matching \"t02\"]", "/ t02", "t020", "t029")
	r.refuse("t010")
	r.key("enter")
	r.expect("filter: t02 (esc clears)")
	r.key("esc")
	r.expect("Tenants  [30 rows]", "t010")
}

// TestTenantDetailAndItsScreens guards the tenant's own screens: the
// registration and state, then its grants, its setting overrides and its audit
// log, each one key away and each going back with esc.
func TestTenantDetailAndItsScreens(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) {
			addTenants(f, 2)
			f.AddAudit(&v1.AuditEntry{Id: "a1", Time: timestamp(-1), Method: "/nopacs.v1.TenantService/GetTenant", Tenant: "t000",
				Object: "t000", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, RequestId: "req-abc", KeyId: "k00000000001"})
		})
		r.key("enter", "enter")
		r.expect("Tenant t000", "Name:", "Database:", "sqlite", "State:", "ready", "Created:", "2026-11-17 15:00:00 +03:00", "g grants", "s settings", "a audit")
		r.key("g")
		r.expect("Grants on tenant t000", "no grant")
		r.key("esc", "s")
		r.expect("Settings of tenant t000", "nothing is set here")
		r.key("esc", "a")
		r.expect("Audit log of tenant t000", "TIME", "OUTCOME", "TenantService/GetTenant", "k00000000001")
		r.key("enter")
		r.expect("Audit entry a1", "Request id:", "req-abc", "Outcome:", "done", "Reason:")
		r.key("esc", "esc", "esc")
		r.expect("Tenants  [2 rows]")
	})
}

// TestKeyListAndDetail guards the key list and a key's own screen: id, holder,
// status and dates in the list; the whole key in the detail, with its grants.
func TestKeyListAndDetail(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) {
			addKeys(f, 2)
			f.AddKey(&v1.Key{Id: "k00000000900", Holder: "old", CreateTime: timestamp(-100), ExpireTime: timestamp(-50), RevokeTime: timestamp(-60)})
		})
		r.key("2")
		r.expect("Keys  [3 rows]", "ID", "HOLDER", "STATUS", "k00000000100", "holder 0", "active", "k00000000900", "revoked", "n new key", "r revoke")
		if w >= 120 {
			r.expect("LAST USED", "never")
		}
		r.key("enter")
		r.expect("Key k00000000100", "Holder:", "holder 0", "Status:", "active", "Expires:", "g grants", "r revoke")
	})
}

// TestGrantsOfAKeySetAndRemove guards the grant screens: a role is chosen from
// the four, and removing needs the key id typed.
func TestGrantsOfAKeySetAndRemove(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addKeys(f, 1); addTenants(f, 1) })
	r.key("2", "enter", "g")
	r.expect("Grants of key k00000000100", "no grant")
	r.key("s")
	r.expect("Set a role for key k00000000100", "Scope", "Role", "[reader]", " writer ", " operator ", "admin")
	r.key("tab", "right", "right")
	r.expect("[operator]")
	r.key("shift+tab", "ctrl+u")
	r.typeText("t000")
	r.key("enter", "enter")
	r.expect("Grants of key k00000000100", "k00000000100", "tenant t000", "operator", "key k00000000100 now holds operator on tenant t000")
	if got := r.calls("SetGrant"); got != 1 {
		t.Errorf("SetGrant calls = %d, want 1", got)
	}

	// Removing is not one key: the id must be typed.
	r.key("d")
	r.expect("Remove a grant", "Take away the operator role of key k00000000100 on tenant t000", "Type the key id")
	r.key("enter")
	r.expect("not done: type k00000000100 exactly")
	r.typeText("k0000000010")
	r.key("enter")
	if got := r.calls("RemoveGrant"); got != 0 {
		t.Fatalf("RemoveGrant was called %d times with the id incomplete", got)
	}
	r.typeText("0")
	r.key("enter")
	if got := r.calls("RemoveGrant"); got != 1 {
		t.Errorf("RemoveGrant calls = %d, want 1", got)
	}
	r.expect("Grants of key k00000000100", "no grant", "holds no grant on tenant t000")
}

// TestGrantsOfATenantSetByKeyID guards the other direction: from a tenant's
// grants the key is asked for, and a whole key typed there is refused without
// being drawn back or sent.
func TestGrantsOfATenantSetByKeyID(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addKeys(f, 1); addTenants(f, 1) })
	r.key("1", "enter", "g", "s")
	r.expect("Set a role on tenant t000", "Key id")
	r.typeText(testSecret)
	r.key("enter", "enter")
	r.expect("that is a whole key")
	if got := r.calls("SetGrant"); got != 0 {
		t.Errorf("SetGrant was called with a whole key")
	}
	// The whole key that was typed is not left on the screen.
	r.refuse(testSecret)
	r.typeText("k00000000100")
	r.key("enter", "enter")
	r.expect("k00000000100", "reader", "tenant t000")
	r.key("enter")
	r.expect("Key k00000000100", "holder 0")
}

// TestSettingsSetAndUnset guards the settings screens: the installation's
// list with where each value comes from, a tenant's overrides, set with a
// name and a value, and unset with the name typed, on a tenant and on the
// installation. Each row is found by the list's filter, so the test holds
// whatever the number of settings and the height of the screen.
func TestSettingsSetAndUnset(t *testing.T) {
	r := newRig(t, 100, 32, func(f *fakeserver.Server) { addTenants(f, 1) })
	r.key("3")
	r.expect("Settings of the installation")
	for _, row := range [][]string{{"key.default-lifetime", "90d"}, {"test.retention", "365d"}} {
		r.key("/")
		r.typeText(row[0])
		r.expect(row[0], row[1], "built-in default")
		r.key("enter", "esc")
	}
	r.key("esc", "1", "enter", "s")
	r.key("s")
	r.typeText("test.retention")
	r.key("enter")
	r.typeText("30d")
	r.key("enter")
	r.expect("test.retention", "30d", "set test.retention on tenant t000")
	r.key("u")
	r.expect("Unset test.retention", "what applies on the installation applies again")
	r.key("enter")
	r.expect("not done: type test.retention exactly")
	r.typeText("test.retention")
	r.key("enter")
	r.expect("nothing is set here", "test.retention unset on tenant t000")
}

// TestAuditFilter guards the audit screens: newest first as the server answers,
// filtered by outcome and key id through a form, and an entry shown whole.
func TestAuditFilter(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) {
			f.AddAudit(&v1.AuditEntry{Id: "a1", Time: timestamp(-1), Method: "/nopacs.v1.AccessService/CreateKey", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, KeyId: "k00000000001", RequestId: "req-1"})
			f.AddAudit(&v1.AuditEntry{Id: "a2", Time: timestamp(-2), Method: "/nopacs.v1.AccessService/RevokeKey", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_REFUSED, KeyId: "k00000000002", RequestId: "req-2",
				Reason: "the key lacks the admin role", Details: map[string]string{"zeta": "last", "alpha": "first"}})
		})
		r.key("4")
		r.expect("Audit log of the installation  [2 rows]", "CreateKey", "RevokeKey", "refused", "f filter")
		lines := r.lines()
		var first, second int
		for i, l := range lines {
			if strings.Contains(l, "CreateKey") {
				first = i
			}
			if strings.Contains(l, "RevokeKey") {
				second = i
			}
		}
		if first == 0 || second <= first {
			t.Errorf("entries are not in the order the server answered: CreateKey at %d, RevokeKey at %d", first, second)
		}
		r.key("f", "right", "right")
		r.expect("Filter the audit log", "Outcome", "[refused]", "Key id")
		r.key("enter")
		r.typeText("k00000000002")
		r.key("enter")
		r.expect("Audit log of the installation, refused only, key k00000000002  [1 row]", "RevokeKey")
		r.refuse("CreateKey")
		r.key("enter")
		r.expect("Audit entry a2", "Reason:", "the key lacks the admin role", "Request id:", "req-2", "Details:", "alpha = first", "zeta = last")
	})
}

// TestHelpListsTheKeysOfTheScreen guards ?: it shows the keys of the screen
// beneath it, and any key closes it.
func TestHelpListsTheKeysOfTheScreen(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addKeys(f, 1) })
	r.key("2", "?")
	r.expect("Keys of: Keys", "up/down", "new key", "revoke", "this help", "quit")
	r.key("x")
	r.expect("Keys  [1 row]")
	r.key("/", "?")
	r.expect("/ ?")
	r.refuse("this help")
}

// TestErrorIsOneLineWithTheRequestID guards the error line: the server's
// message and the request id on one line at the bottom, in the words a
// command prints for the same failure, and the request id kept when a narrow
// terminal makes the message be cut.
func TestErrorIsOneLineWithTheRequestID(t *testing.T) {
	for _, s := range sizes {
		r := newRig(t, s.w, s.h, nil)
		r.fake.FailWith(nopacsv1connect.TenantServiceListTenantsProcedure, connect.CodePermissionDenied,
			"the key lacks the reader role on the installation, and so it cannot list tenants for anyone at all")
		r.key("enter")
		lines := r.lines()
		status := lines[len(lines)-2]
		wantFull := "tenant list: the key lacks the reader role on the installation, and so it cannot list tenants for anyone at all (request id req-000002)"
		if s.w >= 140 {
			if status != wantFull {
				t.Errorf("%dx%d: the error line is %q, want %q", s.w, s.h, status, wantFull)
			}
		} else {
			if !strings.HasPrefix(status, "tenant list: the key lacks") || !strings.HasSuffix(status, "…"+" (request id req-000002)") {
				t.Errorf("%dx%d: the narrow error line is %q: the message must be cut and the request id kept", s.w, s.h, status)
			}
		}
		if strings.Count(status, "\n") != 0 {
			t.Errorf("the error is not one line: %q", status)
		}
		// The next key clears it.
		r.key("down")
		if got := r.lines()[len(r.lines())-2]; strings.Contains(got, "request id") {
			t.Errorf("the error stayed after a key press: %q", got)
		}
	}
}

// TestServerTextIsCleaned guards the rule that text from the server never
// reaches the terminal as it is: an escape sequence or a newline in a tenant's
// name or reason is drawn as "?".
func TestServerTextIsCleaned(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) {
		f.AddTenant(&v1.Tenant{Name: "evil\x1b[2Jname\nsecond", DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE,
			State: v1.TenantState_TENANT_STATE_FAILED, StateReason: "\x1b]0;owned\x07reason", CreateTime: timestamp(0)})
	})
	r.key("enter")
	r.expect("evil?[2Jname?second", "?]0;owned?reason")
	for _, f := range r.frames {
		if strings.ContainsAny(f, "\x1b\x07") {
			t.Fatalf("a frame holds a control character: %q", f)
		}
	}
	r.key("enter")
	r.expect("Tenant evil?[2Jname?second")
}

// TestResizeRedraws guards resizing: the next frame is drawn for the new size,
// and a terminal too small for any screen says so instead of drawing garbage.
func TestResizeRedraws(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addTenants(f, 40) })
	r.key("enter")
	if got := len(r.lines()); got != 24 {
		t.Fatalf("at 80x24 the frame has %d lines", got)
	}
	r.send(sizeMsg(200, 50))
	if got := len(r.lines()); got != 50 {
		t.Errorf("after resizing to 200x50 the frame has %d lines", got)
	}
	r.send(sizeMsg(30, 8))
	r.expect("terminal too small", "30 by 8 now", "needs 40 by 10")
	r.send(sizeMsg(60, 12))
	r.expect("Tenants  [40 rows]", "t000")
}

// TestNoColorMeansNoEscapeSequence guards the style switch: with color off, no
// frame holds an escape sequence; with it on, the selected row and the labels
// are marked.
func TestNoColorMeansNoEscapeSequence(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addTenants(f, 3) })
	r.key("enter", "enter")
	for _, f := range r.frames {
		if strings.Contains(f, "\x1b") {
			t.Fatalf("a plain frame holds an escape sequence: %q", f)
		}
	}
	r.m.c.st = styler{on: true}
	if f := r.screen(); !strings.Contains(f, "\x1b[") {
		t.Errorf("a colored frame holds no emphasis: %q", f)
	}
	r.key("esc")
	if f := r.screen(); !strings.Contains(f, "\x1b[7m") && !strings.Contains(f, "\x1b[7;") {
		t.Errorf("the selected row is not in reverse video: %q", f)
	}
}

// TestRowsNeverWrap guards the layout at 80 columns with long values: every
// line of every frame fits (the rig checks that on each frame) and a long
// holder or tenant name is cut with an ellipsis.
func TestRowsNeverWrap(t *testing.T) {
	long := strings.Repeat("a long name ", 20)
	r := newRig(t, 80, 24, func(f *fakeserver.Server) {
		f.AddKey(&v1.Key{Id: "k00000000100", Holder: long, CreateTime: timestamp(0)})
		f.AddTenant(&v1.Tenant{Name: strings.Repeat("n", 60), DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE, State: v1.TenantState_TENANT_STATE_READY, CreateTime: timestamp(0)})
	})
	r.key("2")
	r.expect("k00000000100", "a long name a long")
	r.expect("…")
	r.key("esc", "1")
	r.expect("nnnnnnnn…")
}
