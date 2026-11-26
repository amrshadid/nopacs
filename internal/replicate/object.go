package replicate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/pace"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// copyObject copies one object of the manifest to the replica, unless the
// replica holds it already and the claim does not ask for it again.
func (s *scanCopy) copyObject(ctx context.Context, o object) error {
	if !o.force {
		has, err := s.holds(ctx, o)
		if err != nil || has {
			return err
		}
	}
	open := func(c context.Context) (io.ReadCloser, error) { return s.primary.Read(c, o.key, 0, -1) }
	sum := o.sum
	if !o.whole {
		// An object holding several files has a checksum per file, none of
		// the whole: one read checks every file's and learns the whole's,
		// and the put then holds the object to that.
		size, whole, ok, err := s.measureFiles(ctx, s.primary, o)
		if err != nil {
			return onPrimary(err, "an object of the scan could not be read from the primary")
		}
		if !ok || size != o.size {
			return &copyError{fault: tenant.FaultWrongAnswer, reason: "an object of the primary does not hold the files its manifest names"}
		}
		sum = whole
	}
	return s.put(ctx, o.key, o.size, sum, open, o.uid(), false)
}

// holds reports whether the replica holds the object already, so it need
// not be put. An object there counts only when something vouches for its
// bytes: the replica's own manifest of the scan, put only after every
// object it names was verified and held verified by the replica's row
// (scanCopy.verified), lists the key with the same lines (the size and
// SHA-256 of each file); or the replica keeps a SHA-256 of the object
// (Info.HasSHA256) equal to the manifest's; or the object, read whole,
// hashes as the manifest says. Its size alone never vouches for it: a put
// whose read back failed, or was cut short by a stop, a lost lease or a
// crash, leaves an object of the right size that nothing checked, and a
// key holds another instance's bytes once that one was purged and its
// labels used again. An object that cannot be read at once (an archive
// tier) and that nothing vouches for is put again.
func (s *scanCopy) holds(ctx context.Context, o object) (bool, error) {
	info, err := s.replica.Stat(ctx, o.key)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return false, nil
	case err != nil:
		return false, onReplica(err, "the replica could not say whether it holds an object")
	}
	listed := slices.Equal(s.verified[o.key], o.lines)
	switch {
	case info.Size != o.size, o.whole && info.HasSHA256 && info.SHA256 != o.sum:
		return false, s.distrust(ctx, o, listed, info)
	case o.whole && info.HasSHA256, listed:
		return true, nil
	case info.State != blob.Online:
		return false, nil
	}
	if o.whole {
		size, sum, err := s.measure(ctx, s.replica, o.key)
		if err != nil {
			return false, onReplica(err, "an object the replica holds could not be read to verify it")
		}
		return size == o.size && sum == o.sum, nil
	}
	size, _, ok, err := s.measureFiles(ctx, s.replica, o)
	if err != nil {
		return false, onReplica(err, "an object the replica holds could not be read to verify it")
	}
	return ok && size == o.size, nil
}

// distrust handles an object the replica holds that is not the manifest's
// bytes. When the replica's verified manifest lists it (listed), that
// manifest no longer vouches for it, so the object is recorded as the
// replica's damage before it is put again: a put then cut short before its
// read back leaves the object named damaged, and the next claim puts it
// again rather than trusting the manifest. A record that fails stops the
// copy before the put, as a database that did not answer.
func (s *scanCopy) distrust(ctx context.Context, o object, listed bool, info blob.Info) error {
	if !listed {
		return nil
	}
	size := info.Size
	var sum tenant.SHA256
	if info.HasSHA256 {
		sum = tenant.SHA256(info.SHA256)
	}
	if err := s.damage(ctx, o.key, o.uid(), &size, sum); err != nil {
		return &databaseError{err: err}
	}
	return nil
}

