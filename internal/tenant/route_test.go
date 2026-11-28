package tenant

import (
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/query"
)

// routeFile returns a data set holding the attributes given, each tag with
// its VR and value as written, as routing reads a received file.
func routeFile(elems ...struct {
	t  tag.Tag
	vr dataelem.VR
	v  string
}) *dataset.Dataset {
	ds := dataset.NewDataset()
	for _, e := range elems {
		_ = ds.Add(dataelem.NewDataElement(e.t, e.vr, []byte(e.v)))
	}
	return ds
}

// el is one element of routeFile.
func el(g, e uint16, vr dataelem.VR, v string) struct {
	t  tag.Tag
	vr dataelem.VR
	v  string
} {
	return struct {
		t  tag.Tag
		vr dataelem.VR
		v  string
	}{tag.New(g, e), vr, v}
}

// subject returns the subject of a file from caller.
func subject(ds *dataset.Dataset, caller Caller) query.Subject {
	return query.Subject{Dataset: ds, Catalog: RouteCatalog(caller), Now: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)}
}

// ctMR is a rule set with two archive rules, a default that holds, and two
// forward rules, one with a series filter.
func ctMR() RuleSet {
	return RuleSet{Receiver: "recv", Version: 1, QuarantineUnmatched: true,
		Archive: []ArchiveRule{
			{Name: "site-a", When: `calling_ae == "SITE_A"`, Storage: "a"},
			{Name: "mr", When: `Modality = "MR"`, Storage: "b", Profile: ProfileRef{Name: "strict", Version: 1}},
		},
		Forward: []ForwardRule{
			{Name: "every", Senders: []string{"pacs"}},
			{Name: "t1", When: `Modality = "MR"`, Series: `SeriesDescription contains "t1"`, Senders: []string{"ai"}},
			{Name: "ct", When: `Modality == "CT"`, Senders: []string{"pacs"}},
		}}
}

// TestRuleSetFirstArchiveMatch guards the archive phase: the first archive
// rule that holds chooses, in order, whatever later rules say; none, and
// the phase has no choice (the default decides); and the forward phase
// evaluates every forward rule whatever the archive phase chose.
func TestRuleSetFirstArchiveMatch(t *testing.T) {
	c := ctMR().Compile()
	mr := routeFile(el(0x0008, 0x0060, dataelem.CS, "MR"), el(0x0008, 0x103E, dataelem.LO, "T1 MPRAGE"))
	ev := c.Evaluate(subject(mr, Caller{CallingAETitle: "SITE_A"}))
	if ev.Archive != 0 || ev.Unusable != "" {
		t.Errorf("an MR file from SITE_A: archive rule %d, want the first, site-a, though mr also holds", ev.Archive)
	}
	if len(ev.Forward) != 2 || ev.Forward[0] != 0 || ev.Forward[1] != 1 {
		t.Errorf("an MR file: forward rules %v, want every and t1", ev.Forward)
	}
	ev = c.Evaluate(subject(mr, Caller{CallingAETitle: "SITE_B"}))
	if ev.Archive != 1 {
		t.Errorf("an MR file from SITE_B: archive rule %d, want mr", ev.Archive)
	}
	ct := routeFile(el(0x0008, 0x0060, dataelem.CS, "CT"))
	ev = c.Evaluate(subject(ct, Caller{}))
	if ev.Archive != -1 || ev.ArchiveUnknown || ev.Unusable != "" {
		t.Errorf("a CT file from no AE: %+v, want no archive rule", ev)
	}
	if len(ev.Forward) != 2 || ev.Forward[1] != 2 {
		t.Errorf("a CT file: forward rules %v, want every and ct", ev.Forward)
	}
}

