package api

import (
	"context"
	"errors"
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The details of the entries of a stream of files and of a download.
const (
	detailFiles = "instances"
	detailBytes = "bytes"
	detailEnded = "ended"
)

// The messages of what ReadFiles finds missing.
const (
	messageNoReadSubject = "the tenant has no subject with this label"
	messageNoReadSession = "the tenant has no session with these labels"
)

// dataService is DataService: the archived bytes of a tenant's instances,
// streamed with each instance's place, length and SHA-256 from its scan's
// manifest, so a client checks each as it reads it.
type dataService struct{ c *core }

var _ nopacsv1connect.DataServiceHandler = dataService{}

// position is a place in the order of ReadFiles: the folded labels of a
// session and a scan, and an instance id, compared in that order, each in
// byte order (data.proto, ReadFiles).
type position struct {
	session, scan, instance string
}

// readQuery is a ReadFilesRequest, checked.
type readQuery struct {
	subject, session, scan, instance string
	// after is the position to resume after, or nil.
	after *position
	// announceOnly sends the markers and announcements alone.
	announceOnly bool
}

// labelField returns an Invalid error naming field unless s is a label.
func labelField(field, s string) error {
	if label.Check(s) != nil {
		return invalid(field, "is a label: 1 to 128 of A-Z, a-z, 0-9, \".\", \"_\" and \"-\"")
	}
	return nil
}

// readQueryOf checks a request: a subject, each level below given only
// with the levels above it, each label a label, an instance id a UID, and
// a start_after that names a position inside what is asked for.
func readQueryOf(r *v1.ReadFilesRequest) (readQuery, error) {
	q := readQuery{subject: r.GetSubjectLabel(), session: r.GetSessionLabel(), scan: r.GetScanLabel(),
		instance: r.GetInstanceId(), announceOnly: r.GetAnnounceOnly()}
	if q.subject == "" {
		return q, invalid("subject_label", "is required")
	}
	if err := labelField("subject_label", q.subject); err != nil {
		return q, err
	}
	if q.session != "" {
		if err := labelField("session_label", q.session); err != nil {
			return q, err
		}
	}
	if q.scan != "" {
		if q.session == "" {
			return q, invalid("scan_label", "is given only with session_label")
		}
		if err := labelField("scan_label", q.scan); err != nil {
			return q, err
		}
	}
	if q.instance != "" {
		if q.scan == "" {
			return q, invalid("instance_id", "is given only with scan_label")
		}
		if err := tenant.ValidateInstanceUID(q.instance); err != nil {
			return q, fieldOf("instance_id", err)
		}
	}
	if sa := r.GetStartAfter(); sa != nil {
		pos, err := positionOf(q, sa)
		if err != nil {
			return q, err
		}
		q.after = &pos
	}
	return q, nil
}

// positionOf checks start_after against the query q: each label it gives
// is the level q names or, where q names none, a label of its own, which
// it must then give; and an instance id is always given.
func positionOf(q readQuery, sa *v1.ReadPosition) (position, error) {
	level := func(field, got, named string) (string, error) {
		switch {
		case named != "" && got != "" && got != named:
			return "", invalid(field, "is empty or the level the request names")
		case named != "":
			return named, nil
		case got == "":
			return "", invalid(field, "is required when the request names no such level")
		}
		return got, labelField(field, got)
	}
	session, err := level("start_after.session_label", sa.GetSessionLabel(), q.session)
	if err != nil {
		return position{}, err
	}
	scan, err := level("start_after.scan_label", sa.GetScanLabel(), q.scan)
	if err != nil {
		return position{}, err
	}
	if sa.GetInstanceId() == "" {
		return position{}, invalid("start_after.instance_id", "is required")
	}
	if err := tenant.ValidateInstanceUID(sa.GetInstanceId()); err != nil {
		return position{}, fieldOf("start_after.instance_id", err)
	}
	return position{session: label.Fold(session), scan: label.Fold(scan), instance: sa.GetInstanceId()}, nil
}

// exists answers NotFound unless the tenant holds the deepest level the
// query names.
func (q readQuery) exists(ctx context.Context, p tenant.Pipeline) error {
	switch {
	case q.instance != "":
		_, err := p.HeldInstance(ctx, label.Path{Subject: q.subject, Session: q.session, Scan: q.scan}, q.instance)
		if fault.KindOf(err) == fault.NotFound {
			// The scan's own absence first, so the message says which.
			if _, serr := lookupScan(ctx, p, label.Path{Subject: q.subject, Session: q.session, Scan: q.scan}); serr != nil {
				return serr
			}
			return fault.Wrap(fault.NotFound, err, messageNoInstance)
		}
		return err
	case q.scan != "":
		_, err := lookupScan(ctx, p, label.Path{Subject: q.subject, Session: q.session, Scan: q.scan})
		return err
	case q.session != "":
		_, err := p.CatalogSession(ctx, q.subject, q.session)
		if fault.KindOf(err) == fault.NotFound {
			return relabeled(ctx, p, label.Path{Subject: q.subject, Session: q.session}, fault.Wrap(fault.NotFound, err, messageNoReadSession))
		}
		return err
	}
	_, err := p.CatalogSubject(ctx, q.subject)
	if fault.KindOf(err) == fault.NotFound {
		return relabeled(ctx, p, label.Path{Subject: q.subject}, fault.Wrap(fault.NotFound, err, messageNoReadSubject))
	}
	return err
}

// ReadFiles streams the instances of an instance, a scan, a session or a
// subject, as the contract says: each scan's marker, then each instance's
// announcement and its bytes, checked against its manifest as they are
// sent; resumed after a position; the caller's key checked again while it
// is open; one audit entry when it opens and one when it ends.
func (s dataService) ReadFiles(ctx context.Context, req *v1.ReadFilesRequest, stream nopacsv1connect.DataServiceReadFilesServerStream) error {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.DataServiceReadFilesProcedure)
	if err != nil {
		return err
	}
	details := readFilesDetails(req)
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), details)
	if err != nil {
		return err
	}
	q, err := readQueryOf(req)
	if err != nil {
		return c.failIn(d, st, err)
	}
	release, err := c.reads.Take(ownerOf(d))
	if err != nil {
		return c.failIn(d, st, err)
	}
	defer release()
	if err := q.exists(ctx, p); err != nil {
		return c.failIn(d, st, err)
	}
	ctx, stop := c.watchKey(ctx, d)
	defer stop()
	stores := c.newReadStores(p, req.GetTenant())
	defer stores.Close()

	c.record(d, st, master.OutcomeDone, "", access.Facts{Details: withDetails(details, map[string]string{detailStream: "opened"})})
	fr := &fileReader{p: p, q: q, stores: stores, send: stream.Send}
	if !q.announceOnly {
		fr.bufs = [2][]byte{make([]byte, ChunkSize), make([]byte, ChunkSize)}
	}
	err = fr.run(ctx)
	end := func(how string) access.Facts {
		return access.Facts{Details: withDetails(details, map[string]string{detailStream: "ended", detailEnded: how,
			detailFiles: strconv.FormatInt(fr.files, 10), detailBytes: strconv.FormatInt(fr.bytes, 10)})}
	}
	if refusal, ok := refusedWhileOpen(ctx); ok {
		// Confirm recorded the refusal.
		return c.toAPI(d, refusal)
	}
	switch {
	case err == nil:
		c.record(d, st, master.OutcomeDone, "", end("complete"))
		return nil
	case ctx.Err() != nil:
		// The client went away, or the server is stopping: the stream ends
		// as the contract says a stream may, and is resumed by the client.
		c.record(d, st, master.OutcomeFailed, "the stream ended before its last instance: the client went away or the server stopped", end("canceled"))
		return ctx.Err()
	}
	return c.failWith(d, st, end(strings.ReplaceAll(fault.KindOf(err).String(), " ", "_")), err)
}

