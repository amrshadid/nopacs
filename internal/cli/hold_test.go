package cli_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestHoldListAndShow guards the holds a server serves today: listing by
// state and subject, and showing one with its reason and the storages that
// hold or are owed its record.
func TestHoldListAndShow(t *testing.T) {
	r := newArchiveRig(t)
	r.fake.AddHold("lab1", &v1.Hold{Id: "hold_a", Target: &v1.HoldTarget{SubjectLabel: "sub-1", SessionLabel: "ses-1"}, Reason: "litigation 114",
		State: v1.HoldState_HOLD_STATE_ACTIVE, PlacedByKeyId: "k1", PlaceTime: timestamppb.New(fixedNow), Storages: []string{"main"}, StoragesPending: []string{"copy"}})
	r.fake.AddHold("lab1", &v1.Hold{Id: "hold_b", Target: &v1.HoldTarget{WholeTenant: true}, Reason: "audit", State: v1.HoldState_HOLD_STATE_RELEASED,
		PlacedByKeyId: "k1", PlaceTime: timestamppb.New(fixedNow), ReleaseReason: "done", ReleaseTime: timestamppb.New(fixedNow), ReleasedByKeyId: "k2"})
	out := mustOut(r, "hold", "list", "lab1")
	if !strings.Contains(out, "hold_a  sub-1/ses-1") || !strings.Contains(out, "hold_b  whole tenant") || !strings.Contains(out, "litigation 114") {
		t.Errorf("list:\n%s", out)
	}
	if out := mustOut(r, "hold", "list", "lab1", "--state", "active"); strings.Contains(out, "hold_b") || !strings.Contains(out, "hold_a") {
		t.Errorf("list --state active:\n%s", out)
	}
	if out := mustOut(r, "hold", "list", "lab1", "--subject", "sub-1"); strings.Contains(out, "hold_b") {
		t.Errorf("list --subject:\n%s", out)
	}
	if res := r.run("hold", "list", "lab1", "--state", "gone"); res.code != 2 {
		t.Errorf("an unknown state: %+v", res)
	}
	show := mustOut(r, "hold", "show", "lab1", "hold_a")
	if field(show, "Covers") != "sub-1/ses-1" || field(show, "Record owed to") != "copy" || field(show, "Reason") != "litigation 114" {
		t.Errorf("show:\n%s", show)
	}
	if out := mustOut(r, "hold", "show", "lab1", "hold_b"); field(out, "Released by") != "k2" || field(out, "Release reason") != "done" {
		t.Errorf("show of a released hold:\n%s", out)
	}
	if res := r.run("hold", "show", "lab1", "hold_x"); res.code != 4 {
		t.Errorf("show of a hold that is not there: %+v", res)
	}
	if hs, _ := document(t, r.run("hold", "list", "lab1", "--json"))["holds"].([]any); len(hs) != 2 {
		t.Errorf("list --json: %v", hs)
	}
}

// TestHoldPlaceAndReleaseNotServed guards the way holds are placed and
// released while a server does not serve them: one plain line, exit 1,
// nothing changed; and once served, placing needs a target or --whole-tenant
// and a reason, and releasing asks for the hold's id typed unless --yes.
func TestHoldPlaceAndReleaseNotServed(t *testing.T) {
	r := newArchiveRig(t)
	expect(t, r.run("hold", "place", "lab1", "sub-1", "--reason", "litigation"), 1, "",
		"nopacs: hold place: not served: this server does not serve it yet; nothing was changed\n")
	expect(t, r.run("hold", "release", "lab1", "hold_a", "--reason", "closed", "--yes"), 1, "",
		"nopacs: hold release: not served: this server does not serve it yet; nothing was changed\n")

	r.fake.ServeHolds()
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"hold", "place", "lab1", "--reason", "x"},
		{"hold", "place", "lab1", "sub-1", "--whole-tenant", "--reason", "x"},
		{"hold", "place", "lab1", "sub-1//scan", "--reason", "x"},
		{"hold", "place", "lab1", "sub-1", "--reason", " "},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if r.fake.Requests() != before {
		t.Errorf("refused commands sent %d requests", r.fake.Requests()-before)
	}
	res := r.run("hold", "place", "lab1", "sub-1/ses-1", "--reason", "litigation")
	if res.code != 0 || !strings.Contains(res.stdout, "hold hold_1 placed on sub-1/ses-1") {
		t.Fatalf("place: %+v", res)
	}
	if res := r.run("hold", "place", "lab1", "--whole-tenant", "--reason", "audit"); res.code != 0 || !strings.Contains(res.stdout, "placed on whole tenant") {
		t.Errorf("place on the whole tenant: %+v", res)
	}
	if res := r.run("hold", "release", "lab1", "hold_1", "--reason", "closed"); res.code != 2 || !strings.Contains(res.stderr, "nothing was released") {
		t.Errorf("release with no terminal and no --yes: %+v", res)
	}
	if res := typed(r.env, "hold_2\n", "hold", "release", "lab1", "hold_1", "--reason", "closed"); res.code != 2 {
		t.Errorf("release with a wrong id typed: %+v", res)
	}
	if show := mustOut(r, "hold", "show", "lab1", "hold_1"); field(show, "State") != "active" {
		t.Errorf("a refused release released:\n%s", show)
	}
	if res := typed(r.env, "hold_1\n", "hold", "release", "lab1", "hold_1", "--reason", "closed"); res.code != 0 || !strings.Contains(res.stdout, "hold hold_1 released") {
		t.Errorf("release with the id typed: %+v", res)
	}
}

