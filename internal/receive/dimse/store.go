package dimse

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/receive"
)

// errNotServed answers every service the receiver does not provide. Only
// Verification and the receiver's Storage SOP classes are negotiated, so a
// request for another service can only come on one of their contexts, and
// is a request the receiver does not serve.
var errNotServed = errors.New("the receiver serves only C-ECHO and C-STORE")

// errAnswered ends the writing of a file whose answer the Sink has already
// given without reading it to its end: refused, or past the receiver's
// largest. The library then reads the rest of the data set and drops it.
var errAnswered = errors.New("the store was answered before its data set was read")

// errWriterPanic is what the Sink reads when writing the file panicked: an
// error, so the file is not kept.
var errWriterPanic = errors.New("writing the received file panicked")

// handler is the receiver's network.Handler: C-ECHO and C-STORE (as a
// stream, CStoreStreamer), and a refusal for everything else. It does not
// embed network.BaseHandler, whose C-STORE answers success without storing
// anything.
type handler struct{ l *Listener }

var (
	_ network.Handler        = (*handler)(nil)
	_ network.CStoreStreamer = (*handler)(nil)
)

// HandleCEcho answers success: the caller was admitted, which is all C-ECHO
// verifies (PS3.4 A.4).
func (h *handler) HandleCEcho(_ context.Context, req *network.CEchoRequest) (*network.CEchoResponse, error) {
	return &network.CEchoResponse{MessageIDRespondedTo: req.MessageID, AffectedSOPClass: req.AffectedSOPClass,
		Status: network.StatusSuccess}, nil
}

// HandleCStoreStream takes one C-STORE: the data set's bytes are written as
// a Part 10 file (network.WriteCStoreFile: the library's file meta
// information, then the bytes untouched) into a pipe the Sink reads, so the
// file streams to the incoming area in bounded memory, and the answer is
// the Sink's (statusOf). A data set that does not arrive whole ends the
// pipe with the library's error, which the Sink refuses; the library then
// sends no answer at all, the association being over.
func (h *handler) HandleCStoreStream(ctx context.Context, req *network.CStoreRequest, transferSyntax string,
	data io.Reader) (resp *network.CStoreResponse, err error) {
	l := h.l
	answer := func(status uint16) *network.CStoreResponse {
		return &network.CStoreResponse{MessageIDRespondedTo: req.MessageID, AffectedSOPClass: req.AffectedSOPClass,
			AffectedSOPInstance: req.AffectedSOPInstance, Status: status}
	}
	defer func() {
		// A panic here would otherwise end the association through the
		// library's recovery with no answer; it is answered out of
		// resources instead, which tells the sender to keep its copy, and
		// nothing was answered success.
		if p := recover(); p != nil {
			// The phrase only: a panic's value may quote what the file held,
			// a UID or a name, which no log line carries.
			l.log.Error("receiver: a store panicked; it is answered out of resources")
			resp, err = answer(network.StatusOutOfResources), nil
		}
	}()
	if !l.enter() {
		return answer(network.StatusOutOfResources), nil
	}
	defer l.stores.Done()
	if l.cfg.Refusal() != "" {
		// The receiver's health and its one event say why; a store is not
		// an event of its own.
		return answer(network.StatusOutOfResources), nil
	}
	pr, pw := io.Pipe()
	// Closed on every way out, a panic included, so the writer is never
	// left blocked on a reader that is gone.
	defer func() { _ = pr.CloseWithError(errAnswered) }()
	wrote := make(chan error, 1)
	go func() {
		var werr error
		defer func() {
			if p := recover(); p != nil {
				werr = errWriterPanic
			}
			// A nil error is the end of the file to the reader; anything
			// else is a failure it must not keep.
			_ = pw.CloseWithError(werr)
			wrote <- werr
		}()
		_, werr = network.WriteCStoreFile(ctx, pw, req, transferSyntax, data)
	}()
	info := network.AssociationInfoFromContext(ctx)
	a := receive.Arrival{
		Receiver: l.cfg.Receiver.Name, SOPClassUID: req.AffectedSOPClass, SOPInstanceUID: req.AffectedSOPInstance,
		TransferSyntaxUID: transferSyntax, Body: pr,
	}
	if info != nil {
		a.Caller = callerOf(strings.TrimSpace(info.CallingAE), strings.TrimSpace(info.CalledAE), info.RemoteAddr)
		if info.ID != 0 {
			a.Association = l.associationID(info.ID)
		}
	}
	if l.seams.beforeAccept != nil {
		l.seams.beforeAccept()
	}
	out := l.cfg.Sink.Accept(ctx, a)
	_ = pr.CloseWithError(errAnswered)
	werr := <-wrote
	return answer(statusOf(out, werr)), nil
}

