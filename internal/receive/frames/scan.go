package frames

import (
	"context"
	"encoding/hex"
	"errors"
	"io"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/readat"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// found is the table read for one line, with what names the file it was
// read from, so it is given only to a line that still names that file.
type found struct {
	key    string
	size   int64
	sha256 string
	frames [][]sidecar.Range
	sums   []string
}

// matches reports whether the table was read from the file the line names.
func (f found) matches(in sidecar.Instance) bool {
	return f.key == in.Key && f.size == in.Size && f.sha256 == in.SHA256
}

// scan checks one scan and, when its sidecar is of version 1, gives every
// line that lacks a table its table, a batch at a time, the last batch
// making the sidecar complete.
func (r *runner) scan(ctx context.Context, storage string, path label.Path) error {
	store, err := r.storage(ctx, storage)
	if err != nil {
		return err
	}
	key, err := metadataKey(path)
	if err != nil {
		return err
	}
	at := r.cfg.Clock.Now()
	head, ok, err := readHead(ctx, store, key, sidecar.HeadLength)
	if err != nil {
		return err
	}
	r.rep.ScansChecked++
	if err := r.pause(ctx, r.cfg.Clock.Now().Sub(at)); err != nil {
		return err
	}
	if !ok {
		// The metadata service reports a scan without its metadata as data
		// loss; there is nothing here to give tables to.
		r.cfg.Logger.Warn("frames: a scan has no metadata on its storage", "subject", path.Subject, "session", path.Session, "scan", path.Scan)
		return nil
	}
	switch sidecar.VersionOf(head) {
	case sidecar.VersionFrames, sidecar.Version:
		return nil
	case 1:
	default:
		r.cfg.Logger.Warn("frames: a scan's metadata is not in a form this version reads; it is left as it is",
			"subject", path.Subject, "session", path.Session, "scan", path.Scan)
		return nil
	}
	total := int64(0)
	for {
		tables, last, err := r.collect(ctx, store, key)
		if err != nil {
			return r.leave(path, err)
		}
		n, err := r.rewrite(ctx, store, storage, path, key, tables, last)
		if err != nil {
			return r.leave(path, err)
		}
		total += n
		if last {
			break
		}
		if n == 0 {
			// A batch that gave no line a table would be read again and
			// again; the next pass tries the scan anew.
			return nil
		}
	}
	r.rep.ScansDone++
	r.rep.InstancesDone += total
	r.event(ctx, &tenant.FramesAdded{SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan,
		Instances: total, ScansDone: r.rep.ScansDone, InstancesDone: r.rep.InstancesDone}, key)
	return nil
}

// leave returns a scan's failure for the pass to stop on, or nil for one
// that leaves only this scan for the next pass: a sidecar not in its form,
// or one that changed under the pass so a line it was to complete has no
// table.
func (r *runner) leave(path label.Path, err error) error {
	if errors.Is(err, sidecar.ErrFormat) || errors.Is(err, sidecar.ErrOrder) || errors.Is(err, sidecar.ErrIncomplete) {
		r.cfg.Logger.Warn("frames: a scan's metadata is left as it is for the next pass", "subject", path.Subject,
			"session", path.Session, "scan", path.Scan, "reason", err.Error())
		return nil
	}
	return err
}

// collect reads the scan's sidecar and, for its lines that lack a table,
// up to a batch of them, reads each one's table from its file. last says
// the batch holds every line that lacks one.
func (r *runner) collect(ctx context.Context, store blob.Store, key string) (map[string]found, bool, error) {
	rc, err := store.Read(ctx, key, 0, -1)
	if err != nil {
		return nil, false, storageError(err, "the scan's metadata cannot be read")
	}
	defer func() { _ = rc.Close() }()
	rd := sidecar.NewReader(rc)
	tables := map[string]found{}
	ranges := 0
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return tables, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		if !in.NeedsFrames() {
			continue
		}
		limit := batchInstances
		if r.cfg.seams.batchInstances > 0 {
			limit = r.cfg.seams.batchInstances
		}
		if len(tables) >= limit || ranges >= batchRanges {
			return tables, false, nil
		}
		at := r.cfg.Clock.Now()
		t, sums, err := r.table(ctx, store, in)
		if err != nil {
			return nil, false, err
		}
		tables[in.SOPInstanceUID] = found{key: in.Key, size: in.Size, sha256: in.SHA256, frames: t, sums: sums}
		for _, f := range t {
			ranges += len(f)
		}
		if err := r.pause(ctx, r.cfg.Clock.Now().Sub(at)); err != nil {
			return nil, false, err
		}
	}
}

