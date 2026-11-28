package profile_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// layer expands rules into a layer of kind, named name.
func layer(t *testing.T, kind profile.LayerKind, name string, doc *v1.ProfileDocument) profile.Layer {
	t.Helper()
	return profile.Layer{Kind: kind, Name: name, Expanded: mustExpand(t, doc, testCatalog(t))}
}

// rules is a document of rules.
func rules(rs ...*v1.ProfileRule) *v1.ProfileDocument { return &v1.ProfileDocument{Rules: rs} }

// TestLayerMayOnlyTighten guards the layer rule: a receiver's rule as
// strict as the tenant's, or stricter, is taken, and the effective profile
// holds both layers' rules in order and decides by the stricter; a rule
// that would loosen what the tenant decides is refused with
// loosens_lower_layer, naming the layer, the element and both actions; one
// of the same strictness that differs with incomparable_actions; and
// nothing is returned when any rule is refused.
func TestLayerMayOnlyTighten(t *testing.T) {
	tenant := layer(t, profile.LayerTenant, "base@1", rules(
		rl("desc", kw("StudyDescription"), clean()),
		rl("pid", kw("PatientID"), hash()),
		rl("station", kw("StationName"), remove()),
	))
	ok := layer(t, profile.LayerReceiver, "ct@2", rules(
		rl("desc-gone", kw("StudyDescription"), remove()),
		rl("pid-same", kw("PatientID"), hash()),
	))
	eff, problems := profile.Stack(tenant, ok)
	if problems != nil {
		t.Fatalf("a tightening layer refused: %v", problems)
	}
	e := eff.Expanded()
	if len(e.GetRules()) != 5 {
		t.Fatalf("%d rules", len(e.GetRules()))
	}
	prog, err := profile.Compile(e)
	if err != nil {
		t.Fatal(err)
	}
	if d := prog.Decide(profile.Location{Tag: 0x00081030, VR: "LO"}, facts{}); d.Action.GetRemove() == nil {
		t.Errorf("the effective profile decides Study Description by %v", d.Action)
	}

	loose := layer(t, profile.LayerReceiver, "mr@1", rules(
		rl("desc-kept", kw("StudyDescription"), keep()),
		rl("pid-shift", kw("PatientID"), replace("X")),
		rl("station-empty", kw("StationName"), empty()),
		rl("pid-other", kw("PatientID"), lookup("codes", v1.LookupMissing_LOOKUP_MISSING_QUARANTINE)),
	))
	eff, problems = profile.Stack(tenant, loose)
	if eff != nil || len(problems) != 3 {
		t.Fatalf("a loosening layer: %v, %v", eff, problems)
	}
	want := []struct {
		rule int
		code profile.Code
		has  []string
	}{
		{1, profile.CodeLoosensLowerLayer, []string{"receiver layer mr@1", "keep", "StudyDescription", "clean", "tenant layer base@1", "rule desc"}},
		{3, profile.CodeLoosensLowerLayer, []string{"empty", "StationName", "remove"}},
		{4, profile.CodeIncomparableActions, []string{"lookup", "PatientID", "hash"}},
	}
	for i, w := range want {
		p := problems[i]
		if p.Rule != w.rule || p.Code != w.code || p.Field != "action" {
			t.Errorf("problem %d: rule %d %s at %s, want rule %d %s", i, p.Rule, p.Code, p.Field, w.rule, w.code)
		}
		for _, s := range w.has {
			if !strings.Contains(p.Message, s) {
				t.Errorf("problem %d does not name %q: %s", i, s, p.Message)
			}
		}
	}
	// replace over hash is stricter (a constant says nothing of the
	// value), so rule 2 is not refused.
	for _, p := range problems {
		if p.Rule == 2 {
			t.Errorf("replace over hash refused: %v", p)
		}
	}
}

