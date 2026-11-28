package receive

import (
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// sliceThickness is Slice Thickness (0018,0050), an element the basic profile
// does not name and a CT file carries.
const sliceThickness = "0050"

// baseDoc is basicProfile with the rules given added: a tenant's base.
func baseDoc(rules ...*v1.ProfileRule) *v1.ProfileDocument {
	doc := basicProfile()
	doc.Rules = append(doc.Rules, rules...)
	return doc
}

// removeKeyword is a rule removing the element of a keyword.
func removeKeyword(name, keyword string) *v1.ProfileRule {
	return &v1.ProfileRule{Name: name, Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: keyword}},
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}}
}

// setBase stores doc as the profile version "floor@1" and makes it the
// tenant's base, as the API's setting does.
func setBase(t *testing.T, e *env, ref tenant.ProfileRef, doc *v1.ProfileDocument) {
	t.Helper()
	e.storeProfile(ref, doc)
	if err := e.store.SetSetting(e.ctx, settings.DeidBaseProfile, ref.String(), e.audit(), nil); err != nil {
		t.Fatal(err)
	}
}

// archivedInstance returns the one instance a reception archived, as a data
// set, and its sidecar line.
func archivedInstance(t *testing.T, e *env, r tenant.Reception) ([]byte, string) {
	t.Helper()
	var obj []byte
	var side string
	for k, v := range archivedObjects(t, e, r) {
		switch {
		case len(k) > 8 && k[:8] == "sidecar:":
			side = string(v)
		case len(k) > 9 && k[:9] == "manifest:":
		default:
			obj = v
		}
	}
	return obj, side
}

// TestBaseRemovesWhatTheReceiverDoesNotName guards that a reception runs the
// receiver's profile stacked over the tenant's base: an element the base
// removes and the receiver's profile does not name is gone from the archived
// file, where it stays without the base; the line records both versions,
// still with the receiver's as its profile, so the instance counts as having
// its profile provenance; and a base equal to the receiver's profile changes
// not one byte.
func TestBaseRemovesWhatTheReceiverDoesNotName(t *testing.T) {
	file := studyFiles[2]
	plain := newEnv(t, envOptions{deid: &deidOptions{}})
	plain.send(file)
	without, _ := archivedInstance(t, plain, plain.settle(1)[0])
	if !decoded(t, without).HasTag("0018", sliceThickness) {
		t.Fatalf("the control file has no Slice Thickness: the test proves nothing")
	}

	based := newEnv(t, envOptions{deid: &deidOptions{}})
	floor := tenant.ProfileRef{Name: "floor", Version: 1}
	setBase(t, based, floor, baseDoc(removeKeyword("thickness", "SliceThickness")))
	based.send(file)
	got, side := archivedInstance(t, based, based.settle(1)[0])
	if decoded(t, got).HasTag("0018", sliceThickness) {
		t.Errorf("the archived file still holds Slice Thickness, which the tenant's base removes")
	}
	_, lines := sidecarOf(t, based, scanPaths(t, based, based.receptions())[0])
	if len(lines) != 1 || lines[0].Profile != testProfile.String() || lines[0].BaseProfile != floor.String() || lines[0].ProfileSHA256 == "" {
		t.Errorf("the line names profile %q and base %q (checksum %q), want %s over %s", lines[0].Profile, lines[0].BaseProfile, lines[0].ProfileSHA256, testProfile, floor)
	}
	if code, refused := tenant.SendRefusal(tenant.InstanceProvenance{Profile: testProfile}, false); refused {
		t.Errorf("an instance with its profile recorded is refused to send: %s", code)
	}
	_ = side

	same := newEnv(t, envOptions{deid: &deidOptions{}})
	setBase(t, same, testProfile, basicProfile())
	same.send(file)
	got, _ = archivedInstance(t, same, same.settle(1)[0])
	if string(got) != string(without) {
		t.Errorf("a base equal to the receiver's profile changed the archived file")
	}
}

