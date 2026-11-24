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

// Options say what to download and where to report.
type Options struct {
	// Client reaches the server.
	Client *client.Client
	// Tenant is the tenant's name.
	Tenant string
	// Ref is the subject, then optionally its session and the session's
	// scan: every part a valid label.
	Ref []string
	// Dir is the directory to download into, made when missing.
	Dir string
	// OnProgress, when set, is told after every chunk and every finished
	// file how far the download is. It runs on the download's goroutine and
	// must not block.
	OnProgress func(Progress)
	// OnFailure, when set, is told of each file removed because it did not
	// match its announcement, in a sentence that names it.
	OnFailure func(message string)
}

// Progress is how far a download is.
type Progress struct {
	// Files is how many files are whole on disk after this run fetched them.
	Files int64
	// Skipped is how many files were already whole and were not fetched.
	Skipped int64
	// Bytes is how many bytes this run received.
	Bytes int64
	// Current is the path of the file being written, or empty.
	Current string
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
	// Failures names each file removed for not matching its announcement,
	// with why.
	Failures []string
}

// Run downloads and returns what it did. It returns an error when the
// download could not finish (the server refused or stopped for good, the
// context ended, the disk failed); files that did not match are not an error
// here but are in Result.Failures, so the caller decides how to say it. After
// an error the files already whole stay, and running again continues.
func Run(ctx context.Context, o Options) (*Result, error) {
	d := &downloader{o: o, c: o.Client, tenant: o.Tenant, root: o.Dir, ref: o.Ref, tried: map[string]bool{}}
	if err := os.MkdirAll(d.root, 0o700); err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot make the directory "+d.root+": "+err.Error())
	}
	// A partial copy of the first file still missing is continued by range
	// before any stream opens: a stream opened at it would start sending
	// the file whole, and what it sent before the client closed it (a
	// stream has up to megabytes in flight) would be fetched for nothing.
	for {
		next, err := d.findPosition(ctx)
		if err != nil {
			return nil, d.interrupted(ctx, err)
		}
		if next == nil {
			break
		}
		d.current = next.final
		if err := d.resumePart(ctx, next); err != nil {
			return nil, d.interrupted(ctx, err)
		}
	}
	stalls := 0
	for {
		before, beforeBytes := d.pos, d.bytes
		resume, err := d.stream(ctx)
		switch {
		case err == nil && resume == nil:
			return d.result(), nil
		case err != nil && ctx.Err() == nil:
			if fault.KindOf(err) != fault.Unavailable {
				return nil, d.interrupted(ctx, err)
			}
			if d.pos == before && d.bytes == beforeBytes {
				stalls++
			} else {
				stalls = 0
			}
			if stalls >= maxStreamRetries {
				return nil, d.interrupted(ctx, err)
			}
			select {
			case <-time.After(time.Duration(stalls+1) * 250 * time.Millisecond):
			case <-ctx.Done():
			}
		case err != nil:
			return nil, d.interrupted(ctx, err)
		}
		if resume != nil {
			if err := d.resumePart(ctx, resume); err != nil {
				return nil, d.interrupted(ctx, err)
			}
		}
	}
}

// downloader is one run of Run.
type downloader struct {
	o      Options
	c      *client.Client
	tenant string
	root   string
	ref    []string

	// pos is the last instance known to be whole on disk, in the stream's
	// order: the next stream starts after it.
	pos *v1.ReadPosition
	// tried holds the files whose partial copy was given up on, so a second
	// attempt streams them whole instead of asking for a range again.
	tried map[string]bool

	files, skipped, resumed int64
	bytes                   int64
	failures                []string
	current                 string
}

// result is what the run did.
func (d *downloader) result() *Result {
	return &Result{Files: d.files, Skipped: d.skipped, Resumed: d.resumed, Bytes: d.bytes, Failures: d.failures}
}

// report tells the caller how far the run is.
func (d *downloader) report() {
	if d.o.OnProgress != nil {
		d.o.OnProgress(Progress{Files: d.files, Skipped: d.skipped, Bytes: d.bytes, Current: d.current})
	}
}

