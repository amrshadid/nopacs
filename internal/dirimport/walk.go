package dirimport

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// part10Header is how many bytes of a file are read before it is handed
// in: the 128-byte preamble and the prefix "DICM" (PS3.10 7.1).
const part10Header = 132

// readNamesBatch is how many names one read of a directory asks for.
const readNamesBatch = 1024

// keptNamesBytes is the most bytes of names a directory keeps, of those
// after the one it enters, while the walk is below it (namesBytes): the
// walk holds one directory's names whole, the one it reads now, and at
// most this much of each one above it, which it reads again when what it
// kept runs out. 64 KiB is a few hundred names: a directory of
// subdirectories is read again once every few hundred of them, and a walk
// as deep as a path may go (1024 levels) keeps at most 64 MiB above the
// directory it reads.
const keptNamesBytes = 64 << 10

// errMoved is an entry that is not, once opened, what its type read
// without following a link said it was: a link put in its place, or the
// entry replaced, between the two; or a directory on another file system
// than the root's.
var errMoved = errors.New("the entry changed between its type being read and its opening")

// walker walks one unit's folder.
type walker struct {
	u        *unitRun
	sink     receive.Sink
	rcv      tenant.Receiver
	resolver *importpath.Resolver
	root     *os.Root
	// folder is the unit's folder; cursor the last path it judged, "" for
	// none.
	folder, cursor string
	// listing says it is the job's listing unit; lists that it queues the
	// folders at unitDepth rather than walk them (its folder lies above
	// that depth).
	listing, lists bool
	unitDepth      int
	maxEntries     int
	// rootDev is the file system the root lies on, when hasDev (deviceOf):
	// an entry on another one is a mount point or under one.
	rootDev uint64
	hasDev  bool
	// keep is the bytes of names a directory above the one being read
	// keeps (keptNamesBytes); held the bytes of names the walk holds now.
	keep, held int
	// pending are the folders found and not yet queued, in walk order.
	pending []string
	buf     [part10Header]byte
}

// descent is a directory the walk enters next: opened, its names read.
type descent struct {
	sub   *os.Root
	name  string
	path  string
	names []string
}

// take returns the descent's names and lets go of them, so only the
// directory walked holds them.
func (d *descent) take() []string {
	n := d.names
	d.names = nil
	return n
}

// walk walks the unit's folder after its cursor and returns how it ended.
func (w *walker) walk() outcome {
	if info, err := w.root.Stat("."); err == nil {
		w.rootDev, w.hasDev = deviceOf(info)
	}
	dir, err := openFolder(w.root, w.folder)
	if err == nil && dir != w.root {
		if info, serr := dir.Stat("."); serr == nil && w.foreign(info) {
			_ = dir.Close()
			err = errMoved
		}
	}
	if err != nil {
		return w.unopened(err)
	}
	if dir != w.root {
		defer func() { _ = dir.Close() }()
	}
	names, wide, err := w.readNames(dir)
	switch {
	case err != nil:
		w.u.log.Warn("imports: the unit's folder could not be read; the unit is tried again later")
		return outcome{end: endTemporary, reason: reasonUnreadable}
	case wide:
		return w.tooWide()
	}
	if out, stop := w.visit(dir, w.folder, names, depthOf(w.folder)); stop {
		return out
	}
	if out, stop := w.flush(); stop {
		return out
	}
	return outcome{end: endWalked}
}

// unopened returns how a unit ends whose folder could not be opened. Only
// a folder that is not there is gone: its walk ends with nothing found, as
// a tree changed since it was listed. A folder that is now a link, a file
// or on another file system is skipped as not regular, before the unit's
// first entry (after it, its row would come before the cursor, and the
// unit fails for now instead). Any other error (no permission, too many
// open files, an input or output error, a stale handle) may pass: the unit
// fails for now, spending an attempt, so a cause that stays fails the job
// rather than ends it with the folder's files uncounted. For the listing
// unit, the job's path not there, or not a directory reached through no
// link, leaves nothing it may read: the unit fails with the root's reason.
func (w *walker) unopened(err error) outcome {
	gone, moved := errors.Is(err, fs.ErrNotExist), errors.Is(err, errMoved)
	switch {
	case w.listing && (gone || moved):
		w.u.log.Warn("imports: the job's path is not a directory under the root; the unit fails")
		return outcome{end: endRoot}
	case gone:
		w.u.log.Info("imports: the unit's folder is gone; its walk ends with nothing found")
		return outcome{end: endWalked}
	case moved && w.cursor == "":
		w.u.log.Warn("imports: the unit's folder is no longer a directory reached through no link; it is skipped")
		if out, stop := w.skip(w.folder, tenant.SkipNotRegular, 0, ""); stop {
			return out
		}
		return outcome{end: endWalked}
	}
	w.u.log.Warn("imports: the unit's folder could not be opened; the unit is tried again later")
	return outcome{end: endTemporary, reason: reasonUnreadable}
}

