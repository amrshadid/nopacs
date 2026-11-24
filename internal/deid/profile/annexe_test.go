package profile

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/anonymize"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestAnnexECodesBecomeActions guards how a column of PS3.15 Table E.1-1
// becomes a base, on rows written here: each code its action, C by the attribute's VR and the dates
// option, a repeating group's row as exact tags or a group rule, a blank
// code no rule; and the document passes the language's checks, so a base
// built from the library's table is valid as it is.
func TestAnnexECodesBecomeActions(t *testing.T) {
	rows := []annexERow{
		{tag: 0x00100010, name: "Patient's Name", code: "Z"},
		{tag: 0x00100020, name: "Patient ID", code: "Z/D"},
		{tag: 0x00101000, name: "Other Patient IDs", code: "X"},
		{tag: 0x00401101, name: "Person Identification Code Sequence", code: "D"},
		{tag: 0x00080060, name: "Modality", code: "K"},
		{tag: 0x0020000D, name: "Study Instance UID", code: "U"},
		{tag: 0x00081030, name: "Study Description", code: "C"},
		{tag: 0x00080020, name: "Study Date", code: "C"},
		{tag: 0x00080030, name: "Study Time", code: "C"},
		{tag: 0x00080055, name: "Station AE Title", code: "C"},
		{tag: 0x00080080, name: "Institution Name", code: "X/Z/D"},
		{tag: 0x00080090, name: "Referring Physician's Name", code: "X/D"},
		{tag: 0x00081010, name: "Station Name", code: "X/Z"},
		{tag: 0x00081140, name: "Referenced Image Sequence", code: "X/Z/U*"},
		{tag: 0x00104000, name: "Patient Comments", code: ""},
		{tag: 0x60003000, pattern: "(60xx,3000)", name: "Overlay Data", code: "C"},
		{tag: 0x50000000, pattern: "(50xx,xxxx)", name: "Curve Data", code: "C"},
	}
	doc, err := annexEDocument(rows, "Retain Longitudinal Temporal Information with Modified Dates Option", true)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, r := range doc.GetRules() {
		sel := r.GetSelector().GetTag()
		if g := r.GetSelector().GetGroup(); g != "" {
			sel = g
		}
		a := r.GetAction()
		k := actionName(a)
		if u := a.GetRemoveUnlessRequired(); u != nil {
			k += "/" + u.GetFallback().String()
		}
		kinds[sel] = k
	}
	want := map[string]string{
		"00100010": "empty", "00100020": "remove_unless_required/REQUIRED_FALLBACK_DUMMY", "00101000": "remove",
		"00401101": "dummy", "00080060": "keep", "0020000D": "uid", "00081030": "clean", "00080020": "shift",
		"00080030": "keep", "00080055": "dummy",
		"00080080": "remove_unless_required/REQUIRED_FALLBACK_DUMMY", "00080090": "remove_unless_required/REQUIRED_FALLBACK_DUMMY",
		"00081010": "remove_unless_required/REQUIRED_FALLBACK_EMPTY", "00081140": "remove_unless_required/REQUIRED_FALLBACK_UID",
		"60003000": "clean", "601E3000": "clean", "50xx": "remove",
	}
	for sel, k := range want {
		if kinds[sel] != k {
			t.Errorf("%s: %q, want %q", sel, kinds[sel], k)
		}
	}
	if _, ok := kinds["00104000"]; ok {
		t.Error("a blank code made a rule")
	}
	if got := len(doc.GetRules()); got != 14+repeatingGroups+1 {
		t.Errorf("%d rules", got)
	}
	if _, err := NewCatalog(Base{Name: "test/annex-e:1", Document: doc}); err != nil {
		t.Errorf("the document is not a valid base: %v", err)
	}
	plain, err := annexEDocument(rows[7:8], "Clean Descriptors Option", false)
	if err != nil || actionName(plain.GetRules()[0].GetAction()) != "dummy" {
		t.Errorf("C on a date without the dates option: %v, %v", plain, err)
	}
	if _, err := annexEAction("Q", nil, false); err == nil {
		t.Error("an unknown code became an action")
	}
}

// actionName is the name of an action's kind, as the JSON form writes it.
func actionName(a *v1.ProfileAction) string {
	m := a.ProtoReflect()
	if fd := m.WhichOneof(m.Descriptor().Oneofs().ByName("kind")); fd != nil {
		return string(fd.Name())
	}
	return ""
}

// ruleKind is a rule's selector (a tag, or a group) and its action's name,
// with remove_unless_required's fallback.
func ruleKind(r *v1.ProfileRule) (sel, kind string) {
	sel = r.GetSelector().GetTag()
	if g := r.GetSelector().GetGroup(); g != "" {
		sel = g
	}
	kind = actionName(r.GetAction())
	if u := r.GetAction().GetRemoveUnlessRequired(); u != nil {
		kind += "/" + u.GetFallback().String()
	}
	return sel, kind
}

