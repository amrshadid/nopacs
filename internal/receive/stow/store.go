package stow

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The Failure Reasons of a refused part (PS3.18 Table I.2-2).
const (
	// ReasonOutOfResources (A700H): the archive cannot take the part now,
	// or it is larger than the receiver's largest file; the client keeps
	// its copy and may send it again.
	ReasonOutOfResources = 0xA700
	// ReasonDoesNotMatch (A900H): the part is of another study than the
	// request stores into.
	ReasonDoesNotMatch = 0xA900
	// ReasonCannotUnderstand (C000H): the part is not a DICOM file whose
	// four placing UIDs can be read and are well formed.
	ReasonCannotUnderstand = 0xC000
	// ReasonProcessingFailure (0110H): the part could not be kept for
	// another reason.
	ReasonProcessingFailure = 0x0110
)

// The phrases of a request refused whole, sent as its answer's text and
// kept as its audit entry's reason: fixed, holding nothing the client sent.
const (
	phraseMediaType   = `instances are stored as multipart/related; type="application/dicom"`
	phraseMetadata    = `instances are stored as multipart/related; type="application/dicom": metadata with its bulk data in separate parts (application/dicom+json) is not taken`
	phraseBoundary    = "the multipart body names no boundary"
	phraseMalformed   = "the request body is not well-formed multipart"
	phraseNoParts     = "the request holds no instances"
	phraseTooLarge    = "the request is larger than the receiver's largest request; the instances it held whole before that are kept"
	phraseTooMany     = "the request holds more than 10000 parts; the instances before that are kept, and the rest are sent in another request"
	phrasePartHeader  = "a part's header is larger than 16 KiB"
	phraseCutPart     = "the request body ended inside a part; that part is not kept, and the instances held whole before it are"
	phraseUnavailable = "the archive cannot take instances now; send them again later"
	phraseStopping    = "the receiver is stopping; send the instances again"
	phraseStalled     = "the request body stopped arriving"
)

// Request is what the route tells Serve of a request its check allowed.
type Request struct {
	// Study is the Study Instance UID the path names, well formed, or ""
	// for a request to the service's root (PS3.18 10.5.1).
	Study string
	// Base is the URL of the tenant's DICOMweb root as the client reached
	// it, which each Retrieve URL (0008,1190) starts with.
	Base string
	// Caller is who sent it, as the received files record it: its address.
	Caller tenant.Caller
	// Association is an id of the request (master.ValidateID's form), which
	// each received file records, empty for none.
	Association string
	// Refused reports that the caller's key or grant may no longer store,
	// found while the request was open; nil never refuses. It is asked when
	// the request's context ends before its body was read whole: Serve then
	// writes no answer and returns a Result marked Unanswered, so the route
	// answers as it answers a new request with that key.
	Refused func() bool
}

// Result is what became of a request, for its audit entry.
type Result struct {
	// Status is the HTTP status of the answer written.
	Status int
	// Stored and Refused count the parts: listed in the answer as stored,
	// and refused whether listed or not.
	Stored, Refused int
	// Reason is the phrase of a request refused or cut short as a whole,
	// "" for one answered part by part.
	Reason string
	// Unanswered says Serve wrote nothing, since the caller was refused
	// while the request was open (Request.Refused): the parts stored
	// before are kept and counted, and the route writes the refusal.
	Unanswered bool
}

// outcome is what became of one part.
type outcome struct {
	class, instance, study, series string
	// reason is 0 for a part stored.
	reason uint16
	// listed says the part has the UIDs an item of the answer needs.
	listed bool
	// overQuota says the part was refused because the tenant is at its
	// storage quota (receive.ReasonOverQuota).
	overQuota bool
}

// tally is what became of a request's parts: the outcomes the answer
// lists, and a count of every part, so a part with nothing to list costs
// the request no memory.
type tally struct {
	listed          []outcome
	stored, refused int
	// overQuota counts the parts refused for the tenant's quota.
	overQuota int
}

// add counts one part's outcome and keeps it when it is listed.
func (t *tally) add(o outcome) {
	if o.reason == 0 {
		t.stored++
	} else {
		t.refused++
	}
	if o.overQuota {
		t.overQuota++
	}
	if o.listed {
		t.listed = append(t.listed, o)
	}
}