// put streams the object at key from open to the replica through
// blob.PutConfirmed against size and sum, and reads it back unless the
// replica confirmed the checksum itself. A read back that differs is
// recorded as the replica's damage of the object (uid, empty for the
// scan's manifest or sidecar), so the next claim puts it again, and the
// copy fails; where the record itself fails, the copy fails all the same
// and the object stays unverified (holds never takes it on its size). The
// put and its read back each have putTimeout, shorter than the lease, so a
// storage that hangs cannot hold a scan's lease for ever; and the put is
// cut, by its context, Config.PutMargin before the due its lease's last
// renewal returned, so a copy whose lease was ended under it (a purge's
// start, a delete's publish) lands nothing after that due, which the purge
// waits for. The put enters the replica's write gate (Config.Pause) before
// its first byte and leaves it after its last effect, and its key is
// recorded before it begins (sweep). sidecar says the bytes expected were
// read from the primary moments before rather than given by the catalog:
// bytes that differ then are a change in flight, not a damaged primary.
func (s *scanCopy) put(ctx context.Context, key string, size int64, sum [sha256.Size]byte,
	open func(context.Context) (io.ReadCloser, error), uid string, sidecar bool) error {
	if err := s.lease.keep(ctx); err != nil {
		return err
	}
	// replica.rate paces the bytes put on the replica; the put's limit grows
	// by the time the bound makes it take, so a bound a site set never makes
	// a large object miss its limit, while a storage that hangs still fails;
	// never past the lease's due less the margin, though.
	pacer, rate := s.w.pacing(s.c.Fence.Replica)
	var extra time.Duration
	if rate > 0 {
		extra = time.Duration(float64(max(size, 0)) / float64(rate) * float64(time.Second))
	}
	left := s.lease.due().Add(-s.w.cfg.PutMargin).Sub(s.w.cfg.Now())
	if left <= 0 {
		return &copyError{fault: tenant.FaultUnreachable, reason: "the copy's lease leaves no time for a put before its due"}
	}
	pctx, cancel := context.WithTimeout(ctx, min(s.w.putTimeout()+extra, left))
	defer cancel()
	if s.w.cfg.Pause != nil {
		release, err := s.w.cfg.Pause.Enter(pctx, s.c.Fence.Replica)
		if err != nil {
			return onReplica(err, "the replica's writes are paused")
		}
		defer release()
	}
	body, err := open(pctx)
	if err != nil {
		return onPrimary(err, "an object of the scan could not be read from the primary")
	}
	src := &source{ctx: pctx, r: body, lease: s.lease}
	var in io.Reader = src
	if rate > 0 {
		in = &pace.Reader{Ctx: pctx, R: src, P: pacer, Rate: rate}
	}
	s.putting(key)
	wr, err := blob.PutConfirmed(pctx, s.replica, key, in, size, sum)
	_ = body.Close() // only read from; nothing to lose
	if err != nil {
		switch {
		case stops(err), ctx.Err() != nil:
			return err
		case src.err != nil:
			return onPrimary(src.err, "an object of the scan could not be read from the primary")
		case errors.Is(err, blob.ErrNotTheBytesExpected) && fault.KindOf(err) == fault.DataLoss:
			if sidecar {
				return changed("the primary's sidecar changed while it was copied")
			}
			return &copyError{fault: tenant.FaultWrongAnswer, reason: "the primary gave bytes other than the catalog's for an object", err: err}
		case errors.Is(err, blob.ErrNotTheBytesExpected):
			// The replica reported keeping other bytes than it was given;
			// PutConfirmed removed them, and recording the damage makes the
			// next claim put the object again even where that removal failed.
			return s.wrongOnReplica(ctx, key, uid, nil, tenant.SHA256{}, "the replica reported keeping other bytes than it was given", err)
		}
		return onReplica(err, "the replica did not take an object")
	}
	s.bytes.add(wr.Size)
	if wr.Confirmed {
		return nil
	}
	rctx, rcancel := context.WithTimeout(ctx, s.w.putTimeout())
	defer rcancel()
	gotSize, gotSum, err := s.measure(rctx, s.replica, key)
	if stops(err) || ctx.Err() != nil {
		return err
	}
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return s.wrongOnReplica(ctx, key, uid, nil, tenant.SHA256{}, "an object put on the replica was not there when read back", nil)
	case err != nil:
		return onReplica(err, "an object put on the replica could not be read back")
	case gotSize != size || gotSum != sum:
		return s.wrongOnReplica(ctx, key, uid, &gotSize, tenant.SHA256(gotSum), "an object read back from the replica is not the bytes put", nil)
	}
	return nil
}

// wrongOnReplica records an object found missing or wrong on the replica
// right after its put as the replica's damage and returns the copy's
// failure, its reason saying when the record failed too.
func (s *scanCopy) wrongOnReplica(ctx context.Context, key, uid string, size *int64, sum tenant.SHA256, reason string, err error) error {
	if derr := s.damage(ctx, key, uid, size, sum); derr != nil {
		reason += ", and could not be recorded as damage"
	}
	return &copyError{fault: tenant.FaultWrongAnswer, reason: reason, err: err}
}