// shippedDocument returns a shipped base's document, failing the test when
// the base is not available.
func shippedDocument(t *testing.T, name string) *v1.ProfileDocument {
	t.Helper()
	b, unavailable, found := Shipped().Lookup(AnnexE(name))
	if !found || unavailable != "" {
		t.Fatalf("%s: found %v, unavailable %q", name, found, unavailable)
	}
	return b.Document
}

// TestAnnexEBasesFollowTheTable guards that each shipped Annex E base is
// the DICOM library's Table E.1-1 (edition 2026d), row for row: every row
// whose column has a code is a rule, in the table's order, a repeating
// group's row its sixteen tags or its group, and the rows of elements the
// pipeline handles in every file none; and that a sample of rows, the
// combined codes among them, became the action PS3.15 E.1.1 gives each,
// as written here from the standard rather than from the adapter.
func TestAnnexEBasesFollowTheTable(t *testing.T) {
	if anonymize.ProfileTableEdition != "2026d" || AnnexEEdition != "2026d" {
		t.Fatalf("the table is edition %s, the bases %s", anonymize.ProfileTableEdition, AnnexEEdition)
	}
	for _, b := range annexE {
		if b.kind != annexEColumn {
			continue
		}
		doc := shippedDocument(t, b.name)
		var want []string
		for e := range anonymize.ProfileEntries() {
			if e.Code(b.column) == "" {
				continue
			}
			t32 := uint32(e.Tag)
			switch {
			case e.Pattern == "" && pipelineOwned(t32):
			case e.Pattern == "":
				want = append(want, fmt.Sprintf("%08X", t32))
			case strings.HasSuffix(e.Pattern, "xx,xxxx)"):
				want = append(want, fmt.Sprintf("%02Xxx", t32>>24))
			default:
				for i := range uint32(16) {
					want = append(want, fmt.Sprintf("%08X", t32+i*2<<16))
				}
			}
		}
		var got []string
		for _, r := range doc.GetRules() {
			sel, _ := ruleKind(r)
			got = append(got, sel)
			if !strings.Contains(r.GetReason(), b.column.String()) {
				t.Errorf("%s: rule %s's reason %q does not name its column", b.name, r.GetName(), r.GetReason())
			}
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: %d rules, the table's column has %d rows with a code, or they differ in order", b.name, len(got), len(want))
		}
	}
	sample := []struct{ base, sel, kind string }{
		{"basic", "00100010", "empty"},                                          // Patient's Name, Z
		{"basic", "00100020", "remove_unless_required/REQUIRED_FALLBACK_DUMMY"}, // Patient ID, Z/D
		{"basic", "00080080", "remove_unless_required/REQUIRED_FALLBACK_DUMMY"}, // Institution Name, X/Z/D
		{"basic", "00080012", "remove_unless_required/REQUIRED_FALLBACK_DUMMY"}, // Instance Creation Date, X/D
		{"basic", "00080022", "remove_unless_required/REQUIRED_FALLBACK_EMPTY"}, // Acquisition Date, X/Z
		{"basic", "00081140", "remove_unless_required/REQUIRED_FALLBACK_UID"},   // Referenced Image Sequence, X/Z/U*
		{"basic", "00081030", "remove"},                                         // Study Description, X
		{"basic", "0020000D", "uid"},                                            // Study Instance UID, U
		{"basic", "0040A730", "dummy"},                                          // Content Sequence, D
		{"basic", "50xx", "remove"},                                             // Curve Data, X
		{"basic", "60003000", "remove"},                                         // Overlay Data, X
		{"basic", "601E3000", "remove"},                                         // Overlay Data, X, the last group
		{"retain-institution-identity", "00080080", "keep"},                     // K
		{"retain-uids", "0020000D", "keep"},                                     // K
		{"retain-patient-characteristics", "00100040", "keep"},                  // Patient's Sex, K
		{"retain-longitudinal-full-dates", "00080020", "keep"},                  // Study Date, K
		{"retain-longitudinal-modified-dates", "00080020", "shift"},             // Study Date, C
		{"retain-longitudinal-modified-dates", "00080033", "keep"},              // Content Time, C: times of day stay
		{"clean-descriptors", "00081030", "clean"},                              // Study Description, C
		{"clean-structured-content", "0040A730", "clean"},                       // Content Sequence, C
		{"clean-graphics", "60003000", "clean"},                                 // Overlay Data, C
		{"clean-graphics", "50xx", "remove"},                                    // Curve Data, C on a group
	}
	for _, c := range sample {
		found := false
		for _, r := range shippedDocument(t, c.base).GetRules() {
			if sel, kind := ruleKind(r); sel == c.sel {
				found = true
				if kind != c.kind {
					t.Errorf("%s %s: %s, want %s", c.base, c.sel, kind, c.kind)
				}
			}
		}
		if !found {
			t.Errorf("%s %s: no rule", c.base, c.sel)
		}
	}
	for _, r := range shippedDocument(t, "basic").GetRules() {
		if sel, _ := ruleKind(r); sel == "00020003" || sel == "00001000" || sel == "FFFAFFFA" || sel == "04000561" {
			t.Errorf("the basic profile names %s, which the pipeline handles", sel)
		}
	}
}

