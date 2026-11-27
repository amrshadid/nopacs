package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

const (
	// laneFiles and laneBytes cut a long run of files still to fetch into
	// pieces that separate streams can take: a scan of ten thousand
	// instances is no longer one stream. A piece ends at the first bound
	// reached.
	laneFiles = 256
	laneBytes = 256 << 20

	// listAttempts is how many times in a row the listing of one reference
	// is begun again after the server stopped answering.
	listAttempts = 3
)

// list asks the archive, with announcements only (no file bytes), what each
// reference holds, drops what is already whole on disk, and cuts the rest
// into lanes: runs of consecutive files of one scan that one stream will
// fetch. A set is what the archive held when the listing ran; a file archived
// later is fetched by the next run.
func (r *run) list(ctx context.Context, refs [][]string) ([]*lane, error) {
	seen := map[string]bool{}
	var lanes []*lane
	for _, ref := range refs {
		files, err := r.announce(ctx, ref)
		if err != nil {
			return nil, err
		}
		more, err := r.cut(files, seen)
		if err != nil {
			return nil, err
		}
		lanes = append(lanes, more...)
	}
	r.sel.prune(seen)
	return lanes, nil
}

// announce returns the announcements of a reference, in the stream's order.
// A server that stops answering is asked again; one that refuses is waited
// for as a stream is.
func (r *run) announce(ctx context.Context, ref []string) ([]*v1.ArchivedFile, error) {
	refusals, stalls := 0, 0
	var since time.Time
	for {
		files, err := r.announceOnce(ctx, ref)
		if err == nil {
			return files, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		if rf, ok := refusalOf(err); ok {
			if refusals == 0 {
				since = r.clk.now()
			}
			refusals++
			if r.clk.now().Sub(since) > refusalPatience {
				return nil, err
			}
			if e := r.throttled(ctx, backoff(refusals, rf.after)); e != nil {
				return nil, e
			}
			continue
		}
		if fault.KindOf(err) != fault.Unavailable {
			return nil, err
		}
		stalls++
		if stalls >= listAttempts {
			return nil, err
		}
		if e := r.clk.sleep(ctx, time.Duration(stalls)*250*time.Millisecond); e != nil {
			return nil, e
		}
	}
}

// announceOnce reads one reference's announcements from one stream.
func (r *run) announceOnce(ctx context.Context, ref []string) ([]*v1.ArchivedFile, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := &v1.ReadFilesRequest{Tenant: r.tenant, SubjectLabel: ref[0], AnnounceOnly: true}
	if len(ref) > 1 {
		req.SessionLabel = ref[1]
	}
	if len(ref) > 2 {
		req.ScanLabel = ref[2]
	}
	st, err := r.c.Data.ReadFiles(ctx, req)
	if err != nil {
		return nil, r.c.StreamError(ctx, "download", err)
	}
	defer func() { _ = st.Close() }()
	var files []*v1.ArchivedFile
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, r.c.StreamError(ctx, "download", err)
		}
		if f := m.GetFile(); f != nil {
			files = append(files, f)
		}
	}
}

// cut turns the announcements of one reference into lanes, counting every
// file of the set and skipping the files already whole. seen holds the files
// of the references before, so a file two references name is fetched once.
func (r *run) cut(files []*v1.ArchivedFile, seen map[string]bool) ([]*lane, error) {
	var lanes []*lane
	var cur []*v1.ArchivedFile
	var start, prev *v1.ArchivedFile
	var curBytes int64
	flush := func() {
		if len(cur) == 0 {
			return
		}
		l := &lane{r: r, files: cur, tried: map[string]bool{}}
		if start != nil {
			l.pos = position(start)
		}
		lanes = append(lanes, l)
		cur, curBytes = nil, 0
	}
	for _, f := range files {
		// A lane is one scan: a change of scan ends the run in hand.
		if prev != nil && (prev.GetSessionLabel() != f.GetSessionLabel() || prev.GetScanLabel() != f.GetScanLabel()) {
			flush()
			prev = nil
		}
		final, err := r.target(f)
		if err != nil {
			return nil, err
		}
		rel := r.rel(final)
		if seen[rel] {
			flush()
			prev = f
			continue
		}
		seen[rel] = true
		r.mu.Lock()
		r.filesTotal++
		r.mu.Unlock()
		if r.alreadyWhole(final, f) {
			flush()
			prev = f
			continue
		}
		if len(cur) == 0 {
			start = prev
		}
		cur = append(cur, f)
		curBytes += f.GetLength()
		r.mu.Lock()
		r.bytesTotal += f.GetLength()
		r.mu.Unlock()
		prev = f
		if len(cur) >= laneFiles || curBytes >= laneBytes {
			flush()
		}
	}
	flush()
	r.report(false)
	return lanes, nil
}

// alreadyWhole says whether the file is on disk, whole and the announced one,
// and counts it as skipped when it is. The selection file's record, with the
// file's size and time as recorded, answers without reading the file; without
// it the file is read and hashed.
func (r *run) alreadyWhole(final string, f *v1.ArchivedFile) bool {
	st, err := os.Stat(final)
	if err != nil || !st.Mode().IsRegular() || st.Size() != f.GetLength() {
		return false
	}
	if !r.sel.vouches(r.rel(final), f.GetSha256(), f.GetLength(), st) && !complete(final, f) {
		return false
	}
	r.passed(final, f, st)
	return true
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
