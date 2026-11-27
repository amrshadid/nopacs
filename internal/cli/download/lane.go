package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// work fetches the lanes with up to parallel streams at once. The first
// failure that cannot be waited out stops the others at their next chunk,
// keeping every .part, and is returned.
func (r *run) work(parent context.Context, lanes []*lane, parallel int) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var (
		once  sync.Once
		first error
		wg    sync.WaitGroup
	)
	stop := func(err error) { once.Do(func() { first = err; cancel() }) }

	// The selection file is saved while the run goes, so a stop at any
	// moment loses at most a few seconds of what was recorded.
	saved := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(saved)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := r.sel.saveIfDue(r.clk.now()); err != nil {
					stop(err)
					return
				}
			case <-done:
				return
			}
		}
	}()

	queue := make(chan *lane)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for l := range queue {
				if err := l.run(ctx); err != nil {
					stop(err)
				}
			}
		}()
	}
	unfed := false
feed:
	for _, l := range lanes {
		select {
		case queue <- l:
		case <-ctx.Done():
			unfed = true
			break feed
		}
	}
	close(queue)
	wg.Wait()
	close(done)
	<-saved
	if first == nil && unfed {
		return parent.Err()
	}
	return first
}

// throttled tells the caller the server refused a stream and waits.
func (r *run) throttled(ctx context.Context, wait time.Duration) error {
	if r.o.OnThrottle != nil {
		r.repMu.Lock()
		r.o.OnThrottle(wait, r.gate.bound())
		r.repMu.Unlock()
	}
	return r.clk.sleep(ctx, wait)
}

// lane is a run of consecutive files of one scan that one stream fetches,
// one after another, in the stream's order.
type lane struct {
	r     *run
	files []*v1.ArchivedFile

	// pos is the last file known to be whole before the next stream starts:
	// the stream opens after it. It starts as the file before the lane's
	// first, or nil at the scan's start.
	pos *v1.ReadPosition
	// n is how many of the lane's files are settled (whole, passed over or
	// failed).
	n int
	// moved counts the bytes the lane has received, to tell a stream that
	// made progress from one that did not.
	moved int64
	// tried holds the files whose partial copy was given up on, so a second
	// attempt streams them whole instead of asking for a range again.
	tried map[string]bool
}

// request is the stream's request for the lane, after pos.
func (l *lane) request() *v1.ReadFilesRequest {
	f := l.files[0]
	return &v1.ReadFilesRequest{
		Tenant: l.r.tenant, SubjectLabel: f.GetSubjectLabel(), SessionLabel: f.GetSessionLabel(), ScanLabel: f.GetScanLabel(), StartAfter: l.pos,
	}
}

// advance settles a file: the next stream starts after it.
func (l *lane) advance(f *v1.ArchivedFile) {
	l.pos = position(f)
	l.n++
}

// run fetches the lane's files. A stream the server cut off is opened again
// after the last whole file; a stream the server refused (the caller holds as
// many as the server allows) is waited for, with the number of streams the
// download holds lowered, since a refusal is the server's answer to "too
// many", not an error.
func (l *lane) run(ctx context.Context) error {
	r := l.r
	stalls, refusals := 0, 0
	var since time.Time
	for l.n < len(l.files) {
		if err := r.gate.acquire(ctx); err != nil {
			return err
		}
		n, moved := l.n, l.moved
		err := l.attempt(ctx)
		if err == nil && l.n >= len(l.files) {
			// The stream was closed here, not run to its end, and the
			// server counts it until it notices: the place is kept for a
			// moment, so the next stream does not meet the old one's count
			// and get refused for it.
			_ = r.clk.sleep(ctx, closeGrace)
		}
		rf, refused := refusalOf(err)
		if refused {
			r.gate.refused()
		}
		r.gate.release()
		progressed := l.n != n || l.moved != moved
		switch {
		case err == nil:
			stalls, refusals = 0, 0
		case ctx.Err() != nil:
			return err
		case refused:
			if progressed {
				refusals = 0
			}
			if refusals == 0 {
				since = r.clk.now()
			}
			refusals++
			if r.clk.now().Sub(since) > refusalPatience {
				return err
			}
			if e := r.throttled(ctx, backoff(refusals, rf.after)); e != nil {
				return e
			}
		case fault.KindOf(err) == fault.Unavailable:
			if progressed {
				stalls = 0
			} else {
				stalls++
			}
			if stalls >= maxStreamRetries {
				return err
			}
			if e := r.clk.sleep(ctx, time.Duration(stalls+1)*250*time.Millisecond); e != nil {
				return e
			}
		default:
			return err
		}
	}
	return nil
}

