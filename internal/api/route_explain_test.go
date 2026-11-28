package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ctStudy are the files of one CT study of two series, a Scout of 2 and a
// SmartScore of 5.
var ctStudy = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"}

// routedArchive sets lab1's scanner1 up with a rule set (an archive rule
// for CT, a forward rule for the SmartScore series), activates it, and
// receives the CT study through it.
func routedArchive(t *testing.T, f *placeFixture) *archive {
	t.Helper()
	f.receiving(t)
	if _, err := f.pipelineOf("lab1").AddSender(ctx, tenant.Sender{Name: "pacs", Type: tenant.SenderDIMSE, Enabled: true,
		DIMSE: tenant.DIMSESender{Host: "pacs.example", Port: 4242, CalledAETitle: "PACS", CallingAETitle: "NOPACS"}},
		f.now(), f.setupEntry(), nil); err != nil {
		t.Fatal(err)
	}
	mustCall[*v1.CreateRuleSetResponse](t, "rule set", createRuleSet("scanner1", &v1.RuleSet{
		ArchiveRules: []*v1.ArchiveRule{{Name: "ct", Storage: "main", When: `Modality == "CT" and calling_ae == "PRISMA1"`}},
		ForwardRules: []*v1.ForwardRule{{Name: "smart", Series: `SeriesDescription contains "smartscore"`, Senders: []string{"pacs"}}},
	}, true)(f.as(local)))
	files := make([][]byte, len(ctStudy))
	for i, n := range ctStudy {
		files[i] = corpusFile(t, n)
	}
	return receiveArchive(t, f, files)
}

// TestDryRunMatchesLiveRouting guards a dry run: over the receiver's
// latest sessions, the version that routed them gives what live routing
// did (the same archive rule and forward rules, from one metadata file per
// scan and the session's AE titles), a version that routes otherwise is
// counted as changed, and the hits count what each rule routed.
func TestDryRunMatchesLiveRouting(t *testing.T) {
	f := newPlaceFixture(t)
	routedArchive(t, f)
	dry := func(version int32) *v1.DryRunRoutesResponse {
		t.Helper()
		return mustCall[*v1.DryRunRoutesResponse](t, "dry run", call(f.as(f.lab1Operator).routes.DryRunRoutes,
			&v1.DryRunRoutesRequest{Tenant: "lab1", Receiver: "scanner1", Version: version, Sessions: 10}))
	}
	same := dry(1)
	if same.GetSame() != 1 || len(same.GetSessions()) != 1 {
		t.Fatalf("the version that routed the session: %v", same)
	}
	s := same.GetSessions()[0]
	if s.GetRecordedArchiveRule() != "ct" || s.GetArchiveRule() != "ct" || len(s.GetForwardRules()) != 1 || s.GetForwardRules()[0] != "smart" ||
		len(s.GetRecordedForwardRules()) != 1 || s.GetRecordedVersion() != 1 {
		t.Errorf("the session: %v", s)
	}
	mustCall[*v1.CreateRuleSetResponse](t, "version 2", createRuleSet("scanner1", &v1.RuleSet{
		ArchiveRules: []*v1.ArchiveRule{{Name: "mr", Storage: "main", When: `Modality == "MR"`}},
	}, false)(f.as(local)))
	changed := dry(2)
	if changed.GetChanged() != 1 || changed.GetSessions()[0].GetArchiveRule() != "" {
		t.Errorf("a version that routes otherwise: %v", changed)
	}
	f.run(t, []tcase{{name: "a reader dry-runs", as: f.lab1Reader, call: func(c clients) outcome {
		return call(c.routes.DryRunRoutes, &v1.DryRunRoutesRequest{Tenant: "lab1", Receiver: "scanner1"})
	}, code: connect.CodePermissionDenied}})
	hits := mustCall[*v1.GetRouteHitsResponse](t, "hits", call(f.as(f.lab1Reader).routes.GetRouteHits,
		&v1.GetRouteHitsRequest{Tenant: "lab1", Receiver: "scanner1", Version: 1})).GetHits()
	got := map[string][2]int64{}
	for _, h := range hits {
		got[h.GetRule()] = [2]int64{h.GetReceptions(), h.GetSeries()}
	}
	if got["ct"] != [2]int64{1, 0} || got["smart"] != [2]int64{1, 1} || len(got) != 2 {
		t.Errorf("the hits: %v", got)
	}
}

