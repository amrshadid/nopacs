package tenant

import (
	"slices"
	"time"
)

// A sender's health is the server's view of its peer, kept in the server's
// memory and never in the tenant's database: it is unknown after a start, so
// a database restored or moved never answers a health that was true of
// another time. A change of it is an event the server reports through
// AppendEvents (EventSenderHealth), as a receiver's is.

// SenderState is whether a sender reaches its peer, the contract's
// SenderState in lowercase without its prefix.
type SenderState string

// The sender states.
const (
	SenderUnknown SenderState = "unknown"
	SenderUp      SenderState = "up"
	SenderDown    SenderState = "down"
)

// SenderDownReason is why a sender is down, the contract's SenderDownReason
// in lowercase without its prefix.
type SenderDownReason string

// The reasons a sender is down.
const (
	SenderDownDisabled       SenderDownReason = "disabled"
	SenderDownPeerFailing    SenderDownReason = "peer_failing"
	SenderDownTenantNotReady SenderDownReason = "tenant_not_ready"
	SenderDownStopping       SenderDownReason = "stopping"
	SenderDownOther          SenderDownReason = "other"
)

// SenderFault is why a connection to a sender's peer failed, the contract's
// SenderFault in lowercase without its prefix; empty for none.
type SenderFault string

// The faults.
const (
	SenderFaultUnreachable       SenderFault = "unreachable"
	SenderFaultAddressNotAllowed SenderFault = "address_not_allowed"
	SenderFaultTLSFailed         SenderFault = "tls_failed"
	SenderFaultRejected          SenderFault = "rejected"
	SenderFaultNotAccepted       SenderFault = "not_accepted"
	SenderFaultWrongAnswer       SenderFault = "wrong_answer"
)

// senderDownReasons and senderFaults are the closed lists of each.
var (
	senderDownReasons = []SenderDownReason{SenderDownDisabled, SenderDownPeerFailing, SenderDownTenantNotReady,
		SenderDownStopping, SenderDownOther}
	senderFaults = []SenderFault{SenderFaultUnreachable, SenderFaultAddressNotAllowed, SenderFaultTLSFailed,
		SenderFaultRejected, SenderFaultNotAccepted, SenderFaultWrongAnswer}
)

// SenderHealthChanged is a sender's state changing, the contract's message
// of that name with its SenderHealth inline.
type SenderHealthChanged struct {
	// Sender names the sender.
	Sender string `json:"sender"`
	// State is its new state.
	State SenderState `json:"state"`
	// DownReason is why it is down; empty unless State is down.
	DownReason SenderDownReason `json:"down_reason"`
	// Fault is the fault of the last connection that failed; empty after
	// one that succeeded.
	Fault SenderFault `json:"fault"`
	// Detail is the server's phrase for the state, at most
	// MaxFailureLength bytes; empty when it is up.
	Detail string `json:"detail"`
	// Since is when it entered the state with this reason.
	Since time.Time `json:"since"`
	// FailedConnections counts the connections in a row that failed.
	FailedConnections int `json:"failed_connections"`
	// NextTry is when a peer that is failing is tried again; zero unless
	// DownReason is peer_failing.
	NextTry time.Time `json:"next_try,omitzero"`
	// LastSuccess is when a connection last succeeded; zero for none
	// since the server started.
	LastSuccess time.Time `json:"last_success,omitzero"`
}

// EventType implements EventPayload.
func (*SenderHealthChanged) EventType() EventType { return EventSenderHealth }

func (p *SenderHealthChanged) validate() error {
	if err := ValidateSenderName(p.Sender); err != nil {
		return err
	}
	switch p.State {
	case SenderUnknown, SenderUp:
		if p.DownReason != "" {
			return invalid("a sender that is not down has no down reason")
		}
	case SenderDown:
		if !slices.Contains(senderDownReasons, p.DownReason) {
			return invalid("a down sender's reason is one of disabled, peer_failing, tenant_not_ready, stopping, other")
		}
	default:
		return invalid("a sender's state is unknown, up or down")
	}
	if p.Fault != "" && !slices.Contains(senderFaults, p.Fault) {
		return invalid("a sender's fault is one of unreachable, address_not_allowed, tls_failed, rejected, not_accepted, wrong_answer")
	}
	if p.FailedConnections < 0 {
		return invalid("a count of failed connections is not negative")
	}
	if !p.NextTry.IsZero() && p.DownReason != SenderDownPeerFailing {
		return invalid("a next try is given only for a sender whose peer is failing")
	}
	if err := validateText("sender health detail", p.Detail, 0, MaxFailureLength); err != nil {
		return err
	}
	return validateTimes(p.Since, p.NextTry, p.LastSuccess)
}