// request is the stream's request for this download, after pos.
func (d *downloader) request(announceOnly bool) *v1.ReadFilesRequest {
	r := &v1.ReadFilesRequest{Tenant: d.tenant, SubjectLabel: d.ref[0], AnnounceOnly: announceOnly, StartAfter: d.pos}
	if len(d.ref) > 1 {
		r.SessionLabel = d.ref[1]
	}
	if len(d.ref) > 2 {
		r.ScanLabel = d.ref[2]
	}
	return r
}

// interrupted says where the download stopped and keeps what is on disk.
func (d *downloader) interrupted(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		err = fault.New(fault.Unavailable, "the download was interrupted")
	}
	return fault.Wrap(fault.KindOf(err), err, fmt.Sprintf("%s; %d files are complete in %s, and running the same download again continues", fault.Message(err), d.files+d.skipped, d.root))
}

// target is a file's place on disk, or an error when a name the server sent
// could not be a label or an instance id. A "../" in it must never leave the
// directory.
func (d *downloader) target(f *v1.ArchivedFile) (final string, err error) {
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
	return filepath.Join(d.root, f.GetSubjectLabel(), f.GetSessionLabel(), f.GetScanLabel(), id+".dcm"), nil
}

// position is the place of an announced file in the stream's order.
func position(f *v1.ArchivedFile) *v1.ReadPosition {
	return &v1.ReadPosition{SessionLabel: f.GetSessionLabel(), ScanLabel: f.GetScanLabel(), InstanceId: f.GetInstanceId()}
}

// complete says whether a file already on disk is the announced one: the
// same length and the same SHA-256.
func complete(path string, f *v1.ArchivedFile) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() != f.GetLength() {
		return false
	}
	h := sha256.New()
	if _, err := hashFile(path, h); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == f.GetSha256()
}

// findPosition reads only the announcements of the stream after pos, which
// costs no file bytes, and moves pos past every file already complete on
// disk, so a rerun starts its stream at the first file still missing. It
// returns that file when a partial copy of it can be continued by range.
func (d *downloader) findPosition(ctx context.Context) (*partial, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := d.c.Data.ReadFiles(ctx, d.request(true))
	if err != nil {
		return nil, d.c.StreamError(ctx, "download", err)
	}
	defer func() { _ = st.Close() }()
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, d.c.StreamError(ctx, "download", err)
		}
		f := m.GetFile()
		if f == nil {
			continue
		}
		final, err := d.target(f)
		if err != nil {
			return nil, err
		}
		if !complete(final, f) {
			if d.resumable(final, f) {
				return &partial{ann: f, final: final}, nil
			}
			return nil, nil
		}
		d.pos = position(f)
		d.skipped++
		d.report()
	}
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

// stream opens the stream after pos and writes what it carries. It returns a
// file to continue by range when the stream met a partial copy, and an error
// when the stream ended before its last file.
func (d *downloader) stream(ctx context.Context) (*partial, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := d.c.Data.ReadFiles(ctx, d.request(false))
	if err != nil {
		return nil, d.c.StreamError(ctx, "download", err)
	}
	defer func() { _ = st.Close() }()
	var cur *partial
	// abandon ends the file in progress when the stream cannot go on: a file
	// whose last byte already arrived is checked and kept, so the next
	// stream starts after it; a partial one stays as a .part to be continued.
	abandon := func() {
		if cur != nil && cur.file != nil {
			if cur.n == cur.ann.GetLength() {
				_ = d.close(cur)
				return
			}
			_ = cur.file.Close()
		}
	}
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			return nil, d.close(cur)
		}
		if err != nil {
			abandon()
			return nil, d.c.StreamError(ctx, "download", err)
		}
		switch p := m.GetPart().(type) {
		case *v1.ReadFilesResponse_Scan:
			if err := d.close(cur); err != nil {
				return nil, err
			}
			cur = nil
		case *v1.ReadFilesResponse_File:
			if err := d.close(cur); err != nil {
				return nil, err
			}
			cur = nil
			next, resume, err := d.begin(p.File)
			if err != nil {
				return nil, err
			}
			if resume {
				return next, nil
			}
			cur = next
		case *v1.ReadFilesResponse_Chunk:
			if cur == nil || cur.skip {
				continue
			}
			if p.Chunk.GetOffset() != cur.n {
				abandon()
				return nil, fault.New(fault.Internal, fmt.Sprintf("the server sent bytes of %s out of order (at %d, expected %d)", output.Clean(cur.ann.GetInstanceId()), p.Chunk.GetOffset(), cur.n))
			}
			data := p.Chunk.GetData()
			if d.overflows(cur, len(data)) {
				continue
			}
			if _, err := cur.file.Write(data); err != nil {
				abandon()
				return nil, fault.Wrap(fault.Internal, err, "writing "+cur.final+partSuffix+": "+err.Error())
			}
			_, _ = cur.h.Write(data)
			cur.n += int64(len(data))
			d.bytes += int64(len(data))
			d.report()
		}
	}
}

