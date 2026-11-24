package dir

import (
	"container/heap"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// readDirChunk is how many directory entries are read at a time.
const readDirChunk = 256

// List returns one page of keys; see blob.Store.
//
// A file system returns a directory's names in no useful order, so a page
// needs the directory's names sorted. A directory is read whole and sorted
// once, and its sorted names kept in a small cache (see dirCache), so that
// paging through a directory of a hundred thousand files costs about one
// read of it, not one per page. A directory with more names than the cache
// may hold is read in chunks instead, keeping only the smallest names after
// the point reached in a bounded heap, once per page: memory stays bounded
// whatever its size, at the price of the reads.
//
// The token is the last key or prefix of the page before. It needs no state
// kept between calls, and it continues correctly whatever was put or deleted
// meanwhile, since the order is that of the keys themselves.
func (s *Store) List(ctx context.Context, opts blob.ListOptions) (blob.Page, error) {
	if err := s.begin(ctx); err != nil {
		return blob.Page{}, err
	}
	if err := blob.CheckListOptions(opts); err != nil {
		return blob.Page{}, err
	}
	if err := checkToken(opts); err != nil {
		return blob.Page{}, err
	}
	// The prefix is a directory, given by its whole segments, and a filter
	// on the names in it, given by the partial segment after the last "/".
	base, filter := ".", opts.Prefix
	if i := strings.LastIndexByte(opts.Prefix, '/'); i >= 0 {
		base, filter = opts.Prefix[:i], opts.Prefix[i+1:]
	}
	if err := s.checkDirs(base); err != nil {
		if notFound(err) {
			return blob.Page{}, nil
		}
		return blob.Page{}, s.fail(err, "the storage could not examine the listing's directory")
	}
	l := &lister{
		s:     s,
		ctx:   ctx,
		after: opts.Token,
		want:  blob.ListLimit(opts) + 1,
		delim: opts.Delimiter != "",
	}
	keyPrefix := ""
	if base != "." {
		keyPrefix = base + "/"
	}
	if err := l.dir(base, keyPrefix, filter); err != nil {
		return blob.Page{}, err
	}
	var page blob.Page
	limit := l.want - 1
	if len(l.out) > limit {
		l.out = l.out[:limit]
		page.Next = l.out[limit-1].name
	}
	for _, e := range l.out {
		if e.isDir {
			page.Prefixes = append(page.Prefixes, e.name)
		} else {
			page.Keys = append(page.Keys, e.name)
		}
	}
	return page, nil
}

// checkToken refuses a token this listing could not have returned: one
// that is not a key or, with a delimiter, a key followed by "/", or that
// does not begin with the prefix.
func checkToken(opts blob.ListOptions) error {
	t := opts.Token
	if t == "" {
		return nil
	}
	bad := !strings.HasPrefix(t, opts.Prefix)
	if !bad {
		k := t
		if opts.Delimiter != "" {
			k = strings.TrimSuffix(t, "/")
		}
		bad = blob.CheckKey(k) != nil
	}
	if bad {
		return fault.New(fault.Invalid, "storage listing: the page token is not one this listing returned")
	}
	return nil
}

// entry is a key, or with a delimiter a prefix, by its whole text: a
// directory's entry ends with "/", which is also where it sorts among the
// keys, since every key under it begins with that text.
type entry struct {
	name  string
	isDir bool
}

// lister fills one page.
type lister struct {
	s     *Store
	ctx   context.Context
	after string // the page token: every entry returned sorts after it
	want  int    // the page size plus one, to know whether another page follows
	delim bool
	out   []entry
}

// dir adds to the page, in order, the entries under the directory dirPath,
// whose keys begin with keyPrefix, keeping only names that begin with
// filter.
func (l *lister) dir(dirPath, keyPrefix, filter string) error {
	cursor := ""
	for len(l.out) < l.want {
		batch, more, err := l.smallest(dirPath, keyPrefix, filter, cursor, l.want-len(l.out))
		if err != nil {
			return err
		}
		for _, e := range batch {
			cursor = e.name
			switch {
			case !e.isDir:
				l.out = append(l.out, e)
			case l.delim:
				has, err := l.s.hasObject(l.ctx, path.Join(dirPath, strings.TrimPrefix(strings.TrimSuffix(e.name, "/"), keyPrefix)))
				if err != nil {
					return err
				}
				if has {
					l.out = append(l.out, e)
				}
			default:
				name := strings.TrimSuffix(strings.TrimPrefix(e.name, keyPrefix), "/")
				if err := l.dir(path.Join(dirPath, name), e.name, ""); err != nil {
					return err
				}
			}
			if len(l.out) >= l.want {
				return nil
			}
		}
		if !more {
			return nil
		}
	}
	return nil
}

// smallest returns, in order, up to n of the entries of the directory
// dirPath that sort after cursor and that the page still needs, and whether
// there may be more.
//
// An entry is needed when something it stands for sorts after the token: a
// file whose key does, a prefix that does, or, without a delimiter, a
// directory whose text the token begins with, since keys inside it may
// still follow the token.
func (l *lister) smallest(dirPath, keyPrefix, filter, cursor string, n int) ([]entry, bool, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, false, fault.Wrap(fault.Unavailable, err, "the listing was canceled")
	}
	f, info, err := l.s.openChecked(native(dirPath), true)
	if err != nil {
		if gone(err) {
			return nil, false, nil // Not a directory any more: nothing to list in it.
		}
		return nil, false, l.s.fail(err, "the storage could not open a directory")
	}
	defer func() { _ = f.Close() }() // Only read.
	names, ok := l.s.listing.lookup(dirPath, info.ModTime())
	if !ok {
		held := l.s.listing.begin(dirPath)
		names, ok, err = l.s.readSorted(f)
		if err != nil {
			l.s.listing.abandon(dirPath, held)
			return nil, false, l.s.fail(err, "the storage could not read a directory")
		}
		if !ok {
			l.s.listing.abandon(dirPath, held)
			return l.scanSmallest(dirPath, keyPrefix, filter, cursor, n)
		}
		l.s.listing.finish(dirPath, held, names, info.ModTime())
	}
	out, more := l.pick(names, keyPrefix, filter, cursor, n)
	return out, more, nil
}

