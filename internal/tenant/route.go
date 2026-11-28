package tenant

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/query"
)

// The bounds of a receiver's rule set, the API contract's.
const (
	// MaxArchiveRules is the most archive rules of one rule set.
	MaxArchiveRules = 64
	// MaxForwardRules is the most forward rules of one rule set.
	MaxForwardRules = 64
	// MaxRuleSetComment bounds a rule set's comment, in bytes.
	MaxRuleSetComment = 1024
	// MaxRuleSetVersion is the greatest version of a receiver's rule set.
	MaxRuleSetVersion = 1000000
	// MaxRuleSetConditionBytes bounds the text of every condition of one
	// rule set together: what one study's choice evaluates is bounded
	// whatever the number of rules (each condition is within
	// query.MaxLength alone).
	MaxRuleSetConditionBytes = 256 << 10
)

// The catalog fields a routing condition may read: the AE titles of the
// association a file came in (PS3.8 9.3.2), absent for a file not received
// by DIMSE. Neither names a patient.
const (
	// RouteFieldCallingAE is the calling AE title.
	RouteFieldCallingAE = "calling_ae"
	// RouteFieldCalledAE is the called AE title.
	RouteFieldCalledAE = "called_ae"
)

// ArchiveRule is one rule of a rule set's archive phase: when its condition
// holds for the file a new session is named by, the session is archived to
// Storage and its files de-identified with Profile in place of the
// receiver's. The first archive rule that holds chooses.
type ArchiveRule struct {
	// Name names it in the rule set, under the rule of a tenant's name,
	// and in a reception and a quarantine record that name it.
	Name string
	// When is the condition, in the condition language (query.Parse); empty
	// holds for every study.
	When string
	// Storage is the storage a new session it chooses is archived to.
	Storage string
	// Profile, when not zero, de-identifies the files it chooses in place
	// of the receiver's profile; only a receiver that de-identifies may
	// name one, since the receiver's choice decides how a file lands (its
	// UIDs remapped and its checksum keyed) before any rule is evaluated.
	Profile ProfileRef
}

// ForwardRule is one rule of a rule set's forward phase. Every forward rule
// is evaluated: each whose condition holds for the reception's first file
// forwards it to Senders, its series filtered by Series, each copy
// de-identified with Profile when one is named.
type ForwardRule struct {
	// Name names it in the rule set, under the rule of a tenant's name.
	Name string
	// When is the condition over the reception's first file; empty holds
	// for every reception.
	When string
	// Series is a condition over each series' first file, by SOP Instance
	// UID: only the series it holds for are forwarded. Empty forwards every
	// series.
	Series string
	// Senders names the senders it forwards to, at least one, at most
	// MaxForwardTo, each once.
	Senders []string
	// Profile, when not zero, de-identifies each copy sent with this
	// profile version, over the archived file; it may only tighten what
	// the archive's profile did (the layer rule of de-identification).
	Profile ProfileRef
}

// RuleSet is one version of a receiver's routing rules. A version is never
// changed once made: a change is a new version, and a receiver runs the one
// it points at, so a reception's record of the version it was routed by
// always means the same rules.
type RuleSet struct {
	// Receiver is the receiver it belongs to.
	Receiver string
	// Version numbers it in its receiver, from 1; zero for a rule set not
	// yet stored, and for a receiver with none.
	Version int32
	// Archive are the archive rules, in order, at most MaxArchiveRules,
	// each name once.
	Archive []ArchiveRule
	// QuarantineUnmatched holds a new session no archive rule chooses as
	// unrouted, rather than archiving it as the receiver says.
	QuarantineUnmatched bool
	// Forward are the forward rules, at most MaxForwardRules, each name
	// once and not an archive rule's.
	Forward []ForwardRule
	// Comment says why the version was made; at most MaxRuleSetComment
	// bytes of text, never a value of a file.
	Comment string
	// Created is when the version was made, and CreatedBy the key that
	// made it; answered by the store.
	Created   time.Time
	CreatedBy string
}

// IsZero reports whether the rule set has no rule and no default of its
// own, so every study goes as the receiver says.
func (r RuleSet) IsZero() bool {
	return len(r.Archive) == 0 && len(r.Forward) == 0 && !r.QuarantineUnmatched
}

// Clone returns a copy of r that shares no slice with it.
func (r RuleSet) Clone() RuleSet {
	out := r
	out.Archive = slices.Clone(r.Archive)
	if r.Forward != nil {
		out.Forward = make([]ForwardRule, len(r.Forward))
		for i, f := range r.Forward {
			f.Senders = slices.Clone(f.Senders)
			out.Forward[i] = f
		}
	}
	return out
}

// Storages returns the storages the archive rules name, each once, in
// order.
func (r RuleSet) Storages() []string {
	var out []string
	for _, a := range r.Archive {
		if !slices.Contains(out, a.Storage) {
			out = append(out, a.Storage)
		}
	}
	return out
}

