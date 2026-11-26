package replicate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// outcome is how one scan's copy ended.
type outcome int

const (
	// stopped: a lease lost or a stop; nothing was written to the
	// database, and the row is left to its lease.
	stopped outcome = iota
	// endedInStep: ended, and recorded in step.
	endedInStep
	// endedAgain: ended, and left pending by a change published since the
	// claim.
	endedAgain
	// failed: the failure was recorded, the row due again later.
	failed
)

// object is one object of a scan's manifest to copy: its key, its size,
// and the manifest's lines of the files it holds.
type object struct {
	key  string
	size int64
	// whole says the object is one file, whose SHA-256 is sum; an object
	// holding several files has a checksum for each file only.
	whole bool
	sum   [sha256.Size]byte
	lines []manifest.File
	// force says it is put again even when the replica holds it: the
	// claim names it damaged, or says to copy everything.
	force bool
}

// uid returns the SOP Instance UID of the object's first file, the one its
// damage is recorded under.
func (o object) uid() string { return o.lines[0].SOPInstanceUID }

// copyScan copies one claimed scan to its replica and ends or fails its
// row, and returns how it ended and the bytes it put.
// The copy runs under a context of its own, which a lease that ends
// cancels with its failure as the cause; that cause, not the bare
// cancellation the copy's steps saw, is how the copy ended.
//
// A copy that did not end (a change published under it, its lease lost, or
// its scan gone) removes, before its row is written, what it put that the
// catalog no longer names (sweep).
func (w *Worker) copyScan(ctx context.Context, c tenant.ReplicaClaim, claimed time.Time, stores *storeSet) (outcome, int64) {
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sc := &scanCopy{w: w, c: c, lease: &lease{w: w, fence: c.Fence, ctx: sctx, cancel: cancel, last: claimed}}
	stop := sc.lease.hold()
	err := sc.run(sctx, stores)
	stop()
	if cause := context.Cause(sctx); cause != nil && ctx.Err() == nil {
		err = cause
	}
	return sc.finish(ctx, err), sc.bytes.load()
}

// scanCopy is one scan's copy at work.
type scanCopy struct {
	w       *Worker
	c       tenant.ReplicaClaim
	lease   *lease
	primary blob.Store
	replica blob.Store
	bytes   counter
	// verified is, by key, the lines of the replica's own manifest of the
	// scan when the replica's row holds that manifest as verified; nil
	// when nothing on the replica is vouched for (verifiedOnReplica).
	verified map[string][]manifest.File
	// mu guards putKeys.
	mu sync.Mutex
	// putKeys are the keys this copy asked the replica to put, each
	// recorded before its put began: a put that failed or was cut may still
	// land.
	putKeys []string
}

// run copies the scan: its objects, then its sidecar, then its manifest.
// A nil error means every object the manifest names is on the replica,
// verified, and the manifest last.
func (s *scanCopy) run(ctx context.Context, stores *storeSet) error {
	var err error
	if s.primary, err = stores.get(ctx, s.c.Primary); err != nil {
		return onPrimary(err, "the primary storage could not be opened")
	}
	if s.replica, err = stores.get(ctx, s.c.Fence.Replica); err != nil {
		return onReplica(err, "the replica storage could not be opened")
	}
	if s.c.ManifestSHA256.IsZero() {
		return &copyError{fault: tenant.FaultWrongAnswer, reason: "the catalog holds no manifest digest for the scan, so no copy can be verified"}
	}
	data, err := s.readManifest(ctx)
	if err != nil {
		return err
	}
	m, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		return &copyError{fault: tenant.FaultWrongAnswer, reason: "the primary's manifest of the scan does not decode", err: err}
	}
	held, err := s.held(ctx)
	if err != nil {
		return err
	}
	objects, err := plan(m, held, s.c)
	if err != nil {
		return err
	}
	if s.verified, err = s.verifiedOnReplica(ctx); err != nil {
		return err
	}
	if err := s.copyObjects(ctx, objects); err != nil {
		return err
	}
	if err := s.copySidecar(ctx); err != nil {
		return err
	}
	return s.putManifest(ctx, data)
}

