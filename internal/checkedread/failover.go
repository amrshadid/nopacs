package checkedread

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Object names one archived object a read opens: where the catalog places
// it, and the scan it belongs to, which says what replicas hold it.
type Object struct {
	// Storage is the storage the catalog places the object on, its scan's
	// primary; Key is its key there, the same on every replica.
	Storage, Key string
	// SOPInstanceUID is the instance the object holds; empty for a scan's
	// metadata file.
	SOPInstanceUID string
	// SeriesUID names the object's scan when the reader knows it; Path
	// otherwise, looked up only when the read turns from the primary.
	SeriesUID string
	Path      label.Path
	// Damaged says a scrub found the primary's object missing or not what
	// was archived (tenant.ScanInstance.Damaged): a replica in step is read
	// instead, and the primary not at all.
	Damaged bool
	// SHA256 is the SHA-256 of the whole object the catalog keeps, for a
	// scan's metadata file whose digest it keeps
	// (tenant.CatalogScan.SidecarSHA256); zero when the reader knows none,
	// and then the object is not checked here (checkFile).
	SHA256 tenant.SHA256
}

// InstanceObject returns the object of the instance in, of the scan whose
// series is seriesUID (empty when not known) and whose labels are path.
func InstanceObject(in tenant.ScanInstance, seriesUID string, path label.Path) Object {
	return Object{Storage: in.Storage, Key: in.Key, SOPInstanceUID: in.SOPInstanceUID, SeriesUID: seriesUID, Path: path, Damaged: in.Damaged}
}

// Opened is an object opened for reading and the storage that serves it.
type Opened struct {
	// Body is the stream of the bytes asked for, which the caller closes;
	// nil for a Stat.
	Body io.ReadCloser
	// Storage names the storage that serves the read; Replica says it is
	// a replica of the object's storage rather than the storage itself.
	Storage string
	Replica bool
	store   blob.Store
	obj     Object
	series  string
}

// recordTimeout bounds the write of a replica's damage, made after the
// request that found it may have ended.
const recordTimeout = 10 * time.Second

// Open opens length bytes of obj from offset, as blob.Store.Read does.
//
// It reads the object's own storage first. It turns to a replica only when
// that storage cannot serve the object before the first byte: the object
// is marked damaged (Object.Damaged, and then the primary is not read at
// all), is not found, is not online (blob.ErrNotOnline), or its storage
// does not answer (fault.Unavailable, judged by the error's kind, never its
// text). A replica is read only when the tenant's database says it is in
// step for the object's scan at the catalog's manifest digest now, so a
// replica that has not yet copied the scan's latest change is never read.
// Replicas are tried in order of name, each opened once; nothing is tried
// again within one call. When no replica serves it, the error is the
// primary's, as a read with no replica answers, or ErrMarked for an object
// marked damaged. A read the primary serves asks the database nothing.
//
// The bytes of a replica are the catalog's to check as the primary's are:
// the caller passes them through CopyChecked with the catalog's length and
// SHA-256, and reports a mismatch to Mismatch.
func (s *Stores) Open(ctx context.Context, obj Object, offset, length int64) (Opened, error) {
	var body io.ReadCloser
	o, err := s.serve(ctx, obj, true, func(st blob.Store) error {
		var err error
		body, err = st.Read(ctx, obj.Key, offset, length)
		return err
	})
	if err != nil {
		return o, err
	}
	if o.Replica {
		body = checkWhole(body, obj, offset, length)
	}
	o.Body = body
	return o, nil
}

// Stat returns what is known of obj without reading it, from the storage
// Open would read it from: the primary, or a replica in step when the
// primary's object is marked damaged, not found or not online, or its
// storage does not answer. An object its primary holds but cannot read at
// once, which no replica serves, is answered from the primary, its state
// saying so, as Stat answers it with no replica.
//
// Stat records nothing of a replica found without the object: it answers
// a request for no byte (HEAD), which a client or a monitor may repeat as
// often as it likes, and a write to the tenant's database for each would
// let a request that reads nothing load it. The GET that reads the bytes,
// or a scrub, records it. A Source's Stat is the first step of a read and
// records as Open does.
func (s *Stores) Stat(ctx context.Context, obj Object) (blob.Info, Opened, error) {
	return s.stat(ctx, obj, false)
}

