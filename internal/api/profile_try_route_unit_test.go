package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// sessionsByStudy is a trial's catalog over a fixed list of sessions, by
// Study Instance UID; it counts the lookups.
type sessionsByStudy struct {
	sessions map[string]tenant.SessionLabels
	asked    []string
}

// CatalogLabels implements trialCatalog.
func (s *sessionsByStudy) CatalogLabels(_ context.Context, study string, _ []string) (tenant.CatalogLabels, error) {
	s.asked = append(s.asked, study)
	if x, ok := s.sessions[study]; ok {
		return tenant.CatalogLabels{Session: &x}, nil
	}
	return tenant.CatalogLabels{}, nil
}

// routingCT reads the public CT file the trial tests route.
func routingCT(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "receive", "testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// byCallingAE is a route condition on the calling AE title.
func byCallingAE(ae string) tenant.RouteCondition {
	return tenant.RouteCondition{Subject: tenant.SubjectCallingAETitle, Test: tenant.TestEquals, Value: ae}
}

// TestATrialOfAHeldFileGoesByItsSessionsChoice guards a held file of a
// study the catalog already has a session for: it is tried by the
// session's recorded route, looked up by the study UID its record names,
// and never by a fresh routing of the receiver's routes, which would hold
// it as unrouted here; a file with no session (or one sent with the
// request) is routed by the receiver's routes.
func TestATrialOfAHeldFileGoesByItsSessionsChoice(t *testing.T) {
	const study = "2.25.1234"
	rcv := tenant.Receiver{Name: "deid", Profile: tenant.ProfileRef{Name: "research", Version: 1},
		Routing: tenant.Routing{QuarantineUnmatched: true, Routes: []tenant.Route{{Name: "lab-a", Storage: "main",
			Conditions: []tenant.RouteCondition{byCallingAE("LAB-A")}}}}}
	cat := &sessionsByStudy{sessions: map[string]tenant.SessionLabels{study: {Storage: "main", Route: tenant.ReceptionRoute{Route: "lab-a"}}}}
	tr := &trialRouting{catalog: cat, en: &engine.Engine{}, limit: 64 << 20, routed: map[tenant.ProfileRef]routedEngine{}}
	in := trialInput{record: "q1", file: routingCT(t), study: study, receiver: &rcv, received: engine.Reception{CallingAETitle: "OTHER"}}
	got, err := tr.of(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if got.unrouted != nil || got.routeName != "lab-a" || got.profile != nil {
		t.Errorf("a held file whose study has a session: route %q, unrouted %v, profile %v; want lab-a, routed", got.routeName, got.unrouted, got.profile)
	}
	if len(cat.asked) != 1 || cat.asked[0] != study {
		t.Errorf("the catalog was asked for %v, want the record's study", cat.asked)
	}
	in.study = "2.25.999"
	if got, err = tr.of(context.Background(), in); err != nil || got.unrouted["cause"] != "no_match" {
		t.Errorf("a held file whose study has no session: unrouted %v (%v), want no_match", got.unrouted, err)
	}
	in.study, in.record = "", ""
	if got, err = tr.of(context.Background(), in); err != nil || got.unrouted["cause"] != "no_match" || len(cat.asked) != 2 {
		t.Errorf("a file sent with the request: unrouted %v (%v), catalog asked %d times", got.unrouted, err, len(cat.asked))
	}
}

// TestATrialAtAnAsReceivedReceiverUsesNoRouteProfile guards the trial's
// agreement with reception (tenant.ReceptionRoute.ProfileFor): at a
// receiver that archives as received, neither a route's profile nor a
// session's recorded one is used; the file is tried with the profile
// asked for, and the route is still named.
func TestATrialAtAnAsReceivedReceiverUsesNoRouteProfile(t *testing.T) {
	const study = "2.25.1234"
	strict := tenant.ProfileRef{Name: "strict", Version: 1}
	// Stored past the validation that refuses it, as a receiver changed
	// meanwhile would be read.
	rcv := tenant.Receiver{Name: "raw", Routing: tenant.Routing{Routes: []tenant.Route{{Name: "lab-a", Storage: "main",
		Conditions: []tenant.RouteCondition{byCallingAE("LAB-A")}, Profile: strict}}}}
	cat := &sessionsByStudy{sessions: map[string]tenant.SessionLabels{study: {Storage: "main", Route: tenant.ReceptionRoute{Route: "lab-a", Profile: strict}}}}
	asked := &engine.Engine{}
	tr := &trialRouting{catalog: cat, en: asked, limit: 64 << 20, routed: map[tenant.ProfileRef]routedEngine{}}
	for what, in := range map[string]trialInput{
		"a route's profile":   {file: routingCT(t), receiver: &rcv, received: engine.Reception{CallingAETitle: "LAB-A"}},
		"a session's profile": {record: "q1", study: study, file: routingCT(t), receiver: &rcv},
	} {
		got, err := tr.of(context.Background(), in)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got.profile != nil || got.en != asked || got.routeName != "lab-a" {
			t.Errorf("%s at a receiver that archives as received: profile %v, the engine asked for %v, route %q",
				what, got.profile, got.en == asked, got.routeName)
		}
	}
}
