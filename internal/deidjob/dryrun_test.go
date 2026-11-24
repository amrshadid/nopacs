package deidjob

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// rawSession archives public files as received into session ses-raw: an
// MR image, two CT images of one series, a secondary capture and an
// ultrasound cine; and returns their SOP Instance UIDs by name.
func (f *fixture) rawSession() map[string]string {
	f.t.Helper()
	names := map[string]string{}
	var scans []scanFiles
	for _, sc := range []struct {
		label string
		files []string
	}{{"mr", []string{"MR_small.dcm"}}, {"ct", []string{"ct5n-2062.dcm", "ct5n-2392.dcm"}},
		{"sc", []string{"SC_rgb_rle_2frame.dcm"}}, {"us", []string{"jpeg_3frame_nobot.dcm"}}} {
		s := scanFiles{label: sc.label}
		for _, n := range sc.files {
			b := corpus(f.t, n)
			_, _, names[n] = uidsOf(f.t, b)
			s.files = append(s.files, b)
		}
		scans = append(scans, s)
	}
	f.archive("ses-raw", scans...)
	return names
}

// TestDryRunReportsHoldsAndChangesNothing guards a dry run over an
// archived session: a profile version run over files archived as received
// passes the MR and CT images and reports the secondary capture and the
// ultrasound cine as files it would hold, each with its reason and cause,
// listed as would_hold; its tallies count what each rule did, the holds by
// reason and the files by class; and the run leaves every row of the
// tenant's database but the job's, and every byte of the storage, as they
// were.
func TestDryRunReportsHoldsAndChangesNothing(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	names := f.rawSession()
	before := f.snapshot()
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidDryRun, ref, "ses-raw")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.State != tenant.JobPartial || j.Counts.Instances != 5 || j.Counts.InstancesSent != 3 || j.Counts.InstancesRefused != 2 ||
		j.Counts.InstancesRemaining() != 0 {
		t.Fatalf("the dry run ended %s with %+v, want partial: 3 passed, 2 held", j.State, j.Counts)
	}
	got := f.failures(j.ID)
	want := map[string]string{"SC_rgb_rle_2frame.dcm": "burned_in_risk/secondary_capture", "jpeg_3frame_nobot.dcm": "burned_in_risk/modality"}
	if len(got) != len(want) {
		t.Errorf("the dry run listed %d failures, want %d: %+v", len(got), len(want), got)
	}
	for n, detail := range want {
		x := got[names[n]]
		if x.Code != tenant.FailureWouldHold || x.Detail != detail || x.SOPClassUID == "" {
			t.Errorf("%s: failure %+v, want would_hold %q with its class", n, x, detail)
		}
	}
	tallies := f.tallies(j.ID)
	for key, n := range map[string]int64{
		"hold|burned_in_risk|secondary_capture": 1, "hold|burned_in_risk|modality": 1,
		"class|1.2.840.10008.5.1.4.1.1.4|passed": 1, "class|1.2.840.10008.5.1.4.1.1.2|passed": 2,
		"class|1.2.840.10008.5.1.4.1.1.7|refused": 1,
	} {
		if tallies[key] != n {
			t.Errorf("tally %s = %d, want %d (all: %v)", key, tallies[key], n, tallies)
		}
	}
	if tallies["rule|maker|remove"] < 1 || tallies["rule|default|keep"]+tallies["rule|default|remove"] == 0 {
		t.Errorf("the tallies do not count what the rules did: %v", tallies)
	}
	for key := range tallies {
		if strings.Contains(key, "5962") || strings.Contains(key, "TOSHIBA") {
			t.Errorf("a tally holds a value of a file: %q", key)
		}
	}
	if after := f.snapshot(); after != before {
		t.Errorf("the dry run changed the tenant's database or storage:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestDryRunUsesNoTenantSecret guards that a dry run never runs under the
// tenant's keys: it never asks for them, the keys its unit runs under are
// none of the tenant's, so the UIDs it remaps differ from those the
// tenant's key makes (which the archive holds for a de-identified file),
// and it keeps no copy of a value of the files (TrialOptions.NoValues).
func TestDryRunUsesNoTenantSecret(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	names := f.rawSession()
	var mu sync.Mutex
	var unitKeys []engine.Keys
	values := 0
	cfg := withKeys(f.config(), func() (engine.Keys, error) {
		k, err := throwawayKeys()
		mu.Lock()
		defer mu.Unlock()
		unitKeys = append(unitKeys, engine.Keys{UID: bytes.Clone(k.UID), Date: bytes.Clone(k.Date), Hash: bytes.Clone(k.Hash)})
		return k, err
	})
	cfg = withTried(cfg, func(tr receive.Trial) {
		mu.Lock()
		defer mu.Unlock()
		if tr.Before != nil || tr.After != nil || tr.Written != nil {
			values++
		}
	})
	s := f.start(cfg)
	j := f.createDeid(tenant.DeidDryRun, ref, "ses-raw")
	s.Poke("lab")
	f.waitJob(s, j.ID, 30*time.Second, ended)
	if n := f.keyCalls.Load(); n != 0 {
		t.Errorf("a dry run asked for the tenant's keys %d times", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(unitKeys) == 0 || values != 0 {
		t.Fatalf("%d units made keys; %d trials kept a value", len(unitKeys), values)
	}
	orig := names["MR_small.dcm"]
	for _, k := range unitKeys {
		if bytes.Equal(k.UID, tenantKeys.UID) || bytes.Equal(k.Date, tenantKeys.Date) || bytes.Equal(k.Hash, tenantKeys.Hash) {
			t.Error("a unit runs under one of the tenant's keys")
		}
		if engine.RemapUID(k.UID, orig) == engine.RemapUID(tenantKeys.UID, orig) {
			t.Error("a unit remaps a UID as the tenant's key does")
		}
	}
}

// TestDryRunOfEachInstancesOwnVersion guards a dry run that names no
// version: each instance is run under the version its catalog row
// records, and one archived as received passes with a note.
func TestDryRunOfEachInstancesOwnVersion(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	mr := corpus(t, "MR_small.dcm")
	f.archive("ses-1", scanFiles{label: "mr", files: [][]byte{deidentify(t, mr, basicProfile(), ref)}, profile: ref})
	g := testkit.NewGenerator(t, mr)
	r, _ := g.File(1, 0)
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	f.archive("ses-2", scanFiles{label: "raw", files: [][]byte{raw}})
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidDryRun, tenant.ProfileRef{}, "")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 1 || j.Counts.InstancesWarned != 1 {
		t.Fatalf("a dry run of each instance's version ended %s with %+v, want done: 1 passed, 1 noted", j.State, j.Counts)
	}
	for _, x := range f.failures(j.ID) {
		if x.Code != tenant.FailureNotDeidentified {
			t.Errorf("the instance archived as received was noted %+v", x)
		}
	}
}
