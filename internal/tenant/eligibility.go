package tenant

// Which archived instances may leave the archive. By default only an
// instance archived de-identified, recording the profile version that did
// it, and whose pixels no pixel rule accepted at risk of burned-in text, is
// sent; a sender marked AllowIdentified by the installation's admin may send
// the rest. The rule is one function, SendRefusal, which every path that
// hands an archived instance to someone outside the archive asks per
// instance at the moment it hands it over, never once when the work is
// made: a mark cleared or a revision changed after a job was made then
// still holds.

// The refusals of SendRefusal, each a JobFailureCode a send job records
// against the instance, refused and counted, while the job goes on with the
// rest. Each names a fact of the catalog, never a value of the file.
const (
	// FailureAsReceived is an instance archived as received: its catalog
	// row records no profile version, so nothing says it was
	// de-identified.
	FailureAsReceived JobFailureCode = "as_received"
	// FailureNoProfileProvenance is an instance whose catalog row names a
	// profile version only in part (a name without a valid version, or the
	// reverse), so which version de-identified it cannot be told.
	FailureNoProfileProvenance JobFailureCode = "no_profile_provenance"
	// FailurePixelRiskAccepted is an instance whose image was at risk of
	// burned-in text (PS3.15 E.3.1) and passed only because a pixel rule
	// accepted it as it is: what its pixels show was not removed.
	FailurePixelRiskAccepted JobFailureCode = "pixel_risk_accepted"
)

// InstanceProvenance is what the catalog records of how an instance was
// archived, the facts SendRefusal judges.
type InstanceProvenance struct {
	// Profile is the profile version that de-identified it; zero for one
	// archived as received.
	Profile ProfileRef
	// PixelRiskAccepted says its image was at risk of burned-in text and a
	// pixel rule accepted it as it is.
	PixelRiskAccepted bool
}

// Provenance returns the facts of in that SendRefusal judges.
func (in ScanInstance) Provenance() InstanceProvenance {
	return InstanceProvenance{Profile: in.Profile, PixelRiskAccepted: in.PixelRiskAccepted}
}

// SendRefusal returns why an archived instance of provenance p may not be
// handed to a destination whose mark is allowIdentified, and true; "" and
// false when it may. The one rule every path that sends archived data asks
// per instance as it hands it over (a send job of any kind, and any later
// answer that moves data out). In order: as received, a profile version
// recorded only in part, an accepted pixel risk; a destination marked
// allowIdentified is refused none of them.
func SendRefusal(p InstanceProvenance, allowIdentified bool) (JobFailureCode, bool) {
	switch {
	case allowIdentified:
		return "", false
	case p.Profile.IsZero():
		return FailureAsReceived, true
	case p.Profile.Validate() != nil:
		return FailureNoProfileProvenance, true
	case p.PixelRiskAccepted:
		return FailurePixelRiskAccepted, true
	}
	return "", false
}

// EventSenderAllowIdentified is a sender's AllowIdentified set or cleared,
// written by AddSender or SetSender with the change.
const EventSenderAllowIdentified EventType = "sender_allow_identified"

// SenderAllowIdentifiedChanged is a sender's mark set or cleared, the
// contract's SenderAllowIdentifiedChanged: a change of what the tenant lets
// leave the archive, so a watcher sees it beside the audit entry.
type SenderAllowIdentifiedChanged struct {
	// Sender names the sender.
	Sender string `json:"sender"`
	// AllowIdentified is the mark after the change.
	AllowIdentified bool `json:"allow_identified"`
}

// EventType implements EventPayload.
func (*SenderAllowIdentifiedChanged) EventType() EventType { return EventSenderAllowIdentified }

// validate implements EventPayload.
func (p *SenderAllowIdentifiedChanged) validate() error { return ValidateSenderName(p.Sender) }

// DetailAllowIdentified is the detail a store adds to the audit entry of a
// sender added with its mark set, or changed so its mark moves: the mark
// after, "true" or "false".
const DetailAllowIdentified = "allow_identified"
