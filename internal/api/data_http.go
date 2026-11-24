package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The content types of a download (data.proto): a whole instance is a
// DICOM file (RFC 3240); a window is bytes of one.
const (
	contentDICOM = "application/dicom"
	contentBytes = "application/octet-stream"
)

// messageNoDownload is every 404 of a download after the check: the same
// whatever is missing (a label that is not one, a scan, an instance), so
// it tells the caller only that this tenant holds nothing at the path.
const messageNoDownload = "the tenant holds no instance at this path"

// Download serves DownloadPattern, once the server's check has allowed
// the request and every value of its path has passed the server's rule of
// a segment: the whole instance, a range of it, or a window its query
// names, with its strong ETag and the conditions of RFC 9110 13, read from
// the storage at the key and place the catalog holds. A whole instance is
// checked against its SHA-256 as it is sent, and a body whose bytes do not
// match is cut before its last byte (the connection closed, or on HTTP/2
// the stream reset), so it never looks complete. Its errors before the
// answer starts are returned for the server to write, as for every route.
func (h Handlers) Download(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	return h.c.download(ctx, w, r)
}

// download is Download.
func (c *core) download(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	d, err := c.decision(ctx, DownloadPattern)
	if err != nil {
		return err
	}
	_, facts := DownloadNeed(r)
	details := facts.Details
	name := r.PathValue("tenant")
	// A download only reads, so a moved or fenced tenant serves it.
	p, st, err := c.inTenant(readingOnly(ctx), d, name, details)
	if err != nil {
		return err
	}
	in, path, err := downloadInstance(ctx, p, r)
	if err != nil {
		return c.failIn(d, st, err)
	}
	if err := checkedread.Marked(in); err != nil {
		// Data loss before any answer: no status, header or byte of the
		// instance is sent, whatever was asked (a range, a window, HEAD).
		return c.failIn(d, st, instanceFailure(err, path))
	}
	win, err := windowOf(r.URL.RawQuery)
	if err != nil {
		return c.failIn(d, st, err)
	}
	release, err := c.reads.Take(ownerOf(d))
	if err != nil {
		return c.failIn(d, st, err)
	}
	defer release()
	sent := func(n int64) access.Facts {
		return access.Facts{Details: withDetails(details, map[string]string{detailBytes: strconv.FormatInt(n, 10)})}
	}

	base, size, ctype := in.Offset, in.Length, contentDICOM
	if win.given {
		if win.offset+win.length > in.Length {
			unsatisfiable(w, in.Length)
			c.record(d, st, master.OutcomeFailed, "the window asked for ends past the instance", sent(0))
			return nil
		}
		base, size, ctype = in.Offset+win.offset, win.length, contentBytes
	}
	tag := strongTag(in.ArchivedSHA256.String())
	hd := w.Header()
	if noneMatchFails(r.Header.Values("If-None-Match"), tag) {
		hd.Set("ETag", tag)
		hd.Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusNotModified)
		c.record(d, st, master.OutcomeDone, "", sent(0))
		return nil
	}
	span, status := byteRange{first: 0, last: size - 1}, http.StatusOK
	if rh := r.Header.Values("Range"); len(rh) == 1 && ifRangeHolds(r.Header.Values("If-Range"), tag) {
		got, kind := byteRangeOf(rh[0], size)
		switch kind {
		case rangeUnsatisfiable:
			unsatisfiable(w, size)
			c.record(d, st, master.OutcomeFailed, "the range asked for holds no byte of the body", sent(0))
			return nil
		case rangeOne:
			span, status = got, http.StatusPartialContent
		}
	}
	n := span.last - span.first + 1

	stores := c.newReadStores(p, name)
	defer stores.Close()
	store, err := stores.Get(ctx, in.Storage)
	if err != nil {
		return c.failIn(d, st, err)
	}
	var body io.ReadCloser
	if r.Method == http.MethodHead {
		// What GET would answer, as far as it is known before a byte is
		// read: an object missing or shorter than the catalog records is
		// data loss.
		info, err := store.Stat(ctx, in.Key)
		if err == nil && info.Size < in.Offset+in.Length {
			err = checkedread.ErrShort
		}
		if err != nil {
			return c.failIn(d, st, instanceFailure(err, path))
		}
	} else {
		if body, err = store.Read(ctx, in.Key, base+span.first, n); err != nil {
			return c.failIn(d, st, instanceFailure(err, path))
		}
		defer func() { _ = body.Close() }() // Only read from.
	}
	head := func() {
		hd.Set("Content-Type", ctype)
		hd.Set("Content-Length", strconv.FormatInt(n, 10))
		hd.Set("Accept-Ranges", "bytes")
		hd.Set("ETag", tag)
		if status == http.StatusPartialContent {
			hd.Set("Content-Range", "bytes "+strconv.FormatInt(span.first, 10)+"-"+strconv.FormatInt(span.last, 10)+"/"+strconv.FormatInt(size, 10))
		}
		w.WriteHeader(status)
	}
	if body == nil {
		head()
		c.record(d, st, master.OutcomeDone, "", sent(0))
		return nil
	}

	ctx, stop := c.watchKey(ctx, d)
	defer stop()
	rc := http.NewResponseController(w)
	begun := false
	emit := func(b []byte) error {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		// The status is written with the first piece, which checkedread.CopyChecked
		// passes only once the piece after it is read: a body that fails
		// within its first piece (every instance of up to httpChunk bytes,
		// checked whole) is still answered with its error.
		if !begun {
			head()
			begun = true
		}
		// A client that reads nothing for StallTimeout is let go: the
		// write fails and the connection is closed.
		_ = rc.SetWriteDeadline(time.Now().Add(StallTimeout))
		_, err := w.Write(b)
		return err
	}
	// The SHA-256 is that of the whole instance, so only an answer of the
	// whole instance can be checked against it; a range or a window is
	// checked for its length, and the client checks what it assembles
	// against the ETag.
	var sum *tenant.SHA256
	if status == http.StatusOK && !win.given {
		s := in.ArchivedSHA256
		sum = &s
	}
	chunk := min(max(n, 1), httpChunk)
	bufs := [2][]byte{make([]byte, chunk), make([]byte, chunk)}
	var got int64
	if s, ok := body.(fileSender); ok && sum == nil && sendsFiles(r) && n > 2*httpChunk && s.Remaining() >= n {
		got, err = sendRange(ctx, s, w, n, func() {
			if !begun {
				head()
				begun = true
			}
			_ = rc.SetWriteDeadline(time.Now().Add(StallTimeout))
		})
		if err == nil {
			var rest int64
			rest, err = checkedread.CopyChecked(body, n-got, nil, bufs, emit)
			got += rest
		}
	} else {
		got, err = checkedread.CopyChecked(body, n, sum, bufs, emit)
	}
	refusal, refused := refusedWhileOpen(ctx)
	switch {
	case err == nil:
		if !begun {
			head()
		}
		c.record(d, st, master.OutcomeDone, "", sent(got))
		return nil
	case !begun && refused:
		// Confirm recorded the refusal.
		return c.toAPI(d, refusal)
	case !begun:
		return c.failWith(d, st, sent(0), instanceFailure(err, path))
	case !refused:
		// A refusal while open was recorded by Confirm.
		reason := "the download ended before its last byte: the client went away or stalled, or the server stopped"
		if checkedread.Mismatched(err) || isFault(err) {
			reason = fault.Message(instanceFailure(err, path))
		}
		c.record(d, st, master.OutcomeFailed, reason, sent(got))
	}
	if checkedread.Mismatched(err) {
		c.log.Error("api: a download was cut: an instance's bytes on its storage do not match its manifest",
			"request_id", w.Header().Get(RequestIDHeader), "scan", path.Subject+"/"+path.Session+"/"+path.Scan, "error", err.Error())
	}
	// The answer has begun: its status and length are sent, so nothing
	// else can be said. net/http closes the connection, or resets the
	// HTTP/2 stream, before the last byte, and the client sees a body
	// short of its Content-Length.
	reason := "the download ended before its last byte: the client went away or stalled, or the server stopped"
	if refused {
		reason = fault.Message(refusal)
		err = refusal
	} else if checkedread.Mismatched(err) || isFault(err) {
		err = instanceFailure(err, path)
	}
	abortAnswer(err, reason)
	return nil
}