// stat is Stat, recording a replica found without the object when record
// is set.
func (s *Stores) stat(ctx context.Context, obj Object, record bool) (blob.Info, Opened, error) {
	var (
		info    blob.Info
		offline *blob.Info
		other   *blob.Info
	)
	o, err := s.serve(ctx, obj, record, func(st blob.Store) error {
		got, err := st.Stat(ctx, obj.Key)
		if err == nil {
			if err = checkFile(obj, got); err != nil && other == nil {
				other = &got
			}
		}
		if err == nil && got.State != blob.Online {
			if offline == nil {
				offline = &got
			}
			return fault.Wrap(fault.Unavailable, blob.ErrNotOnline, "the object is held in a tier its storage cannot read at once")
		}
		info = got
		return err
	})
	if err != nil && offline != nil && !obj.Damaged && errors.Is(err, blob.ErrNotOnline) {
		// Only the primary's answer can be first, since a replica is tried
		// only after it.
		o.Storage, o.Replica = obj.Storage, false
		o.store = s.open[obj.Storage]
		s.servedBy(obj.Storage)
		return *offline, o, nil
	}
	if err != nil && other != nil && !obj.Damaged && errors.Is(err, ErrOtherFile) {
		// The primary keeps another file than the catalog's digest names and
		// no replica in step serves the catalog's: the primary's file is
		// answered, as before digests were kept. A reception into the scan
		// puts its next file before it commits, so the primary holds a file
		// newer than the catalog's for a moment, and a read then is not data
		// loss.
		o.Storage, o.Replica = obj.Storage, false
		o.store = s.open[obj.Storage]
		s.servedBy(obj.Storage)
		return *other, o, nil
	}
	return info, o, err
}

// Readable returns ErrMarked for an object marked damaged that no replica
// in step can serve, and nil otherwise, before anything of it is read: what
// Marked answers for a reader with no replica. It asks the database only
// for an object marked damaged.
func (s *Stores) Readable(ctx context.Context, obj Object) error {
	if !obj.Damaged {
		return nil
	}
	o := Opened{obj: obj, series: obj.SeriesUID}
	names, err := s.inStep(ctx, &o)
	if err != nil || len(names) == 0 {
		return ErrMarked
	}
	return nil
}

// Mismatch handles a read of o that CopyChecked found not to be what the
// catalog records (Mismatched). Bytes of a replica are recorded as that
// replica's damage (tenant.Replication.RecordReplicaDamage), with the size
// the replica holds where it answers, so the replicator copies the object
// again from the primary; the answer is cut all the same, as for the
// primary. Bytes of the primary are left to the reader, which cuts the
// answer and logs it, as every read does. It returns the error of the
// record, for the caller to log; nil when nothing was to be recorded.
func (s *Stores) Mismatch(ctx context.Context, o Opened, err error) error {
	if !o.Replica || !Mismatched(err) || errors.Is(err, ErrMarked) {
		return nil
	}
	s.counts.addWrong(s.tenantName, o.Storage)
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	var size *int64
	if info, serr := o.store.Stat(cctx, o.obj.Key); serr == nil {
		n := info.Size
		size = &n
	}
	return s.recordDamage(cctx, o, size)
}

// serve runs try on the object's storage, and then on each replica in step
// as Open says, and answers which served it. A replica in step that answers
// not found is recorded as its damage only when record is set.
func (s *Stores) serve(ctx context.Context, obj Object, record bool, try func(blob.Store) error) (Opened, error) {
	o := Opened{obj: obj, series: obj.SeriesUID}
	first := ErrMarked
	if !obj.Damaged {
		st, err := s.Get(ctx, obj.Storage)
		if err != nil {
			err = &openError{err: err}
		} else {
			err = try(st)
		}
		if err == nil {
			o.Storage, o.store = obj.Storage, st
			s.servedBy(obj.Storage)
			return o, nil
		}
		if !turns(ctx, err) {
			return o, err
		}
		first = err
	}
	names, err := s.inStep(ctx, &o)
	if err != nil {
		// The database that would name a replica does not answer: the read
		// fails as it would with none.
		return o, first
	}
	for _, name := range names {
		st, err := s.Get(ctx, name)
		if err != nil {
			continue
		}
		if err := try(st); err != nil {
			if ctx.Err() != nil {
				return o, first
			}
			if record && (fault.KindOf(err) == fault.NotFound || errors.Is(err, ErrOtherFile)) {
				// In step and still without the object, or holding another
				// file than the catalog's digest names: the replica's
				// damage, for the replicator to copy again. Any other
				// failure (the replica not answering, the object not online)
				// says nothing of what it holds and records nothing.
				var size *int64
				var other *otherFile
				if errors.As(err, &other) {
					size = &other.size
				}
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
				_ = s.recordDamage(cctx, Opened{Storage: name, Replica: true, obj: obj, series: o.series}, size) // The read fails over or fails as it would; a record that fails is found again by the next read or scrub.
				cancel()
			}
			continue
		}
		o.Storage, o.Replica, o.store = name, true, st
		s.servedBy(name)
		s.counts.addServed(s.tenantName, name)
		if !obj.Damaged {
			// A primary that answers not found for everything (a
			// directory not mounted) would otherwise fail over unseen.
			s.counts.warnTurned(s.tenantName, obj.Storage, first)
		}
		return o, nil
	}
	return o, first
}

