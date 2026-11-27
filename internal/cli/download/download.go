package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

// partSuffix marks a file that is still arriving.
const partSuffix = ".part"

// recordSuffix, after a .part's name, names the file that holds the
// SHA-256 of the file the .part is the start of.
const recordSuffix = partSuffix + ".sha256"

// maxStreamRetries is how many times in a row a stream that ended without
// progress is opened again before the download gives up.
const maxStreamRetries = 3

// reportEvery is the least time between two progress reports, so a fast
// download does not spend its time drawing.
const reportEvery = 100 * time.Millisecond

// Options say what to download and where to report.
type Options struct {
	// Client reaches the server.
	Client *client.Client
	// Tenant is the tenant's name.
	Tenant string
	// Ref is one reference: the subject, then optionally its session and the
	// session's scan, every part a valid label. It is the same as a single
	// entry in Refs.
	Ref []string
	// Refs are further references, each like Ref. A file named by more than
	// one of them is fetched once.
	Refs [][]string
	// Dir is the directory to download into, made when missing.
	Dir string
	// Parallel is how many streams are held open at once: DefaultParallel
	// when zero, at most MaxParallel. The download lowers it by itself when
	// the server refuses a stream, so it never needs to be set to what a
	// server allows.
	Parallel int
	// LimitRate is the most bytes per second all streams receive together;
	// zero is no bound.
	LimitRate int64
	// SelectionPath, when set, is the file that records which files are
	// whole and checked, written atomically while the download runs. A run
	// with the same file skips what it records without reading it again,
	// as long as the file on disk is as it was when recorded.
	SelectionPath string
	// Layout is "" for <Dir>/<subject>/<session>/<scan>/<id>.dcm, or
	// LayoutBagIt for a bag (RFC 8493) in Dir, with the files under data/.
	// The bag's manifest and tag files are written when every file of the
	// set is whole and none failed.
	Layout string
	// OnProgress, when set, is told how far the whole download is, at most
	// ten times a second and at each phase's end. It is never called by two
	// goroutines at once and must not block.
	OnProgress func(Progress)
	// OnFailure, when set, is told of each file removed because it did not
	// match its announcement, in a sentence that names it. It is serialized
	// with OnProgress.
	OnFailure func(message string)
	// OnThrottle, when set, is told that the server refused a stream, how
	// long the download waits before asking again and how many streams it
	// now holds open at most. It is serialized with OnProgress.
	OnThrottle func(wait time.Duration, streams int)
}

// Result is what a finished download did.
type Result struct {
	// Files is how many files this run fetched and checked.
	Files int64
	// Skipped is how many were already whole on disk.
	Skipped int64
	// Resumed is how many were finished from a partial copy.
	Resumed int64
	// Bytes is how many bytes this run received.
	Bytes int64
	// Total is how many files the set holds.
	Total int64
	// Failures names each file removed for not matching its announcement,
	// with why.
	Failures []string
	// Elapsed is how long the run took.
	Elapsed time.Duration
	// Bag is the directory of the bag written, or empty.
	Bag string
}

// refs returns the references of o, each checked to be one to three labels.
func (o Options) refs() ([][]string, error) {
	var all [][]string
	if len(o.Ref) > 0 {
		all = append(all, o.Ref)
	}
	all = append(all, o.Refs...)
	seen := map[string]bool{}
	var out [][]string
	for _, ref := range all {
		if len(ref) < 1 || len(ref) > 3 {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not a subject, session or scan: use <subject>[/<session>[/<scan>]]", output.Clean(refText(ref))))
		}
		for _, p := range ref {
			if err := label.Check(p); err != nil {
				return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not a label: %s", output.Clean(p), err))
			}
		}
		if k := refText(ref); !seen[k] {
			seen[k] = true
			out = append(out, ref)
		}
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Invalid, "name what to download: a subject, a session or a scan")
	}
	return out, nil
}