// parts returns how many parts were counted.
func (t *tally) parts() int { return t.stored + t.refused }

// Serve reads a STOW-RS request (PS3.18 10.5.1) part by part, hands each
// part it can place to the Sink, and writes the answer (PS3.18 10.5.3): 200
// when every part was stored, 202 when some were, 409 when none was (507
// when none was because the tenant is at its storage quota), each
// stored instance with its Retrieve URL and each refused one with its
// Failure Reason. A request refused whole is answered as plain text: 415
// for a body of another media type, 400 for one that is not well-formed
// multipart, holds no part, ends inside a part or has a part's header past
// MaxPartHeader, 413 past the receiver's largest request or MaxParts, 503
// with Retry-After while the archive cannot take files or the endpoint
// stops, 406 when the client takes no JSON answer. The parts stored
// before a request is refused part way are kept and counted in its
// Result; the plain answer does not list them (PS3.18 10.5.3 gives a
// Store Instances Response Module to an answer part by part only), and a
// client finds them with a search (PS3.18 10.6). A request whose caller is
// refused while it is open (Request.Refused) ends at its next read of the
// body, keeping the parts stored before, and is left for the route to
// answer.
func (e *Endpoint) Serve(ctx context.Context, w http.ResponseWriter, r *http.Request, req Request) Result {
	rc := http.NewResponseController(w)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f := &inFlight{cancel: cancel, deadline: func() { _ = rc.SetReadDeadline(time.Now()) }, refusedBy: req.Refused}
	if !e.enter(f) {
		return stopping(w)
	}
	defer e.leave(f)
	answerType, aerr := dicomweb.AcceptJSON(r)
	if aerr != nil {
		return plain(w, aerr.Status, aerr.Message)
	}
	boundary, refusal, why := multipartBoundary(r.Header.Get("Content-Type"))
	if why != "" {
		return plain(w, refusal, why)
	}
	if failure := e.cfg.Refusal(); failure != "" {
		e.cfg.Report(failure, groupCaller(req.Caller))
		w.Header().Set("Retry-After", "60")
		return plain(w, http.StatusServiceUnavailable, phraseUnavailable)
	}

	stall := e.cfg.ReadStall
	body := &stallReader{r: http.MaxBytesReader(w, r.Body, e.cfg.Receiver.STOW.MaxRequestBytes), ctx: ctx, cut: &f.stopped,
		extend: func() { _ = rc.SetReadDeadline(time.Now().Add(stall)) }}
	// A body read to its end lifts the stall deadline, so the connection's
	// next request does not inherit it. A body left unread (a failure, a
	// refusal, a cut) keeps a deadline that has passed instead: net/http
	// reads up to 256 KiB of what is left of a body after the handler
	// returns, whatever the answer's Connection: close says, and without a
	// deadline a client that keeps its connection open and sends nothing
	// more would hold that read, and the connection, for ever.
	unread := false
	defer func() {
		if unread {
			_ = rc.SetReadDeadline(time.Now())
			return
		}
		_ = rc.SetReadDeadline(time.Time{})
	}()
	bound := &headerBound{r: body}
	mr := multipart.NewReader(bound, boundary)
	var t tally
	var prefix prefixBuffer
	for {
		bound.startHeader()
		part, err := mr.NextRawPart()
		bound.endHeader()
		if errors.Is(err, io.EOF) {
			break
		}
		switch {
		case err != nil:
		case headerSize(part.Header) > MaxPartHeader:
			err = errPartHeader
		case t.parts() == MaxParts:
			err = errTooManyParts
		default:
			var o outcome
			o, err = e.storePart(ctx, part, req, &prefix)
			_ = part.Close()
			if err == nil {
				t.add(o)
				continue
			}
		}
		unread = true
		res := e.bodyFailed(ctx, w, f, err)
		res.Stored, res.Refused = t.stored, t.refused
		return res
	}
	if t.parts() == 0 {
		return plain(w, http.StatusBadRequest, phraseNoParts)
	}
	stored, refused := t.stored, t.refused
	// PS3.18 10.5.3, Table 10.5.3-1. A request none of whose parts was
	// stored because the tenant is at its storage quota is answered 507
	// (RFC 4918 11.5, Insufficient Storage): the archive has no room for
	// the tenant now, a temporary state the client waits out, as each
	// part's A700H says too.
	status := http.StatusOK
	switch {
	case stored == 0 && t.overQuota > 0:
		status = http.StatusInsufficientStorage
	case stored == 0:
		status = http.StatusConflict
	case refused > 0:
		status = http.StatusAccepted
	}
	// A receiver that de-identifies answers no Retrieve URL: the archive
	// holds each instance under its remapped UIDs only, and a URL naming
	// them would hand the sender the mapping from the UIDs it sent to the
	// archive's, which no answer of the API gives.
	payload, err := answer(req.Base, req.Study, t.listed, !e.cfg.Receiver.Deidentifies())
	if err != nil {
		e.log.Error("receiver: the answer could not be written", "stored", stored, "refused", refused)
		return Result{Status: http.StatusInternalServerError, Stored: stored, Refused: refused, Reason: "the answer could not be written"}
	}
	h := w.Header()
	h.Set("Content-Type", answerType)
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(payload) // #nosec G705 -- DICOM JSON written by go-dicom, with its media type and nosniff.
	return Result{Status: status, Stored: stored, Refused: refused}
}

