package storageswitch

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// fakeJobs is the tenant's database with the jobs a test sets in place of
// its own: a scrub or a compare is set done with its tallies, rather than
// run.
type fakeJobs struct {
	DB
	jobs    []tenant.Job
	tallies map[string][]tenant.JobTally
}

// ListJobs answers the test's jobs the filter keeps, in one page, newest
// (last added) first, as the tenant's database lists them.
func (j *fakeJobs) ListJobs(_ context.Context, filter tenant.JobFilter, _ master.Page) ([]tenant.Job, string, error) {
	var out []tenant.Job
	for _, job := range slices.Backward(j.jobs) {
		if job.Created.Before(filter.Since) || (len(filter.States) > 0 && !slices.Contains(filter.States, job.State)) {
			continue
		}
		out = append(out, job)
	}
	return out, "", nil
}

// JobTallies answers the test's tallies of the job.
func (j *fakeJobs) JobTallies(_ context.Context, id string) ([]tenant.JobTally, error) {
	return j.tallies[id], nil
}

// add sets job done now, with its tallies.
func (j *fakeJobs) add(job tenant.Job, tallies ...tenant.JobTally) {
	job.State, job.Created = tenant.JobDone, time.Now().UTC()
	j.jobs = append(j.jobs, job)
	if j.tallies == nil {
		j.tallies = map[string][]tenant.JobTally{}
	}
	j.tallies[job.ID] = tallies
}

// compareFrom is a CompareFrom that tells the storage of each compare job
// by the job's id, as RebuildJob.from_storage would.
func compareFrom(from map[string]string) CompareFrom {
	return func(j tenant.Job) string { return from[j.ID] }
}

