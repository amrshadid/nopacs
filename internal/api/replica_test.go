package api_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/storageswitch"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testSwitches is the switch runner the handlers are given: none until a
// test sets one.
type testSwitches struct {
	mu sync.Mutex
	sw api.Switcher
}

// Switcher answers the switcher a test set, or unavailable.
func (s *testSwitches) Switcher(context.Context, string) (api.Switcher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sw == nil {
		return nil, fault.New(fault.Unavailable, "no switch runner in this test")
	}
	return s.sw, nil
}

// set sets the switcher.
func (s *testSwitches) set(sw api.Switcher) {
	s.mu.Lock()
	s.sw = sw
	s.mu.Unlock()
}

// fakeSwitcher answers a preview, an accept and an end as a test sets
// them, and records what it was asked.
type fakeSwitcher struct {
	mu       sync.Mutex
	preview  storageswitch.Preview
	begin    error
	ended    tenant.StorageSwitch
	requests []storageswitch.Request
}

// Validate answers the preview set.
func (f *fakeSwitcher) Validate(context.Context, string, string, bool) (storageswitch.Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.preview, nil
}

// answer sets what Begin and Wait answer.
func (f *fakeSwitcher) answer(begin error, ended tenant.StorageSwitch) {
	f.mu.Lock()
	f.begin, f.ended = begin, ended
	f.mu.Unlock()
}

// Begin records the request and answers the accepted switch, or the error
// set.
func (f *fakeSwitcher) Begin(_ context.Context, r storageswitch.Request) (tenant.StorageSwitch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	if f.begin != nil {
		return tenant.StorageSwitch{}, f.begin
	}
	return tenant.StorageSwitch{ID: "sw1", Primary: r.Primary, Replica: r.Replica, State: tenant.SwitchAccepted}, nil
}

// Wait answers the end set.
func (f *fakeSwitcher) Wait(context.Context, string) (tenant.StorageSwitch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ended, nil
}

// copiesFixture is the place fixture with three directory storages of
// lab1: main, copy and third.
type copiesFixture struct {
	*placeFixture
}

// newCopiesFixture returns the replica fixture.
func newCopiesFixture(t *testing.T) *copiesFixture {
	t.Helper()
	f := &copiesFixture{newPlaceFixture(t)}
	for _, name := range []string{"main", "copy", "third"} {
		if o := call(f.as(local).storages.AddStorage, &v1.AddStorageRequest{Tenant: "lab1", Storage: dirStorage(name, "fast", name)}); o.err != nil {
			t.Fatalf("adding %s: %v", name, o.err)
		}
	}
	return f
}

// attach attaches replica to primary as the local operator.
func (f *copiesFixture) attach(t *testing.T, replica, primary string) {
	t.Helper()
	if o := call(f.as(local).replicas.AttachReplica, &v1.AttachReplicaRequest{Tenant: "lab1", Replica: replica, Primary: primary}); o.err != nil {
		t.Fatalf("attaching %s to %s: %v", replica, primary, o.err)
	}
}

// testListReplicas guards ListReplicas: reader on the tenant; a primary
// filter that is not a name is invalid; each replica is answered with its
// primary, tier and counts, in step with nothing pending on an empty
// primary.
func testListReplicas(t *testing.T) {
	f := newCopiesFixture(t)
	f.attach(t, "copy", "main")
	list := func(primary string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.replicas.ListReplicas, &v1.ListReplicasRequest{Tenant: "lab1", Primary: primary})
		}
	}
	f.run(t, []tcase{
		{name: "unauthenticated", as: nobody, call: list(""), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: list(""), code: connect.CodePermissionDenied},
		{name: "a primary that is not a name", as: f.lab1Reader, call: list("Main"), code: connect.CodeInvalidArgument, msg: "primary"},
		{name: "every replica", as: f.lab1Reader, call: list(""), audit: inTenant("lab1", master.OutcomeDone),
			check: func(t *testing.T, o outcome) {
				rs := o.res.(*v1.ListReplicasResponse).GetReplicas()
				if len(rs) != 1 || rs[0].GetReplica() != "copy" || rs[0].GetPrimary() != "main" || rs[0].GetTier() != v1.StorageTier_STORAGE_TIER_ONLINE ||
					rs[0].GetScansPending() != 0 || rs[0].GetLag().AsDuration() != 0 {
					t.Errorf("replicas %v", rs)
				}
			}},
		{name: "another primary's", as: f.lab1Reader, call: list("third"), check: func(t *testing.T, o outcome) {
			if rs := o.res.(*v1.ListReplicasResponse).GetReplicas(); len(rs) != 0 {
				t.Errorf("replicas of third %v", rs)
			}
		}},
	})
}

