package receive

import (
	"bytes"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// tightenEngine compiles the tests' profile with extra rules under the
// tests' keys, naming the version ref in the method record.
func tightenEngine(t *testing.T, ref tenant.ProfileRef, extra ...*v1.ProfileRule) *engine.Engine {
	t.Helper()
	doc := basicProfile()
	doc.Rules = append(doc.Rules, extra...)
	v, err := profile.New(ref.Name, ref.Version, doc, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	en, err := engine.New(v.Expanded(), testDeidKeys, engine.Options{Method: ref.String()})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// tightenRule is a rule of one keyword with an action.
func tightenRule(name, keyword string, action *v1.ProfileAction) *v1.ProfileRule {
	return &v1.ProfileRule{Name: name, Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: keyword}}, Action: action}
}

// archivedByV1 de-identifies a corpus file with the first version, as
// reception archives it.
func archivedByV1(t *testing.T, en *engine.Engine) []byte {
	t.Helper()
	b := corpus(t, "ct5n-2062.dcm")
	o := TrialOptions{Keys: testDeidKeys, HeaderLimit: header.MaxHeader, Check: func(n int64) Checker { return check.New(check.Options{MaxFileBytes: n}) }}
	tr, err := TryFile(t.Context(), en, b, engine.Reception{}, o)
	if err != nil || tr.Hold != nil || tr.Written == nil {
		t.Fatalf("the first version holds the corpus file: %+v %v", tr.Hold, err)
	}
	return tr.Written
}

func tightenOptions() TightenOptions {
	return TightenOptions{HeaderLimit: header.MaxHeader, Check: func(n int64) Checker { return check.New(check.Options{MaxFileBytes: n}) }}
}

// presence reads whether a file holds an element, and its UIDs and Patient ID.
func presence(t *testing.T, b []byte, at tag.Tag) (has bool, ids [4]string) {
	t.Helper()
	h, err := header.ReadWithin(bytes.NewReader(b), int64(len(b)), header.MaxHeader)
	if err != nil {
		t.Fatal(err)
	}
	ds, _, _, err := h.Dataset(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	has = ds.Contains(at)
	for i, x := range []tag.Tag{tag.New(0x0008, 0x0018), tag.New(0x0020, 0x000D), tag.New(0x0020, 0x000E), tag.New(0x0010, 0x0020)} {
		ids[i] = dsUID(ds, x)
	}
	return has, ids
}

var (
	removeAction = &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	hashAction   = &v1.ProfileAction{Kind: &v1.ProfileAction_Hash{Hash: &v1.HashAction{}}}
)

// TestTightenFileRemovesWhatTheNewerVersionRemoves guards the re-run's
// core: an archived file that holds an element the newer version removes
// comes back without it, its UIDs and Patient ID exactly as archived (a
// re-run never runs the keyed actions again), its method record naming the
// newer version; a file the newer version changes nothing in is not
// rewritten.
func TestTightenFileRemovesWhatTheNewerVersionRemoves(t *testing.T) {
	v1ref, v2ref := tenant.ProfileRef{Name: "research", Version: 1}, tenant.ProfileRef{Name: "research", Version: 2}
	older := tightenEngine(t, v1ref)
	newer := tightenEngine(t, v2ref, tightenRule("kvp", "KVP", removeAction))
	archived := archivedByV1(t, older)
	if has, _ := presence(t, archived, tag.New(0x0018, 0x0060)); !has {
		t.Fatal("the first version removed KVP, so there is nothing to tighten")
	}
	got, err := TightenFile(t.Context(), newer, older, archived, engine.Reception{}, tightenOptions())
	if err != nil || got.Hold != nil || got.Written == nil {
		t.Fatalf("tighten: hold %+v err %v", got.Hold, err)
	}
	if len(got.Changes) != 1 || got.Changes[0].Tag != 0x00180060 || got.Changes[0].Action != "remove" {
		t.Fatalf("changes = %+v, want KVP removed", got.Changes)
	}
	has, after := presence(t, got.Written, tag.New(0x0018, 0x0060))
	_, before := presence(t, archived, tag.New(0x0018, 0x0060))
	if has {
		t.Error("the new revision still holds KVP")
	}
	if after != before {
		t.Errorf("UIDs or Patient ID changed by the re-run: %v became %v", before, after)
	}
	h, _ := header.ReadWithin(bytes.NewReader(got.Written), int64(len(got.Written)), header.MaxHeader)
	ds, _, _, _ := h.Dataset(bytes.NewReader(got.Written))
	if method := dsUID(ds, tag.New(0x0012, 0x0063)); !bytes.Contains([]byte(method), []byte("research@2")) {
		t.Errorf("method record %q does not name the newer version", method)
	}
	same, err := TightenFile(t.Context(), older, older, archived, engine.Reception{}, tightenOptions())
	if err != nil || same.Hold != nil || same.Written != nil || len(same.Changes) != 0 {
		t.Errorf("the same version changed something: %+v %v", same, err)
	}
}

// TestTightenFileHoldsWhatNeedsTheOriginal guards that an element the newer
// version would transform with a key is not made from the archived value,
// which would break the links between records: the file is held with a
// fixed cause and nothing is written.
func TestTightenFileHoldsWhatNeedsTheOriginal(t *testing.T) {
	older := tightenEngine(t, tenant.ProfileRef{Name: "research", Version: 1})
	newer := tightenEngine(t, tenant.ProfileRef{Name: "research", Version: 2}, tightenRule("filter", "FilterType", hashAction))
	archived := archivedByV1(t, older)
	got, err := TightenFile(t.Context(), newer, older, archived, engine.Reception{}, tightenOptions())
	if err != nil || got.Hold == nil || got.Hold.Details["cause"] != TightenNeedsOriginal || got.Written != nil {
		t.Fatalf("a keyed tightening: hold %+v written %v err %v", got.Hold, got.Written != nil, err)
	}
}

// TestTightenFileHoldsWhenTheCheckAfterFinds guards that a file the check
// after holds is not written: here the removed value stays in another
// element.
func TestTightenFileHoldsWhenTheCheckAfterFinds(t *testing.T) {
	keep := &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}
	older := tightenEngine(t, tenant.ProfileRef{Name: "research", Version: 1}, tightenRule("station", "StationName", keep))
	newer := tightenEngine(t, tenant.ProfileRef{Name: "research", Version: 2}, tightenRule("station", "StationName", removeAction))
	archived := archivedByV1(t, older)
	// A station name the first version kept, and the same text in an element
	// the newer version keeps.
	copied := rewrite(t, archived, "", func(ds *dataset.Dataset) {
		_ = ds.SetTextValue(tag.New(0x0008, 0x1010), "SH", "STA-7")
		_ = ds.SetTextValue(tag.New(0x0018, 0x1210), "SH", "STA-7")
	})
	got, err := TightenFile(t.Context(), newer, older, copied, engine.Reception{}, tightenOptions())
	if err != nil || got.Hold == nil || got.Hold.Reason != tenant.QuarantineResidualValue || got.Written != nil {
		t.Fatalf("a value left behind: hold %+v written %v err %v", got.Hold, got.Written != nil, err)
	}
}