// TestValidateListsEachBlocker guards the preview: a replica not yet in
// step, with no backfill ended, is blocked by its pending scans and both
// checks of verification; once in step, by the checks alone; a scrub of
// the replica that found something, a scrub of another storage and a
// compare that found a difference lift nothing; a clean scrub of the
// replica and an empty compare from it alone lift both; skip_verification
// lifts them too. Only the newest done check of each kind decides: newer
// checks that found something put both blockers back, and a check still
// running counts neither way. The token carries its kind, fits the contract's bound,
// and a switch without skip is refused with the first blocker of
// verification until the checks are done.
func TestValidateListsEachBlocker(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 2)
	f.archive(2, "scan-2", 1)
	f.attach()
	jobs := &fakeJobs{DB: f.p}
	from := map[string]string{}
	cfg := f.config()
	cfg.DB, cfg.CompareFrom = jobs, compareFrom(from)
	s := f.switcher(cfg)
	p := f.preview(s, false)
	want := []string{tenant.BlockerNotInStep(2), BlockerNoCleanScrub, BlockerNoEmptyCompare}
	if p.Ready || !slices.Equal(p.Blockers, want) || p.ScansPending != 2 || !slices.Equal(p.Receivers, []string{"recv"}) {
		t.Fatalf("the preview of a replica not in step is %+v; want blockers %v", p, want)
	}
	if !strings.HasPrefix(p.Confirm, tokenPrefix+".") || len(p.Confirm) > tenant.MaxSwitchToken {
		t.Fatalf("the confirm token %q is not one a switch takes", p.Confirm)
	}
	f.replicate()
	f.inStep()
	if p = f.preview(s, false); !slices.Equal(p.Blockers, []string{BlockerNoCleanScrub, BlockerNoEmptyCompare}) {
		t.Fatalf("the blockers of a replica in step, never checked, are %v", p.Blockers)
	}
	r := f.request(p.Confirm)
	r.SkipVerification, r.SkipReason = false, ""
	if _, err := s.Begin(f.ctx, r); fault.KindOf(err) != fault.Precondition || fault.Message(err) != BlockerNoCleanScrub {
		t.Fatalf("a switch with its checks not done answered %v; want the first blocker of verification", err)
	}
	jobs.add(tenant.Job{ID: "j-dirty", Kind: tenant.JobScrub, Storage: "copy"}, tenant.JobTally{Key: tenant.ScrubTally(tenant.FindingDamaged), Count: 1})
	jobs.add(tenant.Job{ID: "j-other", Kind: tenant.JobScrub, Storage: "main"})
	jobs.add(tenant.Job{ID: "j-differs", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}},
		tenant.JobTally{Key: tenant.DifferTally("scans", "added"), Count: 1})
	from["j-differs"] = "copy"
	jobs.add(tenant.Job{ID: "j-all", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}})
	if p = f.preview(s, false); !slices.Equal(p.Blockers, []string{BlockerNoCleanScrub, BlockerNoEmptyCompare}) {
		t.Fatalf("checks that found something, or checked another storage, lifted blockers: %v", p.Blockers)
	}
	if p = f.preview(s, true); !p.Ready || len(p.Blockers) != 0 {
		t.Fatalf("skip_verification left blockers: %+v", p)
	}
	jobs.add(tenant.Job{ID: "j-clean", Kind: tenant.JobScrub, Storage: "copy"}, tenant.JobTally{Key: tenant.TallyCleared, Count: 3})
	jobs.add(tenant.Job{ID: "j-empty", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}})
	from["j-empty"] = "copy"
	if p = f.preview(s, false); !p.Ready || len(p.Blockers) != 0 {
		t.Fatalf("a clean scrub and an empty compare of the replica left blockers: %+v", p)
	}
	// A newer scrub that found damage and a newer compare that found a
	// difference outweigh the older clean ones.
	jobs.add(tenant.Job{ID: "j-dirty-2", Kind: tenant.JobScrub, Storage: "copy"}, tenant.JobTally{Key: tenant.ScrubTally(tenant.FindingDamaged), Count: 1})
	jobs.add(tenant.Job{ID: "j-differs-2", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}},
		tenant.JobTally{Key: tenant.DifferTally("scans", "added"), Count: 1})
	from["j-differs-2"] = "copy"
	if p = f.preview(s, false); !slices.Equal(p.Blockers, []string{BlockerNoCleanScrub, BlockerNoEmptyCompare}) {
		t.Fatalf("older clean checks outweighed newer ones that found something: %v", p.Blockers)
	}
	// Clean checks newer still lift both; a job still running after them,
	// whatever it counted so far, counts neither way.
	jobs.add(tenant.Job{ID: "j-clean-2", Kind: tenant.JobScrub, Storage: "copy"})
	jobs.add(tenant.Job{ID: "j-empty-2", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}})
	from["j-empty-2"] = "copy"
	jobs.jobs = append(jobs.jobs, tenant.Job{ID: "j-running", Kind: tenant.JobScrub, Storage: "copy", State: tenant.JobRunning, Created: time.Now().UTC()})
	jobs.tallies["j-running"] = []tenant.JobTally{{Key: tenant.ScrubTally(tenant.FindingDamaged), Count: 1}}
	if p = f.preview(s, false); !p.Ready || len(p.Blockers) != 0 {
		t.Fatalf("the newest clean checks, with a scrub still running, left blockers: %+v", p)
	}
	r = f.request(p.Confirm)
	r.SkipVerification, r.SkipReason = false, ""
	sw, err := s.Begin(f.ctx, r)
	if err != nil {
		t.Fatalf("a switch whose checks are done was refused: %v", err)
	}
	if done := f.wait(s, sw.ID); done.State != tenant.SwitchDone {
		t.Fatalf("the switch ended as %+v", done)
	}
}

// shiftedClock is the machine's clock moved by an offset a test sets.
type shiftedClock struct{ offset atomic.Int64 }

