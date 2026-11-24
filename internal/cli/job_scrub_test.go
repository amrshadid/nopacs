package cli_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// scrubRig is a rig with tenant lab1 and a storage main.
func scrubRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast")
	return r
}

// TestJobScrubAndRebuild guards "job scrub" and "job rebuild": the request
// each makes (storage, mode, from-storage), the job as a person reads it with
// the counts and tallies named for the kind, the refusals the server makes
// (a second scrub of a storage, a second rebuild: exit 5; a storage the
// tenant lacks: exit 5), and that a replace is refused with exit 2 and sends
// nothing unless --yes is given.
func TestJobScrubAndRebuild(t *testing.T) {
	r := scrubRig(t)
	scrub := r.run("job", "scrub", "lab1", "--storage", "main")
	for label, want := range map[string]string{"ID": "job_1", "State": "queued", "Scrubs": "storage main", "Made by": "a person"} {
		if got := field(scrub.stdout, label); scrub.code != 0 || got != want {
			t.Errorf("job scrub: %s is %q, want %q:\n%s", label, got, want, scrub.stdout)
		}
	}
	if res := r.run("job", "scrub", "lab1", "--storage", "main"); res.code != 5 || !strings.Contains(res.stderr, "another scrub") {
		t.Errorf("a second scrub of one storage: %+v", res)
	}
	if res := r.run("job", "scrub", "lab1", "--storage", "ghost"); res.code != 5 {
		t.Errorf("a scrub of a storage the tenant lacks: %+v", res)
	}
	r.fake.SetJob("lab1", "job_1", v1.JobState_JOB_STATE_PARTIAL, counts(10, 8, 0, 2))
	r.fake.SetJobTallies("lab1", "job_1", &v1.JobTally{Key: "finding|damaged", Count: 2}, &v1.JobTally{Key: "cleared", Count: 1})
	show := r.run("job", "show", "lab1", "job_1")
	for label, want := range map[string]string{"State": "partial", "Read good": "8, 7.8 KiB", "With a finding": "2, 2.0 KiB"} {
		if got := field(show.stdout, label); show.code != 0 || got != want {
			t.Errorf("job show of a scrub: %s is %q, want %q:\n%s", label, got, want, show.stdout)
		}
	}
	if got := field(show.stdout, "Progress"); !strings.HasSuffix(got, "10 of 10 instances answered (100%): 8 read good, 2 with a finding; 0 remaining; scans 1 of 2 done") {
		t.Errorf("job show of a scrub words its progress as a send's: %q", got)
	}
	if !strings.Contains(show.stdout, "finding  damaged") || !strings.Contains(show.stdout, "cleared") {
		t.Errorf("job show of a scrub lacks its tallies:\n%s", show.stdout)
	}
	whole := document(t, r.run("job", "scrub", "lab1", "--json"))
	if sj := whole["job"].(map[string]any)["scrub"].(map[string]any); sj["storage"] != "" && sj["storage"] != nil {
		t.Errorf("job scrub --json: %v", sj)
	}

	// Rebuild: a compare is made at once; a replace needs --yes.
	before := r.fake.Requests()
	res := r.run("job", "rebuild", "lab1", "--mode", "replace")
	if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "nothing was started") || !strings.Contains(res.stderr, "receivers refuse new data") ||
		!strings.Contains(res.stderr, "run a compare first") || strings.Count(res.stderr, "\n") != 1 {
		t.Errorf("a replace without --yes: %+v", res)
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("a refused replace sent %d requests", got-before)
	}
	if res := r.run("job", "rebuild", "lab1"); res.code != 2 {
		t.Errorf("a rebuild with no mode: %+v", res)
	}
	if res := r.run("job", "rebuild", "lab1", "--mode", "sideways"); res.code != 2 || !strings.Contains(res.stderr, "is not one of: compare, replace") {
		t.Errorf("a rebuild with an unknown mode: %+v", res)
	}
	if res := r.run("job", "rebuild", "lab1", "--mode", "replace", "--from-storage", "main", "--yes"); res.code != 2 {
		t.Errorf("a replace from one storage: %+v", res)
	}
	compare := r.run("job", "rebuild", "lab1", "--mode", "compare", "--from-storage", "main")
	for label, want := range map[string]string{"Rebuilds": "the catalog, compare", "From storage": "main", "Cataloged": "0, 0 B"} {
		if got := field(compare.stdout, label); compare.code != 0 || got != want {
			t.Errorf("job rebuild --mode compare: %s is %q, want %q:\n%s", label, got, want, compare.stdout)
		}
	}
	if res := r.run("job", "rebuild", "lab1", "--mode", "compare"); res.code != 5 || !strings.Contains(res.stderr, "another rebuild") {
		t.Errorf("a second rebuild: %+v", res)
	}
	r.fake.SetJob("lab1", "job_3", v1.JobState_JOB_STATE_DONE, counts(10, 10, 0, 0))
	yes := document(t, r.run("job", "rebuild", "lab1", "--mode", "replace", "--yes", "--json"))
	if rj := yes["job"].(map[string]any)["rebuild"].(map[string]any); rj["mode"] != "REBUILD_JOB_MODE_REPLACE" {
		t.Errorf("job rebuild --mode replace --yes --json: %v", rj)
	}

	list := r.run("job", "list", "lab1")
	if list.code != 0 || !strings.Contains(list.stdout, "scrub") || !strings.Contains(list.stdout, "rebuild compare") || !strings.Contains(list.stdout, "rebuild replace") {
		t.Errorf("job list shows no scrub or rebuild:\n%s", list.stdout)
	}
}

