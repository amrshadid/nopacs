package registry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
)

// byRequest returns, oldest first, every entry of the master's log whose
// request id is id, each as "method outcome", with the details that tell
// the entries of a creation apart.
func (e *env) byRequest(id string) []string {
	e.t.Helper()
	var out []string
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := e.master.ListAudit(context.Background(), master.AuditFilter{}, page)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, a := range entries {
			if a.RequestID != id {
				continue
			}
			s := a.Method + " " + string(a.Outcome)
			if a.Details[DetailUndone] != "" {
				s += " undone"
			}
			if a.Details[master.DetailNewState] != "" {
				s += " ->" + a.Details[master.DetailNewState]
			}
			if a.Details[DetailCreationRequest] != "" {
				s += " finishes " + a.Details[DetailCreationRequest]
			}
			out = append([]string{s}, out...)
		}
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// TestCreationAuditedByHowItEnded guards that a creation's request is
// audited once, with its real outcome, in the transaction that settles the
// creation, and that the request's id leads to every entry of it: a system
// entry when the row is written, then the request's own entry with the
// change to ready (done) or with the removal of the row (failed, undone);
// after a crash, the next start's system entry naming the request.
func TestCreationAuditedByHowItEnded(t *testing.T) {
	const started = StartMethod + " done"
	cases := []struct {
		name string
		mode mode
		want []string
	}{
		{"ready", up, []string{started, "CreateTenant done ->ready"}},
		{"does-not-answer", down, []string{started, "CreateTenant failed undone"}},
		{"migration-fails", broken, []string{started, "CreateTenant failed undone"}},
		{"address-refused", refused, []string{started, "CreateTenant failed undone"}},
		{"plain-error", leaky, []string{started, "CreateTenant failed undone"}},
		{"driver-panics", panicky, []string{started, "CreateTenant failed undone"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			r := e.newRegistry(e.config())
			e.flaky.set(c.name, c.mode)
			audit := e.audit("CreateTenant")
			_, _, err := r.Create(ctx, NewTenant{ID: "id-" + c.name, Name: c.name, Driver: flakyDriver, Address: flakyAddress(c.name)}, audit, master.LocalOperator())
			if (err == nil) != (c.mode == up) {
				t.Fatalf("Create = %v", err)
			}
			// Both entries have the same time on the test clock, so they are
			// compared as a set; the crash case below orders them by time.
			got := e.byRequest(audit.RequestID)
			slices.Sort(got)
			slices.Sort(c.want)
			if !slices.Equal(got, c.want) {
				t.Errorf("entries of request %s = %q, want %q", audit.RequestID, got, c.want)
			}
		})
	}

	t.Run("crash", func(t *testing.T) {
		ctx := context.Background()
		e := newEnv(t)
		crashed := e.newRegistry(e.config())
		crashed.afterRow = func() error { return errors.New("crash") }
		audit := e.audit("CreateTenant")
		if _, _, err := crashed.Create(ctx, NewTenant{ID: "id-lab", Name: "lab", Driver: driver.SQLiteDriver}, audit, master.LocalOperator()); err == nil {
			t.Fatal("the crash did not stop Create")
		}
		if err := crashed.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if got, want := e.byRequest(audit.RequestID), []string{started}; !slices.Equal(got, want) {
			t.Errorf("after the crash, entries of the request = %q, want %q", got, want)
		}
		e.clock.Add(time.Second) // so the finishing entry sorts after the started one
		r := e.newRegistry(e.config())
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		want := []string{started, SystemMethod + " done ->ready finishes " + audit.RequestID}
		if got := e.byRequest(audit.RequestID); !slices.Equal(got, want) {
			t.Errorf("after the next start, entries of the request = %q, want %q", got, want)
		}
	})

	t.Run("repeat", func(t *testing.T) {
		ctx := context.Background()
		e := newEnv(t)
		r := e.newRegistry(e.config())
		first, again := e.audit("CreateTenant"), e.audit("CreateTenant")
		nt := NewTenant{ID: "id-lab", Name: "lab", Driver: flakyDriver, Address: flakyAddress("lab")}
		for _, a := range []master.AuditEntry{first, again} {
			if _, _, err := r.Create(ctx, nt, a, master.LocalOperator()); err != nil {
				t.Fatal(err)
			}
		}
		if got := e.byRequest(again.RequestID); !slices.Equal(got, []string{"CreateTenant done"}) {
			t.Errorf("a repeat of a finished creation recorded %q, want its request alone, done", got)
		}
		if got := e.byRequest(first.RequestID); len(got) != 2 {
			t.Errorf("the first request's entries = %q", got)
		}
	})
}

// TestFailedCreationReasonIsSafe guards that the reason a failed creation's
// request entry keeps is the registry's own words and the driver's safe
// message, with no address in it.
func TestFailedCreationReasonIsSafe(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.flaky.set("down", down)
	audit := e.audit("CreateTenant")
	if _, _, err := r.Create(ctx, NewTenant{ID: "id-down", Name: "down", Driver: flakyDriver, Address: flakyAddress("down")}, audit, master.LocalOperator()); err == nil {
		t.Fatal("Create succeeded")
	}
	all, _, err := e.master.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.ID == audit.ID {
			if want := "the database does not answer: the database does not answer"; a.Reason != want {
				t.Errorf("the failed request's reason = %q, want %q", a.Reason, want)
			}
			mustNotContain(t, "the failed request's entry", fmt.Sprint(a), append(passwordForms, flakyHost)...)
			return
		}
	}
	t.Errorf("the request's entry %s is not in the log; ids: %s", audit.ID, strings.Join(ids(all), " "))
}

// ids returns the entries' ids.
func ids(entries []master.AuditEntry) []string {
	out := make([]string, len(entries))
	for i, a := range entries {
		out[i] = a.ID
	}
	return out
}