// TestExplainRoute guards an explanation: over a file sent with the
// request, each rule's result and the comparison that decided it, with its
// tag, VR and whether the file holds it, and no value unless show_values,
// which the audit entry records; over an archived instance, the series
// filter's result too, its AE titles the session's.
func TestExplainRoute(t *testing.T) {
	f := newPlaceFixture(t)
	a := routedArchive(t, f)
	explain := func(show bool, src *v1.ExplainRouteRequest) *v1.ExplainRouteResponse {
		t.Helper()
		src.Tenant, src.Receiver, src.ShowValues = "lab1", "scanner1", show
		return mustCall[*v1.ExplainRouteResponse](t, "explain", call(f.as(f.lab1Operator).routes.ExplainRoute, src))
	}
	file := &v1.ExplainRouteRequest{Source: &v1.ExplainRouteRequest_File{File: corpusFile(t, "MR_small.dcm")}, CallingAeTitle: "PRISMA1"}
	res := explain(false, file)
	if res.GetVersion() != 1 || res.GetDecision().GetArchiveRule() != "" || res.GetDecision().GetStorage() != "main" {
		t.Errorf("an MR file: %v", res.GetDecision())
	}
	ct := res.GetRules()[0]
	dc := ct.GetDecidedBy()
	if ct.GetRule() != "ct" || ct.GetTruth() != v1.RouteTruth_ROUTE_TRUTH_FALSE || dc.GetField() != "Modality" || dc.GetTag() != "(0008,0060)" ||
		dc.GetVr() != "CS" || !dc.GetPresent() || len(dc.GetValues()) != 0 || dc.GetComparison() != `Modality == "CT"` {
		t.Errorf("rule ct over an MR file: %v", ct)
	}
	shown := explain(true, file).GetRules()[0].GetDecidedBy()
	if len(shown.GetValues()) != 1 || shown.GetValues()[0] != "MR" {
		t.Errorf("with show_values: %v", shown)
	}
	f.run(t, []tcase{
		{name: "values asked", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.routes.ExplainRoute, &v1.ExplainRouteRequest{Tenant: "lab1", Receiver: "scanner1", ShowValues: true, Source: file.GetSource()})
		}, audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "scanner1", details: map[string]string{"show_values": "true", "source": "file"}}},
		{name: "a reader explains", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.routes.ExplainRoute, &v1.ExplainRouteRequest{Tenant: "lab1", Receiver: "scanner1", Source: file.GetSource()})
		}, code: connect.CodePermissionDenied},
		{name: "no source", as: f.lab1Operator, call: func(c clients) outcome {
			return call(c.routes.ExplainRoute, &v1.ExplainRouteRequest{Tenant: "lab1", Receiver: "scanner1"})
		}, code: connect.CodeInvalidArgument},
	})
	var smartScan string
	for _, p := range a.scans(t) {
		if len(a.held(t, p)) == 5 {
			smartScan = p.Scan
			in := &v1.ExplainRouteRequest{Source: &v1.ExplainRouteRequest_Instance{Instance: &v1.RouteInstance{
				SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan}}}
			res := explain(false, in)
			if res.GetDecision().GetArchiveRule() != "ct" || len(res.GetDecision().GetForwardRules()) != 1 {
				t.Errorf("the SmartScore scan's instance: %v", res.GetDecision())
			}
			series := res.GetRules()[len(res.GetRules())-1]
			if series.GetPhase() != v1.RulePhase_RULE_PHASE_SERIES || series.GetTruth() != v1.RouteTruth_ROUTE_TRUTH_TRUE {
				t.Errorf("the series filter over it: %v", series)
			}
		}
	}
	if smartScan == "" {
		t.Fatal("no scan of 5 instances")
	}
}