// TestBaseChangeTakesEffectForTheNextReception guards that the base is read
// for each file: one set between two receptions is applied to the second and
// not the first, and one removed is no longer applied to the third.
func TestBaseChangeTakesEffectForTheNextReception(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	has := func(r tenant.Reception) bool {
		obj, _ := archivedInstance(t, e, r)
		return decoded(t, obj).HasTag("0018", sliceThickness)
	}
	seen := map[string]bool{}
	// next returns the reception that ended since the last call.
	next := func(n int) tenant.Reception {
		for _, r := range e.settle(n) {
			if !seen[r.ID] {
				seen[r.ID] = true
				return r
			}
		}
		t.Fatalf("no new reception")
		return tenant.Reception{}
	}
	e.send(studyFiles[0])
	if !has(next(1)) {
		t.Fatalf("the control file has no Slice Thickness")
	}
	setBase(t, e, tenant.ProfileRef{Name: "floor", Version: 1}, baseDoc(removeKeyword("thickness", "SliceThickness")))
	e.send(studyFiles[2])
	if has(next(2)) {
		t.Errorf("the reception after the base was set still holds Slice Thickness")
	}
	if _, err := e.store.UnsetSetting(e.ctx, settings.DeidBaseProfile, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.send(studyFiles[1])
	if !has(next(3)) {
		t.Errorf("the reception after the base was removed lost Slice Thickness")
	}
}

// TestBaseTheReceiverLoosensHoldsTheFile guards a base that a receiver's
// profile would loosen, which no change through the API leaves (each checks
// it) but a database changed by hand could: the file is held as deid_failed,
// profile_unusable, never archived with less than the base requires.
func TestBaseTheReceiverLoosensHoldsTheFile(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{doc: baseDoc(&v1.ProfileRule{Name: "keep-thickness",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "SliceThickness"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}})}})
	setBase(t, e, tenant.ProfileRef{Name: "floor", Version: 1}, baseDoc(removeKeyword("thickness", "SliceThickness")))
	e.send(studyFiles[2])
	e.settle(1)
	qs := e.held()
	if len(qs) != 1 || qs[0].Reason != tenant.QuarantineDeidFailed || qs[0].Details["cause"] != "profile_unusable" {
		t.Fatalf("held %+v, want one record, deid_failed, profile_unusable", qs)
	}
}

// TestBaseListIsInTheCheckAfter guards that the check after runs on what the
// stacked profile says: a text list only the base holds finds its term in an
// element the receiver keeps, and holds the file (residual_value, list_term),
// which the receiver's profile alone archives.
func TestBaseListIsInTheCheckAfter(t *testing.T) {
	body := func() []byte {
		return rewrite(t, corpus(t, studyFiles[2]), "", func(ds *dataset.Dataset) {
			ds.Remove(tag.New(0x0018, 0x0015))
			if err := ds.Add(dataelem.NewDataElement(tag.New(0x0018, 0x0015), dataelem.VR("CS"), []byte("ZZSECRET "))); err != nil {
				t.Fatal(err)
			}
		})
	}
	control := newEnv(t, envOptions{deid: &deidOptions{}})
	control.sendBytes(body(), Received)
	control.settle(1)
	if len(control.held()) != 0 {
		t.Fatalf("the control file is held: %+v", control.held())
	}
	doc := baseDoc(&v1.ProfileRule{Name: "clean-description",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyDescription"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{Lists: []string{"staff"}}}}})
	doc.TextLists = []*v1.TextList{{Name: "staff", Version: 1, Terms: []string{"ZZSECRET"}}}
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	setBase(t, e, tenant.ProfileRef{Name: "floor", Version: 1}, doc)
	e.sendBytes(body(), Received)
	e.settle(1)
	qs := e.held()
	if len(qs) != 1 || qs[0].Reason != tenant.QuarantineResidualValue || qs[0].Details["kind"] != "list_term" {
		t.Fatalf("held %+v, want one record, residual_value, list_term", qs)
	}
}