// fileReader walks what a ReadFiles request names, in the contract's order,
// and sends each message.
type fileReader struct {
	p      tenant.Pipeline
	q      readQuery
	stores *checkedread.Stores
	send   func(*v1.ReadFilesResponse) error
	// bufs are the two buffers of checkedread.CopyChecked; nil with announce_only.
	bufs [2][]byte
	// files and bytes count the instances sent whole and their bytes.
	files, bytes int64
}

// run sends the subject's sessions, or the one session named, in order of
// their folded labels.
func (r *fileReader) run(ctx context.Context) error {
	if r.q.session != "" {
		return r.session(ctx, r.q.session)
	}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		got, err := r.p.ListSessions(ctx, tenant.SessionFilter{SubjectLabel: r.q.subject, Order: tenant.OrderLabel}, page)
		if err != nil {
			return err
		}
		for _, se := range got.Items {
			if r.q.after != nil && label.Fold(se.Label) < r.q.after.session {
				continue
			}
			if err := r.session(ctx, se.Label); err != nil {
				return err
			}
		}
		if got.Next == "" {
			return nil
		}
		page.Cursor = got.Next
	}
}

// session sends a session's scans, or the one scan named, in order of
// their folded labels.
func (r *fileReader) session(ctx context.Context, session string) error {
	if r.q.scan != "" {
		scan, err := lookupScan(ctx, r.p, label.Path{Subject: r.q.subject, Session: session, Scan: r.q.scan})
		if err != nil {
			return err
		}
		return r.scan(ctx, scan)
	}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		got, err := r.p.ListScans(ctx, tenant.ScanFilter{SubjectLabel: r.q.subject, SessionLabel: session, Order: tenant.OrderLabel}, page)
		if err != nil {
			return err
		}
		for _, sc := range got.Items {
			if a := r.q.after; a != nil && label.Fold(sc.SessionLabel) == a.session && label.Fold(sc.Label) < a.scan {
				continue
			}
			if err := r.scan(ctx, sc); err != nil {
				return err
			}
		}
		if got.Next == "" {
			return nil
		}
		page.Cursor = got.Next
	}
}

