package receive

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// fixedProfile is the version a release names in these tests: the basic
// profile, which takes the CT file the receiver's version refuses.
var fixedProfile = tenant.ProfileRef{Name: "fixed", Version: 1}

// refusingKVP is the receiver's version in these tests: the basic profile
// with a rule that quarantines every file holding KVP (0018,0060), as the
// CT files do.
func refusingKVP() *v1.ProfileDocument {
	doc := basicProfile()
	doc.Rules = append(doc.Rules, &v1.ProfileRule{Name: "refuse-kvp",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "KVP"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Quarantine{Quarantine: &v1.QuarantineAction{}}}})
	return doc
}

// instanceProfile returns the profile version the tenant records for the
// archived instance of the original SOP Instance UID inst.
func instanceProfile(t *testing.T, e *env, inst string) tenant.ProfileRef {
	t.Helper()
	ref, err := e.store.(tenant.Profiles).InstanceProfile(e.ctx, remapped(inst))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// TestAReleaseThroughANamedVersionJoinsWithIt guards a release that names
// a profile version, of a file that joins a reception: the file the
// receiver's version refuses (profile_refused) is archived de-identified
// by the version named, which the instance records, with no value of the
// original; released again without one, a second such file is held again
// by the receiver's version.
func TestAReleaseThroughANamedVersionJoinsWithIt(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{doc: refusingKVP()}})
	e.storeProfile(fixedProfile, basicProfile())
	e.send(studyFiles[0])
	e.settle(1)
	qs := e.held()
	if len(qs) != 1 || qs[0].Reason != tenant.QuarantineProfileRefused || !qs[0].Receivable() {
		t.Fatalf("held %+v, want one receivable profile_refused record", qs)
	}
	e.clock.Advance(time.Second)
	out, err := e.p.Release(e.ctx, tenant.Release{ID: qs[0].ID, Mode: tenant.ReleaseAsIs, Profile: fixedProfile}, e.audit(), nil)
	if err != nil || out.ReleaseReception == "" || out.ReleaseProfile != fixedProfile {
		t.Fatalf("the release joined %q naming %v: %v", out.ReleaseReception, out.ReleaseProfile, err)
	}
	var r tenant.Reception
	for _, x := range e.settle(2) {
		if x.ID == out.ReleaseReception {
			r = x
		}
	}
	if !done(r) || r.Route.ReleasedProfile != fixedProfile {
		t.Fatalf("the reception the release joined is %s, keeping %v", r.State, r.Route.ReleasedProfile)
	}
	noOriginal(t, e, r)
	_, _, inst := fileUIDs(t, corpus(t, studyFiles[0]))
	if got := instanceProfile(t, e, inst); got != fixedProfile {
		t.Errorf("the instance records %v, want the version the release named", got)
	}

	e.send(studyFiles[1])
	e.settle(3)
	var second tenant.QuarantinedFile
	for _, q := range e.held() {
		if q.State == tenant.QuarantineHeld {
			second = q
		}
	}
	if second.Reason != tenant.QuarantineProfileRefused {
		t.Fatalf("a second file is held %s, want profile_refused", second.Reason)
	}
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: second.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the second file held again", 3*time.Second, func() bool {
		for _, q := range e.held() {
			if q.ID != second.ID && q.State == tenant.QuarantineHeld && q.Reason == tenant.QuarantineProfileRefused {
				return true
			}
		}
		return false
	})
}

// TestAReleaseThroughANamedVersionLandsAgainWithIt guards a release that
// names a profile version, of a file that joins no reception (its header
// past the bound): the version is kept with the released record, the file
// lands again, and the reception it joins then keeps the version and
// archives it de-identified by it.
func TestAReleaseThroughANamedVersionLandsAgainWithIt(t *testing.T) {
	var limit atomic.Int64
	limit.Store(256)
	e := newEnv(t, envOptions{deid: &deidOptions{doc: refusingKVP()},
		headerLimit: func(context.Context) (int64, error) { return limit.Load(), nil }})
	e.storeProfile(fixedProfile, basicProfile())
	e.send(studyFiles[0])
	e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if q.Reason != tenant.QuarantineHeaderTooLarge || q.Receivable() {
		t.Fatalf("held %s, receivable %v", q.Reason, q.Receivable())
	}
	limit.Store(64 << 20)
	e.clock.Advance(2 * HeaderLimitInterval)
	out, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, Profile: fixedProfile}, e.audit(), nil)
	if err != nil || out.ReleaseReception != "" || out.ReleaseProfile != fixedProfile {
		t.Fatalf("the release joined %q keeping %v: %v", out.ReleaseReception, out.ReleaseProfile, err)
	}
	r := e.settle(1)[0]
	if !done(r) || r.Route.ReleasedProfile != fixedProfile {
		t.Fatalf("the reception is %s keeping %v, want done with the version named", r.State, r.Route.ReleasedProfile)
	}
	noOriginal(t, e, r)
	_, _, inst := fileUIDs(t, corpus(t, studyFiles[0]))
	if got := instanceProfile(t, e, inst); got != fixedProfile {
		t.Errorf("the instance records %v, want the version the release named", got)
	}
}