// TestLayerJudgesWhatDecidesBelow guards which decisions below a layer's
// rule is compared with: a group rule below that covers a tag the layer
// names; the default below for an element no rule names (kept, so
// anything is at least as strict; removed for a private element, so keep
// is refused); a rule below of the same selector with no condition that
// outranks a less specific rule (a tag rule below over a VR rule, so a
// layer's tag rule is compared with the tag rule only); a rule below with
// a condition, which may decide, so it counts.
func TestLayerJudgesWhatDecidesBelow(t *testing.T) {
	cat := testCatalog(t)
	tenant := profile.Layer{Kind: profile.LayerTenant, Name: "base@1", Expanded: mustExpand(t, rules(
		rl("curves", grp("50xx"), remove()),
		rl("names", vr("LO"), remove()),
		rl("desc", kw("SeriesDescription"), replace("SERIES")),
		rl("comments", kw("ImageComments"), keep(), equals(v1.ConditionSubject_CONDITION_SUBJECT_MODALITY, "CT")),
	), cat)}
	for _, c := range []struct {
		name string
		rule *v1.ProfileRule
		code profile.Code
	}{
		{"a tag in a group removed below", rl("curve", tg("50000010"), keep()), profile.CodeLoosensLowerLayer},
		{"a private element no rule names", rl("acme", priv("ACME 1.0", "0009", "10"), keep()), profile.CodeLoosensLowerLayer},
		{"a standard element no rule names", rl("x", kw("BodyPartExamined"), keep()), ""},
		{"the tag rule outranks the VR rule", rl("series", kw("SeriesDescription"), replace("SERIES")), ""},
		{"a conditional rule below counts", rl("comments", kw("ImageComments"), keep()), ""},
		{"stricter than a conditional rule below", rl("comments", kw("ImageComments"), remove()), ""},
		{"a private group no rule names", rl("g", grpPrivate("0019"), keep()), profile.CodeLoosensLowerLayer},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, problems := profile.Stack(tenant, profile.Layer{Kind: profile.LayerRoute, Name: "r@1", Expanded: mustExpand(t, rules(c.rule), cat)})
			switch {
			case c.code == "" && problems != nil:
				t.Errorf("refused: %v", problems)
			case c.code != "" && (len(problems) == 0 || problems[0].Code != c.code):
				t.Errorf("problems %v, want %s", problems, c.code)
			}
		})
	}
}

// TestLayerRemoveDefaultBelow guards that a layer over a profile that
// removes the elements no rule names may not keep one, and that the
// effective profile removes them when either layer says so.
func TestLayerRemoveDefaultBelow(t *testing.T) {
	cat := testCatalog(t)
	tenant := profile.Layer{Kind: profile.LayerTenant, Name: "base@1", Expanded: mustExpand(t, &v1.ProfileDocument{Unnamed: v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE}, cat)}
	if _, problems := profile.Stack(tenant, layer(t, profile.LayerReceiver, "r@1", rules(rl("k", kw("BodyPartExamined"), keep())))); len(problems) == 0 ||
		problems[0].Code != profile.CodeLoosensLowerLayer || !strings.Contains(problems[0].Message, "default below") {
		t.Errorf("keeping what the default below removes: %v", problems)
	}
	eff, problems := profile.Stack(layer(t, profile.LayerTenant, "base@1", rules()),
		profile.Layer{Kind: profile.LayerReceiver, Name: "r@1", Expanded: mustExpand(t, &v1.ProfileDocument{Unnamed: v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE}, cat)})
	if problems != nil || eff.Expanded().GetUnnamed() != v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE {
		t.Errorf("the effective default: %v, %v", problems, eff)
	}
}

// TestLayerOrderAndPixelRules guards the stack's shape: layers out of the
// order tenant, receiver, route, destination, or one twice, are refused;
// a layer above the first with pixel rules is refused; any layer may be
// absent.
func TestLayerOrderAndPixelRules(t *testing.T) {
	base := layer(t, profile.LayerTenant, "base@1", rules())
	route := layer(t, profile.LayerRoute, "route@1", rules())
	if _, problems := profile.Stack(route, base); len(problems) == 0 || problems[0].Code != profile.CodeLayerOrder {
		t.Errorf("a tenant layer over a route: %v", problems)
	}
	if _, problems := profile.Stack(base, route, route); len(problems) == 0 || problems[0].Code != profile.CodeLayerOrder {
		t.Errorf("a route twice: %v", problems)
	}
	if _, problems := profile.Stack(); len(problems) == 0 {
		t.Error("no layer")
	}
	pixels := layer(t, profile.LayerDestination, "dest@1", &v1.ProfileDocument{PixelRules: []*v1.PixelRule{
		{Name: "accept", Reason: "seen", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}},
	}})
	if _, problems := profile.Stack(base, pixels); len(problems) == 0 || problems[0].Code != profile.CodeLayerPixelRule || problems[0].PixelRule != 1 {
		t.Errorf("a destination's pixel rule: %v", problems)
	}
	if _, problems := profile.Stack(base, route, layer(t, profile.LayerDestination, "dest@1", rules())); problems != nil {
		t.Errorf("a stack without a receiver: %v", problems)
	}
}