// TestJobScrubAndRebuildWatch guards that "job watch" and --wait follow a
// scrub and a rebuild as any job: the events of the feed carry no kind, and
// the lines and the exit code (1 for a job that ended partial) are the same.
func TestJobScrubAndRebuildWatch(t *testing.T) {
	r := scrubRig(t)
	r.must("job", "scrub", "lab1")
	r.fake.SetJob("lab1", "job_1", v1.JobState_JOB_STATE_RUNNING, counts(10, 0, 0, 0))
	r.fake.QueueEvent("lab1", jobStep("job_1", v1.JobState_JOB_STATE_PARTIAL, counts(10, 8, 0, 2)))
	res := r.run("job", "watch", "lab1", "job_1")
	if res.code != 1 || res.stderr != "nopacs: job job_1 ended partial\n" || !strings.Contains(res.stdout, "partial   [====================] 10 of 10 instances answered (100%): 8 read good, 2 with a finding;") || field(res.stdout, "With a finding") != "2, 2.0 KiB" {
		t.Errorf("watch of a scrub: %+v", res)
	}
	r.must("job", "rebuild", "lab1", "--mode", "compare")
	r.fake.SetJob("lab1", "job_2", v1.JobState_JOB_STATE_RUNNING, counts(4, 0, 0, 0))
	r.fake.QueueEvent("lab1", jobStep("job_2", v1.JobState_JOB_STATE_DONE, counts(4, 4, 0, 0)))
	res = r.run("job", "watch", "lab1", "job_2")
	if !strings.Contains(res.stdout, "4 of 4 instances answered (100%): 4 cataloged;") {
		t.Errorf("a rebuild's progress is worded as a send's:\n%s", res.stdout)
	}
	if res.code != 0 || field(res.stdout, "State") != "done" || field(res.stdout, "Rebuilds") != "the catalog, compare" {
		t.Errorf("watch of a rebuild: %+v", res)
	}
}