// TestStorageStatusShowsTheNewFields guards "storage status": the table and
// the detail show tier, replica and its lag, whether a switch is running and
// when the last ended, encryption at rest worded no more strongly than the
// storage or its admin said, and scrub coverage; a storage that is not in the
// status is refused (exit 4).
func TestStorageStatusShowsTheNewFields(t *testing.T) {
	r := replicaRig(t)
	r.must("replica", "attach", "lab1", "copy", "--primary", "main")
	r.fake.SetReplicaLag("lab1", &v1.ReplicaStatus{Replica: "copy", ScansInStep: 40, ScansPending: 2, Lag: durationpb.New(90 * time.Minute)})
	r.fake.SetStorageStatus("lab1", "main", func(s *v1.StorageStatus) {
		s.Switching = true
		s.EncryptionAtRest = v1.EncryptionAtRest_ENCRYPTION_AT_REST_DECLARED
		s.Coverage = &v1.ScrubCoverage{Objects: 1000, Verified: 900, Share: 0.9}
	})
	r.fake.SetStorageStatus("lab1", "copy", func(s *v1.StorageStatus) {
		s.EncryptionAtRest = v1.EncryptionAtRest_ENCRYPTION_AT_REST_ENCRYPTED
		s.EncryptionAlgorithm = "aws:kms"
		s.LastSwitchTime = timestamppb.New(fixedNow)
		s.Coverage = &v1.ScrubCoverage{Objects: 500, Verified: 100, Share: 0.2, PresenceOnly: 50, Overdue: 7, OldestUnverified: durationpb.New(40 * 24 * time.Hour)}
	})
	expect(t, r.run("storage", "status", "lab1"), 0,
		"STORAGE  TIER    REPLICA OF  USED  FREE     SWITCHING  ENCRYPTED  COVERAGE\n"+
			"copy     online  main        0 B   unknown  no         yes        20%\n"+
			"main     online  -           0 B   unknown  yes        declared   90%\n", "")
	copyOut := mustOut(r, "storage", "status", "lab1", "copy")
	for label, want := range map[string]string{
		"Replica of":         "main",
		"Behind its primary": "40 scans in step, 2 waiting, behind by 1h",
		"Encrypted at rest":  "yes, aws:kms (the storage says so)",
		"Scrub coverage":     "20.0% of 500 objects read within the interval, 50 checked by presence only, 7 overdue, the oldest unread for 40d",
	} {
		if got := field(copyOut, label); got != want {
			t.Errorf("copy: %s is %q, want %q:\n%s", label, got, want, copyOut)
		}
	}
	mainOut := mustOut(r, "storage", "status", "lab1", "main")
	if field(mainOut, "Encrypted at rest") != "declared by the admin (not checked by the server)" || !strings.HasPrefix(field(mainOut, "Switching"), "yes: its receivers refuse") {
		t.Errorf("main:\n%s", mainOut)
	}
	if res := r.run("storage", "status", "lab1", "nope"); res.code != 4 {
		t.Errorf("a storage that is not in the status: %+v", res)
	}
	doc := document(t, r.run("storage", "status", "lab1", "--json"))
	if ts, _ := doc["tenants"].([]any); len(ts) != 1 {
		t.Errorf("status --json: %v", doc)
	}
}

