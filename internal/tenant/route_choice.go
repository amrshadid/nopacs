package tenant

import (
	"slices"
	"strconv"
)

// MaxForwardSeries bounds the series a forward choice names: a study of
// more series than this is forwarded whole by a rule with a filter that
// holds for all, and a filter that holds for some of more than this many
// is recorded as unusable, never cut short silently.
const MaxForwardSeries = 4096

// ForwardChoice is one forward rule that holds for a reception: the senders
// it is forwarded to when it reaches done, the series of it that go, and
// the profile version each copy is de-identified with.
type ForwardChoice struct {
	// Rule names the forward rule.
	Rule string
	// Senders are the rule's senders.
	Senders []string
	// Filtered says the rule has a series filter: only Series go. False
	// sends every series the reception archived.
	Filtered bool
	// Series are the Series Instance UIDs, as the reception holds them
	// (remapped when the receiver de-identifies), its filter held for; at
	// least one when Filtered.
	Series []string
	// Profile, when not zero, de-identifies each copy sent.
	Profile ProfileRef
}

// ReceptionRoute is what the routing chose for a reception's session, kept
// with the reception when it is identified and with the session when its
// first scan is committed (SessionLabels.Route), so a step run again, a
// later step, and every later arrival of the session go by the same archive
// choice whatever the rules say by then; and, before that, the storage a
// person named when releasing a file into it. The forward choices are each
// reception's own: a later arrival is forwarded by the forward rules the
// receiver runs when it is identified, over its own series.
type ReceptionRoute struct {
	// ReleasedStorage is the storage a release named for the reception's
	// files (Release.Storage): its identify step archives a new session
	// there; the rules still choose the profile and the forwards. Empty
	// when no release named one.
	ReleasedStorage string
	// ReleasedProfile is the profile version a release named for the
	// reception's files (Release.Profile): it de-identifies them in place
	// of the rule's or the receiver's, while the receiver de-identifies
	// (ProfileFor). Kept with the reception only, never taken by a later
	// arrival of the session. Zero when no release named one.
	ReleasedProfile ProfileRef
	// RuleSet is the version of the receiver's rule set the archive choice
	// was made by; zero when the receiver had none.
	RuleSet int32
	// Route names the archive rule that chose, empty when none did.
	Route string
	// Profile, when not zero, is the profile version its files are
	// de-identified with in place of the receiver's.
	Profile ProfileRef
	// ForwardRuleSet is the version the forward choices were made by; zero
	// when none was evaluated.
	ForwardRuleSet int32
	// Forwards are the forward rules that hold for the reception, in the
	// rule set's order.
	Forwards []ForwardChoice
	// CallingAE and CalledAE are the AE titles of the association the
	// session's first file came in (PS3.8 9.3.2), empty for a file not
	// received by DIMSE. Neither names a patient; a dry run of rules reads
	// them.
	CallingAE, CalledAE string
}

// IsZero reports whether the reception's routing chose nothing of its own.
func (r ReceptionRoute) IsZero() bool {
	return r.ReleasedStorage == "" && r.ReleasedProfile.IsZero() && r.RuleSet == 0 && r.Route == "" && r.Profile.IsZero() &&
		r.ForwardRuleSet == 0 && len(r.Forwards) == 0 && r.CallingAE == "" && r.CalledAE == ""
}

// Archived returns the choice a later arrival of the session takes: the
// archive choice and the AE titles, without the released storage and
// profile (which were the first reception's alone) and without the
// forwards (each reception's own).
func (r ReceptionRoute) Archived() ReceptionRoute {
	return ReceptionRoute{RuleSet: r.RuleSet, Route: r.Route, Profile: r.Profile, CallingAE: r.CallingAE, CalledAE: r.CalledAE}
}