// testAttachReplica guards AttachReplica: admin on the tenant; both names
// required; a storage the tenant does not have is not found; a primary
// that is itself a replica is a precondition failure; the replica is
// answered with replica_of set, and a repeat answers it as it stands.
func testAttachReplica(t *testing.T) {
	f := newCopiesFixture(t)
	attach := func(replica, primary string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.replicas.AttachReplica, &v1.AttachReplicaRequest{Tenant: "lab1", Replica: replica, Primary: primary})
		}
	}
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: attach("copy", "main"), code: connect.CodePermissionDenied},
		{name: "no replica", as: f.lab1Admin, call: attach("", "main"), code: connect.CodeInvalidArgument, msg: "replica"},
		{name: "no primary", as: f.lab1Admin, call: attach("copy", ""), code: connect.CodeInvalidArgument, msg: "primary"},
		{name: "a replica the tenant does not have", as: f.lab1Admin, call: attach("none", "main"), code: connect.CodeNotFound, msg: "no storage"},
		{name: "attached", as: f.lab1Admin, call: attach("copy", "main"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "copy", details: map[string]string{"primary": "main"}},
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.AttachReplicaResponse).GetStorage(); s.GetName() != "copy" || s.GetReplicaOf() != "main" {
					t.Errorf("attached %v", s)
				}
			}},
		{name: "again, as it stands", as: f.lab1Admin, call: attach("copy", "main"), check: func(t *testing.T, o outcome) {
			if s := o.res.(*v1.AttachReplicaResponse).GetStorage(); s.GetReplicaOf() != "main" {
				t.Errorf("a repeat answered %v", s)
			}
		}},
		{name: "a primary that is a replica", as: f.lab1Admin, call: attach("third", "copy"), code: connect.CodeFailedPrecondition,
			audit: inTenant("lab1", master.OutcomeFailed)},
	})
}

// testDetachReplica guards DetachReplica: admin on the tenant; an abandon
// reason goes with abandon_purges and only with it; a storage that is not
// a replica is a precondition failure; the storage is answered with
// replica_of empty and the entry names its former primary.
func testDetachReplica(t *testing.T) {
	f := newCopiesFixture(t)
	f.attach(t, "copy", "main")
	detach := func(replica string, abandon bool, reason string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.replicas.DetachReplica, &v1.DetachReplicaRequest{Tenant: "lab1", Replica: replica, AbandonPurges: abandon, AbandonReason: reason})
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: detach("copy", false, ""), code: connect.CodePermissionDenied},
		{name: "a reason without abandon_purges", as: f.lab1Admin, call: detach("copy", false, "lost"), code: connect.CodeInvalidArgument, msg: "abandon_reason"},
		{name: "abandon_purges without a reason", as: f.lab1Admin, call: detach("copy", true, ""), code: connect.CodeInvalidArgument, msg: "abandon_reason"},
		{name: "not a replica", as: f.lab1Admin, call: detach("third", false, ""), code: connect.CodeFailedPrecondition},
		{name: "detached", as: f.lab1Admin, call: detach("copy", false, ""),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "copy", details: map[string]string{"primary": "main", "abandon_purges": "false"}},
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.DetachReplicaResponse).GetStorage(); s.GetName() != "copy" || s.GetReplicaOf() != "" {
					t.Errorf("detached %v", s)
				}
			}},
		{name: "again: no longer a replica", as: f.lab1Admin, call: detach("copy", false, ""), code: connect.CodeFailedPrecondition},
	})
}