// multipartBoundary reads the request's media type (PS3.18 10.5.1.1): it
// must be multipart/related with a boundary, of type application/dicom or
// none named. It returns the boundary, or the status and phrase of the
// refusal.
func multipartBoundary(contentType string) (string, int, string) {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(media, dicomweb.MediaMultipart) {
		return "", http.StatusUnsupportedMediaType, phraseMediaType
	}
	switch kind := params["type"]; {
	case kind == "" || strings.EqualFold(kind, dicomweb.MediaDICOM):
	case strings.EqualFold(kind, dicomweb.MediaDICOMJSON) || strings.EqualFold(kind, "application/dicom+xml"):
		return "", http.StatusUnsupportedMediaType, phraseMetadata
	default:
		return "", http.StatusUnsupportedMediaType, phraseMediaType
	}
	if params["boundary"] == "" {
		return "", http.StatusBadRequest, phraseBoundary
	}
	return params["boundary"], 0, ""
}

// errTooManyParts is a request with a part past MaxParts.
var errTooManyParts = errors.New("stow: the request holds too many parts")

// bodyFailed answers a request whose body could not be read to its end,
// or that was refused part way. The parts stored before are kept: they are
// durable and owed, and the pipeline archives them as any other.
func (e *Endpoint) bodyFailed(ctx context.Context, w http.ResponseWriter, f *inFlight, err error) Result {
	// The rest of the body is not read, so the connection is not used again
	// (RFC 9112 9.6): without this the server reads what is left before it
	// answers, and a client that sends nothing more, or a cut one whose
	// deadline is lifted, would never be answered.
	w.Header().Set("Connection", "close")
	var tooLarge *http.MaxBytesError
	switch {
	case ctx.Err() != nil && f.refused():
		// Checked first: the caller's own refusal is the answer a new
		// request would get, whatever read it ended.
		return Result{Unanswered: true}
	case f.stopped.Load() || errors.Is(err, errCut):
		// Checked first: the cut ends the reads with a deadline, which
		// would otherwise be taken for a stalled client.
		return stopping(w)
	case errors.As(err, &tooLarge):
		return plain(w, http.StatusRequestEntityTooLarge, phraseTooLarge)
	case errors.Is(err, errTooManyParts):
		return plain(w, http.StatusRequestEntityTooLarge, phraseTooMany)
	case errors.Is(err, errPartHeader):
		return plain(w, http.StatusBadRequest, phrasePartHeader)
	case errors.Is(err, errStalled):
		// Checked before the context, which the server cancels once the
		// connection's read has failed.
		return plain(w, http.StatusRequestTimeout, phraseStalled)
	case ctx.Err() != nil:
		return stopping(w)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return plain(w, http.StatusBadRequest, phraseCutPart)
	}
	return plain(w, http.StatusBadRequest, phraseMalformed)
}

// stopping refuses a request because the receiver stops: 503 with
// Retry-After (RFC 9110 10.2.3), since another receiver, or this one
// started again, takes the instances.
func stopping(w http.ResponseWriter) Result {
	w.Header().Set("Retry-After", "60")
	return plain(w, http.StatusServiceUnavailable, phraseStopping)
}

