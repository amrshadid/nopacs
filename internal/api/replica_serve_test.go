package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"slices"
	"strconv"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testBackfillReplica guards BackfillReplica: operator on the tenant, a
// reader refused and a masked key refused as choosing where the archive's
// bytes go; a storage the tenant does not have is not found and one that is
// not a replica a precondition failure; a backfill of a replica in step
// queues a running replicate job of every scan, named by the entry's
// details, and a repeat answers that job and queues nothing more; the job
// is read back with its replica and primary.
func testBackfillReplica(t *testing.T) {
	r := newReplicaFixture(t)
	f := r.f
	masked, _ := f.maskedKey(f.onTenant("lab1", master.RoleAdmin))
	backfill := func(replica string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.replicas.BackfillReplica, &v1.BackfillReplicaRequest{Tenant: "lab1", Replica: replica})
		}
	}
	scans := int64(len(r.a.scans(t)))
	var first string
	f.run(t, []tcase{
		{name: "unauthenticated", as: nobody, call: backfill("copy"), code: connect.CodeUnauthenticated},
		{name: "reader", as: f.lab1Reader, call: backfill("copy"), code: connect.CodePermissionDenied},
		{name: "a masked key", as: masked, call: backfill("copy"), code: connect.CodePermissionDenied, msg: access.MessageMaskedDestination},
		{name: "no replica", as: f.lab1Operator, call: backfill(""), code: connect.CodeInvalidArgument, msg: "replica"},
		{name: "a storage the tenant does not have", as: f.lab1Operator, call: backfill("none"), code: connect.CodeNotFound, msg: "no storage"},
		{name: "not a replica", as: f.lab1Operator, call: backfill("main"), code: connect.CodeFailedPrecondition, msg: "not a replica",
			audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "queued", as: f.lab1Operator, call: backfill("copy"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "copy", details: map[string]string{"job": "*"}},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.BackfillReplicaResponse).GetJob()
				rj := j.GetReplicate()
				if j.GetId() == "" || j.GetState() != v1.JobState_JOB_STATE_RUNNING || rj.GetReplica() != "copy" || rj.GetPrimary() != "main" ||
					rj.GetScheduled() || rj.GetMode() != v1.ReplicateJobMode_REPLICATE_JOB_MODE_BACKFILL || j.GetCounts().GetScans() != scans {
					t.Errorf("the backfill's job is %v, want a running job of copy counting %d scans", j, scans)
				}
				first = j.GetId()
			}},
		{name: "again: the job running", as: f.lab1Operator, call: backfill("copy"), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.BackfillReplicaResponse).GetJob().GetId(); got != first {
				t.Errorf("a repeat answered %s, the first %s", got, first)
			}
		}},
	})
	o := call(f.as(f.lab1Reader).jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: first})
	if o.err != nil {
		t.Fatal(o.err)
	}
	if j := o.res.(*v1.GetJobResponse).GetJob(); j.GetReplicate().GetReplica() != "copy" || j.GetReplicate().GetPrimary() != "main" {
		t.Errorf("the job read back is %v", j)
	}
	// The entry of the repeat names the job it answered, not a new one.
	var named []string
	f.flush()
	for _, en := range f.tenantLog("lab1") {
		if en.Method == "/nopacs.v1.ReplicaService/BackfillReplica" && en.Outcome == master.OutcomeDone {
			named = append(named, en.Details["job"])
		}
	}
	if len(named) != 2 || named[0] != first || named[1] != first {
		t.Errorf("the entries of the backfills name the jobs %v, want %s twice", named, first)
	}
	// A copy to the replica fails: the job, still running, says why it is
	// not moving.
	ctx := context.Background()
	now := time.Now()
	claims, err := r.a.p.ClaimReplicaScans(ctx, "test", now, time.Minute, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims: %d, %v", len(claims), err)
	}
	if err := r.a.p.FailReplicaScan(ctx, tenant.ReplicaFailure{Fence: claims[0].Fence, Fault: tenant.FaultUnreachable,
		Reason: "the storage does not answer", Retry: now.Add(time.Minute), Now: now}); err != nil {
		t.Fatal(err)
	}
	o = call(f.as(f.lab1Reader).jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: first})
	if o.err != nil {
		t.Fatal(o.err)
	}
	if j := o.res.(*v1.GetJobResponse).GetJob(); j.GetState() != v1.JobState_JOB_STATE_RUNNING || j.GetReason() != "replica not answering" {
		t.Errorf("the job after a failed copy is %s with reason %q, want running, \"replica not answering\"", j.GetState(), j.GetReason())
	}
}