// TestLayerListsMerge guards text lists across layers: a layer's list
// joins those below, and its rules naming it run in the effective
// profile; a list of a name below that is the same list is taken once; one
// of a name below with other terms is refused, since rules below name it.
func TestLayerListsMerge(t *testing.T) {
	cat := testCatalog(t)
	tenant := profile.Layer{Kind: profile.LayerTenant, Name: "base@1", Expanded: mustExpand(t, &v1.ProfileDocument{
		TextLists: []*v1.TextList{{Name: "staff", Version: 1, Terms: []string{"Ann Lee"}}},
		Rules:     []*v1.ProfileRule{rl("d", kw("StudyDescription"), cleanLists("staff"))},
	}, cat)}
	recv := profile.Layer{Kind: profile.LayerReceiver, Name: "r@1", Expanded: mustExpand(t, &v1.ProfileDocument{
		TextLists: []*v1.TextList{{Name: "staff", Version: 1, Terms: []string{"Ann Lee"}}, {Name: "sites", Version: 1, Terms: []string{"North Clinic"}}},
		Rules:     []*v1.ProfileRule{rl("i", kw("ImageComments"), cleanLists("sites"))},
	}, cat)}
	eff, problems := profile.Stack(tenant, recv)
	if problems != nil {
		t.Fatal(problems)
	}
	e := eff.Expanded()
	if len(e.GetTextLists()) != 2 || e.GetLanguageEdition() != 3 {
		t.Errorf("lists %d, edition %d", len(e.GetTextLists()), e.GetLanguageEdition())
	}
	clash := profile.Layer{Kind: profile.LayerRoute, Name: "x@1", Expanded: mustExpand(t, &v1.ProfileDocument{
		TextLists: []*v1.TextList{{Name: "staff", Version: 2, Terms: []string{"Someone"}}},
	}, cat)}
	if _, problems := profile.Stack(tenant, clash); len(problems) == 0 || problems[0].Code != profile.CodeDuplicateName {
		t.Errorf("a list of a name below with other terms: %v", problems)
	}
}

// TestEffectiveDescribe guards the effective description: each selector
// once, decided by the last layer that names it, with that layer's kind
// and name and the rule's position and name in it; a conditional rule
// listed apart, marked conditional.
func TestEffectiveDescribe(t *testing.T) {
	eff, problems := profile.Stack(
		layer(t, profile.LayerTenant, "base@1", rules(
			rl("desc", kw("StudyDescription"), clean()),
			rl("station", kw("StationName"), remove()),
		)),
		layer(t, profile.LayerReceiver, "ct@2", rules(
			rl("desc-gone", kw("StudyDescription"), remove()),
			rl("ct-only", kw("ImageComments"), remove(), equals(v1.ConditionSubject_CONDITION_SUBJECT_MODALITY, "CT")),
		)),
	)
	if problems != nil {
		t.Fatal(problems)
	}
	got := eff.Describe()
	if len(got) != 3 {
		t.Fatalf("%d rows: %+v", len(got), got)
	}
	want := []profile.EffectiveRule{
		{What: "StudyDescription", Action: "remove", Layer: profile.LayerReceiver, LayerName: "ct@2", Rule: 1, RuleName: "desc-gone"},
		{What: "StationName", Action: "remove", Layer: profile.LayerTenant, LayerName: "base@1", Rule: 2, RuleName: "station"},
		{What: "ImageComments", Action: "remove", Conditional: true, Layer: profile.LayerReceiver, LayerName: "ct@2", Rule: 2, RuleName: "ct-only"},
	}
	for i, w := range want {
		g := got[i]
		if g.What != w.What || g.Action != w.Action || g.Layer != w.Layer || g.LayerName != w.LayerName || g.Rule != w.Rule || g.RuleName != w.RuleName || g.Conditional != w.Conditional {
			t.Errorf("row %d: %+v, want %+v", i, g, w)
		}
	}
}