// statusOf returns the C-STORE status for the Sink's outcome and the
// writer's error (PS3.4 B.2.3, Table B.2-1):
//
//   - a command that cannot be written as a file: 0117H for an invalid SOP
//     Instance UID (PS3.7 C.4.13, "Invalid SOP Instance"), C000H, "cannot
//     understand", for another invalid value; nothing was kept;
//   - received: 0000H, success, given only when the Sink kept the file
//     durably and the writer ended cleanly, which the Sink's reading to
//     the end implies; a writer that did not is answered as a failure;
//   - refused or failed: A700H, out of resources. Refused is a temporary
//     failure; failed is a file past the receiver's largest, or a receiver
//     that no longer takes files, which the contract also answers A700H
//     (receiver.proto, max_file_bytes) so the sender keeps its copy.
//
// A writer that ended in another error read a data set that did not arrive
// whole; the library sends no answer then, whatever is returned.
func statusOf(out receive.Outcome, werr error) uint16 {
	switch {
	case errors.Is(werr, network.ErrInvalidSOPInstanceUID):
		return network.StatusInvalidObjectInstance
	case errors.Is(werr, network.ErrInvalidStoreCommand):
		return network.StatusUnableToProcess
	case out.Status == receive.Received && werr == nil:
		return network.StatusSuccess
	}
	return network.StatusOutOfResources
}

// HandleCStore is never called: the handler takes every C-STORE as a
// stream. It answers failure, never success, should the library call it.
func (h *handler) HandleCStore(_ context.Context, req *network.CStoreRequest) (*network.CStoreResponse, error) {
	return &network.CStoreResponse{MessageIDRespondedTo: req.MessageID, AffectedSOPClass: req.AffectedSOPClass,
		AffectedSOPInstance: req.AffectedSOPInstance, Status: network.StatusUnableToProcess}, nil
}

// HandleCFind refuses: no query is served.
func (h *handler) HandleCFind(context.Context, *network.CFindRequest) ([]*network.CFindResponse, error) {
	return nil, errNotServed
}

// HandleCMove refuses: no retrieve is served.
func (h *handler) HandleCMove(context.Context, *network.CMoveRequest) (*network.CMoveResponse, error) {
	return nil, errNotServed
}

// HandleCGet refuses: no retrieve is served.
func (h *handler) HandleCGet(context.Context, *network.CGetRequest) (*network.CGetResponse, error) {
	return nil, errNotServed
}

// HandleNEventReport refuses: no N-service is served.
func (h *handler) HandleNEventReport(context.Context, *network.NEventReportRequest) (*network.NEventReportResponse, error) {
	return nil, errNotServed
}

// HandleNGet refuses.
func (h *handler) HandleNGet(context.Context, *network.NGetRequest) (*network.NGetResponse, error) {
	return nil, errNotServed
}

// HandleNSet refuses.
func (h *handler) HandleNSet(context.Context, *network.NSetRequest) (*network.NSetResponse, error) {
	return nil, errNotServed
}

// HandleNAction refuses: no storage commitment is served.
func (h *handler) HandleNAction(context.Context, *network.NActionRequest) (*network.NActionResponse, error) {
	return nil, errNotServed
}

// HandleNCreate refuses.
func (h *handler) HandleNCreate(context.Context, *network.NCreateRequest) (*network.NCreateResponse, error) {
	return nil, errNotServed
}

// HandleNDelete refuses.
func (h *handler) HandleNDelete(context.Context, *network.NDeleteRequest) (*network.NDeleteResponse, error) {
	return nil, errNotServed
}