// Run downloads and returns what it did. It returns an error when the
// download could not finish (the server refused or stopped for good, the
// context ended, the disk failed); files that did not match are not an error
// here but are in Result.Failures, so the caller decides how to say it. After
// an error the files already whole stay, and running again continues.
func Run(ctx context.Context, o Options) (*Result, error) {
	return start(ctx, o, realClock())
}

// start is Run with the clock it waits by.
func start(ctx context.Context, o Options, clk clock) (*Result, error) {
	refs, err := o.refs()
	if err != nil {
		return nil, err
	}
	if o.Layout != "" && o.Layout != LayoutBagIt {
		return nil, fault.New(fault.Invalid, fmt.Sprintf("the layout %q is not one this program writes: use %q or none", output.Clean(o.Layout), LayoutBagIt))
	}
	parallel := o.Parallel
	switch {
	case parallel == 0:
		parallel = DefaultParallel
	case parallel < 0 || parallel > MaxParallel:
		return nil, fault.New(fault.Invalid, fmt.Sprintf("parallel must be from 1 to %d", MaxParallel))
	}
	root := o.Dir
	if o.Layout == LayoutBagIt {
		root = filepath.Join(o.Dir, bagPayloadDir)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot make the directory "+root+": "+err.Error())
	}
	texts := make([]string, len(refs))
	for i, ref := range refs {
		texts[i] = refText(ref)
	}
	sel, err := loadSelection(o.SelectionPath, o.Tenant, o.Layout, texts)
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, err.Error())
	}
	r := &run{o: o, c: o.Client, tenant: o.Tenant, root: root, clk: clk, sel: sel, phase: PhaseListing,
		lim: newLimiter(o.LimitRate, clk), gate: newGate(parallel, clk), started: clk.now()}
	lanes, err := r.list(ctx, refs)
	if err != nil {
		_ = sel.save()
		return nil, r.interrupted(ctx, err)
	}
	r.setPhase(PhaseDownloading)
	r.report(true)
	err = r.work(ctx, lanes, parallel)
	if err != nil {
		return nil, r.interrupted(ctx, err)
	}
	r.setPhase(PhaseDone)
	r.report(true)
	if err := sel.save(); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the files are whole, but "+err.Error())
	}
	res := r.result()
	if o.Layout == LayoutBagIt && len(res.Failures) == 0 {
		if err := r.bag(); err != nil {
			return nil, fault.Wrap(fault.Internal, err, err.Error())
		}
		res.Bag = o.Dir
	}
	return res, nil
}

// run is one run of Run: what the streams share.
type run struct {
	o       Options
	c       *client.Client
	tenant  string
	root    string
	clk     clock
	lim     *limiter
	gate    *gate
	sel     *selection
	started time.Time

	mu                      sync.Mutex
	phase                   string
	files, skipped, resumed int64
	failed, bytes           int64
	filesTotal, bytesTotal  int64
	failures                []string
	current                 string
	tr                      tracker

	// repMu serializes the caller's callbacks.
	repMu      sync.Mutex
	lastReport time.Time
	seq        uint64
	lastSeq    uint64
}

// result is what the run did.
func (r *run) result() *Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &Result{Files: r.files, Skipped: r.skipped, Resumed: r.resumed, Bytes: r.bytes, Total: r.filesTotal, Failures: append([]string(nil), r.failures...), Elapsed: r.clk.now().Sub(r.started)}
}

// setPhase moves the run to a phase.
func (r *run) setPhase(p string) {
	r.mu.Lock()
	r.phase = p
	r.mu.Unlock()
}

// snapshot is the progress now; the caller holds the lock.
func (r *run) snapshot(now time.Time) Progress {
	p := Progress{
		Phase: r.phase, Files: r.files, Skipped: r.skipped, Failed: r.failed, FilesTotal: r.filesTotal,
		Bytes: r.bytes, BytesTotal: r.bytesTotal, Elapsed: now.Sub(r.started), Current: r.current,
	}
	p.Rate = r.tr.rate()
	p.Remaining = remaining(r.bytesTotal-r.bytes, p.Rate)
	return p
}