// Now returns the machine's time plus the offset.
func (c *shiftedClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

// After returns time.After(d).
func (c *shiftedClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// TestTokenRefusals guards the confirm token: a token whose MAC was
// changed, one that is not a switch's (a delete's), one for another pair of
// storages, and one more than an hour old are refused, each recording
// nothing.
func TestTokenRefusals(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 1)
	f.attach()
	clock := &shiftedClock{}
	cfg := f.config()
	cfg.Clock = clock
	s := f.switcher(cfg)
	token := f.preview(s, true).Confirm
	flipped := token[:len(token)-1] + string("0123456789abcdef"[(strings.IndexByte("0123456789abcdef", token[len(token)-1])+1)%16])
	for name, tok := range map[string]string{
		"changed":    flipped,
		"a delete's": "c2." + strings.SplitN(token, ".", 3)[1] + "." + strings.SplitN(token, ".", 3)[2],
		"empty":      "",
	} {
		if _, err := s.Begin(f.ctx, f.request(tok)); fault.KindOf(err) != fault.Precondition {
			t.Errorf("a %s token answered %v; want a precondition failure", name, err)
		}
	}
	other := f.request(token)
	other.Primary, other.Replica = "copy", "main"
	if _, err := s.Begin(f.ctx, other); err == nil {
		t.Error("a token was taken for another pair of storages")
	}
	clock.offset.Store(int64(TokenLife + time.Minute))
	if _, err := s.Begin(f.ctx, f.request(token)); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a token past its life answered %v; want a precondition failure", err)
	}
	clock.offset.Store(0)
	if _, found, err := f.p.LastSwitch(f.ctx, "main"); err != nil || found {
		t.Fatalf("a refused token recorded a switch: %v, %v", found, err)
	}
	if _, err := s.Begin(f.ctx, f.request(token)); err != nil {
		t.Fatalf("the token within its life was refused: %v", err)
	}
}

// TestTokenBoundToTheTenant guards the token's tenant: every tenant's
// tokens are made under the installation's one confirm key, so a token
// another tenant's preview gave, for storages named and placed alike, is
// refused and records nothing; the tenant's own token is taken.
func TestTokenBoundToTheTenant(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 1)
	f.attach()
	s := f.switcher(f.config())
	cfg := f.config()
	cfg.Tenant = place.TenantOf("T0000000000002", "other", f.p)
	other := f.switcher(cfg)
	if _, err := other.Begin(f.ctx, f.request(f.preview(s, true).Confirm)); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a token of another tenant answered %v; want a precondition failure", err)
	}
	if _, found, err := f.p.LastSwitch(f.ctx, "main"); err != nil || found {
		t.Fatalf("a token of another tenant recorded a switch: %v, %v", found, err)
	}
	if _, err := other.Begin(f.ctx, f.request(f.preview(other, true).Confirm)); err != nil {
		t.Fatalf("the tenant's own token was refused: %v", err)
	}
}