// TestRuleSetAbsentIsFalse guards that a field the file does not hold makes
// a comparison false, never an error, and present/absent test for it: a
// file with no Modality matches no rule on it and is no rule's error.
func TestRuleSetAbsentIsFalse(t *testing.T) {
	rs := RuleSet{Receiver: "recv", Archive: []ArchiveRule{
		{Name: "mr", When: `Modality = "MR"`, Storage: "a"},
		{Name: "not-ct", When: `not Modality = "CT"`, Storage: "b"},
		{Name: "no-modality", When: `Modality absent`, Storage: "c"},
	}}
	ev := rs.Compile().Evaluate(subject(routeFile(el(0x0010, 0x0040, dataelem.CS, "F")), Caller{}))
	if ev.Unusable != "" || ev.Archive != 1 {
		t.Fatalf("a file with no Modality: %+v, want not-ct (the comparison false, its negation true)", ev)
	}
	if ev.Outcomes[0].Truth != query.False || ev.Outcomes[0].Decided == nil {
		t.Errorf("the comparison on an absent field: %+v, want false, decided by its leaf", ev.Outcomes[0])
	}
}

// TestRuleSetRemovedIsUnknown guards a dry run over a de-identified header:
// a leaf on an attribute the profile removed is unknown, an archive phase
// that meets an unknown rule before any that holds cannot say which rule
// chooses, and an unknown forward rule is neither forwarded nor dropped.
func TestRuleSetRemovedIsUnknown(t *testing.T) {
	c := ctMR().Compile()
	s := subject(routeFile(el(0x0008, 0x0060, dataelem.CS, "MR")), Caller{CallingAETitle: "SITE_B"})
	s.Removed = func(f query.Field) bool { return len(f.Path) == 1 && f.Path[0].Tag == 0x00080060 }
	ev := c.Evaluate(s)
	if ev.Archive != -1 || !ev.ArchiveUnknown {
		t.Errorf("Modality removed: %+v, want the archive choice unknown", ev)
	}
	if len(ev.ForwardUnknown) != 2 || len(ev.Forward) != 1 {
		t.Errorf("Modality removed: forward %v unknown %v, want every forwarded and t1, ct unknown", ev.Forward, ev.ForwardUnknown)
	}
	if o := ev.Outcomes[1]; o.Truth != query.Unknown || o.Decided == nil || query.FieldString(o.Decided.Field) != "Modality" {
		t.Errorf("the outcome of mr: %+v, want unknown, decided by Modality", o)
	}
}

// TestRuleSetSeriesFilter guards a forward rule's series filter: it holds
// for a series whose first file matches it, folded, and for every series of
// a rule with none.
func TestRuleSetSeriesFilter(t *testing.T) {
	c := ctMR().Compile()
	t1 := routeFile(el(0x0008, 0x103E, dataelem.LO, "Sag T1 MPRAGE"))
	t2 := routeFile(el(0x0008, 0x103E, dataelem.LO, "Ax T2 FLAIR"))
	if !c.SeriesFiltered(1) || c.SeriesFiltered(0) {
		t.Fatal("t1 has a series filter and every has none")
	}
	if o := c.Series(1, subject(t1, Caller{})); o.Truth != query.True {
		t.Errorf("a T1 series: %+v, want the filter to hold", o)
	}
	if o := c.Series(1, subject(t2, Caller{})); o.Truth != query.False {
		t.Errorf("a T2 series: %+v, want the filter not to hold", o)
	}
	if o := c.Series(0, subject(t2, Caller{})); o.Truth != query.True {
		t.Errorf("a rule with no filter over a series: %+v, want it to hold", o)
	}
}