// readManifest reads the primary's manifest of the scan whole and returns
// it when its SHA-256 is the claim's digest. Another manifest is a change
// in flight (a reception writes its next manifest before it commits) or a
// damaged primary: either way nothing it names is copied.
func (s *scanCopy) readManifest(ctx context.Context) ([]byte, error) {
	key, err := layout.ManifestKey(s.c.Path)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	body, err := s.primary.Read(ctx, key, 0, -1)
	if err != nil {
		return nil, onPrimary(err, "the primary's manifest of the scan could not be read")
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	data, err := io.ReadAll(io.LimitReader(body, int64(manifest.MaxBytes)+1))
	if err != nil {
		return nil, onPrimary(err, "the primary's manifest of the scan could not be read")
	}
	if tenant.SHA256(sha256.Sum256(data)) != s.c.ManifestSHA256 {
		return nil, changed("the primary's manifest is not the one claimed: the scan changed, or the primary's manifest is damaged")
	}
	return data, nil
}

// verifiedOnReplica returns, by key, the lines of the replica's own
// manifest of the scan, when the replica's row holds that manifest's
// SHA-256 as the digest last verified: a copy puts the manifest last, only
// once every object it names was put and verified, so each line vouches
// for its object as long as the object keeps the line's size. It returns
// none on a backfill or a claim that puts everything again (the replica
// may hold copies made elsewhere, which nothing here verified), when the
// claim names the replica's manifest or sidecar damaged, and when the
// replica's manifest is missing, cannot be read at once, does not decode,
// or is another than the row's.
func (s *scanCopy) verifiedOnReplica(ctx context.Context) (map[string][]manifest.File, error) {
	if s.c.Backfill || s.c.RecopyAll {
		return nil, nil
	}
	for _, d := range s.c.Damaged {
		if d.SOPInstanceUID == "" {
			return nil, nil
		}
	}
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	row, err := s.w.cfg.Queue.ReplicaScan(cctx, s.c.Fence.Replica, s.c.Fence.SeriesUID)
	cancel()
	if err != nil {
		return nil, &databaseError{err: err}
	}
	if row.ManifestSHA256.IsZero() {
		return nil, nil
	}
	key, err := layout.ManifestKey(s.c.Path)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	body, err := s.replica.Read(ctx, key, 0, -1)
	switch {
	case fault.KindOf(err) == fault.NotFound, errors.Is(err, blob.ErrNotOnline):
		return nil, nil
	case err != nil:
		return nil, onReplica(err, "the replica's manifest of the scan could not be read")
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	data, err := io.ReadAll(io.LimitReader(body, int64(manifest.MaxBytes)+1))
	if err != nil {
		return nil, onReplica(err, "the replica's manifest of the scan could not be read")
	}
	if tenant.SHA256(sha256.Sum256(data)) != row.ManifestSHA256 {
		return nil, nil
	}
	m, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, nil
	}
	out := make(map[string][]manifest.File, len(m.Files))
	for _, f := range m.Files {
		out[f.Key] = append(out[f.Key], f)
	}
	return out, nil
}

// held returns the instances the catalog holds in the scan now, by SOP
// Instance UID. A scan the catalog no longer holds is errScanGone.
func (s *scanCopy) held(ctx context.Context) (map[string]tenant.ScanInstance, error) {
	out := map[string]tenant.ScanInstance{}
	page := master.Page{Limit: master.MaxPageLimit}
	for range manifest.MaxFiles/master.MaxPageLimit + 2 {
		cctx, cancel := context.WithTimeout(ctx, opTimeout)
		rows, next, err := s.w.cfg.Catalog.ListHeldInstances(cctx, s.c.Path, page)
		cancel()
		if fault.KindOf(err) == fault.NotFound {
			return nil, errScanGone
		}
		if err != nil {
			return nil, &databaseError{err: err}
		}
		for _, in := range rows {
			out[in.SOPInstanceUID] = in
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
	return nil, fault.New(fault.Internal, "replicate: the catalog's instances of a scan did not end")
}

// databaseError is a read or a write of the tenant's database (a renewal
// of the lease among them) that failed during a copy: not the replica's
// fault, and likely to fail a write of it too, so nothing is recorded, the
// replica's wait before its next try is not raised, and the row is left
// to its lease.
type databaseError struct{ err error }

func (e *databaseError) Error() string {
	return "the tenant's database did not answer: " + e.err.Error()
}

func (e *databaseError) Unwrap() error { return e.err }

// errScanGone is a claimed scan the catalog no longer holds: its row is
// ended, and the database drops it.
var errScanGone = errors.New("the catalog no longer holds the scan")

// plan returns the objects of m to copy: every object it names, an
// instance a delete hid among them. A replica is a faithful copy of its
// storage, not of the catalog's view: a delete hides an instance and
// leaves its object and its manifest line on the primary until its purge
// (which removes them from every copy), and a replica that skipped them
// would not be in step until then. What keeps bytes not yet committed off
// the replica is the manifest's SHA-256 being the claim's (the catalog's
// committed digest), and each object's put checked against its manifest
// lines' sizes and SHA-256. A line of an instance the catalog holds must
// also agree with the catalog's row of it: one that does not is a change
// of the scan published since the manifest was read, and fails the plan as
// a change, so the scan is tried again soon.
//
// An object whose every file is an instance a purging correction hides
// (the claim's Purging) is not copied: the purge removes it from every
// copy it took when it began, so one put now would outlive the purge, and
// the purge then writes the scan's manifest again without it.
func plan(m manifest.Manifest, held map[string]tenant.ScanInstance, c tenant.ReplicaClaim) ([]object, error) {
	damaged := make(map[string]bool, len(c.Damaged))
	for _, d := range c.Damaged {
		damaged[d.SOPInstanceUID] = true
	}
	purging := make(map[string]bool, len(c.Purging))
	for _, uid := range c.Purging {
		purging[uid] = true
	}
	var out []object
	contradicted := 0
	for i := 0; i < len(m.Files); {
		j := i + 1
		for j < len(m.Files) && m.Files[j].Key == m.Files[i].Key {
			j++
		}
		lines := m.Files[i:j]
		i = j
		o := object{key: lines[0].Key, size: lines[0].Size, lines: lines, force: c.RecopyAll}
		hidden := 0
		for _, f := range lines {
			if purging[f.SOPInstanceUID] {
				hidden++
			}
			if f.Size != o.size {
				return nil, &copyError{fault: tenant.FaultWrongAnswer, reason: "the primary's manifest gives one object two sizes"}
			}
			in, isHeld := held[f.SOPInstanceUID]
			if isHeld && (in.Key != f.Key || in.Offset != f.Offset || in.Length != f.Length || in.Size != f.Size ||
				in.ArchivedSHA256.String() != f.SHA256) {
				contradicted++
			}
			if damaged[f.SOPInstanceUID] {
				o.force = true
			}
		}
		if hidden == len(lines) {
			continue
		}
		if len(lines) == 1 && lines[0].Offset == 0 && lines[0].Length == lines[0].Size {
			sum, err := hex.DecodeString(lines[0].SHA256)
			if err != nil || len(sum) != sha256.Size {
				return nil, &copyError{fault: tenant.FaultWrongAnswer, reason: "the primary's manifest holds a checksum that is not a SHA-256"}
			}
			o.whole, o.sum = true, [sha256.Size]byte(sum)
		}
		out = append(out, o)
	}
	if contradicted > 0 {
		return nil, changed("the primary's manifest names an instance otherwise than the catalog's row of it; nothing is copied")
	}
	return out, nil
}

// copyObjects copies the objects, each under one of the replica's copy
// slots, and returns the first failure; a failure stops the copies not yet
// started.
func (s *scanCopy) copyObjects(ctx context.Context, objects []object) error {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
		}
		mu.Unlock()
		cancel()
	}
	for _, o := range objects {
		if err := s.lease.keep(cctx); err != nil {
			fail(err)
			break
		}
		release, err := s.w.slot(cctx, s.c.Fence.Replica)
		if err != nil {
			fail(err)
			break
		}
		wg.Add(1)
		go func(o object) {
			defer func() { release(); wg.Done() }()
			if err := s.copyObject(cctx, o); err != nil {
				fail(err)
			}
		}(o)
	}
	wg.Wait()
	// The failure that canceled the others was kept before it canceled
	// them, so first is never a copy cut short by the cancel.
	mu.Lock()
	defer mu.Unlock()
	return first
}

// copySidecar copies the scan's sidecar of headers. When the catalog keeps
// its digest (the claim's SidecarSHA256, written in the commit that wrote
// the claimed manifest), the sidecar is put with blob.PutConfirmed against
// that digest, its size from the primary's Stat: bytes of another sidecar
// (a reception's next one, put before its commit, or a rewrite) are
// refused, and the copy fails as a change, so the replica never holds a
// sidecar the catalog does not name beside the manifest it does.
//
// A scan whose digest the catalog does not keep (archived before it was
// kept, or changed since by a writer that did not tell it) is copied as it
// always was: the sidecar is read once to learn its size and SHA-256, then
// put with blob.PutConfirmed against them, so a sidecar rewritten between
// the two reads is refused, not copied half old. Nothing then ties the
// sidecar read to the claimed manifest, and a reception writes its next
// sidecar, then its next manifest, then commits: so after the put the
// primary's manifest is read again and its sidecar measured again, and the
// copy goes on only when the manifest is still the claim's and the sidecar
// still the bytes put. Otherwise a reception not yet committed is in
// flight, and the copy fails as a change. A scan archived before sidecars
// has none, and none is copied.
func (s *scanCopy) copySidecar(ctx context.Context) error {
	key, err := layout.MetadataKey(s.c.Path)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	if !s.c.SidecarSHA256.IsZero() {
		return s.copyKnownSidecar(ctx, key)
	}
	size, sum, err := s.measure(ctx, s.primary, key)
	if fault.KindOf(err) == fault.NotFound {
		return nil
	}
	if err != nil {
		return onPrimary(err, "the primary's sidecar of the scan could not be read")
	}
	release, err := s.w.slot(ctx, s.c.Fence.Replica)
	if err != nil {
		return err
	}
	defer release()
	if err := s.put(ctx, key, size, sum, func(c context.Context) (io.ReadCloser, error) { return s.primary.Read(c, key, 0, -1) }, "", true); err != nil {
		return err
	}
	if _, err := s.readManifest(ctx); err != nil {
		return err
	}
	again, againSum, err := s.measure(ctx, s.primary, key)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return changed("the primary's sidecar went while it was copied")
	case err != nil:
		return onPrimary(err, "the primary's sidecar of the scan could not be read")
	case again != size || againSum != sum:
		return changed("the primary's sidecar changed while it was copied")
	}
	return nil
}

