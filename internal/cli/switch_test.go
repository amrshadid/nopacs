package cli_test

import (
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// repairRig is a replica rig with "copy" attached to "main" and two findings
// on main: a damaged object of instance i-1 and a missing one of i-2.
func repairRig(t *testing.T) *rig {
	t.Helper()
	r := replicaRig(t)
	r.must("replica", "attach", "lab1", "copy", "--primary", "main")
	for _, f := range []*v1.ScrubFinding{
		{Id: "fnd_1", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_DAMAGED, Storage: "main", InstanceId: "i-1", ExpectedSize: proto.Int64(10), FindTime: timestamppb.New(fixedNow)},
		{Id: "fnd_2", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_MISSING, Storage: "main", InstanceId: "i-2", ExpectedSize: proto.Int64(10), FindTime: timestamppb.New(fixedNow)},
		{Id: "fnd_3", Kind: v1.ScrubFindingKind_SCRUB_FINDING_KIND_ORPHAN, Storage: "main", InstanceId: "i-3", FindTime: timestamppb.New(fixedNow)},
	} {
		r.fake.AddScrubFinding("lab1", f)
	}
	return r
}

// TestRepairNamesWhatToMend guards "repair": exactly one of --finding,
// --instance and --all is needed (exit 2, nothing sent); findings are sent as
// given; an instance is turned into the ids of the findings that hold for it
// on the storage, and an instance with none is refused by name; a replica
// cannot be repaired (exit 5); the answer names the job.
func TestRepairNamesWhatToMend(t *testing.T) {
	r := repairRig(t)
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"repair", "lab1", "main"},
		{"repair", "lab1", "main", "--all", "--finding", "fnd_1"},
		{"repair", "lab1", "main", "--instance", "i-1", "--all"},
	} {
		if res := r.run(args...); res.code != 2 || res.stdout != "" {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if r.fake.Requests() != before {
		t.Errorf("refused commands sent %d requests", r.fake.Requests()-before)
	}
	res := r.run("repair", "lab1", "main", "--finding", "fnd_1", "--finding", "fnd_2", "--from", "copy")
	if res.code != 0 || !strings.Contains(res.stdout, "queued job job_2; follow it with: nopacs job watch lab1 job_2") {
		t.Fatalf("repair by finding: %+v", res)
	}
	if rj := r.fake.Job("lab1", "job_2").GetRepair(); rj.GetStorage() != "main" || rj.GetFrom() != "copy" || len(rj.GetFindings()) != 2 {
		t.Errorf("the repair job: %v", rj)
	}
	// An instance is mapped to its finding: the damaged object's.
	if res := r.run("repair", "lab1", "main", "--instance", "i-1"); res.code != 0 {
		t.Fatalf("repair by instance: %+v", res)
	}
	if rj := r.fake.Job("lab1", "job_3").GetRepair(); len(rj.GetFindings()) != 1 || rj.GetFindings()[0] != "fnd_1" {
		t.Errorf("the repair job of an instance: %v", rj)
	}
	// An orphan is not mended by a repair, so its instance has no finding to
	// repair; the command says which instance it could not find.
	if res := r.run("repair", "lab1", "main", "--instance", "i-3"); res.code != 4 || !strings.Contains(res.stderr, "no finding that holds for instance i-3") {
		t.Errorf("repair of an orphan's instance: %+v", res)
	}
	if res := r.run("repair", "lab1", "main", "--all"); res.code != 0 || r.fake.Job("lab1", "job_4").GetRepair().GetAll() != true {
		t.Errorf("repair --all: %+v", res)
	}
	if res := r.run("repair", "lab1", "copy", "--all"); res.code != 5 || !strings.Contains(res.stderr, "the server copies") {
		t.Errorf("repair of a replica: %+v", res)
	}
	doc := document(t, r.run("repair", "lab1", "main", "--all", "--json"))
	if doc["job"].(map[string]any)["repair"] == nil {
		t.Errorf("repair --json: %v", doc)
	}
}

// TestRepairWaitFollowsTheJob guards --wait: the command follows the repair
// job as "job watch" does and exits 0 when it ended done.
func TestRepairWaitFollowsTheJob(t *testing.T) {
	r := repairRig(t)
	finishLater(t, r, "job_2", v1.JobState_JOB_STATE_DONE)
	res := r.run("repair", "lab1", "main", "--all", "--wait")
	if res.code != 0 || !strings.Contains(res.stdout, "Repairs:") || !strings.Contains(res.stdout, "every finding that holds") {
		t.Errorf("repair --wait: %+v", res)
	}
}

// TestSwitchValidateThenConfirm guards the two steps of a switch. The
// preview shows what stands in the way, the receivers that would pause and the
// token, and exits 5 while anything blocks; the real call needs the token the
// preview gave (a wrong one is refused, exit 5) and, once made, shows the
// pause, the associations refused and the new generation; a token works once;
// the checks are passed over only with a reason.
func TestSwitchValidateThenConfirm(t *testing.T) {
	r := repairRig(t)
	r.fake.SetSwitchBlockers("lab1", "main", "replica not in step: 12 scans", "no clean scrub of the replica since its backfill")
	res := r.run("switch", "lab1", "main", "copy", "--validate")
	if res.code != 5 || !strings.Contains(res.stderr, "the switch is not ready: 2 things stand in its way") {
		t.Fatalf("a preview with blockers: %+v", res)
	}
	for _, want := range []string{"Ready:", "no\n", "Blocked by:", "replica not in step: 12 scans", "no clean scrub of the replica since its backfill",
		"Receivers paused:  scanner1", "Confirm token:     sw.main.copy.0"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("the preview lacks %q:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "to switch: nopacs switch") {
		t.Errorf("a preview that is not ready offers the command:\n%s", res.stdout)
	}
	// With the blockers gone the preview is ready and gives the command.
	r.fake.SetSwitchBlockers("lab1", "main")
	res = r.run("switch", "lab1", "main", "copy", "--validate")
	if res.code != 0 || !strings.Contains(res.stdout, "to switch: nopacs switch lab1 main copy --confirm sw.main.copy.0") {
		t.Fatalf("a ready preview: %+v", res)
	}
	word := "sw.main.copy.0"

	before := r.fake.Requests()
	for _, args := range [][]string{
		{"switch", "lab1", "main", "copy"},
		{"switch", "lab1", "main", "copy", "--confirm", ""},
		{"switch", "lab1", "main", "copy", "--validate", "--confirm", word},
		{"switch", "lab1", "main", "copy", "--confirm", word, "--skip-verification"},
		{"switch", "lab1", "main", "copy", "--confirm", word, "--reason", "x"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if r.fake.Requests() != before {
		t.Errorf("refused commands sent %d requests", r.fake.Requests()-before)
	}
	if res := r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.9"); res.code != 5 || !strings.Contains(res.stderr, "does not match") {
		t.Errorf("a wrong token: %+v", res)
	}
	res = r.run("switch", "lab1", "main", "copy", "--confirm", word)
	if res.code != 0 {
		t.Fatalf("the switch: %+v", res)
	}
	for _, want := range []string{"copy is now the primary main", "Writes paused for:", "1.5s", "Associations refused: 4", "Generation:           1"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("the switch said %q, lacking %q", res.stdout, want)
		}
	}
	if res := r.run("switch", "lab1", "main", "copy", "--confirm", word); res.code != 5 {
		t.Errorf("a token used twice: %+v", res)
	}
	// The status now shows the new generation on both storages.
	if out := mustOut(r, "storage", "status", "lab1", "main"); !strings.Contains(out, "Switch generation: 1\n") {
		t.Errorf("status after the switch:\n%s", out)
	}
}

// TestSwitchPassesVerificationOnlyWithAReason guards --skip-verification: a
// switch that a blocker refuses goes ahead with it and its reason, and the
// reason reaches the server.
func TestSwitchPassesVerificationOnlyWithAReason(t *testing.T) {
	r := repairRig(t)
	r.fake.SetSwitchBlockers("lab1", "main", "no empty compare from the replica")
	if res := r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0"); res.code != 5 || !strings.Contains(res.stderr, "no empty compare from the replica") {
		t.Errorf("a switch with a blocker: %+v", res)
	}
	res := r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0", "--skip-verification", "--reason", "compared on another host")
	if res.code != 0 || !strings.Contains(res.stdout, "copy is now the primary main") {
		t.Errorf("a switch with its checks passed over: %+v", res)
	}
}

// TestSwitchRefusalsShowTheirReasons guards what a refusal says: the
// server's reason and the delay to wait, on one line, with the exit code of
// the error's kind.
func TestSwitchRefusalsShowTheirReasons(t *testing.T) {
	r := repairRig(t)
	r.fake.FailWithReason(nopacsv1connect.ReplicaServiceSwitchStorageProcedure, fault.Unavailable, "a switch of this storage is already running", "STORAGE_SWITCHING", 20*time.Second)
	res := r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0")
	if res.code != 1 || res.stdout != "" ||
		!strings.Contains(res.stderr, "switch: STORAGE_SWITCHING: a switch of this storage is already running (try again in 20s)") {
		t.Errorf("a switch refused while another runs: %+v", res)
	}
}

// TestSwitchWaitReadsTheStatusAfterATimeout guards --wait: a call that the
// deadline ended stops nothing in the server, so with --wait the command
// reads the storage's status until its switch is over and shows it; without
// --wait the line says the switch goes on and where to read how it ended.
func TestSwitchWaitReadsTheStatusAfterATimeout(t *testing.T) {
	r := repairRig(t)
	r.fake.FailWith(nopacsv1connect.ReplicaServiceSwitchStorageProcedure, connect.CodeDeadlineExceeded, "the deadline passed")
	res := r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0")
	if res.code != 1 || !strings.Contains(res.stderr, "goes on in the server") || !strings.Contains(res.stderr, "nopacs storage status") {
		t.Errorf("a switch that timed out: %+v", res)
	}
	// A status that shows no rise of the generation is a switch that did not
	// happen, however the call ended.
	res = r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0", "--wait")
	if res.code != 5 || res.stdout != "" || !strings.Contains(res.stderr, "the switch did not happen") || !strings.Contains(res.stderr, "no answer from") {
		t.Errorf("a switch that timed out and did not happen, with --wait: %+v", res)
	}
	// The switch the timed-out call started ends in the server after the
	// command first reads the generation: the second read shows it risen.
	reads := 0
	r.fake.SetStorageStatus("lab1", "main", func(s *v1.StorageStatus) {
		reads++
		if reads > 1 {
			s.LastSwitchTime = timestamppb.New(fixedNow)
			s.SwitchGeneration = 1
		}
	})
	res = r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0", "--wait")
	if res.code != 0 || !strings.Contains(res.stdout, "the switch is over") || !strings.Contains(res.stdout, "Last switch:") || strings.Contains(res.stdout, "Last switch:        -") {
		t.Errorf("a switch that timed out, with --wait: %+v", res)
	}
}

// TestSwitchWaitTrustsAnAnswerOfTheServer guards --wait against reporting a
// refused switch as done: the server uses "unavailable" for real refusals (the
// drain did not finish in time, another switch runs), and an answer it sent is
// final, so the command exits non-zero with the server's reason and never
// waits. The drain timeout asked for is the one the refusal depends on.
func TestSwitchWaitTrustsAnAnswerOfTheServer(t *testing.T) {
	r := repairRig(t)
	r.fake.SetSwitchDrain("lab1", "main", 30*time.Minute)
	res := r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0", "--drain-timeout", "1m", "--wait")
	if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "the switch did not happen") || !strings.Contains(res.stderr, "did not end within the drain timeout") {
		t.Errorf("a drain that took too long, with --wait: %+v", res)
	}
	if out := mustOut(r, "storage", "status", "lab1", "main"); !strings.Contains(out, "Switch generation: 0\n") {
		t.Errorf("the refused switch changed the generation:\n%s", out)
	}
	if res := r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0", "--drain-timeout", "2h"); res.code != 2 || !strings.Contains(res.stderr, "drain_timeout") {
		t.Errorf("a drain timeout above the bound: %+v", res)
	}
	res = r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0", "--drain-timeout", "1h", "--wait")
	if res.code != 0 || !strings.Contains(res.stdout, "copy is now the primary main") {
		t.Errorf("a drain timeout long enough, with --wait: %+v", res)
	}

	r = repairRig(t)
	r.fake.FailWithReason(nopacsv1connect.ReplicaServiceSwitchStorageProcedure, fault.Unavailable, "a switch of this storage is already running", "STORAGE_SWITCHING", 20*time.Second)
	res = r.run("switch", "lab1", "main", "copy", "--confirm", "sw.main.copy.0", "--wait")
	if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "the switch did not happen") || !strings.Contains(res.stderr, "STORAGE_SWITCHING") {
		t.Errorf("a switch refused while another runs, with --wait: %+v", res)
	}
}
