package api

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/blob/readat"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// webStream writes a streamed answer of the retrieve service: its status
// and headers with its first byte, each write under the stall rule of a
// download, and the end of the request recorded as a download's is.
type webStream struct {
	x     *webRequest
	ctx   context.Context
	rc    *http.ResponseController
	head  func()
	begun bool
	sent  int64
}

// openStream holds one of the caller's open reads (holdRead) and starts
// the recheck of its key, for an answer whose head writes the status. The
// function it returns ends both.
func (x *webRequest) openStream(ctx context.Context, head func()) (*webStream, func(), error) {
	if err := x.holdRead(); err != nil {
		return nil, nil, err
	}
	ctx, stop := x.c.watchKey(ctx, x.d)
	s := &webStream{x: x, ctx: ctx, rc: http.NewResponseController(x.w), head: head}
	return s, func() { stop(); x.release() }, nil
}

// Write writes bytes of the body, writing the head first.
func (s *webStream) Write(b []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, context.Cause(s.ctx)
	}
	if !s.begun {
		s.head()
		s.begun = true
	}
	// A client that reads nothing for StallTimeout is let go.
	_ = s.rc.SetWriteDeadline(time.Now().Add(StallTimeout))
	n, err := s.x.w.Write(b)
	s.sent += int64(n)
	return n, err
}

// end records how the answer ended and returns the error for the server
// to write, when nothing of the answer was written yet. Once the answer
// has begun, a failure cannot be said in it: net/http closes the
// connection, or resets the HTTP/2 stream, so the multipart body never
// reaches its closing boundary and a client sees it is not whole.
func (s *webStream) end(err error, results int) error {
	x := s.x
	facts := x.facts(map[string]string{detailResults: strconv.Itoa(results), detailBytes: strconv.FormatInt(s.sent, 10)})
	refusal, refused := refusedWhileOpen(s.ctx)
	switch {
	case err == nil:
		if !s.begun {
			s.head()
		}
		x.c.record(x.d, x.st, master.OutcomeDone, "", facts)
		return nil
	case !s.begun && refused:
		// Confirm recorded the refusal.
		return x.c.toAPI(x.d, refusal)
	case !s.begun:
		return x.c.failWith(x.d, x.st, facts, err)
	case !refused:
		reason := "the answer ended before its last byte: the client went away or stalled, or the server stopped"
		if checkedread.Mismatched(err) || isFault(err) {
			reason = fault.Message(err)
		}
		x.c.record(x.d, x.st, master.OutcomeFailed, reason, facts)
	}
	if checkedread.Mismatched(err) {
		x.c.log.Error("api: a DICOMweb answer was cut: an instance's bytes on its storage do not match its manifest",
			"request_id", x.w.Header().Get(RequestIDHeader), "scan", x.object, "error", err.Error())
	}
	panic(http.ErrAbortHandler)
}

// copyRange copies length bytes of the object key of the scan's storage
// from offset to out through checkedread.CopyChecked, checked against sum when it is
// not nil. part, when not nil, is called before the first byte, so a part
// of a multipart body starts only once its first piece is read and
// checked.
func copyRange(ctx context.Context, sl *scanLines, key string, offset, length int64, sum *tenant.SHA256, part func() (io.Writer, error)) (int64, error) {
	body, err := sl.src.store.Read(ctx, key, offset, length)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }() // Only read from.
	var out io.Writer
	emit := func(b []byte) error {
		if out == nil {
			if out, err = part(); err != nil {
				return err
			}
		}
		_, err := out.Write(b)
		return err
	}
	chunk := min(max(length, 1), httpChunk)
	return checkedread.CopyChecked(body, length, sum, [2][]byte{make([]byte, chunk), make([]byte, chunk)}, emit)
}

// target is what a retrieve names, found in the catalog: the scans it
// reads, and the one instance when it names one.
type target struct {
	scans    iterator[tenant.CatalogScan]
	instance *tenant.ScanInstance
	scan     tenant.CatalogScan
}

// locate finds what the resource names: a study's scans by label, one
// scan, or one instance. Anything the tenant does not hold is NotFound.
func (x *webRequest) locate(ctx context.Context) (target, error) {
	res := x.res
	if res.Series == "" {
		sess, err := x.session(ctx, res.Study)
		if err != nil {
			return target{}, err
		}
		x.object = labelPath(sess.SubjectLabel, sess.Label)
		return target{scans: x.scans(tenant.ScanFilter{SubjectLabel: sess.SubjectLabel, SessionLabel: sess.Label, Order: tenant.OrderLabel}, master.MaxPageLimit)}, nil
	}
	s, err := x.scan(ctx, res.Study, res.Series)
	if err != nil {
		return target{}, err
	}
	x.object = labelPath(s.SubjectLabel, s.SessionLabel, s.Label)
	t := target{scans: &rowsOf[tenant.CatalogScan]{rows: []tenant.CatalogScan{s}}, scan: s}
	if res.Instance != "" {
		in, err := x.p.HeldInstance(ctx, pathOf(s), res.Instance)
		if fault.KindOf(err) == fault.NotFound {
			err = fault.Wrap(fault.NotFound, err, messageNoWebInstance)
		}
		if err != nil {
			return target{}, err
		}
		t.instance = &in
	}
	return t, nil
}