// abortedAnswer is the panic that aborts an answer already begun: it is
// net/http's http.ErrAbortHandler, so every recover lets it through and
// net/http closes the connection, and it carries why the answer was cut,
// so the server's log line of the request names the failure and never
// reads as a success.
type abortedAnswer struct{ cause error }

// Error names the cause.
func (a abortedAnswer) Error() string { return "the answer was cut: " + a.cause.Error() }

// Unwrap returns http.ErrAbortHandler and the cause, so errors.Is finds
// the one and fault.KindOf the other.
func (a abortedAnswer) Unwrap() []error { return []error{http.ErrAbortHandler, a.cause} }

// abortAnswer aborts an answer already begun, its cause err: a fault as it
// is, as is the end of the request's context; any other error (a write to
// a client that went away or stalled) is Unavailable with reason, the
// message the audit entry gives.
func abortAnswer(err error, reason string) {
	if !isFault(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		err = fault.Wrap(fault.Unavailable, err, reason)
	}
	panic(abortedAnswer{cause: err})
}

// fileSender is a storage's stream that can write its next bytes to a
// writer itself (the directory driver's, from its open file), so that
// net/http, given a file, has the system copy it to the socket.
type fileSender interface {
	// Remaining returns how many bytes the stream has not yet given.
	Remaining() int64
	// SendTo writes the next n bytes, or the rest when fewer are left,
	// to w and returns how many it wrote.
	SendTo(w io.Writer, n int64) (int64, error)
}

