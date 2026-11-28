package profile_test

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestReceptionStricterOfTheLayersWins guards that an element both layers
// decide is decided by the stricter decision, whatever the tier of the
// selectors: a base's tag rule (hash) never weakens a receiver's rule of a
// whole VR (remove); a receiver's tag rule that loosens a base's rule of a
// whole VR is refused; and a stack of three layers does the same.
func TestReceptionStricterOfTheLayersWins(t *testing.T) {
	cat := testCatalog(t)
	pid := profile.Location{Tag: 0x00100020, VR: "LO"}
	for name, c := range map[string]struct {
		base, recv *v1.ProfileDocument
	}{
		"a base tag rule over a receiver's VR rule": {rules(rl("pid", kw("PatientID"), hash())), rules(rl("all-lo", vr("LO"), remove()))},
	} {
		out, problems := profile.Reception("base@1", mustExpand(t, c.base, cat), "r@1", mustExpand(t, c.recv, cat))
		if problems != nil {
			t.Errorf("%s: refused: %v", name, problems)
			continue
		}
		prog, err := profile.Compile(out)
		if err != nil {
			t.Fatal(err)
		}
		if d := prog.Decide(pid, facts{}); d.Action.GetRemove() == nil {
			t.Errorf("%s: Patient ID is decided by %v, want remove", name, d.Action)
		}
	}
	if out, problems := profile.Reception("base@1", mustExpand(t, rules(rl("all-lo", vr("LO"), remove())), cat), "r@1",
		mustExpand(t, rules(rl("pid", kw("PatientID"), hash())), cat)); out != nil || len(problems) == 0 {
		t.Errorf("a receiver's tag rule that loosens the base's VR rule was stacked: %v", problems)
	}
	// A base that removes and a receiver that keeps is refused, as before.
	if out, problems := profile.Reception("base@1", mustExpand(t, rules(rl("pid", kw("PatientID"), remove())), cat), "r@1",
		mustExpand(t, rules(rl("pid", kw("PatientID"), keep())), cat)); out != nil || len(problems) == 0 {
		t.Errorf("a receiver that keeps what the base removes was stacked: %v", problems)
	}
	// Three layers: the strictest of all.
	eff, problems := profile.Stack(layer(t, profile.LayerTenant, "a@1", rules(rl("pid", kw("PatientID"), remove()))),
		layer(t, profile.LayerReceiver, "b@1", rules(rl("all-lo", vr("LO"), remove()))),
		layer(t, profile.LayerRoute, "c@1", rules(rl("pid", kw("PatientID"), remove()))))
	if problems != nil {
		t.Fatal(problems)
	}
	prog, err := profile.Compile(eff.Expanded())
	if err != nil {
		t.Fatal(err)
	}
	if d := prog.Decide(pid, facts{}); d.Action.GetRemove() == nil {
		t.Errorf("three layers decide Patient ID by %v, want remove", d.Action)
	}
}

// TestReceptionBasePixelRulesDecideFirst guards the pixel rules of a stacked
// profile: an image the base's rules match is decided by them and never by a
// receiver's, even a catch-all accept after them; one no base rule matches is
// decided by the receiver's rules, the last that matches.
func TestReceptionBasePixelRulesDecideFirst(t *testing.T) {
	cat := testCatalog(t)
	redact := &v1.PixelRule{Name: "banner", Rows: 512, Columns: 512, Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{
		Rectangles: []*v1.PixelRectangle{{Width: 100, Height: 20}}}}}
	accept := &v1.PixelRule{Name: "all", Reason: "the scanner draws nothing", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}}
	base := mustExpand(t, &v1.ProfileDocument{PixelRules: []*v1.PixelRule{redact}}, cat)
	recv := mustExpand(t, &v1.ProfileDocument{PixelRules: []*v1.PixelRule{accept}}, cat)
	out, problems := profile.Reception("base@1", base, "r@1", recv)
	if problems != nil {
		t.Fatal(problems)
	}
	prog, err := profile.Compile(out)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := prog.Pixels(512, 512, facts{}); !ok || r.GetRule().GetRedact() == nil {
		t.Errorf("a 512x512 image is decided by %v; the base redacts it", r)
	}
	if r, ok := prog.Pixels(480, 640, facts{}); !ok || r.GetRule().GetAccept() == nil {
		t.Errorf("an image no base rule matches is decided by %v, want the receiver's accept", r)
	}
	// With no base pixel rule the receiver's alone decide, as before.
	bare := mustExpand(t, rules(), cat)
	out, problems = profile.Reception("base@1", bare, "r@1", recv)
	if problems != nil {
		t.Fatal(problems)
	}
	prog, _ = profile.Compile(out)
	if r, ok := prog.Pixels(512, 512, facts{}); !ok || r.GetRule().GetAccept() == nil {
		t.Errorf("with no base pixel rule the image is decided by %v", r)
	}
}