// Validate returns an Invalid error unless r can be stored.
func (r ReceptionRoute) Validate() error {
	if r.ReleasedStorage != "" {
		if err := ValidateName("storage", r.ReleasedStorage); err != nil {
			return err
		}
	}
	for _, p := range []ProfileRef{r.ReleasedProfile, r.Profile} {
		if !p.IsZero() {
			if err := p.Validate(); err != nil {
				return err
			}
		}
	}
	if r.Route != "" {
		if err := ValidateName("rule", r.Route); err != nil {
			return err
		}
	}
	if r.RuleSet < 0 || r.RuleSet > MaxRuleSetVersion || r.ForwardRuleSet < 0 || r.ForwardRuleSet > MaxRuleSetVersion {
		return invalid("a rule set's version is from 1 to " + strconv.Itoa(MaxRuleSetVersion))
	}
	if r.ForwardRuleSet == 0 && len(r.Forwards) > 0 {
		return invalid("a reception's forwards name the rule set version they were chosen by")
	}
	if len(r.Forwards) > MaxForwardRules {
		return invalid("a reception holds at most " + strconv.Itoa(MaxForwardRules) + " forwards")
	}
	for i, f := range r.Forwards {
		if err := ValidateName("rule", f.Rule); err != nil {
			return err
		}
		if slices.ContainsFunc(r.Forwards[:i], func(o ForwardChoice) bool { return o.Rule == f.Rule }) {
			return invalid("a reception names each forward rule once")
		}
		if len(f.Senders) == 0 || len(f.Senders) > MaxForwardTo {
			return invalid("a forward names from 1 to " + strconv.Itoa(MaxForwardTo) + " senders")
		}
		for _, s := range f.Senders {
			if err := ValidateSenderName(s); err != nil {
				return err
			}
		}
		if f.Filtered != (len(f.Series) > 0) || len(f.Series) > MaxForwardSeries {
			return invalid("a filtered forward names from 1 to " + strconv.Itoa(MaxForwardSeries) + " series; one not filtered names none")
		}
		for _, s := range f.Series {
			if err := ValidateSeriesUID(s); err != nil {
				return err
			}
		}
		if !f.Profile.IsZero() {
			if err := f.Profile.Validate(); err != nil {
				return err
			}
		}
	}
	for _, ae := range []string{r.CallingAE, r.CalledAE} {
		if err := (Caller{CallingAETitle: ae}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ChosenBy returns what archive rule a of rule set version chose, as a
// reception keeps it.
func ChosenBy(version int32, a ArchiveRule) ReceptionRoute {
	return ReceptionRoute{RuleSet: version, Route: a.Name, Profile: a.Profile}
}

// ProfileFor returns the profile version a reception's files are
// de-identified with at receiver rcv: the one a release into it named, its
// archive rule's, or the receiver's. A released or a rule's profile applies
// only while the receiver de-identifies: a receiver changed to archive as
// received meanwhile lands its files as received, and a file landed so is
// never de-identified by a release or a rule.
func (r ReceptionRoute) ProfileFor(rcv Receiver) ProfileRef {
	if !rcv.Deidentifies() {
		return rcv.Profile
	}
	if !r.ReleasedProfile.IsZero() {
		return r.ReleasedProfile
	}
	if !r.Profile.IsZero() {
		return r.Profile
	}
	return rcv.Profile
}

// RuleForwardKey is the unique key of the job a forward rule makes for a
// reception, a sender and one series, so
// one is made once whatever runs the step again; two rules forwarding to
// one sender make a job each, since each may send other series or another
// copy.
func RuleForwardKey(receptionID, rule, sender, seriesUID string) string {
	return "fwd:" + receptionID + ":" + sender + ":" + rule + ":" + seriesUID
}

// RuleForwardJobID is the id of the job RuleForwardKey keys, made from it.
func RuleForwardJobID(receptionID, rule, sender, seriesUID string) string {
	return "fwd-" + digest(RuleForwardKey(receptionID, rule, sender, seriesUID))
}

// RouteMatch is one rule a reception was routed by, as its record keeps
// it: a hit count is a count of these.
type RouteMatch struct {
	// Version is the rule set version, Phase the rule's phase (archive or
	// forward), and Rule its name; an empty Rule in the archive phase is
	// the rule set's default (no archive rule held).
	Version int32
	Phase   RulePhase
	Rule    string
	// Series is how many series a forward sent (0 for every series and for
	// an archive rule).
	Series int
}

// Matches returns the records of r for one reception: its archive choice
// when a rule set made it, and each forward.
func (r ReceptionRoute) Matches() []RouteMatch {
	var out []RouteMatch
	if r.RuleSet != 0 {
		out = append(out, RouteMatch{Version: r.RuleSet, Phase: PhaseArchive, Rule: r.Route})
	}
	for _, f := range r.Forwards {
		out = append(out, RouteMatch{Version: r.ForwardRuleSet, Phase: PhaseForward, Rule: f.Rule, Series: len(f.Series)})
	}
	return out
}
