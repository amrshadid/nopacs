package profile

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"

	"github.com/amrshadid/go-dicom/anonymize"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// How a column of PS3.15 Table E.1-1 becomes a base. The DICOM library
// holds the table (anonymize.ProfileTable), every attribute with its code
// in the basic profile's column and in each option's, the combined codes
// unresolved, and NoPACs keeps no copy of it. Each column is read into
// annexERows and annexEDocument makes the base's rules, one per attribute
// the column gives a code, in the table's order, so a later base (an
// option) overrides an earlier one (the basic profile) as PS3.15 E.3 says
// an option does. A blank code makes no rule: the column says nothing of
// the attribute, and the bases before it stand. A row for an element the
// pipeline handles in every file (Pipeline: the file meta information,
// command and directory elements, digital signatures, Original Attributes
// Sequence) makes no rule either: no rule may name one, and the pipeline
// removes each from every file, which is what the table asks of them or
// more.
//
// The codes of PS3.15 E.1.1 and the action each becomes (annexEAction):
//
//	X       remove
//	Z       empty
//	D       dummy
//	K       keep
//	C       clean, for the VRs clean fits (LO, LT, SH, ST, UC, UT; SQ, whose
//	        items are walked; OB, OW, which are removed). Under the Retain
//	        Longitudinal Temporal Information with Modified Dates option, a
//	        date (DA, DT) is shifted and a time (TM) kept, since the shift
//	        moves dates by whole days and keeps times of day (PS3.15 E.3.6).
//	        On any other VR (an AE title under Retain Device Identity, an
//	        age) a dummy: a value that holds nothing of the original and is
//	        valid for its VR, which is what C asks for when there is no
//	        free text to clean.
//	U       uid
//	X/Z     remove_unless_required{EMPTY}
//	X/D     remove_unless_required{DUMMY}
//	X/Z/D   remove_unless_required{DUMMY}
//	X/Z/U*  remove_unless_required{UID}
//	Z/D     remove_unless_required{DUMMY}: Type 1 dummy and Type 2 empty,
//	        as Z/D says, but Type 3 removed rather than emptied. An
//	        attribute the IOD does not require may be absent, so the file
//	        stays valid, and removing it keeps no less than emptying it.
//
// remove_unless_required resolves a combined code by the attribute's Type
// in the IOD of the file's SOP class, when the file is processed (PS3.15
// E.1.1: "X unless Z is required to maintain IOD conformance"), and an
// attribute whose Type is not known takes the fallback.
//
// A row of a repeating group (PS3.5 7.6) becomes exact tags: "(60xx,3000)"
// one rule for each even group from 6000 to 601E, so no rule reaches the
// other elements of those groups; "(50xx,xxxx)", every element of the
// curve groups, becomes a group rule "50xx", which may only keep, remove
// or empty, so its C (Clean Graphics) removes, as clean does to the bits
// of a curve.

// annexERow is one attribute of one column of Table E.1-1.
type annexERow struct {
	// tag is the attribute's tag, or the first of a repeating group's.
	tag uint32
	// pattern is empty for an attribute of one tag, and the standard's
	// text for a repeating group: "(60xx,3000)", "(50xx,xxxx)".
	pattern string
	// name is the attribute's name as the table gives it.
	name string
	// code is the column's code, empty where it is blank.
	code string
}

// repeatingGroups is how many groups a repeating group spans: the even
// groups gg00 to gg1E (PS3.5 7.6).
const repeatingGroups = 16

// annexEDocument returns the rules of one column of Table E.1-1 as a
// document: column names it in each rule's reason, and modifiedDates is
// set for the Retain Longitudinal Temporal Information with Modified Dates
// option.
func annexEDocument(rows []annexERow, column string, modifiedDates bool) (*v1.ProfileDocument, error) {
	doc := &v1.ProfileDocument{}
	for _, r := range rows {
		if r.code == "" {
			continue
		}
		reason := fmt.Sprintf("PS3.15 Table E.1-1, %s: %s, %s", column, r.name, r.code)
		switch {
		case r.pattern == "":
			act, err := annexEAction(r.code, dictVRs(r.tag), modifiedDates)
			if err != nil {
				return nil, fmt.Errorf("profile: %s: %w", r.name, err)
			}
			doc.Rules = append(doc.Rules, annexERule(r.tag, act, reason))
		case strings.HasSuffix(r.pattern, "xx,xxxx)"):
			act := &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
			switch r.code {
			case "K":
				act = &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}
			case "Z":
				act = &v1.ProfileAction{Kind: &v1.ProfileAction_Empty{Empty: &v1.EmptyAction{}}}
			}
			g := fmt.Sprintf("%02Xxx", r.tag>>24)
			doc.Rules = append(doc.Rules, &v1.ProfileRule{
				Name:     "e1-" + strings.ToLower(g),
				Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Group{Group: g}},
				Action:   act, Reason: reason,
			})
		default:
			act, err := annexEAction(r.code, dictVRs(r.tag), modifiedDates)
			if err != nil {
				return nil, fmt.Errorf("profile: %s: %w", r.name, err)
			}
			for i := range uint32(repeatingGroups) {
				doc.Rules = append(doc.Rules, annexERule(r.tag+i*2<<16, act, reason))
			}
		}
	}
	return doc, nil
}