// coldFixture is the replica fixture with an archive-tier S3 replica
// "cold" of "main" (recall_days 3) in step for every scan.
func coldFixture(t *testing.T) *replicaFixture {
	t.Helper()
	r := newReplicaFixture(t)
	ctx := context.Background()
	entry := master.AuditEntry{ID: "a" + r.f.newID(), Time: time.Now(), KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000",
		Method: "Test", TenantName: "lab1", Outcome: master.OutcomeDone, RequestID: "r1"}
	cold := tenant.Storage{Name: "cold", Type: tenant.StorageS3, Tier: tenant.TierArchive, ReplicaOf: "main", S3: tenant.S3Storage{
		Endpoint: "https://s3.example.org", Region: "us-east-1", Bucket: "lab-cold", Prefix: "cold/", PathStyle: true,
		AccessKeyID: "AKIDEXAMPLE", SealedSecret: []byte("sealed-secret-bytes"), RecallDays: 3, RecallSpeed: tenant.RecallBulk}}
	if _, err := r.a.p.AddStorage(ctx, cold, time.Now(), entry, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims, err := r.a.p.ClaimReplicaScans(ctx, "test", now, time.Minute, tenant.MaxReplicaClaim)
	if err != nil || len(claims) == 0 {
		t.Fatalf("claims: %d, %v", len(claims), err)
	}
	for _, c := range claims {
		if state, err := r.a.p.EndReplicaScan(ctx, c.Fence, c.ManifestSHA256, now); err != nil || state != tenant.ReplicaInStep {
			t.Fatalf("in step: %s, %v", state, err)
		}
	}
	return r
}

// testRecall guards Recall: operator on the tenant, a reader refused; a
// confirm token with validate_only or none without it, days outside 0 to
// 365, and a scan without its session are invalid; a subject the tenant
// does not hold is not found; an online storage named is a precondition
// failure; days above the storage's recall_days are refused to an operator
// and previewed for an admin; a preview counts the target's objects on the
// archive tier, joins a recall a job asked for already, and gives a token;
// the recall asks for exactly the others, its entry naming them; the same
// token again answers what it asked and asks nothing more; a token of
// other days does not match.
func testRecall(t *testing.T) {
	r := coldFixture(t)
	f := r.f
	ctx := context.Background()
	// The subject that holds the most objects: two files of the fixture are
	// of one patient.
	keysOf := map[string][]string{}
	bytesOf := map[string]int64{}
	subject := ""
	for _, p := range r.a.scans(t) {
		for _, h := range r.a.held(t, p) {
			keysOf[p.Subject] = append(keysOf[p.Subject], h.Key)
			bytesOf[p.Subject] += h.Size
		}
		if len(keysOf[p.Subject]) > len(keysOf[subject]) {
			subject = p.Subject
		}
	}
	keys, total := keysOf[subject], bytesOf[subject]
	if len(keys) < 2 {
		t.Fatalf("the fixture's subjects hold %v; the test needs one with two objects", keysOf)
	}
	// A job unit met one object cold and asked for it, the server's own.
	if _, err := r.a.p.RequestRecalls(ctx, tenant.RecallAsk{Storage: "cold", Keys: keys[:1], Days: 3, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	recall := func(sub string, days int32, validate bool, confirm, storage string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.replicas.Recall, &v1.RecallRequest{Tenant: "lab1", SubjectLabel: sub, Days: days, ValidateOnly: validate, Confirm: confirm, Storage: storage})
		}
	}
	n := int64(len(keys))
	var token string
	var asked []string
	f.run(t, []tcase{
		{name: "unauthenticated", as: nobody, call: recall(subject, 0, true, "", ""), code: connect.CodeUnauthenticated},
		{name: "reader", as: f.lab1Reader, call: recall(subject, 0, true, "", ""), code: connect.CodePermissionDenied},
		{name: "a token with validate_only", as: f.lab1Operator, call: recall(subject, 0, true, "x", ""), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "no token without validate_only", as: f.lab1Operator, call: recall(subject, 0, false, "", ""), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "days past 365", as: f.lab1Operator, call: recall(subject, 366, true, "", ""), code: connect.CodeInvalidArgument, msg: "days"},
		{name: "a scan without its session", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.replicas.Recall, &v1.RecallRequest{Tenant: "lab1", SubjectLabel: subject, ScanLabel: "s1", ValidateOnly: true})
		}, code: connect.CodeInvalidArgument},
		{name: "a subject the tenant does not hold", as: f.lab1Operator, call: recall("nobody-here", 0, true, "", ""), code: connect.CodeNotFound},
		{name: "an online storage named", as: f.lab1Operator, call: recall(subject, 0, true, "", "copy"), code: connect.CodeFailedPrecondition,
			msg: "not of the archive tier"},
		{name: "days above recall_days, operator", as: f.lab1Operator, call: recall(subject, 30, true, "", ""), code: connect.CodePermissionDenied,
			msg: "recall_days", audit: inTenant("lab1", master.OutcomeRefused)},
		{name: "days above recall_days, admin's preview", as: f.lab1Admin, call: recall(subject, 30, true, "", "")},
		{name: "the preview", as: f.lab1Operator, call: recall(subject, 0, true, "", ""),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: subject,
				details: map[string]string{"validate_only": "true", "storages": "cold", "days": "cold:3", "objects": strconv.FormatInt(n-1, 10)}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.RecallResponse)
				if !slices.Equal(res.GetStorages(), []string{"cold"}) || res.GetObjects() != n || res.GetBytes() != total || res.GetObjectsRecalling() != 1 ||
					res.GetObjectsOnline() != 0 || res.GetObjectsRequested() != 0 || res.GetConfirm() == "" || len(res.GetConfirm()) > 128 {
					t.Errorf("the preview is %v, want %d objects of %d bytes, one recalling, and a token", res, n, total)
				}
				token = res.GetConfirm()
			}},
		{name: "a token of other days", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.replicas.Recall, &v1.RecallRequest{Tenant: "lab1", SubjectLabel: subject, Days: 2, Confirm: token})
		}, code: connect.CodeFailedPrecondition, msg: "confirm token"},
		{name: "the recall", as: f.lab1Operator, call: func(c clients) outcome { return recall(subject, 0, false, token, "")(c) },
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: subject,
				details: map[string]string{"validate_only": "false", "objects": strconv.FormatInt(n-1, 10)}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.RecallResponse)
				if res.GetObjectsRequested() != n-1 || res.GetObjectsRecalling() != 1 || res.GetConfirm() != "" {
					t.Errorf("the recall answered %v, want %d asked and the one recalling joined", res, n-1)
				}
			}},
		{name: "the same token again", as: f.lab1Operator, call: func(c clients) outcome { return recall(subject, 0, false, token, "")(c) },
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.RecallResponse)
				if res.GetObjectsRequested() != n-1 || res.GetObjectsRecalling() != n {
					t.Errorf("the token sent again answered %v, want what the first call asked, every object now recalling", res)
				}
			}},
	})
	for _, k := range keys {
		rc, ok, err := r.a.p.Recall(ctx, "cold", k)
		if err != nil || !ok || rc.State != tenant.RecallRequested {
			t.Errorf("the recall of an object is %+v (%v, %v), want requested", rc, ok, err)
			continue
		}
		if rc.Days != 3 || (k != keys[0] && rc.Speed != tenant.RecallBulk) {
			t.Errorf("a recall asks %d days at %q, want the storage's 3 at bulk", rc.Days, rc.Speed)
		}
		asked = append(asked, k)
	}
	if len(asked) != len(keys) {
		t.Errorf("%d of %d objects have a recall", len(asked), len(keys))
	}
	counts, err := r.a.p.RecallCounts(ctx, time.Now())
	if err != nil || counts.Requested != n {
		t.Errorf("the recalls are %+v, %v; want one per object, %d, however often asked", counts, err, n)
	}
}