// gone reports whether err from openChecked means the name no longer holds
// a directory to list: removed, replaced by something else, or a link.
func gone(err error) bool {
	return notFound(err) || errors.Is(err, errWrongType) || errors.Is(err, errLinked)
}

// pick returns, from a directory's sorted names, up to n entries that
// sort after cursor, begin with filter, and are needed after the token,
// and whether more follow. It starts with a binary search, so a page costs
// its own size, not the directory's.
func (l *lister) pick(names []dirName, keyPrefix, filter, cursor string, n int) ([]entry, bool) {
	lo := strings.TrimPrefix(cursor, keyPrefix)
	start := sort.Search(len(names), func(i int) bool {
		return names[i].sort > lo && names[i].sort >= filter
	})
	// Skip what lies at or before the token: everything up to it, except,
	// without a delimiter, the directory the token is inside, which may
	// still hold keys after it.
	if a, ok := strings.CutPrefix(l.after, keyPrefix); ok && l.after != "" {
		bound := func(i int) bool { return names[i].sort > a }
		if j := strings.IndexByte(a, '/'); j >= 0 && !l.delim {
			dirOf := a[:j+1]
			bound = func(i int) bool { return names[i].sort >= dirOf }
		}
		start = max(start, sort.Search(len(names), bound))
	}
	var out []entry
	for i := start; i < len(names); i++ {
		d := names[i]
		if !strings.HasPrefix(d.sort, filter) {
			break // Sorted: nothing further begins with filter.
		}
		e := entry{name: keyPrefix + d.sort, isDir: d.isDir}
		needed := e.name > l.after || (e.isDir && !l.delim && strings.HasPrefix(l.after, e.name))
		if !needed {
			continue
		}
		if len(out) == n {
			return out, true
		}
		out = append(out, e)
	}
	return out, false
}