// TestRuleSetUnusable guards a rule that cannot be evaluated: a value longer
// than routing reads, or a condition this version cannot read (as a later
// version may store), makes the archive phase stop there and hold the
// study, never fall through to a later rule; a forward rule so is not
// forwarded.
func TestRuleSetUnusable(t *testing.T) {
	long := routeFile(el(0x0008, 0x0070, dataelem.LO, strings.Repeat("x", query.MaxReadLength+2)), el(0x0008, 0x0060, dataelem.CS, "MR"))
	rs := RuleSet{Receiver: "recv",
		Archive: []ArchiveRule{{Name: "maker", When: `Manufacturer = "x"`, Storage: "a"}, {Name: "any", Storage: "b"}},
		Forward: []ForwardRule{{Name: "maker", When: `Manufacturer = "x"`, Senders: []string{"p"}}}}
	rs.Forward[0].Name = "fwd-maker"
	ev := rs.Compile().Evaluate(subject(long, Caller{}))
	if ev.Unusable != "maker" || ev.Archive != -1 {
		t.Errorf("a value past the read bound: %+v, want maker unusable and no fall-through", ev)
	}
	if len(ev.Forward) != 0 || ev.Outcomes[len(ev.Outcomes)-1].Unusable == "" {
		t.Errorf("a forward rule past the read bound: forwarded %v, want none and its outcome unusable", ev.Forward)
	}
	stored := RuleSet{Receiver: "recv", Archive: []ArchiveRule{{Name: "later", When: `Modality sounds "MR"`, Storage: "a"}, {Name: "any", Storage: "b"}}}
	if ev := stored.Compile().Evaluate(subject(long, Caller{})); ev.Unusable != "later" {
		t.Errorf("a condition this version cannot read: %+v, want it unusable", ev)
	}
}

// TestRouteConditionRefusesPatient guards that no routing condition reads
// who the patient is: every attribute the de-identification floor names
// (Patient's Name and ID, birth date, other IDs, any person's name), at
// the top or inside a sequence, is refused, as are "has", a catalog field
// routing does not give, and a condition that does not parse; ordinary
// attributes, the AE titles and private tags by creator are taken.
func TestRouteConditionRefusesPatient(t *testing.T) {
	for _, text := range []string{
		`PatientName = "DOE"`, `PatientID == "123"`, `PatientBirthDate > 2000-01-01`, `OtherPatientIDsSequence present`,
		`ReferringPhysicianName prefix "Dr"`, `RequestAttributesSequence.PatientID present`, `(0010,0010) present`,
		`has scan (Modality = "MR")`, `session.date > -1d`, `Modality =`,
	} {
		if err := ValidateRouteCondition(text); fault.KindOf(err) != fault.Invalid {
			t.Errorf("%s: %v, want Invalid", text, err)
		} else if strings.Contains(err.Error(), "DOE") || strings.Contains(err.Error(), "123") {
			t.Errorf("%s: the refusal %q quotes a value", text, err)
		}
	}
	for _, text := range []string{
		`Modality in ("CT", "MR") and SeriesDescription contains "t1"`, `calling_ae == "SITE_A" or called_ae prefix "ARCH"`,
		`(0029,"SIEMENS CSA HEADER",10) present`, `ImageType[2] == "LOCALIZER"`, `PatientSex = "F"`,
	} {
		if err := ValidateRouteCondition(text); err != nil {
			t.Errorf("%s: %v", text, err)
		}
	}
}

// TestRuleSetValidate guards the rule set's own bounds: names unique across
// both phases, a forward rule with senders, each once, and a condition that
// reads a patient refused with the rule's name.
func TestRuleSetValidate(t *testing.T) {
	if err := ctMR().Validate(); err != nil {
		t.Fatalf("a valid rule set: %v", err)
	}
	for name, change := range map[string]func(*RuleSet){
		"two rules of one name":  func(r *RuleSet) { r.Forward[0].Name = "mr" },
		"no senders":             func(r *RuleSet) { r.Forward[0].Senders = nil },
		"a sender twice":         func(r *RuleSet) { r.Forward[0].Senders = []string{"pacs", "pacs"} },
		"a patient's name":       func(r *RuleSet) { r.Archive[0].When = `PatientName present` },
		"a series filter on one": func(r *RuleSet) { r.Forward[1].Series = `PatientID present` },
		"no storage":             func(r *RuleSet) { r.Archive[0].Storage = "" },
		"a comment of two lines": func(r *RuleSet) { r.Comment = "a\nb" },
	} {
		rs := ctMR()
		change(&rs)
		if err := rs.Validate(); fault.KindOf(err) != fault.Invalid {
			t.Errorf("%s: %v, want Invalid", name, err)
		}
	}
}

