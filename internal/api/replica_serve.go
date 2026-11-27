package api

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	mrand "math/rand/v2"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// BackfillReplica queues a replicate job that copies to a replica every
// object of its primary it does not hold verified; the replica's job
// queued or running is answered instead.
func (s replicaService) BackfillReplica(ctx context.Context, req *v1.BackfillReplicaRequest) (*v1.BackfillReplicaResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.ReplicaServiceBackfillReplicaProcedure)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{}}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), facts.Details)
	if err != nil {
		return nil, err
	}
	if err := itemName("replica", req.GetReplica()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rs, err := replication(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failWith(d, st, facts, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	// The store answers the replica's job queued or running instead, and
	// writes that job's id in the entry's details in place of this one.
	facts.Details[detailJob] = id
	job, _, err := rs.BackfillReplica(ctx, tenant.ReplicaBackfill{Replica: req.GetReplica(), JobID: id, Now: c.now()},
		changeEntry(d, facts), c.roleGuard(d, master.RoleOperator))
	if err != nil {
		return nil, c.failWith(d, st, facts, storageNotFound(err))
	}
	return &v1.BackfillReplicaResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, job)}, nil
}

// reasonReplicaNotAnswering is why a replicate job is not moving: the last
// copy to its replica failed, as the replica's status says (its
// last_fault), so the scans its backfill owes stay pending until the
// replica answers.
const reasonReplicaNotAnswering = "replica not answering"

// replicaHeld returns the phrase of a replicate job in progress whose
// replica's last copy failed, or "" for one moving, ended, or whose
// replica cannot be read.
func replicaHeld(ctx context.Context, p tenant.Pipeline, j tenant.Job) string {
	rs, ok := p.(tenant.Replication)
	if j.State.Ended() || j.Reason != "" || j.Replicate == nil || !ok {
		return ""
	}
	list, _, err := rs.ListReplicas(ctx, j.Replicate.Primary, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		return ""
	}
	for _, r := range list {
		if r.Replica == j.Replicate.Replica && r.LastFault != "" {
			return reasonReplicaNotAnswering
		}
	}
	return ""
}

// The bounds of a sample, the contract's.
const (
	minSample = 1
	maxSample = 10000
)

// detailCount is the number of instances a sample asked for.
const detailCount = "count"

// VerifySample answers a random sample of the instances a storage holds,
// each as the catalog holds it, drawn again the same way from the same seed.
func (s replicaService) VerifySample(ctx context.Context, req *v1.VerifySampleRequest) (*v1.VerifySampleResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.ReplicaServiceVerifySampleProcedure)
	if err != nil {
		return nil, err
	}
	// The count only: a key holds a SOP Instance UID as received, so none
	// is written to the entry.
	details := map[string]string{detailCount: strconv.Itoa(int(req.GetCount()))}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), details)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: details}
	if err := itemName("storage", req.GetStorage()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	if req.GetCount() < minSample || req.GetCount() > maxSample {
		return nil, c.failWith(d, st, facts, invalid("count", "is 1 to 10000"))
	}
	if _, err := lookupStorage(ctx, p, req.GetStorage()); err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	rs, err := replication(st)
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	seed := req.GetSeed()
	if req.Seed == nil {
		if seed, err = newSeed(); err != nil {
			return nil, c.failWith(d, st, facts, err)
		}
	}
	got, population, err := drawSample(ctx, rs, req.GetStorage(), int(req.GetCount()), seed)
	if err != nil {
		return nil, c.failWith(d, st, facts, storageNotFound(err))
	}
	c.record(d, st, master.OutcomeDone, "", facts)
	out := &v1.VerifySampleResponse{Population: population, Objects: make([]*v1.SampledObject, 0, len(got))}
	for _, o := range got {
		out.Objects = append(out.Objects, sampledMessage(o))
	}
	return out, nil
}

// newSeed returns a seed for a draw no caller named.
func newSeed() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fault.Wrap(fault.Internal, err, "api: no random seed for a sample")
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil // #nosec G115 -- any 64 bits are a seed.
}

// drawSample walks every object of storage in the store's order once and
// keeps a uniform sample of count of them (reservoir sampling, Vitter's
// Algorithm R) from a generator seeded with seed: the same seed over the
// same population, in the same order, keeps the same sample, so a check
// can be repeated. It answers the sample ordered by key, and the
// population.
func drawSample(ctx context.Context, rs tenant.Replication, storage string, count int, seed int64) ([]tenant.StoredObject, int64, error) {
	rng := mrand.New(mrand.NewPCG(uint64(seed), sampleStream)) // #nosec G115,G404 -- a repeatable draw, not a secret.
	kept := make([]tenant.StoredObject, 0, min(count, master.MaxPageLimit))
	var seen int64
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		objs, next, err := rs.StorageObjects(ctx, tenant.StorageObjectQuery{Storage: storage}, page)
		if err != nil {
			return nil, 0, err
		}
		for _, o := range objs {
			if len(kept) < count {
				kept = append(kept, o)
			} else if j := rng.Int64N(seen + 1); j < int64(count) {
				kept[j] = o
			}
			seen++
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	slices.SortFunc(kept, func(a, b tenant.StoredObject) int { return strings.Compare(a.Key, b.Key) })
	return kept, seen, nil
}

// sampleStream is the second word of the sample generator's state: fixed,
// so the seed alone decides the draw.
const sampleStream = 0x6e6f7061637321

// sampledMessage returns one object of a sample as the contract writes it.
func sampledMessage(o tenant.StoredObject) *v1.SampledObject {
	rev := 1
	if k, err := layout.Parse(o.Key); err == nil {
		rev = k.Rev()
	}
	return &v1.SampledObject{Key: o.Key, SizeBytes: o.Size, Sha256: o.ArchivedSHA256.String(), InstanceId: o.SOPInstanceUID,
		Revision: int32(rev), SubjectLabel: o.Path.Subject, SessionLabel: o.Path.Session, ScanLabel: o.Path.Scan} // #nosec G115 -- a revision is at most layout.MaxRevision.
}
