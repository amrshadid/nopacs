package engine

import (
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
)

// TestVocabularyUIDs guards vocabularyUIDTags and publicCodingSchemeUIDs:
// whether the profile keeps the element (no rule names it, or a rule keeps
// it) or remaps every UID (a VR rule on UI), a public coding scheme's UID
// (SNOMED CT, LOINC) and one under the standard's root (DCM, a context
// group) are kept as they are, while a local Coding Scheme UID, Mapping
// Resource UID or Context UID, and every Context Group Extension Creator
// UID that is not registered, are remapped: at the top level and inside a
// code sequence's item.
func TestVocabularyUIDs(t *testing.T) {
	type want struct {
		el
		kept bool
	}
	cases := []want{
		{el{0x0008010C, "UI", "2.16.840.1.113883.6.96"}, true},
		{el{0x00080118, "UI", "2.16.840.1.113883.6.1"}, true},
		{el{0x00080117, "UI", "1.2.840.10008.6.1.2"}, true},
		{el{0x0008010D, "UI", "1.2.3.4.5"}, false},
	}
	local := []want{
		{el{0x0008010C, "UI", "1.2.3.4.6"}, false},
		{el{0x00080118, "UI", "1.2.3.4.7"}, false},
		{el{0x00080117, "UI", "1.2.3.4.8"}, false},
		{el{0x0008010C, "UI", "1.2.840.10008.2.16.4"}, true},
	}
	keepRule := rule("keep-scheme", kw("CodingSchemeUID"), aKeep)
	for name, en := range map[string]*Engine{
		"no rule":     newTestEngine(t, testKeys, Options{}),
		"keep rule":   newTestEngine(t, testKeys, Options{}, keepRule),
		"uid VR rule": newTestEngine(t, testKeys, Options{}, rule("uids", vrSel("UI"), aUID)),
	} {
		for _, set := range [][]want{cases, local} {
			var els []el
			for _, c := range set {
				els = append(els, c.el)
			}
			code := build(t, append([]el{{0x00080100, "SH", "T-A0100"}, {0x00080102, "SH", "SRT"}}, els...)...)
			ds := build(t, append([]el{{0x00080018, "UI", "1.2.3.4.99"}, {0x00081032, "SQ", []*dataset.Dataset{code}}}, els...)...)
			apply(t, en, ds)
			for _, where := range []*dataset.Dataset{ds, item(t, ds, 0x00081032, 0)} {
				for _, c := range set {
					got := mustText(t, where, c.tag)
					if kept := got == c.value; kept != c.kept {
						t.Errorf("%s: %08X %v became %q, want kept %v", name, c.tag, c.value, got, c.kept)
					}
				}
			}
		}
	}
	if !Registered("2.16.840.1.113883.6.96") || Registered("1.2.3.4.6") {
		t.Error("a public coding scheme is not registered, or a local UID is")
	}
}