// annexERule is the rule of one attribute of Table E.1-1.
func annexERule(t uint32, act *v1.ProfileAction, reason string) *v1.ProfileRule {
	return &v1.ProfileRule{
		Name:     "e1-" + strings.ToLower(formatTag(t)),
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Tag{Tag: formatTag(t)}},
		Action:   act, Reason: reason,
	}
}

// annexEAction returns the action a code of Table E.1-1 becomes for an
// attribute of the VRs vrs (the data dictionary's), as the comment above
// lists.
func annexEAction(code string, vrs []string, modifiedDates bool) (*v1.ProfileAction, error) {
	unless := func(f v1.RequiredFallback) *v1.ProfileAction {
		return &v1.ProfileAction{Kind: &v1.ProfileAction_RemoveUnlessRequired{RemoveUnlessRequired: &v1.RemoveUnlessRequiredAction{Fallback: f}}}
	}
	all := func(set []string) bool {
		for _, v := range vrs {
			if !slices.Contains(set, v) {
				return false
			}
		}
		return len(vrs) > 0
	}
	switch code {
	case "X":
		return &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}, nil
	case "Z":
		return &v1.ProfileAction{Kind: &v1.ProfileAction_Empty{Empty: &v1.EmptyAction{}}}, nil
	case "D":
		return &v1.ProfileAction{Kind: &v1.ProfileAction_Dummy{Dummy: &v1.DummyAction{}}}, nil
	case "K":
		return &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}, nil
	case "U":
		return &v1.ProfileAction{Kind: &v1.ProfileAction_Uid{Uid: &v1.UidAction{}}}, nil
	case "C":
		switch {
		case modifiedDates && all([]string{"DA", "DT"}):
			return &v1.ProfileAction{Kind: &v1.ProfileAction_Shift{Shift: &v1.ShiftAction{}}}, nil
		case modifiedDates && all([]string{"TM"}):
			return &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}, nil
		case all(cleanVRs):
			return &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{}}}, nil
		}
		return &v1.ProfileAction{Kind: &v1.ProfileAction_Dummy{Dummy: &v1.DummyAction{}}}, nil
	case "X/Z":
		return unless(v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY), nil
	case "X/D", "X/Z/D", "Z/D":
		return unless(v1.RequiredFallback_REQUIRED_FALLBACK_DUMMY), nil
	case "X/Z/U*":
		return unless(v1.RequiredFallback_REQUIRED_FALLBACK_UID), nil
	}
	return nil, fmt.Errorf("%q is not a code of PS3.15 Table E.1-1", code)
}

// annexERows returns one column of the DICOM library's Table E.1-1, in the
// table's order, without the rows of elements the pipeline handles in
// every file.
func annexERows(col anonymize.ProfileColumn) []annexERow {
	var out []annexERow
	for e := range anonymize.ProfileEntries() {
		t := uint32(e.Tag)
		if e.Pattern == "" && pipelineOwned(t) {
			continue
		}
		out = append(out, annexERow{tag: t, pattern: e.Pattern, name: e.Name, code: e.Code(col)})
	}
	return out
}

// safePrivateDocument returns the rules of PS3.15 Table E.3.10-1, the
// Retain Safe Private option, from the DICOM library's table
// (anonymize.SafePrivateEntries): one rule keeping each private attribute
// the table lists, matched by its group, its private creator and its
// element within the creator's block, never by the block a file put it in
// (PS3.5 7.8.1). A private element no rule keeps is removed, so the option
// is these rules and nothing else. The rules are in the table's order
// (creator, group, element). A rule's name is its group, its element and
// the first four bytes of the SHA-256 of its creator, since a creator's
// text is longer than a name may be and holds characters a name may not.
func safePrivateDocument() *v1.ProfileDocument {
	doc := &v1.ProfileDocument{}
	for e := range anonymize.SafePrivateEntries() {
		h := sha256.Sum256([]byte(e.Creator))
		vr := string(e.VR)
		if vr == "" {
			vr = "not given"
		}
		doc.Rules = append(doc.Rules, &v1.ProfileRule{
			Name: fmt.Sprintf("e3-%04x-%02x-%x", e.Group, e.Element, h[:4]),
			Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_PrivateElement{PrivateElement: &v1.PrivateElement{
				Creator: e.Creator, Group: fmt.Sprintf("%04X", e.Group), Element: fmt.Sprintf("%02X", e.Element),
			}}},
			Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}},
			Reason: fmt.Sprintf("PS3.15 Table E.3.10-1: %s (%04X,xx%02X), VR %s", e.Creator, e.Group, e.Element, vr),
		})
	}
	return doc
}