// testRepair guards Repair: admin on the tenant; findings or all, not both
// nor neither, at most 1000, each once; a replica of the storage named
// from; the storage not itself a replica; a finding the tenant does not
// have is not found; a repair of all queues a repair job, a repeat answers
// it, and one with other findings beside it is refused.
func testRepair(t *testing.T) {
	f := newCopiesFixture(t)
	f.attach(t, "copy", "main")
	repair := func(storage, from string, all bool, findings ...string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.replicas.Repair, &v1.RepairRequest{Tenant: "lab1", Storage: storage, From: from, All: all, Findings: findings})
		}
	}
	many := make([]string, 1001)
	for i := range many {
		many[i] = "f" + strings.Repeat("0", 4) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+i/676))
	}
	var first string
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: repair("main", "", true), code: connect.CodePermissionDenied},
		{name: "neither findings nor all", as: f.lab1Admin, call: repair("main", "", false), code: connect.CodeInvalidArgument, msg: "findings"},
		{name: "both", as: f.lab1Admin, call: repair("main", "", true, "f1"), code: connect.CodeInvalidArgument, msg: "empty with all"},
		{name: "more than 1000", as: f.lab1Admin, call: repair("main", "", false, many...), code: connect.CodeInvalidArgument, msg: "at most 1000"},
		{name: "a finding twice", as: f.lab1Admin, call: repair("main", "", false, "f1", "f1"), code: connect.CodeInvalidArgument, msg: "once"},
		{name: "no such storage", as: f.lab1Admin, call: repair("none", "", true), code: connect.CodeNotFound},
		{name: "the storage is a replica", as: f.lab1Admin, call: repair("copy", "", true), code: connect.CodeFailedPrecondition, msg: "is a replica"},
		{name: "from a storage that is not its replica", as: f.lab1Admin, call: repair("main", "third", true), code: connect.CodeFailedPrecondition,
			msg: "not a replica of main"},
		{name: "a finding the tenant does not have", as: f.lab1Admin, call: repair("main", "", false, "nofinding"), code: connect.CodeNotFound, msg: "scrub finding"},
		{name: "every finding, from copy", as: f.lab1Admin, call: repair("main", "copy", true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"findings": "all", "replica": "copy"}},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.RepairResponse).GetJob()
				r := j.GetRepair()
				if j.GetId() == "" || r.GetStorage() != "main" || r.GetFrom() != "copy" || !r.GetAll() || j.GetState() != v1.JobState_JOB_STATE_QUEUED {
					t.Errorf("job %v", j)
				}
				first = j.GetId()
			}},
		{name: "the same again: the job queued", as: f.lab1Admin, call: repair("main", "copy", true), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.RepairResponse).GetJob().GetId(); got != first {
				t.Errorf("a repeat answered %s, the first %s", got, first)
			}
		}},
		{name: "another repair of main beside it", as: f.lab1Admin, call: repair("main", "", true), code: connect.CodeFailedPrecondition, msg: "queued or running"},
	})
	o := call(f.as(f.lab1Reader).jobs.ListJobs, &v1.ListJobsRequest{Tenant: "lab1"})
	if o.err != nil {
		t.Fatal(o.err)
	}
	if js := o.res.(*v1.ListJobsResponse).GetJobs(); len(js) != 1 || js[0].GetRepair().GetStorage() != "main" {
		t.Errorf("the job listing shows %v, want the one repair job", js)
	}
}

