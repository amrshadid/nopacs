package dir

import (
	"container/list"
	"errors"
	"io/fs"
	"strings"
	"sync"

	"github.com/amrshadid/nopacs/internal/fault"
)

// durableRecordSize bounds the record of directories known durable. A
// directory a Put finds in it costs that Put no sync; one that fell out of
// it costs the next Put into it one sync of its parent, so the bound trades
// a little memory (at most this many paths, under 1 KB each and usually a
// few dozen bytes) for syncs, and is never a question of correctness.
const durableRecordSize = 32768

// durableDirs is the record of directories this process knows to be
// durable: each has been seen to exist and its parent has been synced since,
// so its entry survives a crash of the machine. It is kept in memory, least
// recently used first out, and starts empty with each process: after a
// restart the first Put into each directory pays the syncs of its path once.
//
// Nothing in this process removes a directory, so an entry never goes stale
// through this process. One that something else removed is found at the
// rename into it, and forgotten there.
type durableDirs struct {
	mu       sync.Mutex
	order    *list.List // of string, most recently used at the front
	index    map[string]*list.Element
	inflight map[string]chan struct{}
	// waiting, when set, is called as a Put begins to wait for another's
	// making of a directory durable; a test uses it to see that it waits.
	waiting func(dir string)
}

func (d *durableDirs) init() {
	d.order = list.New()
	d.index = map[string]*list.Element{}
	d.inflight = map[string]chan struct{}{}
}

// forgetPath removes dir and every directory above it from the record.
func (d *durableDirs) forgetPath(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for dir != "." {
		if el := d.index[dir]; el != nil {
			d.order.Remove(el)
			delete(d.index, dir)
		}
		dir = parent(dir)
	}
}

// ensureDirs makes every directory of dir exist and be durable, from the
// top down, so a parent's entry is durable before its child's is made so:
// when it returns nil, a crash cannot lose any directory on the path, and
// an object renamed into dir afterwards needs only dir itself synced.
func (s *Store) ensureDirs(dir string) error {
	if dir == "." {
		return nil
	}
	segs := strings.Split(dir, "/")
	for i := range segs {
		if err := s.ensureDir(strings.Join(segs[:i+1], "/")); err != nil {
			return err
		}
	}
	return nil
}

// ensureDir makes the directory p exist and be durable, unless the record
// says it already is. A Put that finds another Put of this process making
// the same directory durable waits for it, rather than going on as though
// the directory were durable because it exists: existing is not enough,
// since the other Put's sync of the parent may not have happened yet. If
// the other fails, this one tries itself.
func (s *Store) ensureDir(p string) error {
	d := &s.durable
	for {
		d.mu.Lock()
		if el := d.index[p]; el != nil {
			d.order.MoveToFront(el)
			d.mu.Unlock()
			return nil
		}
		if ch := d.inflight[p]; ch != nil {
			waiting := d.waiting
			d.mu.Unlock()
			if waiting != nil {
				waiting(p)
			}
			<-ch
			continue
		}
		ch := make(chan struct{})
		d.inflight[p] = ch
		d.mu.Unlock()

		err := s.makeDurable(p)

		d.mu.Lock()
		delete(d.inflight, p)
		if err == nil {
			d.index[p] = d.order.PushFront(p)
			for d.order.Len() > durableRecordSize {
				oldest := d.order.Back()
				d.order.Remove(oldest)
				delete(d.index, oldest.Value.(string))
			}
		}
		d.mu.Unlock()
		close(ch)
		return err
	}
}

// makeDurable makes p exist as a directory and syncs its parent, whether
// this call made p or found it: a directory found may have been made by a
// Put whose sync of the parent never happened (a crash, or another process
// before a restart). A name that is a symbolic link fails it; one that is a
// file is a Conflict.
func (s *Store) makeDurable(p string) error {
	for attempt := 0; ; attempt++ {
		info, err := s.root.Lstat(native(p))
		if err == nil {
			if err := checkDirInfo(info); err != nil {
				return err
			}
			break
		}
		if !notFound(err) {
			return s.wrap(fault.Unavailable, err, "the storage could not examine a directory")
		}
		err = s.mkdir(s.root, native(p))
		if err == nil {
			s.listing.invalidate(parent(p))
			break
		}
		// Made meanwhile by something else, or its parent removed by
		// something else: examine it again. Anything else, or a name
		// that keeps changing, is a failure.
		if (!errors.Is(err, fs.ErrExist) && !goneMidway(err)) || attempt >= 8 {
			return s.wrap(fault.Unavailable, err, "the storage could not make a directory")
		}
		if goneMidway(err) {
			s.durable.forgetPath(parent(p))
			if err := s.ensureDirs(parent(p)); err != nil {
				return err
			}
		}
	}
	if err := s.syncDir(s.root, native(parent(p))); err != nil {
		return s.wrap(fault.Unavailable, err, "the storage could not sync a directory")
	}
	return nil
}
