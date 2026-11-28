package tenant

import (
	"errors"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/query"
)

// ErrRouteUnusable is the cause a choice gives for a rule that cannot be
// evaluated: a condition this version cannot read (stored by a later
// version, with a word it does not know), or a value it reads that is
// longer than query.MaxReadLength or not held in memory.
var ErrRouteUnusable = fault.New(fault.Precondition, "a routing rule cannot be evaluated")

// RulePhase is the phase of a rule set a rule's outcome belongs to. Its
// text is what a record and the API keep.
type RulePhase string

// The phases.
const (
	// PhaseArchive is an archive rule: the first that holds chooses.
	PhaseArchive RulePhase = "archive"
	// PhaseForward is a forward rule's condition over a reception's first
	// file.
	PhaseForward RulePhase = "forward"
	// PhaseSeries is a forward rule's series filter over a series' first
	// file.
	PhaseSeries RulePhase = "series"
)

// RuleOutcome is what one rule's condition gave over one file.
type RuleOutcome struct {
	// Phase and Rule say which rule, and Index its place in its phase's
	// list, from 0.
	Phase RulePhase
	Rule  string
	Index int
	// Truth is the condition's value; a rule with no condition holds.
	Truth query.Truth
	// Decided is the leaf that decided it (query.Result.Decided); nil for a
	// rule with no condition, and for one that could not be evaluated.
	Decided *query.Leaf
	// Unusable says why the rule could not be evaluated, naming a field,
	// never a value; empty when it was.
	Unusable string
}

// Evaluation is what a rule set gave over a reception's first file.
type Evaluation struct {
	// Archive is the place of the archive rule that chose, from 0; -1 when
	// none did.
	Archive int
	// ArchiveUnknown says an archive rule before any that held was
	// unknown, so which one chooses cannot be known (a dry run over
	// de-identified headers).
	ArchiveUnknown bool
	// Unusable names the archive rule that could not be evaluated: the
	// study is held as unrouted, whatever rules come after it, so a study
	// is never sent where a later rule would send it only because an
	// earlier one is broken.
	Unusable string
	// Forward are the places of the forward rules whose condition holds;
	// ForwardUnknown those whose condition is unknown.
	Forward        []int
	ForwardUnknown []int
	// Outcomes are the rules evaluated, archive rules in order up to the
	// one that decided the phase, then every forward rule.
	Outcomes []RuleOutcome
}

// CompiledRules is a rule set with its conditions parsed once, for every
// file it routes. It is never changed after it is made.
type CompiledRules struct {
	set     RuleSet
	archive []compiledCondition
	forward []compiledCondition
	series  []compiledCondition
}

// compiledCondition is one condition parsed, or why it could not be:
// nil node and empty why for an empty condition, which holds.
type compiledCondition struct {
	node query.Node
	why  string
}

// Compile parses every condition of the rule set. A condition that does not
// parse (one a later version stored) is not an error here: the rule it
// belongs to is unusable when it is reached, as a stored rule set must
// still route what it can and hold what it cannot.
func (r RuleSet) Compile() *CompiledRules {
	c := &CompiledRules{set: r.Clone()}
	parse := func(text string) compiledCondition {
		if text == "" {
			return compiledCondition{}
		}
		if err := ValidateRouteCondition(text); err != nil {
			return compiledCondition{why: "its condition cannot be read by this version: " + fault.Message(err)}
		}
		n, err := query.Parse(text)
		if err != nil {
			return compiledCondition{why: "its condition cannot be read by this version"}
		}
		return compiledCondition{node: n}
	}
	for _, a := range r.Archive {
		c.archive = append(c.archive, parse(a.When))
	}
	for _, f := range r.Forward {
		c.forward = append(c.forward, parse(f.When))
		c.series = append(c.series, parse(f.Series))
	}
	return c
}

// Set returns the rule set compiled.
func (c *CompiledRules) Set() RuleSet { return c.set.Clone() }

// outcome evaluates one condition over s.
func outcome(phase RulePhase, rule string, index int, cc compiledCondition, s query.Subject) RuleOutcome {
	o := RuleOutcome{Phase: phase, Rule: rule, Index: index}
	switch {
	case cc.why != "":
		o.Unusable = cc.why
		return o
	case cc.node == nil:
		o.Truth = query.True
		return o
	}
	res, err := query.Eval(cc.node, s)
	if err != nil {
		var re *query.ReadError
		if errors.As(err, &re) {
			o.Unusable = "the field " + re.Field + " cannot be read: " + re.Why
		} else {
			o.Unusable = "its condition cannot be evaluated"
		}
		return o
	}
	o.Truth, o.Decided = res.Truth, res.Decided
	return o
}

// Evaluate evaluates the rule set over a reception's first file: the
// archive rules in order, up to the first that holds (or is unknown, or
// cannot be evaluated), then every forward rule. A field the file does not
// hold makes a comparison false, never an error; a field s.Removed reports
// makes a leaf unknown.
func (c *CompiledRules) Evaluate(s query.Subject) Evaluation {
	ev := Evaluation{Archive: -1}
	for i, a := range c.set.Archive {
		o := outcome(PhaseArchive, a.Name, i, c.archive[i], s)
		ev.Outcomes = append(ev.Outcomes, o)
		if o.Unusable != "" {
			ev.Unusable = a.Name
			break
		}
		if o.Truth == query.Unknown {
			ev.ArchiveUnknown = true
			break
		}
		if o.Truth == query.True {
			ev.Archive = i
			break
		}
	}
	for i, f := range c.set.Forward {
		o := outcome(PhaseForward, f.Name, i, c.forward[i], s)
		ev.Outcomes = append(ev.Outcomes, o)
		switch {
		case o.Unusable != "":
		case o.Truth == query.True:
			ev.Forward = append(ev.Forward, i)
		case o.Truth == query.Unknown:
			ev.ForwardUnknown = append(ev.ForwardUnknown, i)
		}
	}
	return ev
}

// SeriesFiltered reports whether forward rule i has a series filter.
func (c *CompiledRules) SeriesFiltered(i int) bool {
	return c.set.Forward[i].Series != ""
}

// Series evaluates forward rule i's series filter over a series' first
// file; a rule with no filter holds for every series.
func (c *CompiledRules) Series(i int, s query.Subject) RuleOutcome {
	return outcome(PhaseSeries, c.set.Forward[i].Name, i, c.series[i], s)
}

// RouteCatalog returns the catalog fields routing reads for a file received
// from caller (query.Subject.Catalog): the AE titles, absent when empty.
// Any other field is refused, which ValidateRouteCondition never lets a
// stored condition name.
func RouteCatalog(caller Caller) func(string) (string, bool, error) {
	return func(name string) (string, bool, error) {
		switch name {
		case RouteFieldCallingAE:
			return caller.CallingAETitle, caller.CallingAETitle != "", nil
		case RouteFieldCalledAE:
			return caller.CalledAETitle, caller.CalledAETitle != "", nil
		}
		return "", false, errors.New("tenant: routing reads no catalog field " + name)
	}
}