// damage records an object of the replica found missing (size nil) or
// wrong, under no fence: the claim's row stays leased to this copy, counts
// the change, and is left damaged by the copy's end or failure, so the
// next claim names the object. A record that fails is logged and returned.
func (s *scanCopy) damage(ctx context.Context, key, uid string, size *int64, sum tenant.SHA256) error {
	rev := 0
	if uid != "" {
		if k, err := layout.Parse(key); err == nil && k.Kind == layout.Instance {
			rev = k.Rev()
		}
	}
	if size == nil {
		sum = tenant.SHA256{}
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	_, err := s.w.cfg.Queue.RecordReplicaDamage(cctx, tenant.ReplicaDamageRecord{Replica: s.c.Fence.Replica, SeriesUID: s.c.Fence.SeriesUID,
		Damage: tenant.ReplicaDamage{SOPInstanceUID: uid, Revision: rev, FoundSize: size, FoundSHA256: sum, Found: s.w.cfg.Now()}})
	if err != nil {
		s.w.log.Warn("replicas: an object of the replica found wrong could not be recorded as damage", "replica", s.c.Fence.Replica,
			"kind", fault.KindOf(err).String())
	}
	return err
}

// measure reads the object at key on store whole and returns its size and
// SHA-256, streaming it, with the lease kept while it reads.
func (s *scanCopy) measure(ctx context.Context, store blob.Store, key string) (int64, [sha256.Size]byte, error) {
	body, err := store.Read(ctx, key, 0, -1)
	if err != nil {
		return 0, [sha256.Size]byte{}, err
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	h := sha256.New()
	n, err := io.CopyBuffer(h, &source{ctx: ctx, r: body, lease: s.lease}, make([]byte, copyBuffer))
	if err != nil {
		return 0, [sha256.Size]byte{}, err
	}
	return n, [sha256.Size]byte(h.Sum(nil)), nil
}

// measureFiles reads the object o on store whole and returns its size, its
// SHA-256, and whether each of its files (the manifest's lines) has the
// SHA-256 its line names.
func (s *scanCopy) measureFiles(ctx context.Context, store blob.Store, o object) (int64, [sha256.Size]byte, bool, error) {
	body, err := store.Read(ctx, o.key, 0, -1)
	if err != nil {
		return 0, [sha256.Size]byte{}, false, err
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	fh := newFileHashes(o.lines)
	whole := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(whole, fh), &source{ctx: ctx, r: body, lease: s.lease}, make([]byte, copyBuffer))
	if err != nil {
		return 0, [sha256.Size]byte{}, false, err
	}
	return n, [sha256.Size]byte(whole.Sum(nil)), fh.ok(), nil
}

// copyBuffer is the buffer of one stream a check reads: what a stream
// holds of an object at a time.
const copyBuffer = 256 << 10

// fileHashes hashes, from an object's bytes written to it in order, each
// file of the object at its offset and length.
type fileHashes struct {
	lines []manifest.File
	h     []hash.Hash
	at    int64
}

func newFileHashes(lines []manifest.File) *fileHashes {
	f := &fileHashes{lines: lines, h: make([]hash.Hash, len(lines))}
	for i := range f.h {
		f.h[i] = sha256.New()
	}
	return f
}

// Write hashes the part of p inside each file's range.
func (f *fileHashes) Write(p []byte) (int, error) {
	start, end := f.at, f.at+int64(len(p))
	for i, l := range f.lines {
		lo, hi := max(start, l.Offset), min(end, l.Offset+l.Length)
		if lo < hi {
			_, _ = f.h[i].Write(p[lo-start : hi-start]) // A hash never returns an error.
		}
	}
	f.at = end
	return len(p), nil
}

// ok reports whether every file was read whole with its line's SHA-256.
func (f *fileHashes) ok() bool {
	for i, l := range f.lines {
		if l.Offset+l.Length > f.at || hex.EncodeToString(f.h[i].Sum(nil)) != l.SHA256 {
			return false
		}
	}
	return true
}

// lease is a claim's lease. A timer renews it every third of it while the
// scan is claimed (hold), whatever the copy is doing, and the copy's own
// steps renew it too once a third of it has passed by the worker's clock
// (keep). A renewal that fails ends the lease for good and cancels the
// scan's context (ctx) with the failure as its cause, so no later put,
// sidecar or manifest is written: tenant.ErrLeaseLost once another attempt
// has the row, a databaseError when the database did not answer (the row
// is then left to its lease, and nothing is counted against the replica).
type lease struct {
	w      *Worker
	fence  tenant.ReplicaFence
	ctx    context.Context
	cancel context.CancelCauseFunc
	mu     sync.Mutex
	last   time.Time
	lost   error
}

// keep renews the lease when a third of it has passed by the worker's
// clock and returns nil while it holds: the lease's failure once it ended,
// and ctx's error once ctx has ended.
func (l *lease) keep(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost != nil {
		return l.lost
	}
	now := l.w.cfg.Now()
	if now.Sub(l.last) < l.w.cfg.Lease/3 {
		return nil
	}
	return l.renewLocked(now)
}

// due returns when the lease runs out as its claim or its last renewal set
// it in the database: a lease after the worker's time then.
func (l *lease) due() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last.Add(l.w.cfg.Lease)
}

// renewLocked renews the lease at now, l.mu held, and ends it on a failure.
func (l *lease) renewLocked(now time.Time) error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(l.ctx, opTimeout)
	defer cancel()
	if err := l.w.cfg.Queue.RenewReplicaScan(cctx, l.fence, now, l.w.cfg.Lease); err != nil {
		if l.ctx.Err() != nil {
			return l.ctx.Err()
		}
		if errors.Is(err, tenant.ErrLeaseLost) {
			l.lost = err
		} else {
			l.lost = &databaseError{err: err}
		}
		l.cancel(l.lost)
		return l.lost
	}
	l.last = now
	return nil
}

