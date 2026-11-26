package cli_test

import (
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// replicaRig is an archive rig with a second directory storage, "copy", not
// yet a replica of anything.
func replicaRig(t *testing.T) *rig {
	t.Helper()
	r := newArchiveRig(t)
	r.must("storage", "add", "lab1", "copy", "--type", "dir", "--root", "slow")
	return r
}

// TestReplicaAttachListDetach guards the replica commands' flow: attach makes
// a storage a replica and says how to follow its copy job, list shows how
// far it is, and detach turns it back into an ordinary storage and says that
// no file was deleted; the document of each command holds the answer.
func TestReplicaAttachListDetach(t *testing.T) {
	r := replicaRig(t)
	res := r.run("replica", "attach", "lab1", "copy", "--primary", "main")
	if res.code != 0 || !strings.Contains(res.stdout, "storage copy is a replica of main (tier online)") ||
		!strings.Contains(res.stdout, "queued job job_1; follow it with: nopacs job watch lab1 job_1") {
		t.Fatalf("attach: %+v", res)
	}
	if j := r.fake.Job("lab1", "job_1"); j.GetReplicate().GetReplica() != "copy" || j.GetReplicate().GetPrimary() != "main" {
		t.Errorf("the copy job: %v", j)
	}
	r.fake.SetReplicaLag("lab1", &v1.ReplicaStatus{Replica: "copy", ScansInStep: 12, ScansPending: 3, ScansDamaged: 1})
	expect(t, r.run("replica", "list", "lab1"), 0,
		"REPLICA  PRIMARY  TIER    IN STEP  WAITING  DAMAGED  OLDEST WAIT\n"+
			"copy     main     online  12       3        1        -\n", "")
	doc := document(t, r.run("replica", "list", "lab1", "--primary", "main", "--json"))
	if rs, _ := doc["replicas"].([]any); len(rs) != 1 || doc["more"] != false {
		t.Errorf("list --json: %v", doc)
	}
	if res := r.run("replica", "list", "lab1", "--primary", "nobody"); res.code != 0 || strings.Contains(res.stdout, "copy") {
		t.Errorf("list of another primary: %+v", res)
	}
	// The storage list now says which storage is a replica of which.
	if out := mustOut(r, "storage", "list", "lab1"); !strings.Contains(out, "copy  directory  online  main") {
		t.Errorf("storage list after the attach:\n%s", out)
	}

	res = r.run("replica", "detach", "lab1", "copy")
	if res.code != 0 || !strings.Contains(res.stdout, "storage copy is an ordinary storage again; no file was deleted") {
		t.Errorf("detach: %+v", res)
	}
	// A repeated detach finds a storage that is not a replica: a state that
	// forbids the change.
	if res := r.run("replica", "detach", "lab1", "copy"); res.code != 5 || !strings.Contains(res.stderr, "not a replica") {
		t.Errorf("a repeated detach: %+v", res)
	}
}

// TestReplicaAttachSetsTheTierAfterwards guards the tier flags of attach: the
// storage is a replica first (a tier other than online is only for one), then
// its tier and recall settings are set; when the second call is refused the
// attach stands and the message says so.
func TestReplicaAttachSetsTheTierAfterwards(t *testing.T) {
	r := newArchiveRig(t)
	r.env["S3SECRET"] = s3Secret
	r.must("storage", "add", "lab1", "cold", "--type", "s3", "--endpoint", "https://s3.example", "--region", "r", "--bucket", "b",
		"--access-key-id", "AKIAEXAMPLE", "--credential-env", "S3SECRET")
	res := r.run("replica", "attach", "lab1", "cold", "--primary", "main", "--tier", "archive", "--recall-days", "14", "--recall-tier", "bulk")
	if res.code != 0 || !strings.Contains(res.stdout, "(tier archive)") {
		t.Fatalf("attach with a tier: %+v", res)
	}
	show := mustOut(r, "storage", "show", "lab1", "cold")
	for _, want := range []string{"Tier:              archive\n", "Replica of:        main\n", "Recall days:       14\n", "Recall tier:       bulk\n"} {
		if !strings.Contains(show, want) {
			t.Errorf("storage show lacks %q:\n%s", want, show)
		}
	}
	// A directory cannot be of the archive tier: the attach stands, the tier
	// does not, and the line says both.
	r.must("storage", "add", "lab1", "dircopy", "--type", "dir", "--root", "slow")
	r.fake.FailWith(nopacsv1connect.StorageServiceSetStorageProcedure, connect.CodeFailedPrecondition, "a directory storage is always online")
	res = r.run("replica", "attach", "lab1", "dircopy", "--primary", "main", "--tier", "archive")
	if res.code != 5 || !strings.Contains(res.stderr, "storage dircopy is now a replica of main") || !strings.Contains(res.stderr, "tier settings were refused") {
		t.Errorf("attach with a refused tier: %+v", res)
	}
	if res := r.run("replica", "attach", "lab1", "cold", "--primary", "main", "--recall-tier", "fast"); res.code != 2 {
		t.Errorf("an unknown recall tier: %+v", res)
	}
}

// TestReplicaDetachAbandonNeedsReasonAndConfirmation guards the one detach
// that cannot be undone: while corrections count the replica it is refused
// (exit 5, saying how many), --abandon-purges needs --reason and the other
// way round, and it asks for the replica's name to be typed again unless
// --yes; with no terminal it detaches nothing.
func TestReplicaDetachAbandonNeedsReasonAndConfirmation(t *testing.T) {
	r := replicaRig(t)
	r.must("replica", "attach", "lab1", "copy", "--primary", "main")
	r.fake.CountReplicaInCorrections("lab1", "copy", 2)
	before := r.fake.Requests()
	if res := r.run("replica", "detach", "lab1", "copy", "--abandon-purges"); res.code != 2 || !strings.Contains(res.stderr, "go together") {
		t.Errorf("abandon without a reason: %+v", res)
	}
	if res := r.run("replica", "detach", "lab1", "copy", "--reason", "gone"); res.code != 2 {
		t.Errorf("a reason without abandon: %+v", res)
	}
	res := r.run("replica", "detach", "lab1", "copy", "--abandon-purges", "--reason", "the site burned")
	if res.code != 2 || !strings.Contains(res.stderr, "nothing was detached") || r.fake.Requests() != before {
		t.Errorf("abandon with no terminal and no --yes: %+v, %d requests", res, r.fake.Requests()-before)
	}
	if res := r.run("replica", "detach", "lab1", "copy"); res.code != 5 || !strings.Contains(res.stderr, "2 corrections count this replica") {
		t.Errorf("a detach while corrections count it: %+v", res)
	}
	// Typed on a terminal: a wrong name stops it, the right name goes on.
	if res := typed(r.env, "other\n", "replica", "detach", "lab1", "copy", "--abandon-purges", "--reason", "the site burned"); res.code != 2 || !strings.Contains(res.stderr, "not the id given") {
		t.Errorf("a wrong name typed: %+v", res)
	}
	if res := typed(r.env, "copy\n", "replica", "detach", "lab1", "copy", "--abandon-purges", "--reason", "the site burned"); res.code != 0 {
		t.Errorf("the right name typed: %+v", res)
	}
}

// TestReplicaBackfillNotServed guards the way a command shows a method the
// server does not serve yet: one plain line saying so and that nothing was
// changed, exit 1, nothing on standard output; and once served, the job and
// --wait following it to its end.
func TestReplicaBackfillNotServed(t *testing.T) {
	r := replicaRig(t)
	r.must("replica", "attach", "lab1", "copy", "--primary", "main")
	expect(t, r.run("replica", "backfill", "lab1", "copy"), 1, "",
		"nopacs: replica backfill: not served: this server does not serve it yet; nothing was changed\n")
	r.fake.ServeBackfill()
	res := r.run("replica", "backfill", "lab1", "copy")
	if res.code != 0 || !strings.Contains(res.stdout, "queued job job_2; follow it with: nopacs job watch lab1 job_2") {
		t.Errorf("backfill once served: %+v", res)
	}
	finishLater(t, r, "job_3", v1.JobState_JOB_STATE_DONE)
	res = r.run("replica", "backfill", "lab1", "copy", "--wait")
	if res.code != 0 || !strings.Contains(res.stdout, "Copies to:") || !strings.Contains(res.stdout, "copy") {
		t.Errorf("backfill --wait: %+v", res)
	}
	if res := r.run("replica", "backfill", "lab1", "main"); res.code != 5 {
		t.Errorf("backfill of a storage that is not a replica: %+v", res)
	}
}

// TestErrorsShowTheServersReasonAndRetryDelay guards the details of an API
// error: the reason (STORAGE_SWITCHING, NOT_ONLINE, HELD, RETENTION_MINIMUM)
// and the delay to wait before trying again are read from the error's
// google.rpc.ErrorInfo and google.rpc.RetryInfo and shown on the one line;
// the exit code is that of the error's kind.
func TestErrorsShowTheServersReasonAndRetryDelay(t *testing.T) {
	cases := []struct {
		reason string
		kind   fault.Kind
		retry  time.Duration
		exit   int
		want   string
	}{
		{"STORAGE_SWITCHING", fault.Unavailable, 30 * time.Second, 1, "replica backfill: STORAGE_SWITCHING: the storage is switching (try again in 30s)"},
		{"NOT_ONLINE", fault.Unavailable, 0, 1, "replica backfill: NOT_ONLINE: the storage is switching"},
		{"HELD", fault.Precondition, 0, 5, "replica backfill: HELD: the storage is switching"},
		{"RETENTION_MINIMUM", fault.Precondition, 0, 5, "replica backfill: RETENTION_MINIMUM: the storage is switching"},
	}
	for _, c := range cases {
		r := replicaRig(t)
		r.fake.FailWithReason(nopacsv1connect.ReplicaServiceBackfillReplicaProcedure, c.kind, "the storage is switching", c.reason, c.retry)
		res := r.run("replica", "backfill", "lab1", "copy")
		if res.code != c.exit || res.stdout != "" || !strings.Contains(res.stderr, c.want) {
			t.Errorf("%s: %+v, want exit %d and %q", c.reason, res, c.exit, c.want)
		}
		if c.retry == 0 && strings.Contains(res.stderr, "try again") {
			t.Errorf("%s: a retry delay the server did not give: %q", c.reason, res.stderr)
		}
	}
}

// TestCorrectionShowsDispositionsPerStorageAndAbandonedCopies guards what a
// correction says of its copies: each storage's disposition, with the
// primary a replica is of, and the copies that a detach abandoned, worded so
// that nobody reads them as removed.
func TestCorrectionShowsDispositionsPerStorageAndAbandonedCopies(t *testing.T) {
	r := replicaRig(t)
	r.fake.AddCorrection("lab1", &v1.Correction{Id: "cor_1", Kind: v1.CorrectionKind_CORRECTION_KIND_DELETE, State: v1.CorrectionState_CORRECTION_STATE_PURGED,
		Target: &v1.CorrectionTarget{SubjectLabel: "sub-1"}, Counts: &v1.CorrectionCounts{Instances: 4},
		Dispositions: []*v1.StorageDisposition{
			{Storage: "main", ObjectsRemoved: 4},
			{Storage: "copy", ReplicaOf: "main", ObjectsUnknown: 4},
		},
		CopiesAbandoned: []string{"copy"}, Origin: v1.CorrectionOrigin_CORRECTION_ORIGIN_RETENTION, RetentionRule: "seven-years",
	})
	out := mustOut(r, "correction", "show", "lab1", "cor_1")
	for label, want := range map[string]string{
		"main":                   "4 removed: the bytes are gone from this storage",
		"copy (replica of main)": "0 removed, 0 hidden (older versions kept), 4 unknown: the bytes may still be there",
	} {
		if got := field(out, "  "+label); got != want {
			t.Errorf("disposition of %s is %q, want %q:\n%s", label, got, want, out)
		}
	}
	if got := field(out, "Copies abandoned"); !strings.HasPrefix(got, "copy: a replica was detached before the purge reached it") {
		t.Errorf("Copies abandoned is %q:\n%s", got, out)
	}
	if got := field(out, "Retention rule"); !strings.HasPrefix(got, "seven-years, due") {
		t.Errorf("Retention rule is %q:\n%s", got, out)
	}
}

// TestAReasonOnlyInTheDetailsIsStillShown guards a server whose message does
// not start with the reason: the reason and the retry delay are read from the
// error's details and put on the line, so a person never has to guess why a
// call was refused.
func TestAReasonOnlyInTheDetailsIsStillShown(t *testing.T) {
	r := replicaRig(t)
	r.fake.FailWithDetailOnly(nopacsv1connect.ReplicaServiceBackfillReplicaProcedure, fault.Precondition, "the data is held", "HELD", 90*time.Second)
	res := r.run("replica", "backfill", "lab1", "copy")
	if res.code != 5 || !strings.Contains(res.stderr, "replica backfill: HELD: the data is held (try again in 1m30s)") {
		t.Errorf("a reason in the details only: %+v", res)
	}
}