// readSorted reads the directory f whole and returns its names that could
// be part of a key, sorted as keys sort, with whether it held no more than
// maxCachedNames of them. Past that it stops and returns false, and the
// caller lists the directory in chunks instead.
func (s *Store) readSorted(f *os.File) ([]dirName, bool, error) {
	var names []dirName
	for {
		entries, err := s.readDir(f, readDirChunk)
		for _, d := range entries {
			if blob.CheckKey(d.Name()) != nil {
				continue
			}
			switch {
			case d.Type().IsRegular():
				names = append(names, dirName{sort: d.Name()})
			case d.IsDir():
				names = append(names, dirName{sort: d.Name() + "/", isDir: true})
			}
			if len(names) > maxCachedNames {
				return nil, false, nil
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, err
		}
	}
	slices.SortFunc(names, func(a, b dirName) int { return strings.Compare(a.sort, b.sort) })
	return names, true, nil
}

// scanSmallest is smallest for a directory too large to cache: it reads
// the directory through, in chunks, keeping in a bounded heap only the n
// smallest needed entries after cursor.
func (l *lister) scanSmallest(dirPath, keyPrefix, filter, cursor string, n int) ([]entry, bool, error) {
	h := &entryHeap{}
	more := false
	err := l.s.eachEntry(dirPath, func(d fs.DirEntry) {
		name := d.Name()
		if !strings.HasPrefix(name, filter) {
			return
		}
		e := entry{name: keyPrefix + name}
		switch {
		case d.Type().IsRegular():
		case d.IsDir():
			e.isDir = true
			e.name += "/"
		default:
			return // A symbolic link or a device is not an object.
		}
		if e.name <= cursor {
			return
		}
		needed := e.name > l.after || (e.isDir && !l.delim && strings.HasPrefix(l.after, e.name))
		if !needed {
			return
		}
		if h.Len() < n {
			heap.Push(h, e)
			return
		}
		more = true
		if e.name < (*h)[0].name {
			(*h)[0] = e
			heap.Fix(h, 0)
		}
	})
	if err != nil {
		if gone(err) {
			return nil, false, nil
		}
		return nil, false, l.s.fail(err, "the storage could not read a directory")
	}
	out := []entry(*h)
	slices.SortFunc(out, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	return out, more, nil
}

// eachEntry calls fn for each entry of the directory dirPath that could be
// part of a key, reading the directory a chunk at a time. It skips a name
// that is not valid as a key's segment, which takes in the driver's own
// temporary directory and anything another program left that no key could
// name.
func (s *Store) eachEntry(dirPath string, fn func(fs.DirEntry)) error {
	f, _, err := s.openChecked(native(dirPath), true)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // Only read.
	for {
		entries, err := s.readDir(f, readDirChunk)
		for _, d := range entries {
			if blob.CheckKey(d.Name()) == nil {
				fn(d)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// hasObject reports whether any object lies under the directory dirPath, so
// that a directory left empty, by Deletes or by Puts that failed, is not
// listed as a prefix: on a bucket, a prefix exists only while a key under
// it does. It stops at the first object it finds.
func (s *Store) hasObject(ctx context.Context, dirPath string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fault.Wrap(fault.Unavailable, err, "the listing was canceled")
	}
	f, _, err := s.openChecked(native(dirPath), true)
	if err != nil {
		if gone(err) {
			return false, nil
		}
		return false, s.fail(err, "the storage could not open a directory")
	}
	defer func() { _ = f.Close() }() // Only read.
	for {
		entries, err := s.readDir(f, readDirChunk)
		for _, d := range entries {
			if blob.CheckKey(d.Name()) != nil {
				continue
			}
			if d.Type().IsRegular() {
				return true, nil
			}
			if d.IsDir() {
				has, err := s.hasObject(ctx, path.Join(dirPath, d.Name()))
				if err != nil || has {
					return has, err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, s.fail(err, "the storage could not read a directory")
		}
	}
}

// entryHeap is a max-heap of entries by name, so its top is the largest of
// the smallest n seen so far, the one to drop when a smaller comes.
type entryHeap []entry

func (h entryHeap) Len() int           { return len(h) }
func (h entryHeap) Less(i, j int) bool { return h[i].name > h[j].name }
func (h entryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *entryHeap) Push(x any)        { *h = append(*h, x.(entry)) }
func (h *entryHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

// The bounds of the listing cache. A directory with more names than
// maxCachedNames is never cached and is listed in chunks, a read per page;
// the archive's directories hold one scan's files, thousands at most, and a
// rebuild of the catalog pages through them one after another, so a few are
// enough. The names held cost about 50 bytes each, so the bound is about
// 20 MB.
const (
	maxCachedDirs  = 8
	maxCachedNames = 400000
)

// dirName is one name of a directory as a key sorts it: a directory's name
// ends with "/".
type dirName struct {
	sort  string
	isDir bool
}

// cachedDir is the sorted names of one directory and the modification
// time it had when they were read. A placeholder, not yet ready, marks a
// reading in progress, so that a change made meanwhile can drop it.
type cachedDir struct {
	names   []dirName
	modTime time.Time
	ready   bool
}

// dirCache holds the sorted names of the directories most recently listed.
//
// A cached directory is used only while nothing says it changed. This
// process drops a directory's entry on every change it makes in it (a Put's
// rename, a Delete, a new subdirectory), and a reading that such a change
// overlapped is not kept, since the change drops its placeholder. A change
// by another process is seen by the directory's modification time, compared
// at each page; it is missed only when it falls within the same tick of the
// file system's clock as the reading it follows, which a second process,
// unsupported anyway, could cause. A page continues after the token by the
// order of the keys themselves, so even then a listing never shows a key
// twice or skips one that was there throughout; it may miss one added by
// the other process.
type dirCache struct {
	mu      sync.Mutex
	entries map[string]*cachedDir
	recent  []string // ready entries, least recently used first
	held    int      // names held by ready entries
}

func (c *dirCache) init() { c.entries = map[string]*cachedDir{} }

// lookup returns the cached names of dir when they are ready and were read
// at modTime.
func (c *dirCache) lookup(dir string, modTime time.Time) ([]dirName, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[dir]
	if e == nil || !e.ready || !e.modTime.Equal(modTime) {
		return nil, false
	}
	c.touch(dir)
	return e.names, true
}

// begin puts a placeholder for dir, replacing what was cached, and returns
// it for finish or abandon.
func (c *dirCache) begin(dir string) *cachedDir {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop(dir)
	e := &cachedDir{}
	c.entries[dir] = e
	return e
}

// finish keeps names as dir's, unless a change dropped the placeholder
// meanwhile, and evicts the least recently used directories past the
// bounds.
func (c *dirCache) finish(dir string, e *cachedDir, names []dirName, modTime time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[dir] != e {
		return
	}
	e.names, e.modTime, e.ready = names, modTime, true
	c.held += len(names)
	c.recent = append(c.recent, dir)
	for len(c.recent) > maxCachedDirs || c.held > maxCachedNames {
		c.drop(c.recent[0])
	}
}

// abandon removes dir's placeholder e, if it is still there.
func (c *dirCache) abandon(dir string, e *cachedDir) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[dir] == e {
		delete(c.entries, dir)
	}
}

// invalidate drops what is cached or being read for dir, because this
// process changed it.
func (c *dirCache) invalidate(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop(dir)
}

// drop removes dir's entry. The caller holds mu.
func (c *dirCache) drop(dir string) {
	e := c.entries[dir]
	if e == nil {
		return
	}
	delete(c.entries, dir)
	if e.ready {
		c.held -= len(e.names)
		c.recent = slices.DeleteFunc(c.recent, func(d string) bool { return d == dir })
	}
}

// touch moves dir to the most recently used end. The caller holds mu.
func (c *dirCache) touch(dir string) {
	if i := slices.Index(c.recent, dir); i >= 0 {
		c.recent = append(append(c.recent[:i:i], c.recent[i+1:]...), dir)
	}
}

// cleanTemp removes every file in the temporary directory: none of them is
// an object, and every one is a Put that a crash, a kill or a failed
// removal left behind. It reads that one directory a chunk at a time, and
// reads it again while the reading before removed something, since a
// system may skip names in a directory read while names are removed from
// it. It never walks the tree, so Open costs the same for ten objects as
// for a hundred million.
func (s *Store) cleanTemp() error {
	for {
		removed, err := s.cleanTempOnce()
		if err != nil {
			return s.wrap(fault.Unavailable, err, "the storage could not remove what a crash left in its temporary directory")
		}
		if !removed {
			return nil
		}
		if err := s.syncDir(s.root, tempDir); err != nil {
			return s.wrap(fault.Unavailable, err, "the storage could not sync its temporary directory")
		}
	}
}

func (s *Store) cleanTempOnce() (bool, error) {
	f, _, err := s.openChecked(tempDir, true)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }() // Only read.
	removed := false
	for {
		entries, rerr := s.readDir(f, readDirChunk)
		for _, d := range entries {
			if !d.Type().IsRegular() {
				continue // Nothing a Put makes; left for a person to look at.
			}
			if err := s.root.Remove(filepath.Join(tempDir, d.Name())); err != nil && !notFound(err) {
				return removed, err
			}
			removed = true
		}
		if errors.Is(rerr, io.EOF) {
			return removed, nil
		}
		if rerr != nil {
			return removed, rerr
		}
	}
}
