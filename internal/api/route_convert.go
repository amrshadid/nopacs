package api

import (
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ruleSetValues returns the rule set a request gives for receiver, checked
// for what a request can get wrong alone: the bounds of its lists, before
// they are copied. The rules of each value are tenant.RuleSet.Validate's.
func ruleSetValues(field, receiver string, m *v1.RuleSet) (tenant.RuleSet, error) {
	if m == nil {
		return tenant.RuleSet{}, invalid(field, "is required")
	}
	out := tenant.RuleSet{Receiver: receiver, QuarantineUnmatched: m.GetQuarantineUnmatched(), Comment: m.GetComment()}
	if len(m.GetArchiveRules()) > tenant.MaxArchiveRules {
		return out, invalid(field+".archive_rules", "holds at most "+strconv.Itoa(tenant.MaxArchiveRules)+" rules")
	}
	if len(m.GetForwardRules()) > tenant.MaxForwardRules {
		return out, invalid(field+".forward_rules", "holds at most "+strconv.Itoa(tenant.MaxForwardRules)+" rules")
	}
	for _, a := range m.GetArchiveRules() {
		out.Archive = append(out.Archive, tenant.ArchiveRule{Name: a.GetName(), When: a.GetWhen(), Storage: a.GetStorage(),
			Profile: profileRefOf(a.GetProfile())})
	}
	for i, f := range m.GetForwardRules() {
		if len(f.GetSenders()) > tenant.MaxForwardTo {
			return out, invalid(field+".forward_rules["+strconv.Itoa(i)+"].senders", "names at most "+strconv.Itoa(tenant.MaxForwardTo)+" senders")
		}
		out.Forward = append(out.Forward, tenant.ForwardRule{Name: f.GetName(), When: f.GetWhen(), Series: f.GetSeries(),
			Senders: f.GetSenders(), Profile: profileRefOf(f.GetDestinationProfile())})
	}
	if err := out.Validate(); err != nil {
		return out, fieldOf(field, err)
	}
	return out, nil
}

// profileRefOf returns the version a message names; the zero ref for nil.
func profileRefOf(m *v1.ProfileVersionRef) tenant.ProfileRef {
	if m == nil {
		return tenant.ProfileRef{}
	}
	return tenant.ProfileRef{Name: m.GetName(), Version: m.GetVersion()}
}

// profileRefMessage returns a version as the contract writes it; nil for
// the zero ref.
func profileRefMessage(r tenant.ProfileRef) *v1.ProfileVersionRef {
	if r.IsZero() {
		return nil
	}
	return &v1.ProfileVersionRef{Name: r.Name, Version: r.Version}
}

// ruleSetMessage returns a rule set as the contract writes it, active when
// it is the version its receiver runs.
func ruleSetMessage(rs tenant.RuleSet, active int32) *v1.RuleSet {
	m := &v1.RuleSet{Receiver: rs.Receiver, Version: rs.Version, QuarantineUnmatched: rs.QuarantineUnmatched, Comment: rs.Comment,
		CreateTime: timestamp(rs.Created), CreatedByKey: rs.CreatedBy, Active: rs.Version != 0 && rs.Version == active}
	for _, a := range rs.Archive {
		m.ArchiveRules = append(m.ArchiveRules, &v1.ArchiveRule{Name: a.Name, When: a.When, Storage: a.Storage, Profile: profileRefMessage(a.Profile)})
	}
	for _, f := range rs.Forward {
		m.ForwardRules = append(m.ForwardRules, &v1.ForwardRule{Name: f.Name, When: f.When, Series: f.Series, Senders: f.Senders,
			DestinationProfile: profileRefMessage(f.Profile)})
	}
	return m
}

// phaseMessage returns a rule's phase as the contract writes it.
func phaseMessage(p tenant.RulePhase) v1.RulePhase {
	switch p {
	case tenant.PhaseArchive:
		return v1.RulePhase_RULE_PHASE_ARCHIVE
	case tenant.PhaseForward:
		return v1.RulePhase_RULE_PHASE_FORWARD
	case tenant.PhaseSeries:
		return v1.RulePhase_RULE_PHASE_SERIES
	}
	return v1.RulePhase_RULE_PHASE_UNSPECIFIED
}

// truthMessage returns a condition's result as the contract writes it.
func truthMessage(t query.Truth) v1.RouteTruth {
	switch t {
	case query.True:
		return v1.RouteTruth_ROUTE_TRUTH_TRUE
	case query.Unknown:
		return v1.RouteTruth_ROUTE_TRUTH_UNKNOWN
	}
	return v1.RouteTruth_ROUTE_TRUTH_FALSE
}

// hitMessage returns a rule's hits as the contract writes them.
func hitMessage(h tenant.RouteHit) *v1.RouteHit {
	return &v1.RouteHit{Version: h.Version, Phase: phaseMessage(h.Phase), Rule: h.Rule, Receptions: h.Receptions, Series: h.Series,
		LastTime: timestamp(h.Last)}
}

// receiverObject returns a receiver's name as an audit entry's object
// records it: as given when it passes the rule of a name, "invalid"
// otherwise, since what a caller sends in its place can be anything.
func receiverObject(name string) string {
	if tenant.ValidateName("receiver", name) == nil {
		return name
	}
	return invalidValue
}

// ruleVersion returns an Invalid error unless v is a version a request may
// name: 0 (the one running, or none, as the method says) to the largest.
func ruleVersion(field string, v int32) error {
	if v < 0 || v > tenant.MaxRuleSetVersion {
		return invalid(field, "is from 0 to "+strconv.Itoa(tenant.MaxRuleSetVersion))
	}
	return nil
}