// tooWide is the outcome of a directory the walk is inside that holds more
// names than import.max-dir-entries: its row comes before the cursor, so
// no skip can be kept for it. The unit fails for now, spending an attempt,
// so it is tried again (the setting raised meanwhile, it goes on) and, the
// directory staying too wide, the job fails rather than waits forever.
func (w *walker) tooWide() outcome {
	w.u.log.Warn("imports: a folder the unit is inside holds more names than import.max-dir-entries; the unit is tried again later")
	return outcome{end: endTemporary, reason: reasonTooWide}
}

// visit judges the entries of the directory dir, at dirPath and depth,
// named names (sorted), after the cursor. It reports stop when the unit
// must end now, as out says. Before it enters a directory it keeps of the
// names after that one at most w.keep bytes, and reads dir again when
// those run out, so the walk holds one directory's names whole.
func (w *walker) visit(dir *os.Root, dirPath string, names []string, depth int) (outcome, bool) {
	held := namesBytes(names)
	w.hold(held)
	defer func() { w.hold(-held) }()
	// more says dir holds names after the last of names, not kept.
	more := false
	for {
		d, rest, out, stop := w.judge(dir, dirPath, names, depth)
		if stop {
			return out, true
		}
		var after string
		if d != nil {
			kept, cut := window(rest, w.keep)
			more = more || cut
			names = kept
			w.hold(namesBytes(kept) - held)
			held = namesBytes(kept)
			out, stop := w.visit(d.sub, d.path, d.take(), depth+1)
			_ = d.sub.Close()
			if stop {
				return out, true
			}
			if len(names) > 0 {
				continue
			}
			after = d.name
		} else if len(names) > 0 {
			after = names[len(names)-1]
		}
		if !more {
			return outcome{}, false
		}
		all, wide, err := w.readNames(dir)
		switch {
		case err != nil:
			return outcome{end: endTemporary, reason: reasonUnreadable}, true
		case wide:
			return w.tooWide(), true
		}
		names, more = namesAfter(all, after), false
		w.hold(namesBytes(names) - held)
		held = namesBytes(names)
	}
}

// judge judges the entries of dir named names in turn until it meets a
// directory to enter: it returns that directory, opened, and the names
// after it; nil when every name was judged.
func (w *walker) judge(dir *os.Root, dirPath string, names []string, depth int) (*descent, []string, outcome, bool) {
	for i, name := range names {
		if out, stop := w.u.stopping(); stop {
			return nil, nil, out, true
		}
		p := join(dirPath, name)
		if !w.fresh(p) && !tenant.ImportPathUnder(p, w.cursor) {
			continue
		}
		fits, err := importpath.CheckEntry(p)
		if err != nil {
			// A name the rule of a segment refuses ("\", ":", not UTF-8,
			// too long) can be kept in no row: the entry is recorded under
			// the folder holding it, never by its name, and only the
			// fault's segment number is logged.
			w.u.log.Warn("imports: an entry whose name breaks the rule of a path segment is skipped", "segment_fault", err.Error())
			if out, stop := w.skip(p, tenant.SkipBadName, 0, ""); stop {
				return nil, nil, out, true
			}
			continue
		}
		info, err := dir.Lstat(name)
		var out outcome
		var stop bool
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case !fits:
			// Too long whatever else is true of it: its path is what no row
			// may hold.
			var size int64
			if err == nil {
				size = sizeOf(info)
			}
			out, stop = w.skip(p, tenant.SkipPathTooLong, size, "")
		case err != nil:
			out, stop = w.skip(p, tenant.SkipUnreadable, 0, "")
		case w.foreign(info):
			// A mount point, or what lies under one: the root's owner
			// named one file system.
			out, stop = w.skipIfFresh(p, tenant.SkipNotRegular, "")
		case info.IsDir():
			var d *descent
			d, out, stop = w.dir(dir, name, p, info, depth+1)
			if d != nil {
				return d, names[i+1:], outcome{}, false
			}
		case info.Mode().IsRegular():
			out, stop = w.file(dir, name, p, info)
		default:
			// A link, a device, a pipe or a socket.
			out, stop = w.skip(p, tenant.SkipNotRegular, 0, "")
		}
		if stop {
			return nil, nil, out, true
		}
	}
	return nil, nil, outcome{}, false
}