// storePart decides one part: refused before anything of it is kept, or
// handed to the Sink, whose answer is the part's. It returns an error only
// when the request's body could not be read, which ends the request.
func (e *Endpoint) storePart(ctx context.Context, part *multipart.Part, req Request, buf *prefixBuffer) (outcome, error) {
	if ct := part.Header.Get("Content-Type"); ct != "" {
		media, _, err := mime.ParseMediaType(ct)
		if err != nil || !strings.EqualFold(media, dicomweb.MediaDICOM) {
			// Not a file at all: nothing to list it by.
			return outcome{reason: ReasonCannotUnderstand}, nil
		}
	}
	limit := e.cfg.Receiver.MaxFileBytes
	body := &countingReader{r: part}
	prefix, whole, err := buf.read(body, limit)
	if err != nil {
		return outcome{}, err
	}
	id, idErr := header.Identify(prefix, whole)
	o := outcome{class: id.SOPClassUID, instance: id.SOPInstanceUID, study: id.StudyUID, series: id.SeriesUID}
	o.listed = idErr == nil && uid.Valid(o.class) && uid.Valid(o.instance)
	over := body.n > limit
	switch {
	case over:
		// Handed on all the same: the pipeline refuses it for its size and
		// records that it came, as for a C-STORE too large.
	case idErr != nil || !id.Complete || !placeable(id):
		o.reason = ReasonCannotUnderstand
		return o, nil
	case req.Study != "" && id.StudyUID != req.Study:
		o.reason = ReasonDoesNotMatch
		return o, nil
	}
	a := receive.Arrival{
		Receiver: e.cfg.Receiver.Name, Caller: req.Caller, Association: req.Association,
		Body: io.MultiReader(bytes.NewReader(prefix), body),
	}
	if o.listed {
		a.SOPClassUID, a.SOPInstanceUID, a.TransferSyntaxUID = o.class, o.instance, id.TransferSyntaxUID
	}
	if !over {
		// Placed by what the prefix said: the pipeline holds the file if
		// its whole header says otherwise (receive.Arrival).
		a.StudyUID, a.SeriesUID = o.study, o.series
	}
	if err := ctx.Err(); err != nil {
		// Cut by Stop or by a refusal of the caller while the part was
		// read: nothing of it is handed on.
		return outcome{}, err
	}
	out := e.cfg.Sink.Accept(ctx, a)
	if body.err != nil && !errors.Is(body.err, io.EOF) {
		// The body failed under the Sink, which then kept nothing of the
		// part (receive.Sink); the request ends here.
		return outcome{}, body.err
	}
	if err := ctx.Err(); err != nil && out.Status != receive.Received {
		// Cut by Stop or by a refusal of the caller: the request ends. A
		// part the Sink kept before is durable and counted stored; the
		// next read of the body ends the request.
		return outcome{}, err
	}
	switch {
	case out.Status == receive.Received:
	case out.Status == receive.Refused, body.n > limit:
		o.reason = ReasonOutOfResources
		o.overQuota = out.Reason == receive.ReasonOverQuota
	default:
		o.reason = ReasonProcessingFailure
	}
	return o, nil
}

// placeable reports whether a part's identity places it: its four UIDs
// well formed (PS3.5 9.1). A file without them would be quarantined by the
// pipeline; refused here instead, the client is told now.
func placeable(id header.Identity) bool {
	for _, v := range []string{id.SOPClassUID, id.SOPInstanceUID, id.StudyUID, id.SeriesUID} {
		if !uid.Valid(v) {
			return false
		}
	}
	return true
}

// plain writes a refusal of the whole request as plain text and returns its
// Result.
func plain(w http.ResponseWriter, status int, phrase string) Result {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(phrase)+1))
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, phrase+"\n") // #nosec G705 -- a fixed phrase as plain text with nosniff, never HTML.
	return Result{Status: status, Reason: phrase}
}

// groupCaller returns who called as a refused caller is grouped: the host's
// address alone.
func groupCaller(c tenant.Caller) tenant.GroupCaller {
	ap, err := netip.ParseAddrPort(c.Address)
	if err != nil {
		return tenant.GroupCaller{}
	}
	return tenant.GroupCaller{Address: ap.Addr().String()}
}