// testSwitchStorage guards SwitchStorage: admin on the tenant; a token
// with validate_only or none without it, a skip reason with
// skip_verification only, and a drain of 1 second to 1 hour; storages the
// tenant has; with no switch runner, unavailable; a preview answers the
// runner's blockers and token; a switch answers its end, and a refusal is a
// precondition failure with its phrase; a write refused while switching
// carries STORAGE_SWITCHING and a retry delay.
func testSwitchStorage(t *testing.T) {
	f := newCopiesFixture(t)
	f.attach(t, "copy", "main")
	sw := func(validate bool, confirm string, skip bool, reason string, drain *durationpb.Duration) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.replicas.SwitchStorage, &v1.SwitchStorageRequest{Tenant: "lab1", Primary: "main", Replica: "copy", ValidateOnly: validate,
				Confirm: confirm, SkipVerification: skip, SkipReason: reason, DrainTimeout: drain})
		}
	}
	fake := &fakeSwitcher{preview: storageswitch.Preview{Ready: false, Blockers: []string{"no empty compare from the replica"}, ScansPending: 3,
		Receivers: []string{"scanner1"}, Confirm: "t1.token", Generation: 4}}
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: sw(true, "", false, "", nil), code: connect.CodePermissionDenied},
		{name: "a token with validate_only", as: f.lab1Admin, call: sw(true, "x", false, "", nil), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "no token", as: f.lab1Admin, call: sw(false, "", false, "", nil), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "skip without a reason", as: f.lab1Admin, call: sw(true, "", true, "", nil), code: connect.CodeInvalidArgument, msg: "skip_reason"},
		{name: "a reason without skip", as: f.lab1Admin, call: sw(true, "", false, "trust me", nil), code: connect.CodeInvalidArgument, msg: "skip_reason"},
		{name: "a drain too short", as: f.lab1Admin, call: sw(true, "", false, "", durationpb.New(time.Millisecond)), code: connect.CodeInvalidArgument, msg: "drain_timeout"},
		{name: "a drain too long", as: f.lab1Admin, call: sw(true, "", false, "", durationpb.New(2*time.Hour)), code: connect.CodeInvalidArgument, msg: "drain_timeout"},
		{name: "no switch runner", as: f.lab1Admin, call: sw(true, "", false, "", nil), code: connect.CodeUnavailable},
		{name: "a preview", as: f.lab1Admin, call: func(c clients) outcome {
			f.switches.set(fake)
			return sw(true, "", false, "", nil)(c)
		}, audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "main", details: map[string]string{"validate_only": "true", "replica": "copy"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.SwitchStorageResponse)
				if r.GetReady() || len(r.GetBlockers()) != 1 || r.GetConfirm() != "t1.token" || r.GetScansPending() != 3 || r.GetSwitchGeneration() != 4 {
					t.Errorf("preview %v", r)
				}
			}},
		{name: "a switch made", as: f.lab1Admin, call: func(c clients) outcome {
			fake.answer(nil, tenant.StorageSwitch{ID: "sw1", Primary: "main", Replica: "copy", State: tenant.SwitchDone, ScansDrained: 2,
				AssociationsRefused: 5, Pause: 3 * time.Second, Swapped: t0})
			return sw(false, "t1.token", true, "compared by hand", durationpb.New(time.Minute))(c)
		}, check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.SwitchStorageResponse)
			if !r.GetReady() || r.GetScansPending() != 2 || r.GetAssociationsRefused() != 5 || r.GetPause().AsDuration() != 3*time.Second ||
				r.GetPrimary().GetName() != "main" || r.GetReplica().GetName() != "copy" || r.GetSwitchTime() == nil {
				t.Errorf("switch %v", r)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if n := len(fake.requests); n != 1 || fake.requests[0].Confirm != "t1.token" || fake.requests[0].DrainTimeout != time.Minute ||
				!fake.requests[0].SkipVerification || fake.requests[0].SkipReason != "compared by hand" {
				t.Errorf("the runner was asked %+v", fake.requests)
			}
		}},
		{name: "a switch refused", as: f.lab1Admin, call: func(c clients) outcome {
			fake.answer(nil, tenant.StorageSwitch{ID: "sw1", Primary: "main", Replica: "copy", State: tenant.SwitchRefused, Refusal: tenant.RefusalNotInStep})
			return sw(false, "t1.token", false, "", nil)(c)
		}, code: connect.CodeFailedPrecondition, msg: tenant.RefusalNotInStep},
		{name: "a storage being switched", as: f.lab1Admin, call: func(c clients) outcome {
			fake.answer(storageswitch.Refusal("main"), tenant.StorageSwitch{})
			return sw(false, "t1.token", false, "", nil)(c)
		}, code: connect.CodeUnavailable, msg: "STORAGE_SWITCHING", check: func(t *testing.T, o outcome) {
			var ce *connect.Error
			if !errors.As(o.err, &ce) {
				t.Fatalf("not an API error: %v", o.err)
			}
			r, ok := codes.ReasonOfError(ce)
			if !ok || r.Reason != storageswitch.ReasonStorageSwitching || r.Domain != codes.Domain {
				t.Errorf("error info %+v %v", r, ok)
			}
			if d, ok := codes.RetryOfError(ce); !ok || d != storageswitch.RetryAfter {
				t.Errorf("retry info %v %v, want %v", d, ok, storageswitch.RetryAfter)
			}
		}},
	})
}
