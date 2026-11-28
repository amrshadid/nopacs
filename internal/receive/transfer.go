package receive

import (
	"context"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReasonNotDeidentified is a file that must be de-identified (Arrival
// .RequireDeidentified) handed to a receiver that archives as received:
// nothing was kept.
const ReasonNotDeidentified = "the receiver does not de-identify what it receives"

// AcceptTransfer takes one file a transfer job hands to the receiver of type
// job a arrival names, through the whole of Accept: the same limits, the
// tenant's storage quota admission, the same check of the file, and from
// there the receiver's routing, de-identification profile and naming, so what
// arrives is archived as the tenant's own, never as the source tenant had it.
// A receiver that does not exist, is disabled, or is not of type job is
// Failed (the receiver does not take files), and so is a receiver of another
// type, which a job never feeds.
func (p *Pipeline) AcceptTransfer(ctx context.Context, a Arrival) Outcome {
	rcv, err := p.receivers.get(ctx, a.Receiver)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return Outcome{Status: Failed, Reason: reasonNoReceiver}
	case err != nil:
		p.stats.Refused.Add(1)
		return Outcome{Status: Refused, Reason: reasonUnavailable}
	case !rcv.Enabled:
		return Outcome{Status: Failed, Reason: reasonNoReceiver}
	}
	a.transfer = true
	if !transferAdmitted(rcv, a) {
		return Outcome{Status: Failed, Reason: reasonNoReceiver}
	}
	return p.Accept(ctx, a)
}

// transferAdmitted reports whether the arrival a may go to the receiver rcv
// as far as transfers are concerned: a receiver of type job takes the files
// of transfer jobs only, and those go to no other receiver.
func transferAdmitted(rcv tenant.Receiver, a Arrival) bool {
	return (rcv.Type == tenant.ReceiverJob) == a.transfer
}

// TransferRow is what the tenant's catalog holds for an instance that a
// transfer hands to a receiver: the SOP Instance UID the instance gets and
// the checksum it is received with.
type TransferRow struct {
	// SOPInstanceUID is the UID of the instance in the tenant: the UID the
	// file carries when the receiver archives as received, the keyed
	// remapping of it when the receiver de-identifies.
	SOPInstanceUID string
	// SHA256 is the received checksum: the SHA-256 of the bytes as handed in
	// when the receiver archives as received, and the keyed form of it
	// (HMAC-SHA-256 under the tenant's UID key) when it de-identifies, so the
	// catalog holds no checksum an original file could be matched against.
	SHA256 tenant.SHA256
}

// TransferRow returns the row the tenant's catalog will hold for the file of
// SOP Instance UID uid and SHA-256 sum that a transfer hands to the receiver
// named receiver: the very mapping the check of a landed file applies
// (remapPlacingUID, keyedSum), so a confirmation compares with what the
// catalog holds and never with a guess. A receiver that does not exist is
// NotFound.
func (p *Pipeline) TransferRow(ctx context.Context, receiver, uid string, sum tenant.SHA256) (TransferRow, error) {
	rcv, err := p.receivers.get(ctx, receiver)
	if err != nil {
		return TransferRow{}, err
	}
	if !rcv.Deidentifies() {
		return TransferRow{SOPInstanceUID: uid, SHA256: sum}, nil
	}
	keys, err := p.deidKeys(ctx)
	if err != nil {
		return TransferRow{}, err
	}
	return TransferRow{SOPInstanceUID: remapPlacingUID(keys, uid), SHA256: keyedSum(keys, sum)}, nil
}
