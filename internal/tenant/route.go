package tenant

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/repattern"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The bounds of a receiver's routing, the API contract's.
const (
	// MaxRoutes is the most routes a receiver has.
	MaxRoutes = 64
	// MaxRouteConditions is the most conditions of one route.
	MaxRouteConditions = 16
	// MaxRouteConditionText bounds an equals or prefix text: the longest
	// value a subject holds, an LO of several values, is far below it.
	MaxRouteConditionText = 1024
	// MaxRouteValue bounds the value a condition reads, in bytes: a
	// longer value makes the condition unusable, and the study is held as
	// unrouted, rather than tested. A value of the subjects is far below
	// it (PS3.5 6.2: an AE, CS or SH value is at most 16 bytes, a UI 64, an
	// LO 64, each value of several), so a longer one is not a value a
	// route was written for, and testing it would cost what an attacker
	// chooses.
	MaxRouteValue = 1024
	// MaxRoutePatternBudget bounds the compiled size of every pattern of a
	// receiver's routes together (repattern.Compile's cost), so what one
	// study's choice may run is bounded whatever the number of routes.
	MaxRoutePatternBudget = 200000
)

// RouteSubject is what a route's condition reads: the closed list of what
// tells files of one kind or source from another that a profile's
// conditions read, never what identifies a patient, so no route names a
// patient. Its text is what a store keeps.
type RouteSubject string

// The subjects.
const (
	// SubjectModality is Modality (0008,0060).
	SubjectModality RouteSubject = "modality"
	// SubjectSOPClassUID is SOP Class UID (0008,0016).
	SubjectSOPClassUID RouteSubject = "sop_class_uid"
	// SubjectManufacturer is Manufacturer (0008,0070).
	SubjectManufacturer RouteSubject = "manufacturer"
	// SubjectModelName is Manufacturer's Model Name (0008,1090).
	SubjectModelName RouteSubject = "model_name"
	// SubjectSoftwareVersions is Software Versions (0018,1020).
	SubjectSoftwareVersions RouteSubject = "software_versions"
	// SubjectCallingAETitle is the calling AE title of the association
	// the file came in; absent for a file not received by DIMSE.
	SubjectCallingAETitle RouteSubject = "calling_ae_title"
	// SubjectCalledAETitle is the called AE title of the association the
	// file came in; absent for a file not received by DIMSE.
	SubjectCalledAETitle RouteSubject = "called_ae_title"
)

// RouteSubjects are the subjects a route may read, in the contract's order.
var RouteSubjects = []RouteSubject{SubjectModality, SubjectSOPClassUID, SubjectManufacturer, SubjectModelName,
	SubjectSoftwareVersions, SubjectCallingAETitle, SubjectCalledAETitle}

// RouteTest is what a route's condition tests. Its text is what a store
// keeps.
type RouteTest string

// The tests.
const (
	// TestEquals holds when the value equals the text, whole and with case.
	TestEquals RouteTest = "equals"
	// TestPrefix holds when the value starts with the text, with case.
	TestPrefix RouteTest = "prefix"
	// TestPattern holds when the whole value matches the RE2 pattern.
	TestPattern RouteTest = "pattern"
	// TestPresent holds when the value is present and not empty, or, with
	// Present false, when it is absent or empty.
	TestPresent RouteTest = "present"
)

// RouteCondition is one condition of a route. A value absent or empty is
// absent, so equals "" never holds.
type RouteCondition struct {
	// Subject is what it reads.
	Subject RouteSubject
	// Test is what it tests.
	Test RouteTest
	// Value is the text of equals and prefix, the pattern of pattern.
	Value string
	// Present is the test of present.
	Present bool
	// Negate makes the condition hold when the test fails.
	Negate bool
}