// TestJobFindings guards "job findings" and "job finding": the filters (kind,
// storage, cleared) reach the server, the table never shows an object's key,
// one finding shows what was expected and what was found, an id the tenant
// lacks is exit 4, and an unknown kind is exit 2.
func TestJobFindings(t *testing.T) {
	r := scrubRig(t)
	at := timestamppb.New(fixedNow)
	objectKey := "tenants/lab1/1.2.826.0.1.3680043.8.498.99/obj"
	r.fake.AddScrubFinding("lab1", &v1.ScrubFinding{Id: "fnd_1", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_DAMAGED, Storage: "main", Key: objectKey,
		SubjectLabel: "sub-1", SessionLabel: "ses-1", ScanLabel: "scan-1", InstanceId: "i-1", ExpectedSize: proto.Int64(100), FoundSize: proto.Int64(100),
		ExpectedSha256: strings.Repeat("a", 64), FoundSha256: strings.Repeat("b", 64), Job: "job_1", FindTime: at})
	r.fake.AddScrubFinding("lab1", &v1.ScrubFinding{Id: "fnd_2", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_ORPHAN, Storage: "cold", Key: objectKey + "2", Job: "job_1", FindTime: at})
	r.fake.AddScrubFinding("lab1", &v1.ScrubFinding{Id: "fnd_3", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_MANIFEST_DAMAGED, Storage: "main", Key: objectKey + "3",
		SubjectLabel: "sub-2", SessionLabel: "ses-1", ScanLabel: "scan-9", Job: "job_1", FindTime: at, ClearTime: at, ClearJob: "job_2"})

	all := r.run("job", "findings", "lab1")
	if all.code != 0 || !strings.Contains(all.stdout, "fnd_1") || !strings.Contains(all.stdout, "damaged") || !strings.Contains(all.stdout, "sub-1/ses-1/scan-1") ||
		!strings.Contains(all.stdout, "orphan") || strings.Contains(all.stdout, "fnd_3") || strings.Contains(all.stdout, "1.2.826") ||
		!strings.Contains(all.stdout, "ID") || !strings.Contains(all.stdout, "holds") {
		t.Errorf("job findings:\n%s", all.stdout)
	}
	if res := r.run("job", "findings", "lab1", "--cleared"); res.code != 0 || !strings.Contains(res.stdout, "fnd_3") || !strings.Contains(res.stdout, "manifest-damaged") {
		t.Errorf("job findings --cleared:\n%s", res.stdout)
	}
	if res := r.run("job", "findings", "lab1", "--kind", "orphan", "--storage", "cold"); res.code != 0 || strings.Contains(res.stdout, "fnd_1") || !strings.Contains(res.stdout, "fnd_2") {
		t.Errorf("job findings --kind orphan:\n%s", res.stdout)
	}
	if res := r.run("job", "findings", "lab1", "--kind", "rotten"); res.code != 2 || !strings.Contains(res.stderr, "is not one of: missing, orphan, damaged, disagreeing, manifest-damaged") {
		t.Errorf("job findings --kind rotten: %+v", res)
	}
	doc := document(t, r.run("job", "findings", "lab1", "--json"))
	if list := doc["findings"].([]any); len(list) != 2 || list[0].(map[string]any)["kind"] != "SCRUB_FINDING_KIND_DAMAGED" {
		t.Errorf("job findings --json: %v", doc)
	}

	show := r.run("job", "finding", "lab1", "fnd_1")
	for label, want := range map[string]string{"Kind": "damaged", "State": "holds", "Storage": "main", "Scan": "sub-1/ses-1/scan-1",
		"Expected size": "100 bytes", "Expected SHA-256": strings.Repeat("a", 64), "Found SHA-256": strings.Repeat("b", 64), "Found by": "job_1", "Cleared": "-"} {
		if got := field(show.stdout, label); show.code != 0 || got != want {
			t.Errorf("job finding: %s is %q, want %q:\n%s", label, got, want, show.stdout)
		}
	}
	if strings.Contains(show.stdout, "1.2.826") {
		t.Errorf("job finding shows the object's key:\n%s", show.stdout)
	}
	if res := r.run("job", "finding", "lab1", "fnd_3"); res.code != 0 || field(res.stdout, "State") != "cleared" || field(res.stdout, "Cleared by") != "job_2" {
		t.Errorf("job finding of a cleared one: %+v", res)
	}
	if res := r.run("job", "finding", "lab1", "fnd_99"); res.code != 4 || res.stdout != "" {
		t.Errorf("job finding of an id not there: %+v", res)
	}
	whole := document(t, r.run("job", "finding", "lab1", "fnd_1", "--json"))
	if whole["id"] != "fnd_1" || whole["key"] != objectKey {
		t.Errorf("job finding --json: %v", whole)
	}
}