// auditIDs returns the ids of the audit entries the fixture's tenant
// database holds.
func (f *fixture) auditIDs() map[string]bool {
	f.t.Helper()
	lister, ok := f.p.(interface {
		ListAudit(ctx context.Context, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error)
	})
	if !ok {
		f.t.Fatal("the SQLite tenant store lists no audit entries")
	}
	out := map[string]bool{}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := lister.ListAudit(f.ctx, master.AuditFilter{}, page)
		if err != nil {
			f.t.Fatal(err)
		}
		for _, e := range entries {
			out[e.ID] = true
		}
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// TestRepeatedTokenIsGuardedAndAudited guards a token sent again after the
// switch it began has swapped: a request by a key the guard refuses is
// refused with the guard's error, not answered the switch; one by a key it
// admits is answered the switch the token began and records its own audit
// entry, as the first request did.
func TestRepeatedTokenIsGuardedAndAudited(t *testing.T) {
	f := newFixture(t)
	f.archive(1, "scan-1", 1)
	f.attach()
	f.replicate()
	f.inStep()
	s := f.switcher(f.config())
	token := f.preview(s, true).Confirm
	sw, err := s.Begin(f.ctx, f.request(token))
	if err != nil {
		t.Fatal(err)
	}
	if done := f.wait(s, sw.ID); done.State != tenant.SwitchDone {
		t.Fatalf("the switch ended as %+v", done)
	}
	denied := f.request(token)
	denied.Guard = func(context.Context) error {
		return fault.New(fault.Denied, "the key holds no grant to switch storages")
	}
	if got, err := s.Begin(f.ctx, denied); fault.KindOf(err) != fault.Denied {
		t.Fatalf("a repeat by a key the guard refuses answered %+v, %v; want it denied", got, err)
	}
	again := f.request(token)
	got, err := s.Begin(f.ctx, again)
	if err != nil || got.ID != sw.ID {
		t.Fatalf("a repeat by an admitted key answered %+v, %v; want the switch the token began", got, err)
	}
	if ids := f.auditIDs(); !ids[again.Audit.ID] || ids[denied.Audit.ID] {
		t.Fatalf("the audit holds the admitted repeat's entry %v and the denied one's %v; want only the admitted one",
			ids[again.Audit.ID], ids[denied.Audit.ID])
	}
}

// TestSwitchResealsS3Secrets guards the secrets of S3 places: a switch of
// two S3 storages seals each secret again for the name its place comes to,
// so after the swap each name's sealed secret opens, under that name, to
// the secret of the place it now names; without the reseal the database
// refuses the swap and the switch is refused, changing nothing. The places
// are opened as directories here (no S3 server): an S3 place is touched
// only by the owner record, which this test writes to a directory.
func TestSwitchResealsS3Secrets(t *testing.T) {
	for _, reseal := range []bool{true, false} {
		f := newFixture(t)
		add := func(name, prefix, replicaOf, secret string) {
			sealed, err := place.SealSecret(f.keys, testTenantID, name, secret)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: name, Type: tenant.StorageS3, Tier: tenant.TierOnline, ReplicaOf: replicaOf,
				S3: tenant.S3Storage{Endpoint: "https://s3.example.org", Region: "us-east-1", Bucket: "lab-archive", Prefix: prefix,
					PathStyle: true, AccessKeyID: "AKIDEXAMPLE", SealedSecret: sealed}}, time.Now(), f.audit(), nil); err != nil {
				t.Fatal(err)
			}
		}
		add("bucket-a", "a/", "", "secret-of-a")
		add("bucket-b", "b/", "bucket-a", "secret-of-b")
		cfg := f.config()
		cfg.OpenPlace = func(ctx context.Context, row tenant.Storage) (blob.Store, error) {
			return f.openRow(ctx, tenant.Storage{Directory: tenant.DirectoryStorage{Path: "s3-" + strings.TrimSuffix(row.S3.Prefix, "/")}})
		}
		if !reseal {
			cfg.Reseal = nil
		}
		s := f.switcher(cfg)
		p, err := s.Validate(f.ctx, "bucket-a", "bucket-b", true)
		if err != nil {
			t.Fatal(err)
		}
		sw, err := s.Begin(f.ctx, Request{Primary: "bucket-a", Replica: "bucket-b", Confirm: p.Confirm, SkipVerification: true,
			SkipReason: "checked by hand", DrainTimeout: time.Second, Audit: f.audit()})
		if err != nil {
			t.Fatal(err)
		}
		done := f.wait(s, sw.ID)
		opens := func(name, want string) {
			t.Helper()
			sealed, err := f.p.(place.Secrets).StorageSecret(f.ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := f.keys.Open(place.SecretContext(testTenantID, name), sealed)
			if err != nil || string(plain) != want {
				t.Errorf("with reseal %v the secret of %s opens to %q, %v; want %q", reseal, name, plain, err, want)
			}
		}
		if reseal {
			if done.State != tenant.SwitchDone {
				t.Fatalf("a switch of two S3 storages ended as %+v", done)
			}
			opens("bucket-a", "secret-of-b")
			opens("bucket-b", "secret-of-a")
			continue
		}
		if done.State != tenant.SwitchRefused {
			t.Fatalf("a switch of two S3 storages without their secrets sealed again ended as %+v; want refused", done)
		}
		opens("bucket-a", "secret-of-a")
		opens("bucket-b", "secret-of-b")
	}
}