// attempt does one thing toward the lane's next file: it continues a partial
// copy by range before any stream opens (a stream opened at it would start
// sending the file whole, and what it sent before the client closed it would
// be fetched for nothing), or opens a stream after the last whole file.
func (l *lane) attempt(ctx context.Context) error {
	f := l.files[l.n]
	final, err := l.r.target(f)
	if err != nil {
		return err
	}
	if l.resumable(final, f) {
		return l.resumePart(ctx, &partial{ann: f, final: final})
	}
	n := l.n
	resume, err := l.stream(ctx)
	if err != nil {
		return err
	}
	if resume != nil {
		return l.resumePart(ctx, resume)
	}
	if l.n == n && l.n < len(l.files) {
		return fault.New(fault.Internal, "the server stopped announcing files of "+output.Clean(f.GetSubjectLabel()+"/"+f.GetSessionLabel()+"/"+f.GetScanLabel())+" before "+output.Clean(f.GetInstanceId()))
	}
	return nil
}

// partial is the file being written by the stream.
type partial struct {
	ann   *v1.ArchivedFile
	final string
	file  *os.File
	h     hashState
	n     int64
	skip  bool
}

// hashState is the running SHA-256 of a file's bytes.
type hashState interface {
	io.Writer
	Sum(b []byte) []byte
	Reset()
}

// last says whether cur is the lane's last file and is whole: the stream can
// end there, without waiting for the next announcement, which for the last
// file of a piece of a scan is the first file of another lane.
func (l *lane) last(cur *partial) bool {
	return cur != nil && !cur.skip && cur.file != nil && cur.n == cur.ann.GetLength() && l.n+1 >= len(l.files)
}

// stream opens the stream after pos and writes what it carries, until the
// lane's last file is whole. It returns a file to continue by range when the
// stream met a partial copy, and an error when the stream ended before its
// last file.
func (l *lane) stream(ctx context.Context) (*partial, error) {
	r := l.r
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := r.c.Data.ReadFiles(ctx, l.request())
	if err != nil {
		return nil, r.c.StreamError(ctx, "download", err)
	}
	defer func() { _ = st.Close() }()
	var cur *partial
	// abandon ends the file in progress when the stream cannot go on: a file
	// whose last byte already arrived is checked and kept, so the next
	// stream starts after it; a partial one stays as a .part to be continued.
	abandon := func() {
		if cur != nil && cur.file != nil {
			if cur.n == cur.ann.GetLength() {
				_ = l.close(cur)
				return
			}
			_ = cur.file.Close()
		}
	}
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			return nil, l.close(cur)
		}
		if err != nil {
			abandon()
			return nil, r.c.StreamError(ctx, "download", err)
		}
		switch p := m.GetPart().(type) {
		case *v1.ReadFilesResponse_Scan:
			if err := l.close(cur); err != nil {
				return nil, err
			}
			cur = nil
		case *v1.ReadFilesResponse_File:
			if err := l.close(cur); err != nil {
				return nil, err
			}
			cur = nil
			if l.n >= len(l.files) {
				return nil, nil
			}
			next, resume, err := l.begin(p.File)
			if err != nil {
				return nil, err
			}
			if resume {
				return next, nil
			}
			cur = next
			if l.last(cur) {
				return nil, l.close(cur)
			}
		case *v1.ReadFilesResponse_Chunk:
			if cur == nil || cur.skip {
				continue
			}
			if p.Chunk.GetOffset() != cur.n {
				abandon()
				return nil, fault.New(fault.Internal, fmt.Sprintf("the server sent bytes of %s out of order (at %d, expected %d)", output.Clean(cur.ann.GetInstanceId()), p.Chunk.GetOffset(), cur.n))
			}
			data := p.Chunk.GetData()
			if l.overflows(cur, len(data)) {
				continue
			}
			if err := r.lim.wait(ctx, len(data)); err != nil {
				abandon()
				return nil, err
			}
			if _, err := cur.file.Write(data); err != nil {
				abandon()
				return nil, fault.Wrap(fault.Internal, err, "writing "+cur.final+partSuffix+": "+err.Error())
			}
			_, _ = cur.h.Write(data)
			cur.n += int64(len(data))
			l.moved += int64(len(data))
			r.addBytes(int64(len(data)), cur.final)
			r.report(false)
			if l.last(cur) {
				return nil, l.close(cur)
			}
		}
	}
}