// dir judges the directory name of parent, at p and depth: for the listing
// unit at the unit depth, a folder to queue; otherwise a directory to
// enter, returned opened with its names.
func (w *walker) dir(parent *os.Root, name, p string, info fs.FileInfo, depth int) (*descent, outcome, bool) {
	sub, err := openDir(parent, name, info)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, outcome{}, false
	case errors.Is(err, errMoved):
		out, stop := w.skipIfFresh(p, tenant.SkipNotRegular, "")
		return nil, out, stop
	case err != nil:
		out, stop := w.skipIfFresh(p, tenant.SkipUnreadable, "")
		return nil, out, stop
	}
	names, wide, err := w.readNames(sub)
	switch {
	case err != nil:
		_ = sub.Close()
		out, stop := w.skipIfFresh(p, tenant.SkipUnreadable, "")
		return nil, out, stop
	case wide:
		_ = sub.Close()
		if !w.fresh(p) {
			// The walk is inside it, and it has grown, or the setting been
			// lowered, since.
			return nil, w.tooWide(), true
		}
		out, stop := w.skip(p, tenant.SkipDirectoryTooWide, 0, tenant.ImportDetailMaxDirEntries)
		return nil, out, stop
	}
	if w.lists && depth == w.unitDepth {
		// A unit's folder: queued, not walked. Its names were read to
		// skip it whole here when it is too wide, since its own unit
		// could keep no row for itself.
		_ = sub.Close()
		w.pending = append(w.pending, p)
		if len(w.pending) == tenant.MaxImportUnitBatch {
			out, stop := w.flush()
			return nil, out, stop
		}
		return nil, outcome{}, false
	}
	return &descent{sub: sub, name: name, path: p, names: names}, outcome{}, false
}

// file judges the regular file name of dir, at p, and hands it in unless
// it is skipped.
func (w *walker) file(dir *os.Root, name, p string, info fs.FileInfo) (outcome, bool) {
	parts, why := w.resolver.Resolve(p)
	switch why {
	case importpath.OutsideLayout:
		return w.skip(p, tenant.SkipOutsideLayout, info.Size(), "")
	case importpath.UnmappedSession:
		return w.skip(p, tenant.SkipUnmappedSession, info.Size(), "")
	}
	if info.Size() > w.rcv.MaxFileBytes {
		return w.skip(p, tenant.SkipTooLarge, info.Size(), tenant.ImportDetailMaxFileBytes)
	}
	if f := w.u.sup.cfg.seams.open; f != nil {
		f(p)
	}
	// Opened without blocking, so a pipe put in the file's place since its
	// type was read cannot hold the unit; what was opened is then checked
	// to be what was read.
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return outcome{}, false
	case err != nil:
		return w.skip(p, tenant.SkipUnreadable, info.Size(), "")
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	switch {
	case err != nil:
		return w.skip(p, tenant.SkipUnreadable, info.Size(), "")
	case !opened.Mode().IsRegular() || !os.SameFile(info, opened):
		return w.skip(p, tenant.SkipNotRegular, 0, "")
	}
	if n, ok := linkCount(opened); ok && n > 1 {
		// A file with another name may be that of a file outside the root
		// (a hard link made by anyone who can write in the tree): which
		// name is the file's own cannot be told.
		return w.skip(p, tenant.SkipNotRegular, 0, "")
	}
	_, err = io.ReadFull(f, w.buf[:])
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return w.skip(p, tenant.SkipNotPart10, opened.Size(), "")
	case err != nil:
		return w.skip(p, tenant.SkipUnreadable, opened.Size(), "")
	case string(w.buf[128:]) != "DICM":
		return w.skip(p, tenant.SkipNotPart10, opened.Size(), "")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return w.skip(p, tenant.SkipUnreadable, opened.Size(), "")
	}
	if out, stop := w.flush(); stop {
		return out, true
	}
	if out, stop := w.u.stopping(); stop {
		return out, true
	}
	return w.handIn(f, p, opened.Size(), keptParts(parts))
}

