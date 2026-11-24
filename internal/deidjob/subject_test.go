package deidjob

import (
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// longitudinalProfile shifts dates by subject (nopacs/longitudinal:1), so
// de-identifying a file needs its subject, which the profile keeps out of
// the archived file: Patient ID is emptied (PS3.15 Table E.1-1, Z).
func longitudinalProfile() *v1.ProfileDocument {
	return &v1.ProfileDocument{Bases: []string{"nopacs/longitudinal:1"}}
}

// TestJobsJudgeADateShiftingArchive guards both modes of a deid job over
// files a date-shifting version archived, whose Patient ID it emptied: the
// job identifies each file's subject by the archive's subject label, so a
// check passes them (no engine refusal) and a dry run of the very version
// that archived them says each would archive (no no_subject hold).
func TestJobsJudgeADateShiftingArchive(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("long", longitudinalProfile())
	archived := deidentify(t, corpus(t, "ct5n-2062.dcm"), longitudinalProfile(), ref)
	f.archive("ses-1", scanFiles{label: "ct", files: [][]byte{archived}, profile: ref})
	s := f.start(f.config())
	for _, c := range []struct {
		mode tenant.DeidMode
		ref  tenant.ProfileRef
	}{{tenant.DeidCheck, tenant.ProfileRef{}}, {tenant.DeidDryRun, ref}} {
		j := f.createDeid(c.mode, c.ref, "ses-1")
		s.Poke("lab")
		j = f.waitJob(s, j.ID, 30*time.Second, ended)
		if j.State != tenant.JobDone || j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 0 {
			t.Errorf("a %v job over a date-shifting archive ended %s with %+v, failures %+v, tallies %v",
				c.mode, j.State, j.Counts, f.failures(j.ID), f.tallies(j.ID))
		}
	}
}
