package cli_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestDeidCoverage guards "deid coverage": the tenant is required (exit 2);
// before any job has ended it says there is no report and how to make one,
// and as JSON answers a null report and no elements; --refresh makes the job, waits for
// it and shows the new report, each element by its tag, VR and private
// creator with its counts, a creator the server withheld as "(withheld)",
// and the report's header with what it was made from; the report is shown
// again without --refresh; a partial report says how many scans could not be
// read; and nothing but tags, VRs, vendor names and counts is printed.
func TestDeidCoverage(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	if res := r.run("deid", "coverage"); res.code != 2 {
		t.Errorf("no tenant: %+v, want exit 2", res)
	}
	out := mustOut(r, "deid", "coverage", "lab1")
	if !strings.Contains(out, "No coverage report yet") || !strings.Contains(out, "nopacs deid coverage lab1 --refresh") {
		t.Errorf("before any job: %q", out)
	}
	if res := r.run("deid", "coverage", "lab1", "--json"); res.code != 0 || !strings.Contains(res.stdout, `"report": null`) || !strings.Contains(res.stdout, `"elements": []`) {
		t.Errorf("before any job, as JSON: %+v", res)
	}

	r.fake.SetCoverageRefresh("lab1", &v1.CoverageReport{ScansRead: 4, ScansSkipped: 1, Instances: 10, InstancesRecorded: 8, InstancesCapped: 1,
		ElementsNotListed: 7, DistinctElements: 3}, []*v1.CoverageElement{
		{Tag: "0029,xx10", Private: true, PrivateCreator: "ACME RESEARCH", Vr: "LO", Instances: 8, Occurrences: 8},
		{Tag: "0018,0050", Vr: "DS", Instances: 8, Occurrences: 8},
		{Tag: "0031,xx02", Private: true, CreatorWithheld: true, Vr: "UN", Instances: 2, Occurrences: 3},
	})
	out = mustOut(r, "deid", "coverage", "lab1", "--refresh")
	for _, want := range []string{"partial: 1 scan(s) could not be read", "Scans read", "10, 8 with a record", "0018,0050", "DS", "0029,xx10", "ACME RESEARCH",
		"(withheld)", "PRIVATE CREATOR", "OCCURRENCES", "Not listed", "7 occurrences"} {
		if !strings.Contains(out, want) {
			t.Errorf("--refresh shows %q, missing %q", out, want)
		}
	}
	// In order of tag.
	if strings.Index(out, "0018,0050") > strings.Index(out, "0029,xx10") || strings.Index(out, "0029,xx10") > strings.Index(out, "0031,xx02") {
		t.Errorf("the elements are not in order of tag: %q", out)
	}
	again := mustOut(r, "deid", "coverage", "lab1")
	if again != out {
		t.Errorf("without --refresh the report is %q, want %q", again, out)
	}
	res := r.run("deid", "coverage", "lab1", "--json")
	if res.code != 0 || !strings.Contains(res.stdout, `"creatorWithheld": true`) && !strings.Contains(res.stdout, `"creatorWithheld":true`) || !strings.Contains(res.stdout, "ACME RESEARCH") {
		t.Errorf("as JSON: %+v", res)
	}
	list := mustOut(r, "job", "list", "lab1")
	if !strings.Contains(list, "deid coverage") {
		t.Errorf("the job list is %q", list)
	}
}

// TestProfileDescribeEffective guards "profile describe --effective": it
// takes a tenant and a receiver and no document; it lists each selector of
// the receiver's profile stacked over the tenant's base profile with its
// action, the layer it came from and that layer's profile; with no base the
// receiver's own rules are the answer; a receiver that archives as received
// has no profile (exit 5).
func TestProfileDescribeEffective(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	dir := t.TempDir()
	base := writeFile(t, dir, "base.json", `{"bases": ["annex-e/basic:2026d"], "rules": [
 {"name": "kvp", "selector": {"keyword": "KVP"}, "action": {"remove": {}}},
 {"name": "station", "selector": {"keyword": "StationName"}, "action": {"remove": {}}}]}`)
	recv := writeFile(t, dir, "recv.json", `{"bases": ["annex-e/basic:2026d"], "rules": [
 {"name": "station-gone", "selector": {"keyword": "StationName"}, "action": {"remove": {}}}]}`)
	r.must("profile", "create", "lab1", "floor", "--document", base)
	r.must("profile", "create", "lab1", "station", "--document", recv)
	r.fake.AddReceiver("lab1", &v1.Receiver{Name: "scanner1", Deidentification: &v1.Receiver_Profile{Profile: &v1.ProfileVersionRef{Name: "station", Version: 1}}})
	r.fake.AddReceiver("lab1", &v1.Receiver{Name: "raw", Deidentification: &v1.Receiver_AsReceived{AsReceived: &v1.AsReceived{}}})

	out := mustOut(r, "profile", "describe", "--effective", "lab1", "scanner1")
	rows := map[string][]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 4 && (f[0] == "KVP" || f[0] == "StationName") {
			rows[f[0]] = f
		}
	}
	if len(rows["StationName"]) < 4 || rows["StationName"][1] != "remove" || rows["StationName"][2] != "receiver" || rows["StationName"][3] != "station@1" {
		t.Errorf("with no base, StationName is %v in %q, want remove by the receiver's station@1", rows["StationName"], out)
	}
	if len(rows["KVP"]) != 0 {
		t.Errorf("with no base, KVP, which no profile names, is listed: %v", rows["KVP"])
	}
	r.must("setting", "set", "deid.base-profile", "floor@1", "--tenant", "lab1")
	out = mustOut(r, "profile", "describe", "--effective", "lab1", "scanner1")
	rows = map[string][]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 4 && (f[0] == "KVP" || f[0] == "StationName") {
			rows[f[0]] = f
		}
	}
	if len(rows["KVP"]) < 4 || rows["KVP"][1] != "remove" || rows["KVP"][2] != "tenant" || rows["KVP"][3] != "floor@1" {
		t.Errorf("with a base, KVP is %v in %q, want remove by the tenant layer's floor@1", rows["KVP"], out)
	}
	if len(rows["StationName"]) < 4 || rows["StationName"][2] != "receiver" {
		t.Errorf("with a base, StationName is %v, want the receiver layer", rows["StationName"])
	}
	if !strings.Contains(out, "Receiver:") || !strings.Contains(out, "station@1") {
		t.Errorf("the header is missing: %q", out)
	}
	if res := r.run("profile", "describe", "--effective", "lab1", "scanner1", "--json"); res.code != 0 || !strings.Contains(res.stdout, `"layerProfile": "floor@1"`) && !strings.Contains(res.stdout, `"layerProfile":"floor@1"`) {
		t.Errorf("as JSON: %+v", res)
	}
	if res := r.run("profile", "describe", "--effective", "lab1", "raw"); res.code != 5 {
		t.Errorf("a receiver that archives as received: %+v, want exit 5", res)
	}
	for _, args := range [][]string{{"lab1"}, {"lab1", "scanner1", "--document", base}} {
		if res := r.run(append([]string{"profile", "describe", "--effective"}, args...)...); res.code != 2 {
			t.Errorf("describe --effective %v: %+v, want exit 2", args, res)
		}
	}
}