// hold renews the lease from a timer every third of it until the returned
// stop is called, apart from any read: a storage that takes a put's whole
// body before it answers (an S3 single-part put buffers it to compute its
// checksum) reads nothing for a time, and a lease renewed only as bytes
// are read would run out under it while another worker claims the scan.
// stop waits for a renewal in flight, so none runs after it returns.
func (l *lease) hold() (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(max(l.w.cfg.Lease/3, time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-l.ctx.Done():
				return
			case <-t.C:
				l.mu.Lock()
				err := l.lost
				if err == nil {
					err = l.renewLocked(l.w.cfg.Now())
				}
				l.mu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	return func() { close(done); <-finished }
}

// source is a stream read from a storage for a copy or a check: it keeps
// the claim's lease as it is read, and remembers the storage's own error
// apart from the lease's.
type source struct {
	ctx   context.Context
	r     io.Reader
	lease *lease
	// err is the first error of r, the storage read from.
	err error
}

// Read keeps the lease, then reads.
func (s *source) Read(p []byte) (int, error) {
	if err := s.lease.keep(s.ctx); err != nil {
		return 0, err
	}
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && s.err == nil {
		s.err = err
	}
	return n, err
}

// counter counts bytes from several copies at once.
type counter struct{ n atomic.Int64 }

func (c *counter) add(n int64) { c.n.Add(n) }
func (c *counter) load() int64 { return c.n.Load() }

// copyError is a copy's failure as its row records it: the contract's
// storage fault, a phrase of the server's (never a storage's own words,
// which may name a path or an endpoint), and whether it is a change of the
// scan in flight, tried again soon and not counted against the replica.
type copyError struct {
	fault, reason string
	err           error
	changed       bool
}

func (e *copyError) Error() string { return e.reason }
func (e *copyError) Unwrap() error { return e.err }

// changed returns the failure of a scan that changed under its copy.
func changed(reason string) error {
	return &copyError{fault: tenant.FaultWrongAnswer, reason: reason, changed: true}
}

// onPrimary returns err, a failure of the primary, as a copy's failure; a
// lost lease, a database that did not answer and an ended context pass as
// they are.
func onPrimary(err error, reason string) error {
	if stops(err) || errors.Is(err, context.Canceled) {
		return err
	}
	return &copyError{fault: faultOf(err), reason: reason, err: err}
}

// onReplica returns err, a failure of the replica, as a copy's failure; a
// lost lease, a database that did not answer and an ended context pass as
// they are.
func onReplica(err error, reason string) error { return onPrimary(err, reason) }

// stops reports whether err stops the copy with nothing recorded against
// a storage: a lost lease, or the tenant's database not answering.
func stops(err error) bool {
	return errors.Is(err, tenant.ErrLeaseLost) || errors.As(err, new(*databaseError))
}

// faultOf returns the contract's storage fault for a storage's error, read
// from its kind and the port's sentinels, never its words: other bytes than
// expected are a wrong answer; not found is not found; a refusal or a
// precondition the storage holds is refused; anything else, a time out
// among them, is a storage that did not answer.
func faultOf(err error) string {
	switch {
	case errors.Is(err, blob.ErrNotTheBytesExpected), fault.KindOf(err) == fault.DataLoss:
		return tenant.FaultWrongAnswer
	case errors.Is(err, context.DeadlineExceeded):
		return tenant.FaultUnreachable
	}
	switch fault.KindOf(err) {
	case fault.NotFound:
		return tenant.FaultNotFound
	case fault.Precondition, fault.Denied, fault.Unauthenticated:
		return tenant.FaultRefused
	case fault.Invalid:
		return tenant.FaultWrongAnswer
	}
	return tenant.FaultUnreachable
}
