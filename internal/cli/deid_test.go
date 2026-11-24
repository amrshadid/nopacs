package cli_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestDeidCommands guards "deid check" and "deid dry-run": the job they
// make, by mode, version and target (none for the whole tenant), as a
// person reads it and with --json; the version named, or the latest; --own
// for each instance's version; the refusals of a missing version and of
// two targets; and "job show" of a deid job with its counts per rule and
// per reason as a table, and with --json.
func TestDeidCommands(t *testing.T) {
	r := jobRig(t)
	check := r.run("deid", "check", "lab1")
	for label, want := range map[string]string{"ID": "job_1", "State": "queued", "Judges": "the whole tenant", "Mode": "check",
		"Version": "each instance's own"} {
		if got := field(check.stdout, label); check.code != 0 || got != want {
			t.Errorf("deid check: %s is %q, want %q:\n%s", label, got, want, check.stdout)
		}
	}
	dry := r.run("deid", "dry-run", "lab1", "research@3", "sub-0017/ses-1")
	for label, want := range map[string]string{"Judges": "sub-0017/ses-1", "Mode": "dry run", "Version": "research@3"} {
		if got := field(dry.stdout, label); dry.code != 0 || got != want {
			t.Errorf("deid dry-run: %s is %q, want %q:\n%s", label, got, want, dry.stdout)
		}
	}
	doc := document(t, r.run("deid", "dry-run", "lab1", "research", "--json"))
	dj := doc["job"].(map[string]any)["deid"].(map[string]any)
	if dj["mode"] != "DEID_JOB_MODE_DRY_RUN" || dj["profile"] != "research" || dj["profileVersion"] != float64(1) {
		t.Errorf("deid dry-run --json: %v", dj)
	}
	own := r.run("deid", "dry-run", "lab1", "--own", "sub-0017")
	if got := field(own.stdout, "Version"); own.code != 0 || got != "each instance's own" {
		t.Errorf("deid dry-run --own: Version %q:\n%s", got, own.stdout)
	}
	if res := r.run("deid", "dry-run", "lab1"); res.code == 0 || !strings.Contains(res.stderr, "--own") {
		t.Errorf("a dry run naming no version: %+v", res)
	}
	if res := r.run("deid", "dry-run", "lab1", "research", "sub-1", "sub-2"); res.code == 0 {
		t.Errorf("a dry run of two targets: %+v", res)
	}
	r.fake.SetJob("lab1", "job_2", v1.JobState_JOB_STATE_PARTIAL, counts(10, 8, 0, 2))
	r.fake.SetJobTallies("lab1", "job_2", &v1.JobTally{Key: "rule|maker|remove", Count: 8},
		&v1.JobTally{Key: "hold|burned_in_risk|secondary_capture", Count: 2})
	show := r.run("job", "show", "lab1", "job_2")
	if show.code != 0 || !strings.Contains(show.stdout, "KIND") || !strings.Contains(show.stdout, "maker  remove") ||
		!strings.Contains(show.stdout, "burned_in_risk  secondary_capture") {
		t.Errorf("job show of a deid job:\n%s", show.stdout)
	}
	js := document(t, r.run("job", "show", "lab1", "job_2", "--json"))
	if ts := js["job"].(map[string]any)["tallies"].([]any); len(ts) != 2 {
		t.Errorf("job show --json of a deid job: tallies %v", ts)
	}
	list := r.run("job", "list", "lab1")
	if list.code != 0 || !strings.Contains(list.stdout, "deid check") || !strings.Contains(list.stdout, "the whole tenant") {
		t.Errorf("job list of deid jobs:\n%s", list.stdout)
	}
}