// Route is one rule of a receiver's routing: when every condition holds
// for the file a new session is named by, its files go to Storage, are
// de-identified with Profile in place of the receiver's, and are forwarded
// to ForwardTo in place of the receiver's senders.
type Route struct {
	// Name names it in the receiver, under the rule of a tenant's name,
	// and in a reception and a quarantine record that name it.
	Name string
	// Conditions must all hold; none holds for every file.
	Conditions []RouteCondition
	// Storage is the storage a new session it chooses is archived to.
	Storage string
	// Profile, when not zero, de-identifies the files it chooses in place
	// of the receiver's profile; only a receiver that de-identifies may
	// name one, since the receiver's choice decides how a file lands
	// (its UIDs remapped and its checksum keyed) before any route is
	// chosen.
	Profile ProfileRef
	// ReplacesForwardTo says ForwardTo replaces the receiver's senders
	// for the receptions it chooses; an empty ForwardTo then forwards to
	// none. False keeps the receiver's.
	ReplacesForwardTo bool
	// ForwardTo names the senders it forwards to, at most MaxForwardTo,
	// each once; empty unless ReplacesForwardTo.
	ForwardTo []string
}

// Routing is a receiver's routes, in order: the first whose conditions
// all hold chooses. None that holds leaves the receiver's own storage,
// profile and senders, unless QuarantineUnmatched holds the study as
// unrouted.
type Routing struct {
	// Routes are the routes in order, at most MaxRoutes, each name once.
	Routes []Route
	// QuarantineUnmatched holds a new session no route chooses as
	// unrouted, rather than archiving it to the receiver's storage.
	QuarantineUnmatched bool
}

// IsZero reports whether the routing has no route and no default of its
// own, so every study goes as the receiver says.
func (r Routing) IsZero() bool { return len(r.Routes) == 0 && !r.QuarantineUnmatched }

// clone returns a copy of r that shares no slice with it.
func (r Routing) clone() Routing {
	out := Routing{QuarantineUnmatched: r.QuarantineUnmatched}
	if r.Routes != nil {
		out.Routes = make([]Route, len(r.Routes))
		for i, rt := range r.Routes {
			rt.Conditions = slices.Clone(rt.Conditions)
			rt.ForwardTo = slices.Clone(rt.ForwardTo)
			out.Routes[i] = rt
		}
	}
	return out
}

// Storages returns the storages the routes name, each once, in order.
func (r Routing) Storages() []string {
	var out []string
	for _, rt := range r.Routes {
		if !slices.Contains(out, rt.Storage) {
			out = append(out, rt.Storage)
		}
	}
	return out
}

