package cli_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestIntegerFlagsAreCheckedBeforeTheyAreNarrowed guards the conversion of an
// integer flag to the 32 bits the contract carries: a number that would wrap
// into a valid one (4294967303 is 7 in 32 bits, 4294967297 is 1) is refused
// as invalid with the bounds named, and nothing is sent.
func TestIntegerFlagsAreCheckedBeforeTheyAreNarrowed(t *testing.T) {
	r := newArchiveRig(t)
	r.env["S3SECRET"] = s3Secret
	r.must("storage", "add", "lab1", "cold", "--type", "s3", "--endpoint", "https://s3.example", "--region", "r", "--bucket", "b",
		"--access-key-id", "AKIAEXAMPLE", "--credential-env", "S3SECRET")
	r.must("retention", "rule", "add", "lab1", "seven-years", "--dispose-after", "10y", "--action", "delete")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"replica", "attach", "lab1", "cold", "--primary", "main", "--tier", "archive", "--recall-days", "4294967303"}, "--recall-days 4294967303 is not from 1 to 365"},
		{[]string{"replica", "attach", "lab1", "cold", "--primary", "main", "--tier", "archive", "--recall-days", "366"}, "is not from 1 to 365"},
		{[]string{"storage", "set", "lab1", "cold", "--recall-days", "4294967303"}, "--recall-days 4294967303 is not from 1 to 365"},
		{[]string{"storage", "set", "lab1", "cold", "--recall-days", "-1"}, "--recall-days -1 is not from 1 to 365"},
		{[]string{"storage", "add", "lab1", "cold2", "--type", "s3", "--endpoint", "https://s3.example", "--region", "r", "--bucket", "b",
			"--access-key-id", "AKIAEXAMPLE", "--credential-env", "S3SECRET", "--tier", "archive", "--recall-days", "4294967303"}, "is not from 1 to 365"},
		{[]string{"retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "x", "--preview", "--max-sessions", "4294967297"}, "--max-sessions 4294967297 is not from 1 to 1000"},
		{[]string{"retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "x", "--preview", "--max-sessions", "-5"}, "is not from 1 to 1000"},
		{[]string{"retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "x", "--preview", "--max-sessions", "1001"}, "is not from 1 to 1000"},
	} {
		before := r.fake.Requests()
		res := r.run(c.args...)
		if res.code != 2 || !strings.Contains(res.stderr, c.want) {
			t.Errorf("nopacs %v: %+v, want exit 2 with %q", c.args, res, c.want)
		}
		if n := r.fake.Requests() - before; n != 0 {
			t.Errorf("nopacs %v sent %d requests", c.args, n)
		}
	}
}

// TestDisposeMaxSessionsFollowsTheServersBounds guards the fake's disposal and
// the command against it: a bound inside 1 to 1000 and within the pass's own
// limit is accepted, and one above the pass's limit is refused by the server
// with its reason (exit 2).
func TestDisposeMaxSessionsFollowsTheServersBounds(t *testing.T) {
	r := newArchiveRig(t)
	r.must("retention", "rule", "add", "lab1", "seven-years", "--dispose-after", "10y", "--action", "delete")
	r.fake.AddDueSession("lab1", dueSession("sub-1", "ses-1", "seven-years", false))
	r.fake.SetMaxPerPass(50)
	if res := r.run("retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "x", "--preview", "--max-sessions", "50"); res.code != 0 {
		t.Errorf("a bound at the pass's limit: %+v", res)
	}
	res := r.run("retention", "dispose", "lab1", "--rule", "seven-years", "--reason", "x", "--preview", "--max-sessions", "60")
	if res.code != 2 || !strings.Contains(res.stderr, "retention.max-per-pass (50)") {
		t.Errorf("a bound above the pass's limit: %+v", res)
	}
}

// TestRetentionClockFindsTheTenantAsked guards the status read of "retention
// clock": the entry of the tenant asked for is the one shown, and an answer
// that holds only another tenant's is reported as holding none for this one.
func TestRetentionClockFindsTheTenantAsked(t *testing.T) {
	r := newArchiveRig(t)
	r.fake.SetRetentionStatus("lab1", &v1.RetentionStatus{Rules: 2})
	r.fake.AnswerStatusAs("lab1", "other")
	res := r.run("retention", "clock", "lab1")
	if res.code != 4 || res.stdout != "" || !strings.Contains(res.stderr, "no retention state for tenant lab1") {
		t.Errorf("an answer for another tenant: %+v", res)
	}
}
