package jobmeet

import (
	"context"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// Item is one instance of a unit's stream.
type Item struct {
	// Instance is the instance as the stream gave it; for a deleted one
	// only its UID, size and reception are known.
	Instance tenant.ScanInstance
	// Deleted says a correction deleted the instance since the job was
	// made: it is to be refused, never read.
	Deleted bool
}

// Page is how many instances of each kind one read of the stream takes.
const Page = 256

// Stream walks a unit's stream after its cursor in the catalog's order:
// the instances the catalog holds and, merged into their places, those a
// correction deleted since the job counted them, so the unit accounts for
// every instance the job counted, in order, and its cursor never passes
// one it has not answered.
type Stream struct {
	a     Archive
	fence tenant.WorkFence
	// live and gone are the pages read, each with the last UID read and
	// whether its source is exhausted.
	live, gone source
	// last is the last UID returned, so no instance is returned twice.
	last string
}

// source is one ordered source of a Stream: a page, where it is in it, the
// last UID read and whether the source has no more.
type source struct {
	page  []tenant.ScanInstance
	i     int
	after string
	done  bool
}

// NewStream returns the stream of the unit holding fence, after the cursor
// after, with the instances a correction deleted merged in
// (Archive.DeletedJobInstances).
func NewStream(a Archive, fence tenant.WorkFence, after string) *Stream {
	return &Stream{a: a, fence: fence, live: source{after: after}, gone: source{after: after}, last: after}
}

// Next returns the next instance of the stream, false at its end. ctx
// bounds the database reads it makes.
func (s *Stream) Next(ctx context.Context) (Item, bool, error) {
	for {
		if err := s.fill(&s.live, func() ([]tenant.ScanInstance, error) {
			return s.a.JobScanInstances(ctx, s.fence, s.live.after, Page)
		}); err != nil {
			return Item{}, false, err
		}
		if err := s.fill(&s.gone, func() ([]tenant.ScanInstance, error) {
			return s.a.DeletedJobInstances(ctx, s.fence, s.gone.after, Page)
		}); err != nil {
			return Item{}, false, err
		}
		l, haveLive := s.live.peek()
		g, haveGone := s.gone.peek()
		switch {
		case !haveLive && !haveGone:
			return Item{}, false, nil
		case haveGone && g.SOPInstanceUID <= s.last:
			// Already answered as held, and deleted since the live page was
			// read: Meet refused it. Never answered twice.
			s.gone.i++
		case haveGone && (!haveLive || g.SOPInstanceUID < l.SOPInstanceUID):
			s.gone.i++
			s.last = g.SOPInstanceUID
			return Item{Instance: g, Deleted: true}, true, nil
		case haveGone && g.SOPInstanceUID == l.SOPInstanceUID:
			// Held again (a delete undone, and the catalog shows it): the
			// held row is the one.
			s.gone.i++
		default:
			s.live.i++
			s.last = l.SOPInstanceUID
			return Item{Instance: l}, true, nil
		}
	}
}

// fill reads the next page of src when it is used up and not done.
func (s *Stream) fill(src *source, read func() ([]tenant.ScanInstance, error)) error {
	if src.i < len(src.page) || src.done {
		return nil
	}
	page, err := read()
	if err != nil {
		return err
	}
	src.page, src.i, src.done = page, 0, len(page) < Page
	if len(page) > 0 {
		src.after = page[len(page)-1].SOPInstanceUID
	}
	return nil
}

// peek returns the next instance of src without taking it.
func (src *source) peek() (tenant.ScanInstance, bool) {
	if src.i < len(src.page) {
		return src.page[src.i], true
	}
	return tenant.ScanInstance{}, false
}