// Senders returns the senders the forward rules name, each once, in order.
func (r RuleSet) Senders() []string {
	var out []string
	for _, f := range r.Forward {
		for _, s := range f.Senders {
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// Profiles returns the profile versions the archive rules name, each once,
// in order.
func (r RuleSet) Profiles() []ProfileRef {
	var out []ProfileRef
	for _, a := range r.Archive {
		if !a.Profile.IsZero() && !slices.Contains(out, a.Profile) {
			out = append(out, a.Profile)
		}
	}
	return out
}

// Validate returns an Invalid error unless r can be stored, by the
// contract's rule of each rule: names, bounds, and conditions that parse
// and read only what routing may read (ValidateRouteCondition). Whether the
// storages, senders and profile versions it names exist is the store's and
// the caller's to check.
func (r RuleSet) Validate() error {
	if err := ValidateName("receiver", r.Receiver); err != nil {
		return err
	}
	if r.Version < 0 || r.Version > MaxRuleSetVersion {
		return invalid("a rule set's version is from 1 to " + strconv.Itoa(MaxRuleSetVersion))
	}
	if len(r.Archive) > MaxArchiveRules {
		return invalid("a rule set has at most " + strconv.Itoa(MaxArchiveRules) + " archive rules")
	}
	if len(r.Forward) > MaxForwardRules {
		return invalid("a rule set has at most " + strconv.Itoa(MaxForwardRules) + " forward rules")
	}
	if len(r.Comment) > MaxRuleSetComment || !utf8.ValidString(r.Comment) || strings.ContainsAny(r.Comment, "\x00\r\n") {
		return invalid("a rule set's comment is at most " + strconv.Itoa(MaxRuleSetComment) + " bytes of text on one line")
	}
	names := map[string]bool{}
	total := 0
	condition := func(at, field, text string) error {
		total += len(text)
		if total > MaxRuleSetConditionBytes {
			return invalid("the conditions of a rule set are at most " + strconv.Itoa(MaxRuleSetConditionBytes) + " bytes together")
		}
		if text == "" {
			return nil
		}
		if err := ValidateRouteCondition(text); err != nil {
			return invalid(at + ": " + field + ": " + fault.Message(err))
		}
		return nil
	}
	for i, a := range r.Archive {
		at := "archive rule " + strconv.Itoa(i+1)
		if err := ValidateName("rule", a.Name); err != nil {
			return invalid(at + ": " + fault.Message(err))
		}
		at = "archive rule " + a.Name
		if names[a.Name] {
			return invalid(at + ": a rule set names each rule once")
		}
		names[a.Name] = true
		if err := condition(at, "when", a.When); err != nil {
			return err
		}
		if err := ValidateName("storage", a.Storage); err != nil {
			return invalid(at + ": " + fault.Message(err))
		}
		if !a.Profile.IsZero() {
			if err := a.Profile.Validate(); err != nil {
				return invalid(at + ": " + fault.Message(err))
			}
		}
	}
	for i, f := range r.Forward {
		at := "forward rule " + strconv.Itoa(i+1)
		if err := ValidateName("rule", f.Name); err != nil {
			return invalid(at + ": " + fault.Message(err))
		}
		at = "forward rule " + f.Name
		if names[f.Name] {
			return invalid(at + ": a rule set names each rule once")
		}
		names[f.Name] = true
		if err := condition(at, "when", f.When); err != nil {
			return err
		}
		if err := condition(at, "series", f.Series); err != nil {
			return err
		}
		if len(f.Senders) == 0 {
			return invalid(at + ": a forward rule names at least one sender")
		}
		if len(f.Senders) > MaxForwardTo {
			return invalid(at + ": a forward rule names at most " + strconv.Itoa(MaxForwardTo) + " senders")
		}
		for j, s := range f.Senders {
			if err := ValidateSenderName(s); err != nil {
				return invalid(at + ": " + fault.Message(err))
			}
			if slices.Contains(f.Senders[:j], s) {
				return invalid(at + ": a forward rule names each sender once")
			}
		}
		if !f.Profile.IsZero() {
			if err := f.Profile.Validate(); err != nil {
				return invalid(at + ": " + fault.Message(err))
			}
		}
	}
	return nil
}

// ValidateRouteCondition returns an Invalid error unless text is a
// condition routing can evaluate: it parses (query.Parse); it has no "has"
// or "every", which look at other levels of the catalog while routing reads
// one file; its catalog fields are the AE titles (RouteFieldCallingAE,
// RouteFieldCalledAE); and no field names an attribute that names or
// identifies the patient (engine.FloorReason), at any step of its path, so
// no rule set can choose by who the patient is. The error names the byte or
// the field, never a value of the condition.
func ValidateRouteCondition(text string) error {
	n, err := query.Parse(text)
	if err != nil {
		var se *query.SyntaxError
		if errors.As(err, &se) {
			return invalid("at byte " + strconv.Itoa(se.Offset) + ": " + se.Msg)
		}
		return invalid(strings.TrimPrefix(err.Error(), "query: "))
	}
	return walkRouteLeaves(n, func(l *query.Leaf) error {
		f := l.Field
		if f.Catalog != "" {
			if f.Catalog != RouteFieldCallingAE && f.Catalog != RouteFieldCalledAE {
				return invalid("the field " + f.Catalog + " is not one routing reads; its catalog fields are " +
					RouteFieldCallingAE + " and " + RouteFieldCalledAE)
			}
			return nil
		}
		for _, s := range f.Path {
			if s.Creator != "" {
				continue
			}
			if why := engine.FloorReason(s.Tag); why != "" {
				return invalid("the field " + query.FieldString(f) + " names who the patient is (" + why + "); a routing condition never reads it")
			}
		}
		return nil
	})
}

// walkRouteLeaves calls fn on each leaf of n, refusing a "has" or "every".
func walkRouteLeaves(n query.Node, fn func(*query.Leaf) error) error {
	switch n := n.(type) {
	case *query.Leaf:
		return fn(n)
	case *query.Not:
		return walkRouteLeaves(n.Child, fn)
	case *query.And:
		for _, c := range n.Children {
			if err := walkRouteLeaves(c, fn); err != nil {
				return err
			}
		}
		return nil
	case *query.Or:
		for _, c := range n.Children {
			if err := walkRouteLeaves(c, fn); err != nil {
				return err
			}
		}
		return nil
	case *query.Has:
		return invalid("has and every are search conditions; routing reads one file")
	}
	return invalid("a node is missing")
}