// TestLayerOverriddenRuleIsNotJudged guards that a layer is judged by the
// rule that decides an element, not by one of its own that a later rule of
// the same selector replaces: a layer that begins with a base's weaker action
// for an element and then states the stricter one the layer below requires is
// accepted, since the later wins; the same layer with the weaker action last,
// or with no stricter one, is refused.
func TestLayerOverriddenRuleIsNotJudged(t *testing.T) {
	base := layer(t, profile.LayerTenant, "base@1", rules(rl("station", kw("StationName"), remove())))
	for name, c := range map[string]struct {
		doc  *v1.ProfileDocument
		want bool
	}{
		"weaker first, stricter after": {rules(rl("weak", kw("StationName"), empty()), rl("strict", kw("StationName"), remove())), true},
		"stricter first, weaker after": {rules(rl("strict", kw("StationName"), remove()), rl("weak", kw("StationName"), empty())), false},
		"the weaker alone":             {rules(rl("weak", kw("StationName"), empty())), false},
	} {
		_, problems := profile.Stack(base, layer(t, profile.LayerReceiver, "ct@1", c.doc))
		if (problems == nil) != c.want {
			t.Errorf("%s: accepted %v, want %v (%v)", name, problems == nil, c.want, problems)
		}
	}
}

// TestReceptionStacksOverTheBase guards the profile a receiver runs with a
// base: what only the base decides is in it, the stricter of what both
// decide wins, the pixel rules of the base come before the receiver's, a
// receiver that loosens the base gets none, and neither input is changed.
func TestReceptionStacksOverTheBase(t *testing.T) {
	baseDoc := rules(rl("station", kw("StationName"), remove()), rl("desc", kw("StudyDescription"), clean()))
	recvDoc := rules(rl("desc-gone", kw("StudyDescription"), remove()))
	base, recv := mustExpand(t, baseDoc, testCatalog(t)), mustExpand(t, recvDoc, testCatalog(t))
	base.PixelRules = []*v1.ExpandedPixelRule{{}}
	recv.PixelRules = []*v1.ExpandedPixelRule{{}, {}}
	got, problems := profile.Reception("floor@1", base, "ct@1", recv)
	if problems != nil {
		t.Fatal(problems)
	}
	noPixels := proto.Clone(got).(*v1.ExpandedProfile)
	noPixels.PixelRules = nil
	for _, l := range noPixels.Layers {
		l.PixelRules = 0
	}
	prog, err := profile.Compile(noPixels)
	if err != nil {
		t.Fatal(err)
	}
	if d := prog.Decide(profile.Location{Tag: 0x00081010, VR: "SH"}, facts{}); d.Action.GetRemove() == nil {
		t.Errorf("Station Name, which only the base names, is decided by %v", d.Action)
	}
	if d := prog.Decide(profile.Location{Tag: 0x00081030, VR: "LO"}, facts{}); d.Action.GetRemove() == nil {
		t.Errorf("Study Description is decided by %v, want the receiver's remove", d.Action)
	}
	if len(got.GetPixelRules()) != 3 || len(base.GetPixelRules()) != 1 || len(recv.GetPixelRules()) != 2 {
		t.Errorf("%d pixel rules, and the inputs hold %d and %d", len(got.GetPixelRules()), len(base.GetPixelRules()), len(recv.GetPixelRules()))
	}
	loose := mustExpand(t, rules(rl("desc-kept", kw("StudyDescription"), keep())), testCatalog(t))
	if got, problems := profile.Reception("floor@1", base, "mr@1", loose); got != nil || len(problems) == 0 {
		t.Errorf("a receiver that keeps what the base cleans got a profile: %v", problems)
	}
}