// sendPiece is the most bytes one SendTo of a download writes: the stall
// deadline and the key's recheck are looked at between pieces, so a piece
// must go in well under StallTimeout to a client that reads at all.
const sendPiece = 8 << 20

// sendsFiles reports whether net/http writes the answer to r straight to
// its connection, which a file can then reach by sendfile: HTTP/1.1
// without TLS (the local socket, or a loopback listener behind a proxy).
// TLS encrypts the bytes in this process and HTTP/2 frames them, so
// neither can take a file.
func sendsFiles(r *http.Request) bool {
	return r.ProtoMajor == 1 && r.TLS == nil
}

// sendRange writes all but the last httpChunk bytes of a range of n bytes
// from s to w, in pieces of sendPiece, calling before ahead of each, and
// returns how many it wrote. The last piece is left to checkedread.CopyChecked, so a
// range is never sent whole before its length is proven; a stream that
// gives fewer bytes than asked is checkedread.ErrShort. Only a range or a window is
// sent this way: a whole instance is checked against its SHA-256, which
// needs its bytes in this process.
func sendRange(ctx context.Context, s fileSender, w io.Writer, n int64, before func()) (int64, error) {
	bulk := n - httpChunk
	var sent int64
	for sent < bulk {
		if err := ctx.Err(); err != nil {
			return sent, context.Cause(ctx)
		}
		before()
		want := min(bulk-sent, sendPiece)
		k, err := s.SendTo(w, want)
		sent += k
		switch {
		case err != nil:
			return sent, err
		case k < want:
			return sent, checkedread.ErrShort
		}
	}
	return sent, nil
}

// unsatisfiable answers 416 with the length of the body every range is
// counted in (RFC 9110 15.5.17).
func unsatisfiable(w http.ResponseWriter, size int64) {
	w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
}

// downloadInstance returns the instance a download's path names, with its
// scan's labels: each label must pass the label rule and the id the rule
// of a UID, and the instance is looked up by its id in the scan with those
// labels, compared byte for byte, among the instances the tenant holds
// archived and confirmed. Anything else is NotFound, with one message.
func downloadInstance(ctx context.Context, p tenant.Pipeline, r *http.Request) (tenant.ScanInstance, label.Path, error) {
	path := label.Path{Subject: r.PathValue("subject"), Session: r.PathValue("session"), Scan: r.PathValue("scan")}
	id := r.PathValue("instance")
	if path.Check() != nil || tenant.ValidateInstanceUID(id) != nil {
		return tenant.ScanInstance{}, path, fault.New(fault.NotFound, messageNoDownload)
	}
	in, err := p.HeldInstance(ctx, path, id)
	if fault.KindOf(err) == fault.NotFound {
		return in, path, fault.Wrap(fault.NotFound, err, messageNoDownload)
	}
	return in, path, err
}
