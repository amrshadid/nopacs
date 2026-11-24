package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// jsonDirEnv names the directory TestWriteJSONOutputs writes into. The test
// is the first half of the outside check of the JSON documents: it runs every
// command with --json against the in-process server and leaves each output as
// a file, and scripts/check/cli-json.py, which is Python and not NoPACs, parses
// them with Python's json module.
const jsonDirEnv = "NOPACS_CHECK_JSON_DIR"

// fakeCorrection returns the id the fake gives the nth correction it makes.
func fakeCorrection(n int) string { return fmt.Sprintf("01jd%022d", n) }

// TestWriteJSONOutputs runs every command with --json and writes standard
// output to <dir>/<name>.json, for scripts/check/cli-json.py. It skips only when
// the variable is unset; "make check-json" always sets it.
func TestWriteJSONOutputs(t *testing.T) {
	dir := os.Getenv(jsonDirEnv)
	if dir == "" {
		t.Skipf("%s is not set; make check-json runs this test", jsonDirEnv)
	}
	r := newRig(t)
	r.fake.AddAudit(&v1.AuditEntry{Id: "1", Time: timestamppb.New(fixedNow), KeyId: "k1", Method: "/nopacs.v1.TenantService/CreateTenant", Object: "lab1", Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, RequestId: "req-1"})
	r.env["LAB2_DB"] = "postgres://lab2:x@db.example.org/lab2"
	r.env["LAB1_S3"] = "made-up-secret-access-key-for-the-test"
	for _, id := range []string{"rcp-retry", "rcp-discard"} {
		r.fake.AddReception("lab1", &v1.Reception{Id: id, Receiver: "scanner1", StudyInstanceUid: "1.2." + id, Arrival: 1, Step: v1.ReceptionStep_RECEPTION_STEP_FAILED, StartTime: timestamppb.New(fixedNow)})
	}
	for _, id := range []string{"q-release", "q-reject", "q-replace"} {
		r.fake.AddQuarantined("lab1", &v1.QuarantinedFile{Id: id, Reason: v1.QuarantineReason_QUARANTINE_REASON_BAD_LABEL, State: v1.QuarantineState_QUARANTINE_STATE_HELD, Receiver: "scanner1", ContentKept: true, QuarantineTime: timestamppb.New(fixedNow)}, `{}`)
	}
	r.fake.AddScrubFinding("lab1", &v1.ScrubFinding{Id: "fnd_1", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_MISSING, Storage: "main", Key: "k", SubjectLabel: "sub-1",
		SessionLabel: "ses-1", ScanLabel: "scan-1", InstanceId: "i-1", ExpectedSize: proto.Int64(10), Job: "job_2", FindTime: timestamppb.New(fixedNow)})
	work := t.TempDir()
	doc := writeFile(t, work, "research.json", basicJSON)
	ct, err := os.ReadFile(filepath.Join("..", "receive", "testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	dcm := writeFile(t, work, "one.dcm", string(ct))
	r.fake.SetTrialAnswer(&v1.TryProfileResponse{Checksum: "c", Results: []*v1.TrialResult{{Name: dcm, Verdict: v1.TrialVerdict_TRIAL_VERDICT_PASSED, DeidentificationOnly: true}},
		Summary: &v1.TrialSummary{Files: 1, Passed: 1}})
	r.fake.SetExplainAnswer(&v1.ExplainProfileResponse{Checksum: "c"})
	r.fake.SetCodeAnswer(&v1.CodeResponse{Attribute: "00100020", Code: "abcdefghijklmnop"})
	codes := writeFile(t, work, "codes.csv", "PID-0001,P001\n")
	routes := filepath.Join(work, "routes.json")
	steps := []struct {
		name string
		args []string
	}{
		{"version", []string{"version"}},
		{"whoami", []string{"whoami"}},
		{"tenant-create", []string{"tenant", "create", "lab1"}},
		{"tenant-create-postgres", []string{"tenant", "create", "lab2", "--db-env", "LAB2_DB"}},
		{"tenant-list", []string{"tenant", "list"}},
		{"tenant-show", []string{"tenant", "show", "lab1"}},
		{"storage-add", []string{"storage", "add", "lab1", "main", "--type", "dir", "--root", "fast"}},
		{"storage-add-s3", []string{"storage", "add", "lab1", "cold", "--type", "s3", "--endpoint", "https://s3.example", "--region", "r", "--bucket", "lab1-archive", "--access-key-id", "AKIAEXAMPLE", "--credential-env", "LAB1_S3"}},
		{"storage-list", []string{"storage", "list", "lab1"}},
		{"storage-show", []string{"storage", "show", "lab1", "cold"}},
		{"storage-set", []string{"storage", "set", "lab1", "cold", "--capacity", "1TiB"}},
		{"storage-test", []string{"storage", "test", "lab1", "main"}},
		{"receiver-add", []string{"receiver", "add", "lab1", "scanner1", "--storage", "main", "--port", "11112", "--aet", "NOPACS", "--allow-aet", "PRISMA1", "--enabled"}},
		{"receiver-list", []string{"receiver", "list", "lab1"}},
		{"receiver-show", []string{"receiver", "show", "lab1", "scanner1"}},
		{"receiver-set", []string{"receiver", "set", "lab1", "scanner1", "--quiet-period", "5m"}},
		{"receiver-route-export", []string{"receiver", "route", "export", "lab1", "scanner1", "--to", routes}},
		{"receiver-route-import", []string{"receiver", "route", "import", "lab1", "scanner1", routes}},
		{"receiver-add-import", []string{"receiver", "add", "lab1", "old-tree", "--type", "import", "--storage", "main", "--root", "old", "--layout", "plain"}},
		{"job-import", []string{"job", "import", "lab1", "old-tree", "proj1"}},
		{"job-files", []string{"job", "files", "lab1", "job_1"}},
		{"job-units", []string{"job", "units", "lab1", "job_1"}},
		{"job-scrub", []string{"job", "scrub", "lab1", "--storage", "main"}},
		{"job-findings", []string{"job", "findings", "lab1", "--cleared"}},
		{"job-finding", []string{"job", "finding", "lab1", "fnd_1"}},
		{"job-rebuild-compare", []string{"job", "rebuild", "lab1", "--mode", "compare"}},
		{"tenant-status", []string{"tenant", "status", "lab1"}},
		{"reception-list", []string{"reception", "list", "lab1"}},
		{"reception-show", []string{"reception", "show", "lab1", "rcp-retry"}},
		{"reception-retry", []string{"reception", "retry", "lab1", "rcp-retry"}},
		{"reception-discard", []string{"reception", "discard", "lab1", "rcp-discard", "--reason", "test data", "--yes"}},
		{"quarantine-list", []string{"quarantine", "list", "lab1", "--state", "held"}},
		{"quarantine-show", []string{"quarantine", "show", "lab1", "q-release", "--header"}},
		{"quarantine-release", []string{"quarantine", "release", "lab1", "q-release", "--reason", "naming rule fixed"}},
		{"quarantine-reject", []string{"quarantine", "reject", "lab1", "q-reject", "--reason", "test data", "--yes"}},
		{"receiver-remove", []string{"receiver", "remove", "lab1", "scanner1", "--yes"}},
		{"storage-remove", []string{"storage", "remove", "lab1", "cold", "--yes"}},
		{"key-create", []string{"key", "create", "--holder", "ci"}},
		{"key-list", []string{"key", "list"}},
		{"key-show", []string{"key", "show", "k00000000001"}},
		{"grant-set", []string{"grant", "set", "k00000000001", "--role", "reader", "--tenant", "lab1"}},
		{"grant-list", []string{"grant", "list", "--key-id", "k00000000001"}},
		{"grant-remove", []string{"grant", "remove", "k00000000001", "--tenant", "lab1"}},
		{"key-revoke", []string{"key", "revoke", "k00000000001"}},
		{"setting-get", []string{"setting", "get", "test.retention"}},
		{"setting-set", []string{"setting", "set", "test.retention", "90d", "--tenant", "lab1"}},
		{"setting-list", []string{"setting", "list", "--tenant", "lab1"}},
		{"setting-unset", []string{"setting", "unset", "test.retention", "--tenant", "lab1"}},
		{"audit-list", []string{"audit", "list"}},
		{"audit-verify", []string{"audit", "verify"}},
		{"audit-checkpoints", []string{"audit", "checkpoints"}},
		{"audit-export", []string{"audit", "export", "--out", filepath.Join(work, "audit.jsonl")}},
		{"audit-public-key", []string{"audit", "public-key"}},
		{"profile-create", []string{"profile", "create", "lab1", "research", "--document", doc}},
		{"profile-get", []string{"profile", "get", "lab1", "research"}},
		{"profile-list", []string{"profile", "list", "lab1"}},
		{"profile-versions", []string{"profile", "versions", "lab1", "research"}},
		{"profile-export", []string{"profile", "export", "lab1", "research@1"}},
		{"profile-validate", []string{"profile", "validate", "lab1", "--document", doc}},
		{"profile-describe", []string{"profile", "describe", "lab1", "research"}},
		{"profile-try", []string{"profile", "try", "lab1", "--profile", "research", dcm}},
		{"profile-explain", []string{"profile", "explain", "lab1", "--profile", "research", dcm}},
		{"profile-diff", []string{"profile", "diff", "lab1", "research@1", "research@1"}},
		{"profile-try-local", []string{"profile", "try", "--local", "--document", doc, dcm}},
		{"profile-lookup-import", []string{"profile", "lookup", "import", "lab1", "subjects", codes}},
		{"profile-lookup-list", []string{"profile", "lookup", "list", "lab1"}},
		{"deid-code", []string{"deid", "code", "lab1", "--patient-id", "PID-0001"}},
		{"correction-delete-preview", []string{"correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn", "--preview"}},
		{"correction-delete", []string{"correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn", "--yes"}},
		{"correction-list", []string{"correction", "list", "lab1"}},
		{"correction-show", []string{"correction", "show", "lab1", fakeCorrection(1)}},
		{"correction-undo", []string{"correction", "undo", "lab1", fakeCorrection(1), "--reason", "wrong subject"}},
		{"correction-delete-again", []string{"correction", "delete", "lab1", "sub-2/ses-1", "--reason", "consent withdrawn", "--yes"}},
		{"correction-purge", []string{"correction", "purge", "lab1", fakeCorrection(2), "--yes", "--wait"}},
		{"correction-allow", []string{"correction", "allow", "lab1", fakeCorrection(2), "--subject", "sub-2", "--reason", "consent given again"}},
		{"correction-relabel", []string{"correction", "relabel", "lab1", "sub-3", "--to", "sub-4", "--reason", "wrong subject id", "--wait"}},
		{"correction-replace", []string{"correction", "replace", "lab1", "q-replace", "--reason", "corrected file sent again"}},
		{"tenant-remove", []string{"tenant", "remove", "lab1", "--yes"}},
	}
	for _, s := range steps {
		res := r.run(append(s.args, "--json")...)
		if res.code != 0 || res.stderr != "" {
			t.Fatalf("nopacs %v: exit %d, standard error %q", s.args, res.code, res.stderr)
		}
		// The directory is the one make check-json made for this run.
		if err := os.WriteFile(filepath.Join(dir, s.name+".json"), []byte(res.stdout), 0o600); err != nil { //nolint:gosec // dir is the test's own scratch directory
			t.Fatal(err)
		}
	}
	// The contexts need a server that asks for a key, so they run against a
	// network fake of their own, with a file of contexts in the test's
	// private directory.
	n := newNetworkRig(t)
	keyFile := fakeserver.PrivateFile(t, testKey+"\n")
	cfgEnv := map[string]string{"NOPACS_CONFIG": filepath.Join(t.TempDir(), "contexts.json")}
	for _, s := range []struct {
		name string
		args []string
	}{
		{"context-add", []string{"context", "add", "prod", "--server", n.env[client.EnvServer], "--key-file", keyFile, "--ca-file", n.env[client.EnvCAFile]}},
		{"context-list", []string{"context", "list"}},
		{"context-show", []string{"context", "show", "prod"}},
		{"context-use", []string{"context", "use", "prod"}},
		{"context-remove", []string{"context", "remove", "prod"}},
	} {
		res := runWith(cfgEnv, append(s.args, "--json")...)
		if res.code != 0 || res.stderr != "" {
			t.Fatalf("nopacs %v: exit %d, standard error %q", s.args, res.code, res.stderr)
		}
		if err := os.WriteFile(filepath.Join(dir, s.name+".json"), []byte(res.stdout), 0o600); err != nil { //nolint:gosec // dir is the test's own scratch directory
			t.Fatal(err)
		}
	}
}