// TestSafePrivateBaseIsTheTable guards the Retain Safe Private base: one
// rule keeping each row of the DICOM library's Table E.3.10-1, matched by
// group, creator and element, each a row the library's lookup confirms,
// and no other rule.
func TestSafePrivateBaseIsTheTable(t *testing.T) {
	doc := shippedDocument(t, "retain-safe-private")
	rows := 0
	for range anonymize.SafePrivateEntries() {
		rows++
	}
	if len(doc.GetRules()) != rows || rows < 400 {
		t.Fatalf("%d rules, the table has %d rows", len(doc.GetRules()), rows)
	}
	for _, r := range doc.GetRules() {
		p := r.GetSelector().GetPrivateElement()
		g, err1 := strconv.ParseUint(p.GetGroup(), 16, 16)
		e, err2 := strconv.ParseUint(p.GetElement(), 16, 8)
		if p == nil || err1 != nil || err2 != nil || actionName(r.GetAction()) != "keep" {
			t.Fatalf("rule %s: %v", r.GetName(), r)
		}
		if _, ok := anonymize.LookupSafePrivate(uint16(g), p.GetCreator(), byte(e)); !ok {
			t.Errorf("rule %s keeps %s (%s,xx%s), which Table E.3.10-1 does not list", r.GetName(), p.GetCreator(), p.GetGroup(), p.GetElement())
		}
	}
}

// TestAnnexEBasesAreFrozen guards that an Annex E base of this edition
// expands to the same text in every build: the checksum of each, expanded
// alone, is the one written here. A DICOM library whose table changes
// under the same edition fails here, since a base's name promises one
// meaning; a new edition is a new name.
func TestAnnexEBasesAreFrozen(t *testing.T) {
	want := map[string]string{
		"basic":                              "984ee921c82e391244d4f8d1b98eb39e5f287f23ddb95af4e3493514a833f4d9",
		"clean-graphics":                     "488105a6b472678f9c2e0b4b67f917ac0832d16ff282e31a6f0f261e5fb4585c",
		"clean-structured-content":           "ba3cefd1c61854a8d2c896b246d2204f649d05dc9064d0d4b4b63389d26ebc49",
		"clean-descriptors":                  "fe81bc1f2a1b0c92d7847c16c8af937fec2c5c8869a8593973f90132b3801b9f",
		"retain-longitudinal-full-dates":     "b4684ecf0d1d7f942bca8d6d13d7ca244784380dddfddcb88c3982816734462d",
		"retain-longitudinal-modified-dates": "92683ef98a8cf4ee4be1cfdbe6d7bdf823de40c9688815d20d3f6371568ca8a9",
		"retain-patient-characteristics":     "862d9a40ad9e217458c0e746a6573afe76ed1b16a5ab35b29fae24b6160c97bd",
		"retain-device-identity":             "6640e59bf8c6e072cd2a1b58155357e59e2996bf9eb703fd7b74568bdfb284c0",
		"retain-uids":                        "6132497d1183b78c1d6a2305e0f155350fd05b47c56622ffb497cfcf80fc3022",
		"retain-safe-private":                "2c1d5157db5129bfb955c5d6d73760fa9e4c044e51b10d50fbbb3f8ec109478a",
		"retain-institution-identity":        "b7e6c779ea254e0a3a21121adf1616b81d5c2620bc06b0583bf2406ff282ccb5",
	}
	for _, b := range annexE {
		if b.kind == annexEVisualFeatures || b.kind == annexECleanPixels {
			continue
		}
		v, err := New("frozen", 1, &v1.ProfileDocument{Bases: []string{AnnexE(b.name)}}, Shipped())
		if err != nil {
			t.Fatalf("%s: %v", b.name, err)
		}
		if want[b.name] != v.Checksum() {
			t.Errorf("%s: checksum %s, want %s", b.name, v.Checksum(), want[b.name])
		}
	}
}

// TestMethodCodesFollowTheStoredNames guards that the codes a version
// records follow the options its stored base names give, whatever their
// edition, in the order of the profile and its options, and that a base of
// another family has none.
func TestMethodCodesFollowTheStoredNames(t *testing.T) {
	var got []string
	for _, c := range MethodCodes([]string{"annex-e/retain-uids:2026d", "nopacs/strict:1", "annex-e/basic:2025a"}) {
		got = append(got, c.Value)
	}
	if strings.Join(got, " ") != "113100 113110" {
		t.Errorf("codes %v", got)
	}
	if _, _, ok := AnnexEName("nopacs/strict:1"); ok {
		t.Error("a composite read as an Annex E base")
	}
}