// begin starts an announced file: skipped when it is already on disk and
// whole, continued by range when a partial copy is there (the bool), and
// otherwise opened fresh.
func (l *lane) begin(f *v1.ArchivedFile) (*partial, bool, error) {
	final, err := l.r.target(f)
	if err != nil {
		return nil, false, err
	}
	o := &partial{ann: f, final: final}
	if complete(final, f) {
		st, _ := os.Stat(final)
		o.skip = true
		l.r.passed(final, f, st)
		l.advance(f)
		l.r.report(false)
		return o, false, nil
	}
	if l.resumable(final, f) {
		return o, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return nil, false, fault.Wrap(fault.Internal, err, "cannot make the directory for "+final+": "+err.Error())
	}
	// The record is written before the first byte, so a .part on disk is
	// never taken for the start of a file other than the one it is.
	if err := os.WriteFile(final+recordSuffix, []byte(f.GetSha256()+"\n"), 0o600); err != nil { // #nosec G306 G703 -- validated labels under the directory
		return nil, false, fault.Wrap(fault.Internal, err, "cannot write "+final+recordSuffix+": "+err.Error())
	}
	file, err := os.OpenFile(final+partSuffix, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- validated labels under the directory
	if err != nil {
		return nil, false, fault.Wrap(fault.Internal, err, "cannot write "+final+partSuffix+": "+err.Error())
	}
	o.file, o.h = file, sha256.New()
	l.r.mu.Lock()
	l.r.current = final
	l.r.mu.Unlock()
	return o, false, nil
}

// resumable says whether a partial copy of f at final can be continued by
// range: a .part shorter than the file, whose record names the announced
// SHA-256, and not given up on already in this run.
func (l *lane) resumable(final string, f *v1.ArchivedFile) bool {
	st, err := os.Stat(final + partSuffix)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0 && st.Size() < f.GetLength() && !l.tried[final] &&
		recorded(final) == f.GetSha256()
}

// close ends the file being written: it is renamed when whole and correct,
// and removed and named as a failure when not.
func (l *lane) close(o *partial) error {
	if o == nil || o.skip || o.file == nil {
		return nil
	}
	err := o.file.Close()
	o.file = nil
	if err != nil {
		return fault.Wrap(fault.Internal, err, "writing "+o.final+partSuffix+": "+err.Error())
	}
	return l.verify(o)
}

// verify checks a finished .part against its announcement and renames it, or
// removes it and records the failure.
func (l *lane) verify(o *partial) error {
	part := o.final + partSuffix
	got := hex.EncodeToString(o.h.Sum(nil))
	switch {
	case o.n != o.ann.GetLength():
		l.fail(part, o, fmt.Sprintf("%d bytes arrived, %d were announced", o.n, o.ann.GetLength()))
	case got != o.ann.GetSha256():
		l.fail(part, o, "its SHA-256 is "+got+", the announced one is "+o.ann.GetSha256())
	default:
		if err := os.Rename(part, o.final); err != nil {
			return fault.Wrap(fault.Internal, err, "renaming "+part+": "+err.Error())
		}
		_ = os.Remove(o.final + recordSuffix)
		l.r.whole(o.final, o.ann)
		l.advance(o.ann)
		l.r.report(false)
	}
	return nil
}

// fail removes a damaged partial file and records it; the stream goes on past
// it.
func (l *lane) fail(part string, o *partial, why string) {
	_ = os.Remove(part)
	_ = os.Remove(o.final + recordSuffix)
	l.advance(o.ann)
	msg := fmt.Sprintf("%s: %s", o.final, why)
	l.r.mu.Lock()
	l.r.failed++
	l.r.failures = append(l.r.failures, msg)
	l.r.mu.Unlock()
	if l.r.o.OnFailure != nil {
		l.r.repMu.Lock()
		l.r.o.OnFailure(msg)
		l.r.repMu.Unlock()
	}
}

// overflows gives up the file being written when n more bytes would take it
// past its announced length: the .part is closed and removed, the failure
// recorded, and the rest of its bytes passed over, so a server that sends
// more than it announced never fills the disk. It reports whether it gave
// the file up.
func (l *lane) overflows(o *partial, n int) bool {
	if o.n+int64(n) <= o.ann.GetLength() {
		return false
	}
	_ = o.file.Close()
	o.file, o.skip = nil, true
	l.fail(o.final+partSuffix, o, fmt.Sprintf("more than the %d bytes announced arrived", o.ann.GetLength()))
	return true
}

// recorded returns the SHA-256 the record beside a .part names, or "" when
// there is none: a .part without one is never continued.
func recorded(final string) string {
	b, err := os.ReadFile(final + recordSuffix) // #nosec G304 -- validated labels under the directory
	if err != nil || len(b) > 128 {
		return ""
	}
	return strings.TrimSpace(string(b))
}