// handIn hands the file f, at p, to the receiver and waits for its record.
func (w *walker) handIn(f *os.File, p string, size int64, parts map[string]string) (outcome, bool) {
	out := w.sink.Accept(w.u.ctx, receive.Arrival{Receiver: w.rcv.Name, Body: progressReader{r: f, u: w.u},
		Import: &tenant.ImportArrival{Fence: w.u.fence, Path: p, Size: size, Parts: parts}})
	now := w.u.sup.cfg.Now()
	switch {
	case out.Status == receive.Received:
		w.judged(p)
		return outcome{}, false
	case out.Status == receive.Failed && out.Reason == receive.ReasonTooLarge:
		// It grew past the largest file since its size was read.
		return w.skip(p, tenant.SkipTooLarge, size, tenant.ImportDetailMaxFileBytes)
	case out.Status == receive.Failed && out.Reason == receive.ReasonImportEnded:
		w.u.log.Info("imports: the job ended while the unit walked; the unit ends")
		return outcome{end: endWalked}, true
	case out.Reason == receive.ReasonLeaseLost:
		w.u.lost.Store(true)
		return outcome{end: endStopped}, true
	case w.u.ctx.Err() != nil:
		return w.u.canceled(), true
	case out.Status == receive.Refused:
		// Not the unit's own cause: the archive takes no file for now.
		return outcome{end: endRelease, reason: out.Reason, due: now.Add(RefusedDelay)}, true
	}
	// Failed for another cause (the receiver disabled or removed since the
	// unit started, an arrival the pipeline refuses): an attempt is spent,
	// so a cause that stays fails the job rather than holds it.
	reason := out.Reason
	if reason == "" {
		reason = reasonInternal
	}
	return outcome{end: endTemporary, reason: reason}, true
}

// progressReader is a file handed in, each read of which counts as the
// unit's progress, so a large file read slowly is not taken for a stall.
type progressReader struct {
	r io.Reader
	u *unitRun
}

// Read reads from the file and notes the progress.
func (p progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.u.moved()
	return n, err
}

// skip records the entry at p as skipped with code, in one transaction with
// the unit's cursor, the folders found before it queued first.
func (w *walker) skip(p string, code tenant.ImportSkipCode, size int64, detail string) (outcome, bool) {
	if out, stop := w.flush(); stop {
		return out, true
	}
	job, err := w.u.p.RecordImportSkip(w.u.ctx, tenant.ImportSkip{Fence: w.u.fence, Path: p, Code: code, Size: size,
		Detail: detail, Now: w.u.sup.cfg.Now()})
	if err != nil {
		return w.u.failure(err, reasonDatabase), true
	}
	w.judged(p)
	if job.State.Ended() {
		w.u.log.Info("imports: the job ended while the unit walked; the unit ends")
		return outcome{end: endWalked}, true
	}
	return outcome{}, false
}

// skipIfFresh is skip for an entry after the cursor, and for one the walk
// is inside, which was judged readable before, a failure for now.
func (w *walker) skipIfFresh(p string, code tenant.ImportSkipCode, detail string) (outcome, bool) {
	if !w.fresh(p) {
		return outcome{end: endTemporary, reason: reasonUnreadable}, true
	}
	return w.skip(p, code, 0, detail)
}

// flush queues the folders found and not yet queued, which moves the
// listing unit's cursor to the last of them.
func (w *walker) flush() (outcome, bool) {
	if len(w.pending) == 0 {
		return outcome{}, false
	}
	job, err := w.u.p.AddImportUnits(w.u.ctx, w.u.fence, w.pending, w.u.sup.cfg.Now())
	if err != nil {
		return w.u.failure(err, reasonDatabase), true
	}
	w.cursor = w.pending[len(w.pending)-1]
	w.pending = w.pending[:0]
	w.u.moved()
	w.u.t.wake()
	if job.State.Ended() {
		return outcome{end: endWalked}, true
	}
	return outcome{}, false
}

// judged moves the cursor to p, recorded.
func (w *walker) judged(p string) {
	w.cursor = p
	w.u.moved()
	if f := w.u.sup.cfg.seams.judged; f != nil {
		f(p)
	}
}

// fresh reports whether p comes after the cursor, in the walk's order.
func (w *walker) fresh(p string) bool {
	return w.cursor == "" || importpath.Compare(p, w.cursor) > 0
}