// table reads the frame table of the instance a line names from its file
// on the storage, by bounded ranged reads: its header, then the items of
// its pixel data; and then, for a table that is not empty, each frame's
// checksum, from one streamed read of the file that is checked against the
// line's checksum. A file that is not what the line says (missing,
// shorter, not DICOM, a header past the bound, no pixel data, other bytes
// than it was archived with) gets an empty table: that is a fact of the
// file, which the metadata and data services report on their own, and a
// reader of its frames is told they cannot be read by range, so a frame of
// a changed file is never answered unchecked.
func (r *runner) table(ctx context.Context, store blob.Store, in sidecar.Instance) ([][]sidecar.Range, []string, error) {
	limit := int64(header.MaxHeader)
	if r.cfg.HeaderLimit != nil {
		limit = r.cfg.HeaderLimit(ctx)
	}
	// A line names its instance's object by key and no offset in it: the
	// object is read as the instance's file, which it is while each
	// instance is one object. A layout that packs instances records each
	// one's place in its line before this pass may read it.
	ra := readat.New(ctx, store, in.Key, in.Size)
	h, err := header.ReadWithin(ra, in.Size, limit)
	if err == nil {
		var t [][]sidecar.Range
		if t, err = Table(h, ra); err == nil {
			if len(t) == 0 {
				return [][]sidecar.Range{}, nil, nil
			}
			var sums []string
			if sums, err = r.checksums(ctx, store, in, t); err == nil {
				if sums == nil {
					return [][]sidecar.Range{}, nil, nil
				}
				return t, sums, nil
			}
		}
	}
	var notDICOM *header.NotDICOMError
	var tooLarge *header.TooLargeError
	switch {
	case ctx.Err() != nil:
		return nil, nil, ctx.Err()
	case errors.As(err, &notDICOM), errors.As(err, &tooLarge), fault.KindOf(err) == fault.NotFound,
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return [][]sidecar.Range{}, nil, nil
	}
	return nil, nil, storageError(err, "an instance's file cannot be read")
}

// checksums reads the instance's file once, streamed, and returns each
// frame's checksum, or nil when the file is not the one the line names:
// not its size, or not its checksum.
func (r *runner) checksums(ctx context.Context, store blob.Store, in sidecar.Instance, t [][]sidecar.Range) ([]string, error) {
	rc, err := store.Read(ctx, in.Key, 0, -1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }() // Only read from.
	counted := &countReader{r: io.LimitReader(rc, in.Size+1)}
	sums, whole, err := Checksums(counted, t)
	if err != nil {
		return nil, err
	}
	if counted.n != in.Size || hex.EncodeToString(whole[:]) != in.SHA256 {
		return nil, nil
	}
	return sums, nil
}

// countReader counts the bytes read through it.
type countReader struct {
	r io.Reader
	n int64
}

// Read reads and counts.
func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// rewrite gives the lines of the scan's sidecar the tables found, under the
// scan's lock: through sidecar.Complete when the tables are the last batch,
// which makes the sidecar complete, and through sidecar.Merge otherwise,
// which keeps it at version 1 with this batch's lines done. It returns how
// many lines took a table. The sidecar is read as it is under the lock, so
// a line a reception replaced meanwhile, which has its own table, is kept
// as the reception wrote it, and a table is given only to a line that
// still names the file it was read from.
func (r *runner) rewrite(ctx context.Context, store blob.Store, storage string, path label.Path, key string,
	tables map[string]found, last bool) (int64, error) {
	unlock := r.cfg.Lock(storage, path)
	defer unlock()
	head, ok, err := readHead(ctx, store, key, sidecar.HeadLength)
	if err != nil {
		return 0, err
	}
	if !ok || sidecar.VersionOf(head) != 1 {
		// Completed or removed meanwhile: nothing is left to give.
		return 0, nil
	}
	old, err := store.Read(ctx, key, 0, -1)
	if err != nil {
		return 0, storageError(err, "the scan's metadata cannot be read")
	}
	defer func() { _ = old.Close() }()
	lines, err := store.Read(ctx, key, 0, -1)
	if err != nil {
		return 0, storageError(err, "the scan's metadata cannot be read")
	}
	defer func() { _ = lines.Close() }()
	rd := sidecar.NewReader(lines)
	given := int64(0)
	added := func() (sidecar.Instance, bool, error) {
		for {
			in, _, err := rd.Next()
			if errors.Is(err, io.EOF) {
				return sidecar.Instance{}, false, nil
			}
			if err != nil {
				return sidecar.Instance{}, false, err
			}
			f, ok := tables[in.SOPInstanceUID]
			if in.NeedsFrames() && ok && f.matches(in) {
				in.Frames, in.FrameSHA256 = f.frames, f.sums
				given++
				return in, true, nil
			}
		}
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		var err error
		if last {
			_, err = sidecar.Complete(pw, old, added)
		} else {
			_, err = sidecar.Merge(pw, old, added, nil)
		}
		_ = pw.CloseWithError(err)
		done <- err
	}()
	_, perr := store.Put(ctx, key, pr)
	_ = pr.CloseWithError(errors.New("the storage stopped reading"))
	if merr := <-done; merr != nil && !errors.Is(merr, io.ErrClosedPipe) {
		return 0, merr
	}
	if perr != nil {
		return 0, storageError(perr, "the scan's metadata cannot be written")
	}
	return given, nil
}
