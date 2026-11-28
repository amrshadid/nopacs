package cli_test

import (
	"strings"
	"testing"
)

// TestSearchBackfill guards "search backfill": the tenant and the attribute
// are both required; an attribute the search tables do not hold is refused
// (exit 2) and one already filled is a precondition failure (exit 5); an
// attribute being filled makes a queued job that says what it fills and how
// to follow it; a second while it has not ended is refused (exit 5); and
// the job is listed with its kind.
func TestSearchBackfill(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	for _, args := range [][]string{{"search", "backfill"}, {"search", "backfill", "lab1"}} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if res := r.run("search", "backfill", "lab1", "NotAnAttribute"); res.code != 2 || !strings.Contains(res.stderr, "NotAnAttribute") {
		t.Errorf("an attribute the search tables lack: %+v, want exit 2 naming it", res)
	}
	if res := r.run("search", "backfill", "lab1", "Modality"); res.code != 5 || !strings.Contains(res.stderr, "nothing to backfill") {
		t.Errorf("an attribute that is filled: %+v, want exit 5", res)
	}
	out := mustOut(r, "search", "backfill", "lab1", "EchoTime")
	if !strings.Contains(out, "Fills:") || !strings.Contains(out, "EchoTime") || !strings.Contains(out, "queued") || !strings.Contains(out, "Filled:") {
		t.Errorf("the job is shown as %q", out)
	}
	if res := r.run("search", "backfill", "lab1", "EchoTime"); res.code != 5 || !strings.Contains(res.stderr, "one runs at a time") {
		t.Errorf("a second backfill while one runs: %+v, want exit 5", res)
	}
	list := mustOut(r, "job", "list", "lab1")
	if !strings.Contains(list, "search backfill") || !strings.Contains(list, "EchoTime") {
		t.Errorf("the job list is %q", list)
	}
	if res := r.run("job", "list", "lab1", "--json"); res.code != 0 || !strings.Contains(res.stdout, "searchBackfill") {
		t.Errorf("the job list as JSON is %+v", res)
	}
}