// foreign reports whether info, read without following a link, is of an
// entry on another file system than the root's.
func (w *walker) foreign(info fs.FileInfo) bool {
	if !w.hasDev {
		return false
	}
	dev, ok := deviceOf(info)
	return ok && dev != w.rootDev
}

// hold adds n to the bytes of names the walk holds.
func (w *walker) hold(n int) {
	w.held += n
	if f := w.u.sup.cfg.seams.held; f != nil && n > 0 {
		f(w.held)
	}
}

// readNames reads the names of dir (readNames), each batch read counting
// as the unit's progress.
func (w *walker) readNames(dir *os.Root) ([]string, bool, error) {
	return readNames(dir, w.maxEntries, w.u.moved)
}

// keptParts returns the parts a file's arrival may keep: each value of at
// most tenant.MaxImportPartValue bytes of UTF-8. A part left out (a map's
// cell too long) is then missing to the naming rule, and a label that uses
// it cannot be made: the file is quarantined, never named by a value cut
// short.
func keptParts(parts map[string]string) map[string]string {
	if len(parts) == 0 {
		return nil
	}
	out := make(map[string]string, len(parts))
	for k, v := range parts {
		if len(v) <= tenant.MaxImportPartValue && utf8.ValidString(v) && len(out) < tenant.MaxImportParts {
			out[k] = v
		}
	}
	return out
}

// sizeOf returns a regular file's size, 0 for anything else.
func sizeOf(info fs.FileInfo) int64 {
	if info.Mode().IsRegular() {
		return info.Size()
	}
	return 0
}

// join returns the path of name in the directory at dir.
func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// depthOf returns how many segments p has; 0 for the root.
func depthOf(p string) int {
	if p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}

// nameOverhead is what a name held costs beside its bytes: its string
// header (a pointer and a length, 16 bytes on 64-bit) and the allocator's
// rounding, as measured for names read from a directory.
const nameOverhead = 32

// namesBytes returns about how many bytes names hold.
func namesBytes(names []string) int {
	n := 0
	for _, s := range names {
		n += len(s) + nameOverhead
	}
	return n
}

// window returns a copy of the first of names that fit in limit bytes
// (namesBytes), and whether some were left out. The copy lets go of the
// rest, which a slice of names would keep alive.
func window(names []string, limit int) ([]string, bool) {
	n, size := 0, 0
	for n < len(names) && size+len(names[n])+nameOverhead <= limit {
		size += len(names[n]) + nameOverhead
		n++
	}
	return slices.Clone(names[:n:n]), n < len(names)
}

// namesAfter returns the names of the sorted list names after name.
func namesAfter(names []string, name string) []string {
	i, found := slices.BinarySearch(names, name)
	if found {
		i++
	}
	return names[i:]
}

// readNames returns the names of the directory dir, sorted byte by byte
// (importpath.Compare's order, for names of one directory), or wide when
// it holds more than limit of them, which are then not kept. It calls
// read after each batch read.
func readNames(dir *os.Root, limit int, read func()) ([]string, bool, error) {
	f, err := dir.Open(".")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	var names []string
	for {
		batch, err := f.Readdirnames(readNamesBatch)
		read()
		names = append(names, batch...)
		if len(names) > limit {
			return nil, true, nil
		}
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			break
		}
		if err != nil {
			return nil, false, err
		}
	}
	slices.Sort(names)
	return names, false, nil
}

// openDir opens the directory name of parent, which info (read without
// following a link) says is a directory, and checks it is that directory:
// errMoved when a link or another entry was put in its place.
func openDir(parent *os.Root, name string, info fs.FileInfo) (*os.Root, error) {
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := sub.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = sub.Close()
		if err != nil {
			return nil, err
		}
		return nil, errMoved
	}
	return sub, nil
}

// openFolder opens the folder at p under root one segment at a time, each
// read without following a link and checked once opened (openDir); the
// root itself for "". A segment that is a link, or not a directory, is
// errMoved.
func openFolder(root *os.Root, p string) (*os.Root, error) {
	if p == "" {
		return root, nil
	}
	cur := root
	for seg := range strings.SplitSeq(p, "/") {
		info, err := cur.Lstat(seg)
		if err == nil && !info.IsDir() {
			err = errMoved
		}
		var next *os.Root
		if err == nil {
			next, err = openDir(cur, seg, info)
		}
		if cur != root {
			_ = cur.Close()
		}
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}