// copyKnownSidecar puts the primary's sidecar at key on the replica against
// the catalog's digest of it (copySidecar).
func (s *scanCopy) copyKnownSidecar(ctx context.Context, key string) error {
	info, err := s.primary.Stat(ctx, key)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return changed("the primary's sidecar is not there though the catalog keeps its digest")
	case err != nil:
		return onPrimary(err, "the primary's sidecar of the scan could not be read")
	}
	release, err := s.w.slot(ctx, s.c.Fence.Replica)
	if err != nil {
		return err
	}
	defer release()
	return s.put(ctx, key, info.Size, [sha256.Size]byte(s.c.SidecarSHA256),
		func(c context.Context) (io.ReadCloser, error) { return s.primary.Read(c, key, 0, -1) }, "", true)
}

// putManifest puts the manifest's bytes, read and checked against the
// claim's digest, on the replica, last.
func (s *scanCopy) putManifest(ctx context.Context, data []byte) error {
	if err := s.lease.keep(ctx); err != nil {
		return err
	}
	key, err := layout.ManifestKey(s.c.Path)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	release, err := s.w.slot(ctx, s.c.Fence.Replica)
	if err != nil {
		return err
	}
	defer release()
	return s.put(ctx, key, int64(len(data)), [sha256.Size]byte(s.c.ManifestSHA256),
		func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, "", false)
}

