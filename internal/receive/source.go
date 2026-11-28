package receive

import (
	"context"
	"io"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// Arrival is one file a source hands in: its bytes as they come, and what
// the source was told about it and its sender. Nothing here is trusted: the
// file's own header is what places it, and what the source was told is kept
// only where the file could not be read.
type Arrival struct {
	// Receiver names the receiver the file came to.
	Receiver string
	// Caller is who sent it, as far as the source knows.
	Caller tenant.Caller
	// Association is the source's id of the association or request that
	// brought it (master.ValidateID's form), empty for none.
	Association string
	// SOPClassUID, SOPInstanceUID and TransferSyntaxUID are what the sender
	// said the file is (PS3.7 9.1.1: the C-STORE request's Affected SOP
	// Class and Instance UIDs, and the presentation context's transfer
	// syntax), empty when the source was told nothing.
	SOPClassUID, SOPInstanceUID, TransferSyntaxUID string
	// StudyUID and SeriesUID are the Study and Series Instance UIDs a
	// source read from the file's own first bytes to decide where it goes
	// before it was kept (a STOW-RS part, checked against the study its
	// request names), empty when the source read none. When either is
	// set, SOPClassUID and SOPInstanceUID are read from the same bytes,
	// and the pipeline holds the file as QuarantineUIDMismatch when its
	// whole header gives any of the four otherwise: a data set that repeats
	// a placing attribute, or gives one after a later tag, says one thing
	// in its first bytes and another read whole (PS3.5 7.1), and it is
	// never placed by the second after the source decided by the first.
	StudyUID, SeriesUID string
	// Import, when not nil, is the import unit that hands the file in, the
	// file's path under its root and the parts that path gives, for a
	// receiver of type import (AcceptImport's rules apply); nil for every
	// other source. Its Size is set from the bytes landed.
	Import *tenant.ImportArrival
	// RequireDeidentified says the file must not be archived as received: a
	// transfer sets it for an instance the source archived as received (or
	// whose image a pixel rule accepted at risk), so a receiver changed to
	// archive as received after the job checked it takes none (the file is
	// Failed with ReasonNotDeidentified, and nothing lands).
	RequireDeidentified bool
	// transfer says a transfer job handed the file in (AcceptTransfer): only
	// then does a receiver of type job take it. Unexported, so no source
	// outside this package can claim it.
	transfer bool
	// Body yields the file's bytes, a DICOM file (PS3.10 7.1). It is read
	// to its end or to the receiver's largest file and one byte more.
	Body io.Reader
}

// Status is what a source tells its sender about one file.
type Status int

// The statuses.
const (
	// Received is a file kept whole and durable: the sender may forget it.
	// A file held in quarantine is received too, since its bytes are kept
	// for a person to decide about.
	Received Status = iota + 1
	// Refused is a temporary failure: nothing was kept, and the sender may
	// send the file again later (PS3.4 Table B.2-1, "Refused: Out of
	// Resources").
	Refused
	// Failed is a file that will not be kept if sent again as it is: too
	// large for the receiver, or a receiver that does not take files.
	Failed
)

// String returns the status's name.
func (s Status) String() string {
	switch s {
	case Received:
		return "received"
	case Refused:
		return "refused"
	case Failed:
		return "failed"
	}
	return "unknown"
}

// Outcome is the answer to one Arrival.
type Outcome struct {
	// Status is what the source tells its sender.
	Status Status
	// Reason says why a file was refused or failed: a fixed phrase of this
	// package, safe to send back and to log.
	Reason string
}

// Sink takes the files a source hands in. The pipeline is one; a source is
// written against this interface only, so a DICOM listener, an import of a
// directory and a test all hand files in the same way.
type Sink interface {
	// Accept takes one file and returns, only once the file is durable or
	// known not to be kept, what its sender is told.
	Accept(ctx context.Context, a Arrival) Outcome
}