// marked returns the data loss of a target that names one instance a scrub
// marked, before anything of it is read or answered (checkedread.Marked);
// nil otherwise.
func (t target) marked() error {
	if t.instance == nil {
		return nil
	}
	if err := checkedread.Marked(*t.instance); err != nil {
		return instanceFailure(err, pathOf(t.scan))
	}
	return nil
}

// instancesOf iterates the instances of a target.
func (x *webRequest) instancesOf(t target) iterator[instanceRow] {
	if t.instance != nil {
		return &rowsOf[instanceRow]{rows: []instanceRow{{scan: t.scan, held: *t.instance}}}
	}
	return &scanInstances{x: x, scans: t.scans, size: master.MaxPageLimit}
}

// syntaxOf returns the transfer syntax of an instance's file: its line's,
// or, for a line written before the field existed, the file's meta group,
// read by a bounded ranged read.
func syntaxOf(ctx context.Context, sl *scanLines, held tenant.ScanInstance) (string, error) {
	in, err := sl.line(ctx, held.SOPInstanceUID)
	if err != nil {
		return "", err
	}
	if in.TransferSyntax != "" {
		return in.TransferSyntax, nil
	}
	ts, err := header.ReadTransferSyntax(readat.New(ctx, sl.src.store, held.Key, held.Size), held.Size)
	if err != nil {
		return "", fileFailure(err)
	}
	return ts, nil
}

// retrieve answers a retrieve of instances (PS3.18 10.4.1.1.1): each as it
// is stored, byte for byte, checked against its SHA-256 as it is sent.
// A request that accepts only some transfer syntaxes is given the
// instances stored in them, 206 when others were left out, and 406 when
// none is (PS3.18 10.4.3.1); nothing is transcoded.
func (x *webRequest) retrieve(ctx context.Context) error {
	want, perr := dicomweb.AcceptInstances(x.r)
	if perr != nil {
		return x.refuse(perr)
	}
	if want.Bare && x.res.Level != dicomweb.Instance {
		return x.refuse(&dicomweb.Error{Status: http.StatusNotAcceptable, Message: `several instances are retrieved as multipart/related; type="` + dicomweb.MediaDICOM + `"`})
	}
	t, err := x.locate(ctx)
	if err != nil {
		return x.fail(err)
	}
	if err := t.marked(); err != nil {
		return x.fail(err)
	}
	ls := x.newLines()
	defer ls.closeAll()
	status := http.StatusOK
	if !want.Any {
		kept, left, err := x.countAccepted(ctx, t, want, ls)
		if err != nil {
			return x.fail(err)
		}
		if kept == 0 {
			return x.refuse(&dicomweb.Error{Status: http.StatusNotAcceptable, Message: messageServedMediaType})
		}
		if left > 0 {
			status = http.StatusPartialContent
		}
		if t, err = x.locate(ctx); err != nil {
			return x.fail(err)
		}
	}
	if want.Bare {
		return x.retrieveBare(ctx, *t.instance, t.scan, ls)
	}
	var mp *dicomweb.Multipart
	stream, stop, err := x.openStream(ctx, func() {
		x.w.Header().Set("Content-Type", mp.ContentType())
		x.w.WriteHeader(status)
	})
	if err != nil {
		return x.fail(err)
	}
	defer stop()
	mp = dicomweb.NewMultipart(stream, dicomweb.MediaDICOM)
	if x.r.Method == http.MethodHead {
		return stream.end(nil, 0)
	}
	ctx = stream.ctx
	it := x.instancesOf(t)
	sent := 0
	for {
		row, ok, err := it.next(ctx)
		if err != nil {
			return stream.end(err, sent)
		}
		if !ok {
			break
		}
		sl, err := ls.of(ctx, row.scan)
		if err != nil {
			return stream.end(err, sent)
		}
		ts, err := syntaxOf(ctx, sl, row.held)
		if err != nil {
			return stream.end(err, sent)
		}
		if !want.Accepts(ts) {
			continue
		}
		in, sum := row.held, row.held.ArchivedSHA256
		if err := checkedread.Marked(in); err != nil {
			// Before the instance's part begins: none of its bytes is sent.
			return stream.end(instanceFailure(err, pathOf(row.scan)), sent)
		}
		if _, err := copyRange(ctx, sl, in.Key, in.Offset, in.Length, &sum, func() (io.Writer, error) {
			return mp.Part(dicomweb.MediaDICOM, ts)
		}); err != nil {
			return stream.end(instanceFailure(err, pathOf(row.scan)), sent)
		}
		sent++
	}
	if sent == 0 {
		// Everything was removed between the look and the answer.
		return stream.end(fault.New(fault.NotFound, messageNoStudy), 0)
	}
	if err := mp.Close(); err != nil {
		return stream.end(err, sent)
	}
	return stream.end(nil, sent)
}