// testVerifySample guards VerifySample: operator on the tenant, a reader
// refused; a count outside 1 to 10000 is invalid; a storage the tenant does
// not have is not found; a sample of a replica answers its population and
// each object with the key, size, SHA-256 and revision the catalog holds,
// ordered by key, and its entry holds the count and no key; the same seed
// draws the same sample; and a tool outside the server that reads each
// sampled object straight from the replica's directory and hashes it finds
// the one corrupted there.
func testVerifySample(t *testing.T) {
	r := newReplicaFixture(t)
	f := r.f
	sample := func(storage string, count int32, seed *int64) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.replicas.VerifySample, &v1.VerifySampleRequest{Tenant: "lab1", Storage: storage, Count: count, Seed: seed})
		}
	}
	seed := int64(42)
	held := map[string]tenant.ScanInstance{}
	for _, p := range r.a.scans(t) {
		for _, h := range r.a.held(t, p) {
			held[h.Key] = h
		}
	}
	var drawn []*v1.SampledObject
	f.run(t, []tcase{
		{name: "unauthenticated", as: nobody, call: sample("copy", 1, nil), code: connect.CodeUnauthenticated},
		{name: "reader", as: f.lab1Reader, call: sample("copy", 1, nil), code: connect.CodePermissionDenied},
		{name: "a count of 0", as: f.lab1Operator, call: sample("copy", 0, nil), code: connect.CodeInvalidArgument, msg: "count"},
		{name: "a count past 10000", as: f.lab1Operator, call: sample("copy", 10001, nil), code: connect.CodeInvalidArgument, msg: "count"},
		{name: "a storage the tenant does not have", as: f.lab1Operator, call: sample("none", 1, nil), code: connect.CodeNotFound},
		{name: "two of the replica's", as: f.lab1Operator, call: sample("copy", 2, &seed),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "copy", details: map[string]string{"count": "2"}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.VerifySampleResponse)
				drawn = res.GetObjects()
				if res.GetPopulation() != int64(len(held)) || len(drawn) != 2 || drawn[0].GetKey() >= drawn[1].GetKey() {
					t.Fatalf("the sample is %v, want 2 of %d ordered by key", res, len(held))
				}
				for _, s := range drawn {
					h, ok := held[s.GetKey()]
					if !ok || s.GetSizeBytes() != h.Size || s.GetSha256() != h.ArchivedSHA256.String() || s.GetInstanceId() != h.SOPInstanceUID ||
						s.GetRevision() != 1 || s.GetSubjectLabel() == "" || s.GetScanLabel() == "" {
						t.Errorf("a sampled object %v is not as the catalog holds it", s)
					}
				}
			}},
		{name: "the same seed again", as: f.lab1Operator, call: sample("copy", 2, &seed), check: func(t *testing.T, o outcome) {
			got := o.res.(*v1.VerifySampleResponse).GetObjects()
			if len(got) != len(drawn) || got[0].GetKey() != drawn[0].GetKey() || got[1].GetKey() != drawn[1].GetKey() {
				t.Errorf("the same seed drew %v, the first draw %v", got, drawn)
			}
		}},
		{name: "more than the population", as: f.lab1Operator, call: sample("main", 100, nil), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.VerifySampleResponse).GetObjects(); len(got) != len(held) {
				t.Errorf("a sample of 100 answered %d, want the whole population of %d", len(got), len(held))
			}
		}},
	})
	if t.Failed() {
		return
	}
	// The entry holds the count, never a key, which holds a UID as received.
	f.flush()
	for _, en := range f.tenantLog("lab1") {
		for _, s := range drawn {
			if en.Object == s.GetKey() || slices.Contains(mapValues(en.Details), s.GetKey()) {
				t.Errorf("an entry holds a sampled key: %+v", en)
			}
		}
	}
	// Corrupt one sampled object on the replica, then check every sampled
	// object as a tool outside the server does: read it straight from the
	// storage and hash it.
	ctx := context.Background()
	bad := drawn[1].GetKey()
	if _, err := r.copy.Put(ctx, bad, bytes.NewReader([]byte("not the archived bytes"))); err != nil {
		t.Fatal(err)
	}
	var mismatched []string
	for _, s := range drawn {
		rc, err := r.copy.Read(ctx, s.GetKey(), 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(h.Sum(nil)) != s.GetSha256() {
			mismatched = append(mismatched, s.GetKey())
		}
	}
	if !slices.Equal(mismatched, []string{bad}) {
		t.Errorf("the check outside the server found %v differing, want the corrupted %s alone", mismatched, bad)
	}
}