// begin starts an announced file: skipped when it is already on disk and
// whole, continued by range when a partial copy is there (the bool), and
// otherwise opened fresh.
func (d *downloader) begin(f *v1.ArchivedFile) (*partial, bool, error) {
	final, err := d.target(f)
	if err != nil {
		return nil, false, err
	}
	o := &partial{ann: f, final: final}
	d.current = final
	if complete(final, f) {
		o.skip = true
		d.skipped++
		d.pos = position(f)
		d.report()
		return o, false, nil
	}
	if d.resumable(final, f) {
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
	return o, false, nil
}

// resumable says whether a partial copy of f at final can be continued by
// range: a .part shorter than the file, whose record names the announced
// SHA-256, and not given up on already in this run.
func (d *downloader) resumable(final string, f *v1.ArchivedFile) bool {
	st, err := os.Stat(final + partSuffix)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0 && st.Size() < f.GetLength() && !d.tried[final] &&
		recorded(final) == f.GetSha256()
}

// close ends the file being written: it is renamed when whole and correct,
// and removed and named as a failure when not.
func (d *downloader) close(o *partial) error {
	if o == nil || o.skip || o.file == nil {
		return nil
	}
	err := o.file.Close()
	o.file = nil
	if err != nil {
		return fault.Wrap(fault.Internal, err, "writing "+o.final+partSuffix+": "+err.Error())
	}
	return d.verify(o)
}

// verify checks a finished .part against its announcement and renames it, or
// removes it and records the failure.
func (d *downloader) verify(o *partial) error {
	part := o.final + partSuffix
	got := hex.EncodeToString(o.h.Sum(nil))
	switch {
	case o.n != o.ann.GetLength():
		d.fail(part, o, fmt.Sprintf("%d bytes arrived, %d were announced", o.n, o.ann.GetLength()))
	case got != o.ann.GetSha256():
		d.fail(part, o, "its SHA-256 is "+got+", the announced one is "+o.ann.GetSha256())
	default:
		if err := os.Rename(part, o.final); err != nil {
			return fault.Wrap(fault.Internal, err, "renaming "+part+": "+err.Error())
		}
		_ = os.Remove(o.final + recordSuffix)
		d.files++
		d.pos = position(o.ann)
		d.report()
	}
	return nil
}

// fail removes a damaged partial file and records it; the stream goes on past
// it.
func (d *downloader) fail(part string, o *partial, why string) {
	_ = os.Remove(part)
	_ = os.Remove(o.final + recordSuffix)
	d.pos = position(o.ann)
	msg := fmt.Sprintf("%s: %s", o.final, why)
	d.failures = append(d.failures, msg)
	if d.o.OnFailure != nil {
		d.o.OnFailure(msg)
	}
}

// overflows gives up the file being written when n more bytes would take it
// past its announced length: the .part is closed and removed, the failure
// recorded, and the rest of its bytes passed over, so a server that sends
// more than it announced never fills the disk. It reports whether it gave
// the file up.
func (d *downloader) overflows(o *partial, n int) bool {
	if o.n+int64(n) <= o.ann.GetLength() {
		return false
	}
	_ = o.file.Close()
	o.file, o.skip = nil, true
	d.fail(o.final+partSuffix, o, fmt.Sprintf("more than the %d bytes announced arrived", o.ann.GetLength()))
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