// finish writes the row's end or failure for err and returns the outcome.
// A copy that did not end in step (a change published under it, its lease
// lost, its scan gone) first removes what it put that the catalog no
// longer names (sweep).
func (s *scanCopy) finish(ctx context.Context, err error) outcome {
	w, c := s.w, s.c
	log := w.log.With("replica", c.Fence.Replica, "scan", scanPath(c.Path))
	if sweeps(err) {
		s.sweep(ctx)
	}
	switch {
	case errors.Is(err, tenant.ErrLeaseLost):
		log.Info("replicas: the lease of a scan was lost while it copied; another worker has it, and this copy stops")
		return stopped
	case ctx.Err() != nil:
		log.Info("replicas: the worker stopped while a scan copied; its lease runs out and the next claim copies it")
		return stopped
	case errors.As(err, new(*databaseError)):
		log.Warn("replicas: the catalog could not be read for a scan's copy; its lease runs out and it is copied again",
			"kind", fault.KindOf(err).String())
		return stopped
	case err == nil, errors.Is(err, errScanGone):
		cctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		state, eerr := w.cfg.Queue.EndReplicaScan(cctx, c.Fence, c.ManifestSHA256, w.cfg.Now())
		if eerr != nil {
			if !errors.Is(eerr, tenant.ErrLeaseLost) {
				log.Warn("replicas: a scan's copy could not be recorded; its lease runs out and it is copied again", "kind", fault.KindOf(eerr).String())
			} else if err == nil {
				// The lease was ended under the copy (its scan deleted, a
				// purge begun): what it put may be what the catalog dropped.
				s.sweep(ctx)
			}
			return stopped
		}
		if state == tenant.ReplicaInStep {
			w.succeeded(c.Fence.Replica)
			return endedInStep
		}
		if err == nil {
			// A change was published while the copy ran.
			s.sweep(ctx)
		}
		return endedAgain
	}
	var ce *copyError
	if !errors.As(err, &ce) {
		ce = &copyError{fault: faultOf(err), reason: "the copy failed", err: err}
	}
	now := w.cfg.Now()
	retry := now.Add(w.cfg.ChangedDelay)
	if !ce.changed {
		retry = w.retryAfter(c.Fence.Replica, now)
	}
	log.Warn("replicas: a scan's copy failed; it is tried again", "fault", ce.fault, "reason", ce.reason,
		"kind", fault.KindOf(ce.err).String(), "retry", retry)
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	ferr := w.cfg.Queue.FailReplicaScan(cctx, tenant.ReplicaFailure{Fence: c.Fence, Fault: ce.fault, Reason: clip(ce.reason),
		Retry: retry, Now: now})
	if ferr != nil {
		if !errors.Is(ferr, tenant.ErrLeaseLost) {
			log.Warn("replicas: a scan's failed copy could not be recorded; its lease runs out", "kind", fault.KindOf(ferr).String())
		} else if !ce.changed {
			// The lease was ended under the copy, whose put then failed.
			s.sweep(ctx)
		}
		return stopped
	}
	return failed
}

// clip cuts a reason to what a failure keeps.
func clip(s string) string {
	if len(s) > tenant.MaxReplicaReason {
		return s[:tenant.MaxReplicaReason]
	}
	return s
}

// scanPath names a scan in a log line by its labels, which are the
// archive's own after de-identification.
func scanPath(p label.Path) string { return p.Subject + "/" + p.Session + "/" + p.Scan }