// scan sends a scan's marker and its instances in order of instance id,
// after the position to resume after when it lies in this scan.
func (r *fileReader) scan(ctx context.Context, sc tenant.CatalogScan) error {
	path := label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
	after := ""
	if a := r.q.after; a != nil && label.Fold(sc.SessionLabel) == a.session && label.Fold(sc.Label) == a.scan {
		after = a.instance
	}
	if err := r.send(&v1.ReadFilesResponse{Part: &v1.ReadFilesResponse_Scan{Scan: &v1.ScanMarker{
		SubjectLabel: sc.SubjectLabel, SessionLabel: sc.SessionLabel, ScanLabel: sc.Label,
		ManifestSha256: sc.ManifestSHA256.String(), InstanceCount: sc.Instances,
	}}}); err != nil {
		return err
	}
	if r.q.instance != "" {
		in, err := r.p.HeldInstance(ctx, path, r.q.instance)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			// No longer held since the stream opened: nothing to send.
			return nil
		case err != nil:
			return err
		case in.SOPInstanceUID <= after:
			return nil
		}
		return r.instance(ctx, path, in)
	}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		held, next, err := r.p.ListHeldInstances(ctx, path, page)
		if err != nil {
			return err
		}
		for _, in := range held {
			if in.SOPInstanceUID <= after {
				continue
			}
			if err := r.instance(ctx, path, in); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// instance sends an instance's announcement and, unless only
// announcements are asked for, its bytes, read from its storage and
// checked against its manifest as they are sent. An instance a scrub
// marked is announced, and the stream then ends as data loss before any
// of its bytes: the announcement gives the client the instance's place,
// so a new call with start_after at it goes on past it, rather than
// stopping at it again.
func (r *fileReader) instance(ctx context.Context, path label.Path, in tenant.ScanInstance) error {
	marked := checkedread.Marked(in)
	if err := r.send(&v1.ReadFilesResponse{Part: &v1.ReadFilesResponse_File{File: &v1.ArchivedFile{
		InstanceId: in.SOPInstanceUID, SopInstanceUid: in.SOPInstanceUID,
		SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan,
		Key: in.Key, Offset: in.Offset, Length: in.Length, Sha256: in.ArchivedSHA256.String(),
	}}}); err != nil {
		return err
	}
	if r.q.announceOnly {
		r.files++
		return nil
	}
	if marked != nil {
		scan := path.Subject + "/" + path.Session + "/" + path.Scan
		return fault.Wrap(fault.DataLoss, marked, "an instance of the scan "+scan+
			" was found missing or damaged on its storage by a scrub; it is announced and none of its bytes is sent: resume after it")
	}
	store, err := r.stores.Get(ctx, in.Storage)
	if err != nil {
		return err
	}
	rc, err := store.Read(ctx, in.Key, in.Offset, in.Length)
	if err != nil {
		return instanceFailure(err, path)
	}
	defer func() { _ = rc.Close() }() // Only read from.
	var offset int64
	sum := in.ArchivedSHA256
	n, err := checkedread.CopyChecked(rc, in.Length, &sum, r.bufs, func(b []byte) error {
		err := r.send(&v1.ReadFilesResponse{Part: &v1.ReadFilesResponse_Chunk{Chunk: &v1.FileChunk{Offset: offset, Data: b}}})
		offset += int64(len(b))
		return err
	})
	r.bytes += n
	if err != nil {
		return instanceFailure(err, path)
	}
	r.files++
	return nil
}

// instanceFailure returns the error of reading an instance's bytes: one
// missing from its storage, or whose bytes are not what its manifest
// records, is data loss, and its message names the scan, never the
// instance, whose id is an identifier as received; a storage that fails
// otherwise is the storage not answering, and the client resumes. An error
// of the stream itself (the client went away) passes as it is.
func instanceFailure(err error, path label.Path) error {
	scan := path.Subject + "/" + path.Session + "/" + path.Scan
	switch {
	case errors.Is(err, blob.ErrNotOnline):
		return notOnline(err, "an instance of the scan "+scan+" cannot be read")
	case fault.KindOf(err) == fault.NotFound:
		return fault.Wrap(fault.DataLoss, err, "an instance of the scan "+scan+" is missing from its storage")
	case errors.Is(err, checkedread.ErrMarked):
		return fault.Wrap(fault.DataLoss, err, "an instance of the scan "+scan+" was found damaged on its storage by a scrub; none of its bytes is sent")
	case checkedread.Mismatched(err):
		return fault.Wrap(fault.DataLoss, err, "an instance of the scan "+scan+" does not match the scan's manifest; the bytes of it already sent are not to be kept")
	case isFault(err):
		return storageFailure(err, "an instance of the scan "+scan+" cannot be read; resume after the last instance received whole")
	}
	return err
}

// reasonNotOnline is the reason of an object its storage holds in a tier
// it cannot read at once (access.proto, the unavailable kind): the
// message starts with it, so a client that reads only the message, a
// plain HTTP download's among them, can tell it from a storage that does
// not answer.
const reasonNotOnline = "NOT_ONLINE"

// notOnline is a read of an object its storage cannot read at once (an S3
// object in an archive storage class, blob.ErrNotOnline): unavailable,
// never not found or data loss, since the bytes are neither missing nor
// damaged; waiting brings nothing back until the object is recalled, so
// the message says so rather than asking for a retry. "not_online" is the
// word the scrub's tally and a job's failure use for the same case.
func notOnline(err error, message string) error {
	return fault.Wrap(fault.Unavailable, err, reasonNotOnline+": "+message+
		": its storage holds it in a tier it cannot read at once (not_online); it is neither missing nor damaged, and reads again once it is back online")
}

// isFault reports whether err carries a fault kind: an error of this
// server's packages, a storage's among them, as opposed to one of the RPC
// library's stream.
func isFault(err error) bool {
	var f *fault.Error
	return errors.As(err, &f)
}