// openError is the failure to open an object's own storage, as opposed to
// a failure to read the object on it.
type openError struct{ err error }

// Error returns the opener's error's text.
func (e *openError) Error() string { return e.err.Error() }

// Unwrap returns the opener's error, so its kind passes through.
func (e *openError) Unwrap() error { return e.err }

// StorageFailed reports whether err, from Open or Stat, is the failure to
// open the object's own storage (its directory not there, an endpoint not
// allowed, a secret that does not open), which no replica made good: a
// failure of the storage, not of the object, which a reader reports as the
// storage's rather than as the instance missing.
func StorageFailed(err error) bool {
	var oe *openError
	return errors.As(err, &oe)
}

// turns reports whether a read that failed with err turns to a replica:
// the object is not found or not online, or its storage does not answer.
// The end of the read's own context never turns.
func turns(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	switch fault.KindOf(err) {
	case fault.NotFound, fault.Unavailable:
		return true
	}
	return errors.Is(err, blob.ErrNotOnline) || errors.Is(err, ErrOtherFile)
}

// inStep returns the replicas in step for o's scan now, by name, without
// the object's own storage, finding the scan's series from its labels when
// the reader did not know it.
func (s *Stores) inStep(ctx context.Context, o *Opened) ([]string, error) {
	if s.replicas == nil {
		return nil, nil
	}
	if o.series == "" {
		sc, err := s.replicas.CatalogScan(ctx, o.obj.Path)
		if err != nil {
			return nil, err
		}
		o.series = sc.SeriesUID
	}
	in, err := s.replicas.InStepReplicas(ctx, []string{o.series})
	if err != nil {
		return nil, err
	}
	names := slices.Sorted(slices.Values(in[o.series]))
	return slices.DeleteFunc(names, func(n string) bool { return n == o.obj.Storage }), nil
}

// recordDamage records o's object on its replica as missing (size nil) or
// holding size bytes that are not the catalog's, when the catalog says the
// replica must hold that object now.
//
// An instance's object is recorded, and a scan's metadata file only when
// the catalog keeps its digest (Object.SHA256): the replicator then puts
// the file against that digest, so a replica in step holds it, and one
// missing or other there is its damage. A metadata file with no digest is
// not recorded (a scan archived before metadata files were written has
// none anywhere, and nothing says which file a replica must hold); a
// scrub, which reads every object of the copy, says what is. Recording it
// would mark the scan damaged on the replica after each copy and turn off
// its reads for every instance of the scan.
//
// A metadata file is recorded only while the catalog still keeps the
// digest the reader read it against: a pass that rewrites the file records
// a new digest and queues the replicas, whose next copy puts the new file,
// so a reader holding the old digest would otherwise take a good replica
// for a damaged one.
//
// An instance whose key is no longer the catalog's (a correction published
// a new revision after the reader read the catalog, and the old key may be
// purged from the replica) is not recorded either: the replica answered
// for a key it need no longer hold. The catalog is asked only here, on the
// path of a replica found missing or wrong. A check that fails records
// nothing; the next read or scrub finds a real damage again.
func (s *Stores) recordDamage(ctx context.Context, o Opened, size *int64) error {
	if o.obj.SOPInstanceUID == "" {
		if o.obj.SHA256.IsZero() || o.series == "" || o.obj.Path == (label.Path{}) {
			return nil
		}
		if sc, err := s.replicas.CatalogScan(ctx, o.obj.Path); err != nil || sc.SeriesUID != o.series ||
			sc.SidecarSHA256 != o.obj.SHA256 {
			return nil
		}
		_, err := s.replicas.RecordReplicaDamage(ctx, tenant.ReplicaDamageRecord{Replica: o.Storage, SeriesUID: o.series,
			Damage: tenant.ReplicaDamage{FoundSize: size, Found: s.now()}})
		return err
	}
	k, err := layout.Parse(o.obj.Key)
	if err != nil || k.Kind != layout.Instance {
		return nil
	}
	rev := k.Rev()
	if in, err := s.replicas.HeldInstance(ctx, o.obj.Path, o.obj.SOPInstanceUID); err != nil || in.Key != o.obj.Key {
		return nil
	}
	_, err = s.replicas.RecordReplicaDamage(ctx, tenant.ReplicaDamageRecord{Replica: o.Storage, SeriesUID: o.series,
		Damage: tenant.ReplicaDamage{SOPInstanceUID: o.obj.SOPInstanceUID, Revision: rev, FoundSize: size, Found: s.now()}})
	return err
}