// TestStorageFlagsForTierAndEncryption guards the new flags of add and set:
// a replica is added with --replica-of, the tier and recall settings and the
// directory's declaration of encryption reach the server by the right mask
// path, flags of the other type are refused before anything is sent, and
// --replica-of cannot be changed by set.
func TestStorageFlagsForTierAndEncryption(t *testing.T) {
	r := newArchiveRig(t)
	r.env["S3SECRET"] = s3Secret
	res := r.run("storage", "add", "lab1", "copy", "--type", "dir", "--root", "slow", "--replica-of", "main", "--encrypted-at-rest-declared")
	if res.code != 0 || field(res.stdout, "Replica of") != "main" || field(res.stdout, "Encrypted at rest") != "declared by the admin (not checked by the server)" {
		t.Fatalf("add a replica: %+v", res)
	}
	res = r.run("storage", "add", "lab1", "cold", "--type", "s3", "--endpoint", "https://s3.example", "--region", "r", "--bucket", "b", "--access-key-id", "AKIAEXAMPLE",
		"--credential-env", "S3SECRET", "--replica-of", "main", "--tier", "archive", "--recall-days", "30", "--recall-tier", "expedited")
	if res.code != 0 || field(res.stdout, "Tier") != "archive" || field(res.stdout, "Recall days") != "30" || field(res.stdout, "Recall tier") != "expedited" {
		t.Fatalf("add an archive-tier replica: %+v", res)
	}
	if res := r.run("storage", "set", "lab1", "cold", "--recall-days", "7", "--recall-tier", "bulk"); res.code != 0 || field(res.stdout, "Recall days") != "7" || field(res.stdout, "Recall tier") != "bulk" {
		t.Errorf("set the recall settings: %+v", res)
	}
	if res := r.run("storage", "set", "lab1", "copy", "--encrypted-at-rest-declared=false"); res.code != 0 || field(res.stdout, "Encrypted at rest") != "not declared" {
		t.Errorf("set the declaration back: %+v", res)
	}
	if res := r.run("storage", "set", "lab1", "cold", "--tier", "online"); res.code != 0 || field(res.stdout, "Tier") != "online" {
		t.Errorf("set the tier: %+v", res)
	}
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"storage", "set", "lab1", "copy", "--replica-of", "main"},
		{"storage", "set", "lab1", "copy", "--tier", "cold"},
		{"storage", "set", "lab1", "cold", "--recall-tier", "slow"},
		{"storage", "add", "lab1", "d2", "--type", "dir", "--root", "slow", "--recall-days", "3"},
		{"storage", "add", "lab1", "d3", "--type", "dir", "--root", "slow", "--tier", "glacier"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if r.fake.Requests() != before {
		t.Errorf("refused commands sent %d requests", r.fake.Requests()-before)
	}
	r.must("storage", "add", "lab1", "cold2", "--type", "s3", "--endpoint", "https://s3.example", "--region", "r", "--bucket", "b2", "--access-key-id", "AKIAEXAMPLE", "--credential-env", "S3SECRET")
	if res := r.run("storage", "add", "lab1", "cold3", "--type", "s3", "--endpoint", "https://s3.example", "--region", "r", "--bucket", "b3", "--access-key-id", "AKIAEXAMPLE",
		"--credential-env", "S3SECRET", "--encrypted-at-rest-declared"); res.code != 2 || !strings.Contains(res.stderr, "directory storage") {
		t.Errorf("a declaration on an S3 storage: %+v", res)
	}
}

// TestKeyListOfATenantAndGrantsByATenantAdmin guards "key list --tenant":
// only the keys that hold a grant on the tenant are listed, with their id and
// holder only, and "grant set" and "grant remove" pass the tenant, so a
// tenant's admin can use them on its own tenant; a refusal of the server
// (a tenant's admin acting on its own key) is shown with exit 3.
func TestKeyListOfATenantAndGrantsByATenantAdmin(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("key", "create", "--holder", "alice")
	r.must("key", "create", "--holder", "bob")
	r.must("grant", "set", "k00000000001", "--role", "reader", "--tenant", "lab1")
	out := mustOut(r, "key", "list", "--tenant", "lab1")
	if !strings.Contains(out, "ID") || !strings.Contains(out, "HOLDER") || !strings.Contains(out, "alice") || strings.Contains(out, "bob") || strings.Contains(out, "STATUS") {
		t.Errorf("key list --tenant:\n%s", out)
	}
	if ks, _ := document(t, r.run("key", "list", "--tenant", "lab1", "--json"))["keys"].([]any); len(ks) != 1 {
		t.Errorf("key list --tenant --json: %v", ks)
	}
	if out := mustOut(r, "key", "list"); !strings.Contains(out, "bob") || !strings.Contains(out, "STATUS") {
		t.Errorf("key list: %s", out)
	}
}