// report tells the caller how far the run is, at most every reportEvery
// unless forced.
func (r *run) report(force bool) {
	if r.o.OnProgress == nil {
		return
	}
	now := r.clk.now()
	r.repMu.Lock()
	skip := !force && now.Sub(r.lastReport) < reportEvery
	r.repMu.Unlock()
	if skip {
		return
	}
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.tr.add(now, r.bytes)
	p := r.snapshot(now)
	r.mu.Unlock()
	r.repMu.Lock()
	defer r.repMu.Unlock()
	if seq < r.lastSeq {
		return
	}
	r.lastSeq, r.lastReport = seq, now
	r.o.OnProgress(p)
}

// addBytes counts bytes received.
func (r *run) addBytes(n int64, current string) {
	r.mu.Lock()
	r.bytes += n
	r.current = current
	r.mu.Unlock()
}

// interrupted says where the download stopped and keeps what is on disk.
func (r *run) interrupted(ctx context.Context, err error) error {
	_ = r.sel.save()
	if ctx.Err() != nil {
		err = fault.New(fault.Unavailable, "the download was interrupted")
	}
	r.mu.Lock()
	done := r.files + r.skipped
	r.mu.Unlock()
	return fault.Wrap(fault.KindOf(err), err, fmt.Sprintf("%s; %d files are complete in %s, and running the same download again continues", fault.Message(err), done, r.root))
}

// target is a file's place on disk, or an error when a name the server sent
// could not be a label or an instance id. A "../" in it must never leave the
// directory.
func (r *run) target(f *v1.ArchivedFile) (final string, err error) {
	for _, p := range []string{f.GetSubjectLabel(), f.GetSessionLabel(), f.GetScanLabel()} {
		if label.Check(p) != nil {
			return "", fault.New(fault.Internal, fmt.Sprintf("the server announced a file under a name that is not a label (%q); nothing was written for it", output.Clean(p)))
		}
	}
	id := f.GetInstanceId()
	if label.Check(id) != nil || len(id) > 64 {
		return "", fault.New(fault.Internal, fmt.Sprintf("the server announced a file with an id that is not one (%q); nothing was written for it", output.Clean(id)))
	}
	if f.GetLength() < 0 || len(f.GetSha256()) != 64 {
		return "", fault.New(fault.Internal, "the server announced a file with no usable length or SHA-256: "+output.Clean(id))
	}
	return filepath.Join(r.root, f.GetSubjectLabel(), f.GetSessionLabel(), f.GetScanLabel(), id+".dcm"), nil
}

// rel is a file's path under the download's file directory, with slashes,
// the form the selection file records.
func (r *run) rel(final string) string {
	p, err := filepath.Rel(r.root, final)
	if err != nil {
		return filepath.ToSlash(final)
	}
	return filepath.ToSlash(p)
}

// whole records a file as fetched, checked and in place.
func (r *run) whole(final string, f *v1.ArchivedFile) {
	st, _ := os.Stat(final)
	r.sel.record(r.rel(final), f.GetSha256(), f.GetLength(), st)
	r.mu.Lock()
	r.files++
	r.current = final
	r.mu.Unlock()
}

// passed records a file that was already whole and not fetched.
func (r *run) passed(final string, f *v1.ArchivedFile, st os.FileInfo) {
	r.sel.record(r.rel(final), f.GetSha256(), f.GetLength(), st)
	r.mu.Lock()
	r.skipped++
	r.current = final
	r.mu.Unlock()
}

// bag writes the bag's tag files from what the selection records.
func (r *run) bag() error {
	var files []BagFile
	for _, e := range r.sel.entries() {
		files = append(files, BagFile{Path: bagPayloadDir + "/" + e.Path, SHA256: e.SHA256, Bytes: e.Bytes})
	}
	return writeBag(filepath.Dir(r.root), files, bagInfo{date: r.clk.now(), description: "DICOM files of tenant " + r.tenant + " from a NoPACs archive, each checked against the SHA-256 the archive announced"})
}
