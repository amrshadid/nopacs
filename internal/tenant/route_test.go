package tenant

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// facts is a file's values for a route, by subject.
type facts map[RouteSubject]string

// Value implements RouteFacts: a subject absent from the map is absent.
func (f facts) Value(s RouteSubject) (string, bool) {
	v, ok := f[s]
	return v, ok
}

// TestRouteFirstMatch guards the order of a routing: the first route whose
// conditions all hold chooses, whatever later routes say, and none chooses
// when none holds.
func TestRouteFirstMatch(t *testing.T) {
	r := Routing{Routes: []Route{
		{Name: "ct-a", Storage: "a", Conditions: []RouteCondition{
			{Subject: SubjectCallingAETitle, Test: TestEquals, Value: "A"},
			{Subject: SubjectModality, Test: TestEquals, Value: "CT"},
		}},
		{Name: "any-a", Storage: "b", Conditions: []RouteCondition{{Subject: SubjectCallingAETitle, Test: TestEquals, Value: "A"}}},
		{Name: "all", Storage: "c"},
	}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		f    facts
		want string
	}{
		{facts{SubjectCallingAETitle: "A", SubjectModality: "CT"}, "ct-a"},
		{facts{SubjectCallingAETitle: "A", SubjectModality: "MR"}, "any-a"},
		{facts{SubjectCallingAETitle: "B"}, "all"},
	} {
		rt, err := r.Choose(c.f)
		if err != nil || rt == nil || rt.Name != c.want {
			t.Errorf("%v: chose %v (%v), want %s", c.f, rt, err, c.want)
		}
	}
	r.Routes = r.Routes[:2]
	if rt, err := r.Choose(facts{SubjectCallingAETitle: "B"}); rt != nil || err != nil {
		t.Errorf("no route holds, and %v chose (%v)", rt, err)
	}
}

// TestRouteOperators guards each test of a condition and negate, with the
// rule for a value absent or empty: absent, so equals "" never holds.
func TestRouteOperators(t *testing.T) {
	cases := []struct {
		c    RouteCondition
		f    facts
		want bool
	}{
		{RouteCondition{Subject: SubjectModality, Test: TestEquals, Value: "CT"}, facts{SubjectModality: "CT"}, true},
		{RouteCondition{Subject: SubjectModality, Test: TestEquals, Value: "CT"}, facts{SubjectModality: "ct"}, false},
		{RouteCondition{Subject: SubjectModality, Test: TestEquals, Value: ""}, facts{SubjectModality: ""}, false},
		{RouteCondition{Subject: SubjectManufacturer, Test: TestPrefix, Value: "SIE"}, facts{SubjectManufacturer: "SIEMENS"}, true},
		{RouteCondition{Subject: SubjectManufacturer, Test: TestPrefix, Value: "SIE"}, facts{SubjectManufacturer: "GE"}, false},
		{RouteCondition{Subject: SubjectManufacturer, Test: TestPrefix, Value: "SIE"}, facts{}, false},
		{RouteCondition{Subject: SubjectModelName, Test: TestPattern, Value: "Prisma.*"}, facts{SubjectModelName: "Prisma fit"}, true},
		{RouteCondition{Subject: SubjectModelName, Test: TestPattern, Value: "Prisma"}, facts{SubjectModelName: "Prisma fit"}, false},
		{RouteCondition{Subject: SubjectSoftwareVersions, Test: TestPresent, Present: true}, facts{SubjectSoftwareVersions: "VE11"}, true},
		{RouteCondition{Subject: SubjectSoftwareVersions, Test: TestPresent, Present: true}, facts{SubjectSoftwareVersions: ""}, false},
		{RouteCondition{Subject: SubjectSoftwareVersions, Test: TestPresent, Present: false}, facts{}, true},
		{RouteCondition{Subject: SubjectCalledAETitle, Test: TestEquals, Value: "X", Negate: true}, facts{SubjectCalledAETitle: "Y"}, true},
		{RouteCondition{Subject: SubjectCalledAETitle, Test: TestEquals, Value: "X", Negate: true}, facts{}, true},
		{RouteCondition{Subject: SubjectCalledAETitle, Test: TestEquals, Value: "X", Negate: true}, facts{SubjectCalledAETitle: "X"}, false},
	}
	for i, c := range cases {
		if _, err := c.c.validate(); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		got, err := c.c.holds(c.f, map[string]*regexp.Regexp{})
		if err != nil || got != c.want {
			t.Errorf("case %d (%+v over %v): %v (%v), want %v", i, c.c, c.f, got, err, c.want)
		}
	}
}