// Senders returns the senders the routes forward to, each once, in order.
func (r Routing) Senders() []string {
	var out []string
	for _, rt := range r.Routes {
		for _, s := range rt.ForwardTo {
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// Profiles returns the profile versions the routes name, each once, in
// order.
func (r Routing) Profiles() []ProfileRef {
	var out []ProfileRef
	for _, rt := range r.Routes {
		if !rt.Profile.IsZero() && !slices.Contains(out, rt.Profile) {
			out = append(out, rt.Profile)
		}
	}
	return out
}

// Validate returns an Invalid error unless r can be stored, by the
// contract's rule of each route; whether the storages, senders and profile
// versions it names exist is the store's and the caller's to check.
func (r Routing) Validate() error {
	if len(r.Routes) > MaxRoutes {
		return invalid("a receiver has at most " + strconv.Itoa(MaxRoutes) + " routes")
	}
	budget := 0
	for i, rt := range r.Routes {
		at := "route " + strconv.Itoa(i+1)
		if err := ValidateName("route", rt.Name); err != nil {
			return invalid(at + ": " + fault.Message(err))
		}
		at = "route " + rt.Name
		if slices.ContainsFunc(r.Routes[:i], func(o Route) bool { return o.Name == rt.Name }) {
			return invalid(at + ": a receiver names each route once")
		}
		if err := ValidateName("storage", rt.Storage); err != nil {
			return invalid(at + ": " + fault.Message(err))
		}
		if !rt.Profile.IsZero() {
			if err := rt.Profile.Validate(); err != nil {
				return invalid(at + ": " + fault.Message(err))
			}
		}
		if !rt.ReplacesForwardTo && len(rt.ForwardTo) > 0 {
			return invalid(at + ": a route that keeps the receiver's senders names none of its own")
		}
		if len(rt.ForwardTo) > MaxForwardTo {
			return invalid(at + ": a route forwards to at most 16 senders")
		}
		for j, s := range rt.ForwardTo {
			if err := ValidateSenderName(s); err != nil {
				return invalid(at + ": " + fault.Message(err))
			}
			if slices.Contains(rt.ForwardTo[:j], s) {
				return invalid(at + ": a route names each sender it forwards to once")
			}
		}
		if len(rt.Conditions) > MaxRouteConditions {
			return invalid(at + ": a route has at most " + strconv.Itoa(MaxRouteConditions) + " conditions")
		}
		for j, c := range rt.Conditions {
			cost, err := c.validate()
			if err != nil {
				return invalid(at + ", condition " + strconv.Itoa(j+1) + ": " + fault.Message(err))
			}
			if budget += cost; budget > MaxRoutePatternBudget {
				return invalid("the patterns of a receiver's routes compile to more than " + strconv.Itoa(MaxRoutePatternBudget) + " instructions and ranges together")
			}
		}
	}
	return nil
}

// validate checks one condition and returns its pattern's compiled size.
func (c RouteCondition) validate() (int, error) {
	if !slices.Contains(RouteSubjects, c.Subject) {
		return 0, invalid("a condition reads one of " + subjectsText())
	}
	switch c.Test {
	case TestEquals:
		if len(c.Value) > MaxRouteConditionText {
			return 0, invalid("an equals text is at most 1024 bytes")
		}
		return 0, validateText("equals text", c.Value, 0, MaxRouteConditionText)
	case TestPrefix:
		if c.Value == "" {
			return 0, invalid("an empty prefix holds for every value: test present")
		}
		return 0, validateText("prefix", c.Value, 1, MaxRouteConditionText)
	case TestPattern:
		_, cost, err := repattern.Compile(c.Value)
		if err != nil {
			return 0, invalid(err.Error())
		}
		return cost, nil
	case TestPresent:
		if c.Value != "" {
			return 0, invalid("a present test has no text")
		}
		return 0, nil
	}
	return 0, invalid("a condition tests equals, prefix, pattern or present")
}

// subjectsText lists the subjects for a message.
func subjectsText() string {
	words := make([]string, len(RouteSubjects))
	for i, s := range RouteSubjects {
		words[i] = string(s)
	}
	return strings.Join(words, ", ")
}

// RouteFacts gives a route the values it reads of one file: the value of a
// subject, without the padding PS3.5 6.2 says is not part of a value, and
// whether it is present.
type RouteFacts interface {
	// Value returns the subject's value and whether it is present.
	Value(subject RouteSubject) (value string, present bool)
}

// ErrRouteUnusable is the cause a choice gives for a route that cannot be
// evaluated: one that fails the rules of this version, such as a subject
// it does not know, as a route stored by a later version can.
var ErrRouteUnusable = fault.New(fault.Precondition, "a route cannot be evaluated")

// Choose returns the first route whose conditions all hold for facts, or
// nil when none does. A route that cannot be evaluated (one that fails the
// rules of this version, or a condition whose value is longer than
// MaxRouteValue) is returned with ErrRouteUnusable, before any route after
// it is tried, so a study is never sent where a later route would send it
// only because an earlier one is broken. Each pattern is compiled once for
// the choice, however many conditions name it.
func (r Routing) Choose(facts RouteFacts) (*Route, error) {
	patterns := map[string]*regexp.Regexp{}
	for i := range r.Routes {
		rt := &r.Routes[i]
		holds, err := rt.holds(facts, patterns)
		if err != nil {
			return rt, err
		}
		if holds {
			return rt, nil
		}
	}
	return nil, nil
}

// holds reports whether every condition of the route holds for facts.
func (rt *Route) holds(facts RouteFacts, patterns map[string]*regexp.Regexp) (bool, error) {
	for _, c := range rt.Conditions {
		ok, err := c.holds(facts, patterns)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// holds reports whether the condition holds for facts, its pattern taken
// from patterns or compiled into it.
func (c RouteCondition) holds(facts RouteFacts, patterns map[string]*regexp.Regexp) (bool, error) {
	if !slices.Contains(RouteSubjects, c.Subject) {
		return false, ErrRouteUnusable
	}
	v, present := facts.Value(c.Subject)
	if len(v) > MaxRouteValue {
		return false, ErrRouteUnusable
	}
	present = present && v != ""
	var r bool
	switch c.Test {
	case TestEquals:
		r = present && v == c.Value
	case TestPrefix:
		r = present && strings.HasPrefix(v, c.Value)
	case TestPattern:
		re, ok := patterns[c.Value]
		if !ok {
			var err error
			if re, _, err = repattern.Compile(c.Value); err != nil {
				return false, ErrRouteUnusable
			}
			patterns[c.Value] = re
		}
		r = present && re.MatchString(v)
	case TestPresent:
		r = present == c.Present
	default:
		return false, ErrRouteUnusable
	}
	return r != c.Negate, nil
}

// ReceptionRoute is what the routing chose for a reception's session, kept
// with the reception when it is identified and with the session when its
// first scan is committed (SessionLabels.Route), so a step run again, a
// later step, and every later arrival of the session go by the same choice
// whatever the routes say by then; and, before that, the storage a person
// named when releasing a file into it.
type ReceptionRoute struct {
	// ReleasedStorage is the storage a release named for the reception's
	// files (Release.Storage): its identify step archives a new session
	// there; the routes still choose the profile and the senders. Empty
	// when no release named one.
	ReleasedStorage string
	// ReleasedProfile is the profile version a release named for the
	// reception's files (Release.Profile): it de-identifies them in place
	// of the route's or the receiver's, while the receiver de-identifies
	// (ProfileFor). Kept with the reception only, never taken by a later
	// arrival of the session. Zero when no release named one.
	ReleasedProfile ProfileRef
	// Route names the route that chose, empty when none did.
	Route string
	// Profile, when not zero, is the profile version its files are
	// de-identified with in place of the receiver's.
	Profile ProfileRef
	// ReplacesForwardTo says ForwardTo replaces the receiver's senders
	// when the reception reaches done.
	ReplacesForwardTo bool
	// ForwardTo names the senders it is forwarded to when
	// ReplacesForwardTo.
	ForwardTo []string
}

// IsZero reports whether the reception's routing chose nothing of its own.
func (r ReceptionRoute) IsZero() bool {
	return r.ReleasedStorage == "" && r.ReleasedProfile.IsZero() && r.Route == "" && r.Profile.IsZero() && !r.ReplacesForwardTo && len(r.ForwardTo) == 0
}

// Validate returns an Invalid error unless r can be stored.
func (r ReceptionRoute) Validate() error {
	if r.ReleasedStorage != "" {
		if err := ValidateName("storage", r.ReleasedStorage); err != nil {
			return err
		}
	}
	if !r.ReleasedProfile.IsZero() {
		if err := r.ReleasedProfile.Validate(); err != nil {
			return err
		}
	}
	if r.Route != "" {
		if err := ValidateName("route", r.Route); err != nil {
			return err
		}
	}
	if !r.Profile.IsZero() {
		if err := r.Profile.Validate(); err != nil {
			return err
		}
	}
	if !r.ReplacesForwardTo && len(r.ForwardTo) > 0 {
		return invalid("a reception that keeps its receiver's senders names none of its own")
	}
	if len(r.ForwardTo) > MaxForwardTo {
		return invalid("a reception is forwarded to at most 16 senders")
	}
	for i, s := range r.ForwardTo {
		if err := ValidateSenderName(s); err != nil {
			return err
		}
		if slices.Contains(r.ForwardTo[:i], s) {
			return invalid("a reception names each sender it is forwarded to once")
		}
	}
	return nil
}

// ChosenBy returns what a route chose, as a reception keeps it.
func ChosenBy(rt Route) ReceptionRoute {
	return ReceptionRoute{Route: rt.Name, Profile: rt.Profile, ReplacesForwardTo: rt.ReplacesForwardTo,
		ForwardTo: slices.Clone(rt.ForwardTo)}
}

// Senders returns the senders a reception of receiver rcv is forwarded to
// when it reaches done: those its route chose, or the receiver's.
func (r ReceptionRoute) Senders(rcv Receiver) []string {
	if r.ReplacesForwardTo {
		return r.ForwardTo
	}
	return rcv.ForwardTo
}

// ProfileFor returns the profile version a reception's files are
// de-identified with at receiver rcv: the one a release into it named, its
// route's, or the receiver's. A released or a route's profile applies only
// while the receiver de-identifies: a receiver changed to archive as
// received meanwhile lands its files as received, and a file landed so is
// never de-identified by a release or a route.
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