// countAccepted counts the instances of the target stored in a transfer
// syntax the request accepts, and those left out.
func (x *webRequest) countAccepted(ctx context.Context, t target, want dicomweb.InstanceWant, ls *lines) (kept, left int, err error) {
	it := x.instancesOf(t)
	for {
		row, ok, err := it.next(ctx)
		if err != nil || !ok {
			return kept, left, err
		}
		sl, err := ls.of(ctx, row.scan)
		if err != nil {
			return 0, 0, err
		}
		ts, err := syntaxOf(ctx, sl, row.held)
		if err != nil {
			return 0, 0, err
		}
		if want.Accepts(ts) {
			kept++
		} else {
			left++
		}
	}
}

// retrieveBare answers one instance as application/dicom with no multipart
// body around it, which a request that accepts only that form asks for.
func (x *webRequest) retrieveBare(ctx context.Context, in tenant.ScanInstance, s tenant.CatalogScan, ls *lines) error {
	sl, err := ls.of(ctx, s)
	if err != nil {
		return x.fail(err)
	}
	ts, err := syntaxOf(ctx, sl, in)
	if err != nil {
		return x.fail(err)
	}
	stream, stop, err := x.openStream(ctx, func() {
		h := x.w.Header()
		h.Set("Content-Type", dicomweb.MediaDICOM+"; transfer-syntax="+ts)
		h.Set("Content-Length", strconv.FormatInt(in.Length, 10))
		x.w.WriteHeader(http.StatusOK)
	})
	if err != nil {
		return x.fail(err)
	}
	defer stop()
	if x.r.Method == http.MethodHead {
		return stream.end(nil, 0)
	}
	sum := in.ArchivedSHA256
	if _, err := copyRange(stream.ctx, sl, in.Key, in.Offset, in.Length, &sum, func() (io.Writer, error) { return stream, nil }); err != nil {
		return stream.end(instanceFailure(err, pathOf(s)), 0)
	}
	return stream.end(nil, 1)
}

// bulkData answers a bulk value of an instance (PS3.18 10.4.1.1.5): the
// bytes at the place of its file a BulkDataURI of the metadata names, its
// query's offset and length, as the plain download's window names them.
// The SHA-256 is that of the whole instance, so a part of it is checked
// for its length only.
func (x *webRequest) bulkData(ctx context.Context) error {
	multi, perr := dicomweb.AcceptBulk(x.r)
	if perr != nil {
		return x.refuse(perr)
	}
	win, err := windowOf(x.r.URL.RawQuery)
	if err == nil && !win.given {
		err = invalid("query", messageBadWindow)
	}
	if err != nil {
		return x.refuse(&dicomweb.Error{Status: http.StatusBadRequest, Message: fault.Message(err)})
	}
	t, err := x.locate(ctx)
	if err != nil {
		return x.fail(err)
	}
	if err := t.marked(); err != nil {
		return x.fail(err)
	}
	in := *t.instance
	if win.offset+win.length > in.Length {
		return x.fail(fault.New(fault.NotFound, "the instance holds no bulk value at this place"))
	}
	ls := x.newLines()
	defer ls.closeAll()
	sl, err := ls.of(ctx, t.scan)
	if err != nil {
		return x.fail(err)
	}
	var mp *dicomweb.Multipart
	stream, stop, err := x.openStream(ctx, func() {
		h := x.w.Header()
		if multi {
			h.Set("Content-Type", mp.ContentType())
		} else {
			h.Set("Content-Type", dicomweb.MediaOctetStream)
			h.Set("Content-Length", strconv.FormatInt(win.length, 10))
		}
		x.w.WriteHeader(http.StatusOK)
	})
	if err != nil {
		return x.fail(err)
	}
	defer stop()
	mp = dicomweb.NewMultipart(stream, dicomweb.MediaOctetStream)
	if x.r.Method == http.MethodHead {
		return stream.end(nil, 0)
	}
	part := func() (io.Writer, error) { return stream, nil }
	if multi {
		part = func() (io.Writer, error) { return mp.Part(dicomweb.MediaOctetStream, "") }
	}
	if _, err := copyRange(stream.ctx, sl, in.Key, in.Offset+win.offset, win.length, nil, part); err != nil {
		return stream.end(instanceFailure(err, pathOf(t.scan)), 0)
	}
	if multi {
		if err := mp.Close(); err != nil {
			return stream.end(err, 1)
		}
	}
	return stream.end(nil, 1)
}