// TestRouteUnusable guards a route stored with what this version cannot
// evaluate: the choice stops there with ErrRouteUnusable, and names that
// route, rather than going on to a later route.
func TestRouteUnusable(t *testing.T) {
	r := Routing{Routes: []Route{
		{Name: "later-kind", Storage: "a", Conditions: []RouteCondition{{Subject: "body_part", Test: TestEquals, Value: "HEAD"}}},
		{Name: "all", Storage: "b"},
	}}
	rt, err := r.Choose(facts{})
	if !errors.Is(err, ErrRouteUnusable) || rt == nil || rt.Name != "later-kind" {
		t.Errorf("chose %v (%v), want later-kind and ErrRouteUnusable", rt, err)
	}
}

// TestRouteValueBound guards the bound of a value a condition reads: a
// value past MaxRouteValue makes the route unusable, at once, whatever the
// test (here a pattern that would take time over a long value), and a
// value at the bound is tested.
func TestRouteValueBound(t *testing.T) {
	r := Routing{Routes: []Route{{Name: "maker", Storage: "a", Conditions: []RouteCondition{
		{Subject: SubjectManufacturer, Test: TestPattern, Value: `(a|b|c|d)*x`}}}}}
	start := time.Now()
	rt, err := r.Choose(facts{SubjectManufacturer: strings.Repeat("abcd", 1<<20)})
	if !errors.Is(err, ErrRouteUnusable) || rt == nil || rt.Name != "maker" {
		t.Errorf("a 4 MiB value: %v (%v), want maker and ErrRouteUnusable", rt, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("a 4 MiB value took %v", d)
	}
	if rt, err := r.Choose(facts{SubjectManufacturer: strings.Repeat("a", MaxRouteValue-1) + "x"}); err != nil || rt == nil {
		t.Errorf("a value at the bound: %v (%v), want it tested and chosen", rt, err)
	}
}

// TestRoutingValidate guards the bounds and rules of a routing a request
// can break: routes, conditions, names, texts, patterns, senders.
func TestRoutingValidate(t *testing.T) {
	ok := Route{Name: "r", Storage: "s"}
	many := func(n int) []Route {
		out := make([]Route, n)
		for i := range out {
			out[i] = Route{Name: "r" + strconv.Itoa(i), Storage: "s"}
		}
		return out
	}
	conds := func(n int) []RouteCondition {
		return make([]RouteCondition, n)
	}
	pattern := func(p string) Route {
		return Route{Name: "r", Storage: "s", Conditions: []RouteCondition{{Subject: SubjectModality, Test: TestPattern, Value: p}}}
	}
	bad := map[string]Routing{
		"too many routes":       {Routes: many(MaxRoutes + 1)},
		"a name twice":          {Routes: []Route{ok, ok}},
		"a bad name":            {Routes: []Route{{Name: "R!", Storage: "s"}}},
		"no storage":            {Routes: []Route{{Name: "r"}}},
		"too many conditions":   {Routes: []Route{{Name: "r", Storage: "s", Conditions: conds(MaxRouteConditions + 1)}}},
		"the receiver subject":  {Routes: []Route{{Name: "r", Storage: "s", Conditions: []RouteCondition{{Subject: "receiver", Test: TestPresent}}}}},
		"an empty prefix":       {Routes: []Route{{Name: "r", Storage: "s", Conditions: []RouteCondition{{Subject: SubjectModality, Test: TestPrefix}}}}},
		"a long equals":         {Routes: []Route{{Name: "r", Storage: "s", Conditions: []RouteCondition{{Subject: SubjectModality, Test: TestEquals, Value: strings.Repeat("x", 1025)}}}}},
		"no test":               {Routes: []Route{{Name: "r", Storage: "s", Conditions: []RouteCondition{{Subject: SubjectModality}}}}},
		"a pattern not RE2":     {Routes: []Route{pattern(`(?<=a)b`)}},
		"a long pattern":        {Routes: []Route{pattern(strings.Repeat("a", 257))}},
		"a large repetition":    {Routes: []Route{pattern(`a{101}`)}},
		"senders not replacing": {Routes: []Route{{Name: "r", Storage: "s", ForwardTo: []string{"pacs"}}}},
		"a sender twice":        {Routes: []Route{{Name: "r", Storage: "s", ReplacesForwardTo: true, ForwardTo: []string{"pacs", "pacs"}}}},
		"a bad profile":         {Routes: []Route{{Name: "r", Storage: "s", Profile: ProfileRef{Name: "p"}}}},
	}
	for name, r := range bad {
		if err := r.Validate(); fault.KindOf(err) != fault.Invalid {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
	good := Routing{QuarantineUnmatched: true, Routes: append(many(MaxRoutes-1), pattern(`CT|MR`))}
	good.Routes[len(good.Routes)-1].Name = "last"
	if err := good.Validate(); err != nil {
		t.Errorf("a routing at its bounds: %v", err)
	}
}

// TestReceiverRouteProfile guards that only a receiver that de-identifies
// has a route naming a profile: the receiver's choice decides how a file
// lands before a route is chosen.
func TestReceiverRouteProfile(t *testing.T) {
	r := Receiver{Name: "recv", Type: ReceiverDIMSE, Storage: "s", QuietPeriod: DefaultQuietPeriod, MaxOpenTime: DefaultMaxOpenTime,
		MaxFileBytes: DefaultMaxFileBytes, DIMSE: DIMSEReceiver{Port: 11112, AETitle: "A"},
		Routing: Routing{Routes: []Route{{Name: "r", Storage: "s", Profile: ProfileRef{Name: "p", Version: 1}}}}}
	if err := r.Validate(); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a route with a profile on a receiver that archives as received: %v, want a precondition failure", err)
	}
	r.Profile = ProfileRef{Name: "basic", Version: 1}
	if err := r.Validate(); err != nil {
		t.Errorf("a route with a profile on a receiver that de-identifies: %v", err)
	}
	if got := (ReceptionRoute{Profile: ProfileRef{Name: "p", Version: 1}}).ProfileFor(r); got.Name != "p" {
		t.Errorf("a route's profile on a receiver that de-identifies gives %v", got)
	}
	r.Profile = ProfileRef{}
	if got := (ReceptionRoute{Profile: ProfileRef{Name: "p", Version: 1}}).ProfileFor(r); !got.IsZero() {
		t.Errorf("a route's profile on a receiver changed to archive as received gives %v, want none", got)
	}
}

// TestReleasedProfileComesFirst guards the profile version a release named
// for a reception: at a receiver that de-identifies it comes before the
// route's and the receiver's; at one changed to archive as received none
// applies; and a release naming one through a receiver that archives as
// received, or of a record made de-identified through such a receiver, is
// a precondition failure.
func TestReleasedProfileComesFirst(t *testing.T) {
	rcv := Receiver{Name: "recv", Profile: ProfileRef{Name: "basic", Version: 1}}
	released, routed := ProfileRef{Name: "fixed", Version: 2}, ProfileRef{Name: "p", Version: 1}
	if got := (ReceptionRoute{ReleasedProfile: released, Profile: routed}).ProfileFor(rcv); got != released {
		t.Errorf("a released and a route's profile give %v, want the released", got)
	}
	if got := (ReceptionRoute{ReleasedProfile: released}).ProfileFor(Receiver{Name: "recv"}); !got.IsZero() {
		t.Errorf("a released profile at a receiver that archives as received gives %v, want none", got)
	}
	if (ReceptionRoute{ReleasedProfile: released}).IsZero() {
		t.Error("a reception route with only a released profile reads as zero")
	}
	rel := Release{Profile: released}
	if err := ReleaseAllowed(QuarantinedFile{}, rel, false); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a profile named through a receiver that archives as received: %v", err)
	}
	if err := ReleaseAllowed(QuarantinedFile{Deidentified: true}, Release{}, false); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a record made de-identified through a receiver that archives as received: %v", err)
	}
	if err := ReleaseAllowed(QuarantinedFile{Deidentified: true}, rel, true); err != nil {
		t.Errorf("a record made de-identified, a profile named, through a receiver that de-identifies: %v", err)
	}
}
