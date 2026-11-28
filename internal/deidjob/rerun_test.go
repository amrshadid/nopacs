package deidjob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// fakeReviser records the revisions a re-run asks to publish and answers
// each as answer says (nil for all published).
type fakeReviser struct {
	mu     sync.Mutex
	got    []Revision
	answer func(r Revision) error
	after  func(n int)
}

// Revise implements Reviser.
func (f *fakeReviser) Revise(_ context.Context, r Revision) error {
	f.mu.Lock()
	f.got = append(f.got, r)
	n := len(f.got)
	f.mu.Unlock()
	if f.after != nil {
		f.after(n)
	}
	if f.answer != nil {
		return f.answer(r)
	}
	return nil
}

// revisions returns the revisions asked so far, by instance.
func (f *fakeReviser) revisions() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, r := range f.got {
		out[r.Target.Instance]++
	}
	return out
}

// withReviser makes a configuration serve re-runs through r.
func withReviser(c Config, r Reviser) Config {
	c.Revisions = func(context.Context, string, tenant.Store) (Reviser, func(), error) { return r, func() {}, nil }
	return c
}

// holdsKVP reads whether a file holds KVP (0018,0060).
func holdsKVP(t *testing.T, b []byte) bool {
	t.Helper()
	h, err := header.ReadWithin(bytes.NewReader(b), int64(len(b)), header.MaxHeader)
	if err != nil {
		t.Fatal(err)
	}
	ds, _, _, err := h.Dataset(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return ds.Contains(tag.New(0x0018, 0x0060))
}

// rerunSession archives, de-identified by the first version, two CT
// images of one scan (which hold KVP) and an MR image (which does not).
func (f *fixture) rerunSession() (map[string][]byte, tenant.ProfileRef) {
	f.t.Helper()
	ref := f.storeVersion("research", basicProfile())
	files := f.deidSession("ses-1", ref, map[string][]string{"ct": {"ct5n-2062.dcm", "ct5n-2392.dcm"}, "mr": {"MR_small.dcm"}}, nil)
	return files, ref
}

// TestRerunRevisesOnlyWhatTheNewerVersionChanges guards the re-run: a
// newer version that removes KVP gives the two CT images, which hold it, a
// new revision without it, each named by its labels and UID, made from the
// archived checksum and under the newer version; the MR image, which holds
// none, is skipped and not rewritten; the job's tallies say so, with the
// element and its action and no value.
func TestRerunRevisesOnlyWhatTheNewerVersionChanges(t *testing.T) {
	f := newFixture(t)
	files, _ := f.rerunSession()
	newer := f.storeVersion("research", withRules(kwRule("kvp", "KVP", removeAct)))
	rev := &fakeReviser{}
	s := f.start(withReviser(f.config(), rev))
	j := f.createDeid(tenant.DeidRerun, newer, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 3 || j.Counts.InstancesRefused != 0 {
		t.Fatalf("the re-run ended %s with %+v (failures %+v), want done: 3 passed", j.State, j.Counts, f.failures(j.ID))
	}
	got := rev.revisions()
	_, _, ct1 := uidsOf(t, files["ct5n-2062.dcm"])
	_, _, ct2 := uidsOf(t, files["ct5n-2392.dcm"])
	_, _, mr := uidsOf(t, files["MR_small.dcm"])
	if len(got) != 2 || got[ct1] != 1 || got[ct2] != 1 || got[mr] != 0 {
		t.Fatalf("revisions asked for %v, want the two CT images once each and not the MR image", got)
	}
	for _, r := range rev.got {
		if holdsKVP(t, r.File) || r.Profile != newer || r.Target.Subject != "sub-01" || r.Target.Session != "ses-1" || r.Target.Scan != "ct" ||
			r.OldSHA256 != tenant.SHA256(sha256.Sum256(files["ct5n-2062.dcm"])) && r.OldSHA256 != tenant.SHA256(sha256.Sum256(files["ct5n-2392.dcm"])) {
			t.Errorf("a revision is %+v, want the file without KVP under %s of an archived CT image", r.Target, newer)
		}
	}
	sum := ""
	if v, err := f.p.(tenant.Deidentification).ProfileVersion(f.ctx, newer.Name, newer.Version); err == nil {
		sum = v.Checksum
	}
	for _, r := range rev.got {
		if r.ProfileSHA256 == "" || r.ProfileSHA256 != sum || r.Base != "" || r.Unnamed == nil {
			t.Errorf("a revision records checksum %q (want %q), base %q, unnamed %v: want the newer run's", r.ProfileSHA256, sum, r.Base, r.Unnamed)
		}
	}
	tallies := f.tallies(j.ID)
	for key, n := range map[string]int64{"rerun|revised": 2, "rerun|unchanged": 1, "rerun|element|00180060|remove": 2} {
		if tallies[key] != n {
			t.Errorf("tally %s = %d, want %d (all: %v)", key, tallies[key], n, tallies)
		}
	}
}

// TestRerunPreviewWritesNothing guards the preview: it counts what the
// re-run would change, by element and action, and asks the reviser for
// nothing.
func TestRerunPreviewWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.rerunSession()
	newer := f.storeVersion("research", withRules(kwRule("kvp", "KVP", removeAct)))
	rev := &fakeReviser{}
	s := f.start(withReviser(f.config(), rev))
	before := f.snapshot()
	j := f.createDeid(tenant.DeidRerunPreview, newer, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	tallies := f.tallies(j.ID)
	if len(rev.revisions()) != 0 || tallies["rerun|would_change"] != 2 || tallies["rerun|unchanged"] != 1 || tallies["rerun|element|00180060|remove"] != 2 {
		t.Errorf("the preview asked %v and tallied %v, want nothing asked, 2 would change, 1 unchanged", rev.revisions(), tallies)
	}
	if after := f.snapshot(); after != before {
		t.Error("the preview changed the tenant's rows or the storage")
	}
}

// TestRerunRefusesALooserVersion guards the layer rule: a version that
// keeps what the archived files' version removes would need the original,
// which de-identified data does not keep, so every instance is refused as
// loosening the lower layer and nothing is revised.
func TestRerunRefusesALooserVersion(t *testing.T) {
	f := newFixture(t)
	f.rerunSession()
	looser := f.storeVersion("research", withRules(kwRule("institution", "InstitutionName", keepAct)))
	rev := &fakeReviser{}
	s := f.start(withReviser(f.config(), rev))
	j := f.createDeid(tenant.DeidRerun, looser, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.State != tenant.JobFailed && j.State != tenant.JobPartial || j.Counts.InstancesRefused != 3 || len(rev.revisions()) != 0 {
		t.Fatalf("the looser re-run ended %s with %+v and %v revisions, want 3 refused and none made", j.State, j.Counts, rev.revisions())
	}
	for u, x := range f.failures(j.ID) {
		if x.Code != tenant.FailureWouldHold || x.Detail != string(profile.CodeLoosensLowerLayer) {
			t.Errorf("%s: failure %+v, want would_hold %q", u, x, string(profile.CodeLoosensLowerLayer))
		}
	}
}

// TestRerunHoldsWhatNeedsTheOriginal guards a held file: a version that
// would hash an element the older one kept needs the original value, so
// the file is held with a fixed cause and no revision is asked for.
func TestRerunHoldsWhatNeedsTheOriginal(t *testing.T) {
	f := newFixture(t)
	f.rerunSession()
	newer := f.storeVersion("research", withRules(kwRule("kernel", "ConvolutionKernel", hashAct)))
	rev := &fakeReviser{}
	s := f.start(withReviser(f.config(), rev))
	j := f.createDeid(tenant.DeidRerun, newer, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.Counts.InstancesRefused != 2 || len(rev.revisions()) != 0 {
		t.Fatalf("the re-run counts %+v and revised %v, want the 2 CT images held and none revised", j.Counts, rev.revisions())
	}
	for u, x := range f.failures(j.ID) {
		if x.Code != tenant.FailureWouldHold || x.Detail != "deid_failed/rerun_needs_original" {
			t.Errorf("%s: failure %+v, want would_hold deid_failed/rerun_needs_original", u, x)
		}
	}
}

// TestRerunSkipsWhatAHoldCovers guards the answers of the corrections: an
// instance a hold covers is recorded as refused with its fixed reason and
// the rest are revised; one changed since it was judged is a note.
func TestRerunSkipsWhatAHoldCovers(t *testing.T) {
	f := newFixture(t)
	files, _ := f.rerunSession()
	newer := f.storeVersion("research", withRules(kwRule("kvp", "KVP", removeAct)))
	_, _, ct1 := uidsOf(t, files["ct5n-2062.dcm"])
	rev := &fakeReviser{answer: func(r Revision) error {
		if r.Target.Instance == ct1 {
			return fault.Wrap(fault.Precondition, tenant.ErrHeld, "a hold covers it")
		}
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "it changed")
	}}
	s := f.start(withReviser(f.config(), rev))
	j := f.createDeid(tenant.DeidRerun, newer, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	got := f.failures(j.ID)
	if x := got[ct1]; x.Code != tenant.FailureWouldHold || x.Detail != detailHeldByHold {
		t.Errorf("the held instance: %+v, want would_hold %q", x, detailHeldByHold)
	}
	noted := 0
	for u, x := range got {
		if u != ct1 && x.Code == tenant.FailureUndecided && x.Detail == detailChangedSince {
			noted++
		}
	}
	if noted != 1 || j.Counts.InstancesRefused != 1 {
		t.Errorf("failures %+v, counts %+v, want one refused for the hold and one note for a change", got, j.Counts)
	}
}

// TestRerunGoesOnAfterARestart guards the resume: stopped after the first
// revision and started again, the job revises every instance once, none
// twice (the stop records what was judged), and ends done.
func TestRerunGoesOnAfterARestart(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.deidSession("ses-1", ref, map[string][]string{"ct": {"ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm"}}, nil)
	newer := f.storeVersion("research", withRules(kwRule("kvp", "KVP", removeAct)))
	reached := make(chan struct{})
	var once sync.Once
	rev := &fakeReviser{after: func(n int) {
		if n == 1 {
			once.Do(func() { close(reached) })
		}
	}}
	j := f.createDeid(tenant.DeidRerun, newer, "ses-1")
	s1, err := New(withReviser(f.config(), rev))
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	s1.Poke("lab")
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("the re-run asked for no revision")
	}
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	if err := s1.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	s2 := f.start(withReviser(f.config(), rev))
	s2.Poke("lab")
	j = f.waitJob(s2, j.ID, 30*time.Second, ended)
	got := rev.revisions()
	total := 0
	for u, n := range got {
		total += n
		if n != 1 {
			t.Errorf("instance %s was revised %d times", u, n)
		}
	}
	if j.State != tenant.JobDone || total != 4 || j.Counts.InstancesRemaining() != 0 {
		t.Errorf("the job ended %s with %+v and %d revisions, want done and 4", j.State, j.Counts, total)
	}
}

// The actions the re-run tests' rules take.
var (
	removeAct = &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	keepAct   = &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}
	hashAct   = &v1.ProfileAction{Kind: &v1.ProfileAction_Hash{Hash: &v1.HashAction{}}}
)

// kwRule is a rule of one keyword with an action.
func kwRule(name, keyword string, a *v1.ProfileAction) *v1.ProfileRule {
	return &v1.ProfileRule{Name: name, Selector: kw(keyword), Action: a}
}

// TestChangeTrialCountsPerElementWithoutValues guards change control: a
// version tried over a receiver's newest sessions counts the files it would
// change, leave and hold, names each element it changes with its action and
// how many files, and why files would be held, and nothing of a file's
// values is in the answer.
func TestChangeTrialCountsPerElementWithoutValues(t *testing.T) {
	f := newFixture(t)
	f.rerunSession()
	s := f.start(withReviser(f.config(), &fakeReviser{}))
	removes := f.storeVersion("research", withRules(kwRule("kvp", "KVP", removeAct)))
	got, err := s.TryChange(f.ctx, "lab", ChangeTrial{Receiver: "recv", Profile: removes, Sessions: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions != 1 || got.Files != 2 || got.Changed != 1 || got.Unchanged != 1 || got.Held != 0 ||
		len(got.Elements) != 1 || got.Elements[0] != (TrialElement{Tag: "00180060", Action: "remove", Files: 1}) {
		t.Errorf("the trial of a version that removes KVP found %+v", got)
	}
	hashes := f.storeVersion("research", withRules(kwRule("kernel", "ConvolutionKernel", hashAct)))
	held, err := s.TryChange(f.ctx, "lab", ChangeTrial{Receiver: "recv", Profile: hashes, Sessions: 5})
	if err != nil || held.Held != 1 || len(held.Holds) != 1 || held.Holds[0].Reason != "deid_failed/rerun_needs_original" {
		t.Errorf("the trial of a version that needs the original found %+v (%v)", held, err)
	}
	for _, r := range []ChangeTrialResult{got, held} {
		if text := fmt.Sprintf("%+v", r); strings.Contains(text, "STANDARD") || strings.Contains(text, "120") {
			t.Errorf("a trial's answer holds a value of a file: %s", text)
		}
	}
	none, err := s.TryChange(f.ctx, "lab", ChangeTrial{Receiver: "nobody", Profile: removes, Sessions: 5})
	if err != nil || none.Sessions != 0 || none.Files != 0 {
		t.Errorf("a receiver that received nothing: %+v (%v)", none, err)
	}
}

// TestRerunUnderABaseRecordsTheBase guards a re-run on a tenant with a base
// profile: the newer version, stacked over the base as a reception runs it,
// still revises the files that hold what it removes, and each revision
// records the base in force.
func TestRerunUnderABaseRecordsTheBase(t *testing.T) {
	f := newFixture(t)
	f.rerunSession()
	base := f.storeVersion("floor", basicProfile())
	if err := f.p.SetSetting(f.ctx, settings.DeidBaseProfile, base.String(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	newer := f.storeVersion("research", withRules(kwRule("kvp", "KVP", removeAct)))
	rev := &fakeReviser{}
	s := f.start(withReviser(f.config(), rev))
	j := f.createDeid(tenant.DeidRerun, newer, "ses-1")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if len(rev.got) != 2 || j.Counts.InstancesRefused != 0 {
		t.Fatalf("under a base the re-run revised %d files and refused %d (failures %+v)", len(rev.got), j.Counts.InstancesRefused, f.failures(j.ID))
	}
	for _, r := range rev.got {
		if r.Base != base.String() {
			t.Errorf("a revision records the base %q, want %q", r.Base, base)
		}
	}
}