// TestReceiverRuleSetProfile guards that only a receiver that
// de-identifies runs an archive rule naming a profile: the receiver's
// choice decides how a file lands before a rule is evaluated; and that the
// rule's profile applies only while it does.
func TestReceiverRuleSetProfile(t *testing.T) {
	r := Receiver{Name: "recv", Type: ReceiverDIMSE, Storage: "s", QuietPeriod: DefaultQuietPeriod, MaxOpenTime: DefaultMaxOpenTime,
		MaxFileBytes: DefaultMaxFileBytes, DIMSE: DIMSEReceiver{Port: 11112, AETitle: "A"},
		Routing: RuleSet{Receiver: "recv", Version: 1, Archive: []ArchiveRule{{Name: "r", Storage: "s", Profile: ProfileRef{Name: "p", Version: 1}}}}}
	if err := r.Validate(); fault.KindOf(err) != fault.Precondition {
		t.Errorf("an archive rule with a profile on a receiver that archives as received: %v, want a precondition failure", err)
	}
	r.Profile = ProfileRef{Name: "basic", Version: 1}
	if err := r.Validate(); err != nil {
		t.Errorf("an archive rule with a profile on a receiver that de-identifies: %v", err)
	}
	if got := (ReceptionRoute{Profile: ProfileRef{Name: "p", Version: 1}}).ProfileFor(r); got.Name != "p" {
		t.Errorf("a rule's profile on a receiver that de-identifies gives %v", got)
	}
	r.Profile = ProfileRef{}
	if got := (ReceptionRoute{Profile: ProfileRef{Name: "p", Version: 1}}).ProfileFor(r); !got.IsZero() {
		t.Errorf("a rule's profile on a receiver changed to archive as received gives %v, want none", got)
	}
	r.Routing.Receiver = "other"
	if err := r.Validate(); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a receiver running another receiver's rule set: %v, want Invalid", err)
	}
}

// TestReleasedProfileComesFirst guards the profile version a release named
// for a reception: at a receiver that de-identifies it comes before the
// rule's and the receiver's; at one changed to archive as received none
// applies; and a release naming one through a receiver that archives as
// received, or of a record made de-identified through such a receiver, is
// a precondition failure.
func TestReleasedProfileComesFirst(t *testing.T) {
	rcv := Receiver{Name: "recv", Profile: ProfileRef{Name: "basic", Version: 1}}
	released, routed := ProfileRef{Name: "fixed", Version: 2}, ProfileRef{Name: "p", Version: 1}
	if got := (ReceptionRoute{ReleasedProfile: released, Profile: routed}).ProfileFor(rcv); got != released {
		t.Errorf("a released and a rule's profile give %v, want the released", got)
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

// TestReceptionRouteMatches guards the records a reception's choice gives:
// its archive rule (the default as an empty rule) when a rule set made it,
// each forward with its version and series count, and a later arrival's
// choice (Archived) carrying no forwards and no released choices.
func TestReceptionRouteMatches(t *testing.T) {
	r := ReceptionRoute{RuleSet: 2, ForwardRuleSet: 3, ReleasedStorage: "s",
		Forwards: []ForwardChoice{{Rule: "t1", Senders: []string{"p"}, Filtered: true, Series: []string{"1.2.3", "1.2.4"}}}}
	m := r.Matches()
	if len(m) != 2 || m[0] != (RouteMatch{Version: 2, Phase: PhaseArchive}) || m[1] != (RouteMatch{Version: 3, Phase: PhaseForward, Rule: "t1", Series: 2}) {
		t.Errorf("the records: %+v", m)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("a valid choice: %v", err)
	}
	if a := r.Archived(); a.ReleasedStorage != "" || len(a.Forwards) != 0 || a.ForwardRuleSet != 0 || a.RuleSet != 2 {
		t.Errorf("the choice a later arrival takes: %+v", a)
	}
	if (ReceptionRoute{}).Matches() != nil {
		t.Error("a reception no rule set routed has records")
	}
	bad := r
	bad.Forwards = []ForwardChoice{{Rule: "t1", Senders: []string{"p"}, Filtered: true}}
	if err := bad.Validate(); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a filtered forward with no series: %v, want Invalid", err)
	}
}
