package sidecar

import (
	"errors"
	"fmt"
	"io"
)

// Added yields the instances a merge adds, in increasing order of SOP
// Instance UID, each once: false when there are no more. It is called once
// per instance, as the merge reaches it, so an instance's data set is made
// only when it is written and memory follows one line, not the scan.
type Added func() (Instance, bool, error)

// List returns an Added that yields the instances given, for a caller that
// has them at hand.
func List(ins []Instance) Added {
	i := 0
	return func() (Instance, bool, error) {
		if i == len(ins) {
			return Instance{}, false, nil
		}
		i++
		return ins[i-1], true, nil
	}
}

// Merge writes to w the instances of the sidecar old (nil for none) and of
// added, in order: an instance of old whose UID is in drop or among added is
// left out, and added's take their place. It returns how many instances it
// wrote. Memory follows the longest line.
//
// Every instance added must have its frame table (ErrIncomplete
// otherwise). The sidecar written is of version VersionFrames when old is
// nil or of that version, of version 3 (keeping old's scan header) when old
// is, and of version 1 when old is, since the lines it keeps of old may have
// none; Complete is the rewrite that gives them theirs.
func Merge(w io.Writer, old io.Reader, added Added, drop func(sopUID string) bool) (int, error) {
	return merge(w, old, nil, added, drop, false)
}

// MergeScan is Merge for a scan whose header the caller knows. The sidecar
// written is of version 3 when old is nil or of version 3, with the header
// scan as it is given: the caller makes it from the catalog, which alone
// says what was committed, and old's header, which an attempt that never
// committed may have written, is not kept. An old header naming another
// study or series is ErrFormat. When old is of an older version the header is not
// written and the result is as Merge's, so no line of a sidecar that is
// not rewritten lacks what version 3 requires: the instances added lose the
// fields only version 3 has, which Upgrade gives back from the catalog.
func MergeScan(w io.Writer, old io.Reader, scan ScanHeader, added Added, drop func(sopUID string) bool) (int, error) {
	return merge(w, old, &scan, added, drop, false)
}

// Complete is Merge for the rewrite that gives a sidecar of version 1 the
// frame tables its lines lack: added holds a line, with its table, for
// every line of old that has pixel data and no table, and the sidecar
// written is of version VersionFrames. A line it would keep without a table
// is ErrIncomplete, and what was written to w is then not a sidecar.
func Complete(w io.Writer, old io.Reader, added Added) (int, error) {
	return merge(w, old, nil, added, nil, true)
}

// Upgrade rewrites the sidecar old, of any version, as version 3 with the
// scan header scan: each line of old, in order, goes through fill, which
// returns it with the fields version 3 requires; a fill that fails fails the
// rewrite, and what was written to w is then not a sidecar. A line of old
// that already has them is given to fill as it is. It returns how many
// lines it wrote.
func Upgrade(w io.Writer, old io.Reader, scan ScanHeader, fill func(Instance) (Instance, error)) (int, error) {
	out := NewWriterV3(w, scan)
	rd := NewReader(old)
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		if in, err = fill(in); err != nil {
			return 0, err
		}
		if err := out.Add(in); err != nil {
			return 0, err
		}
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return out.count, nil
}

// merge is Merge, writing VersionFrames whatever old's when complete, and
// version 3 when scan is given and old allows it.
func merge(w io.Writer, old io.Reader, scan *ScanHeader, added Added, drop func(sopUID string) bool, complete bool) (int, error) {
	out := NewWriter(w)
	var rd *Reader
	if old != nil {
		rd = NewReader(old)
	}
	next := func() (Instance, []byte, bool, error) {
		if rd == nil {
			return Instance{}, nil, false, nil
		}
		in, line, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return Instance{}, nil, false, nil
		}
		return in, line, err == nil, err
	}
	cur, line, ok, err := next()
	if err != nil {
		return 0, err
	}
	switch {
	case rd == nil:
		if scan != nil {
			out.version, out.scan = Version, *scan
		}
	case rd.version == 1 && !complete:
		out.version = 1
	case rd.version == Version:
		h := *rd.scan
		if scan != nil {
			if scan.StudyUID != h.StudyUID || scan.SeriesUID != h.SeriesUID {
				return 0, fmt.Errorf("%w: the sidecar names another study or series than the scan", ErrFormat)
			}
			h = *scan
		}
		out.version, out.scan = Version, h
	}
	if out.version == Version {
		if err := out.scan.Validate(); err != nil {
			return 0, err
		}
	}
	add, more, err := added()
	if err != nil {
		return 0, err
	}
	for ok || more {
		switch {
		case ok && (!more || cur.SOPInstanceUID < add.SOPInstanceUID):
			if drop == nil || !drop(cur.SOPInstanceUID) {
				if out.count > 0 && cur.SOPInstanceUID <= out.last {
					return 0, ErrOrder
				}
				if out.version >= VersionFrames && cur.NeedsFrames() {
					return 0, ErrIncomplete
				}
				if err := out.addLine(cur.SOPInstanceUID, line); err != nil {
					return 0, err
				}
			}
			if cur, line, ok, err = next(); err != nil {
				return 0, err
			}
		default:
			if ok && cur.SOPInstanceUID == add.SOPInstanceUID {
				if cur, line, ok, err = next(); err != nil {
					return 0, err
				}
			}
			if out.version != Version {
				add = add.withoutV3()
			}
			if err := out.Add(add); err != nil {
				return 0, err
			}
			if add, more, err = added(); err != nil {
				return 0, err
			}
		}
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return out.count, nil
}
