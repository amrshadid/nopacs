package engine

import (
	"fmt"
	"strconv"

	"github.com/amrshadid/go-dicom/anonymize"

	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// Cause is the fixed reason a file could not be de-identified. A program
// tests it; a person reads Failure's text, which never holds a value of the
// file.
type Cause string

// The causes. Each is a closed word the quarantine records as the detail of
// its reason, so a list of held files can be grouped by it.
const (
	// CauseNoSubject: a date had to be shifted and the file has no Patient
	// ID (0010,0020) to find its subject's offset by.
	CauseNoSubject Cause = "no_subject"
	// CauseLookupMissing: a lookup table holds no code for a value and the
	// rule says to quarantine.
	CauseLookupMissing Cause = "lookup_missing"
	// CauseBadValue: a value the action must read or write is not one of
	// its VR (PS3.5 6.2), is too long to process, is not in memory, or the
	// action does not fit the VR the file gives the element.
	CauseBadValue Cause = "bad_value"
	// CauseTooDeep: sequences nest deeper than MaxDepth.
	CauseTooDeep Cause = "too_deep"
	// CauseTooLarge: the data set holds more than MaxElements elements,
	// sequences' items included; or its cleans, or its value patterns and
	// redacts, would pass their work's bound for one file.
	CauseTooLarge Cause = "too_large"
	// CauseRuleQuarantine: a rule whose action is quarantine applies to an
	// element the file holds; Failure.Rule names it.
	CauseRuleQuarantine Cause = "rule"
	// CauseInternal: the engine itself failed on the file (a panic,
	// recovered), and every change it had made was undone. The file is
	// held like any other refusal, so a fault of the code is never a file
	// archived half de-identified.
	CauseInternal Cause = "internal"
	// CauseBurnedInRisk: the image may carry burned-in text, by its header
	// as received, and no pixel rule matches it; Failure.RiskCause says
	// why it is at risk. Its reason is ReasonBurnedIn.
	CauseBurnedInRisk Cause = "burned_in_risk"
	// CausePixelsTooLarge: the image needs work on its pixels and its
	// decoded size passes Image.Limit.
	CausePixelsTooLarge Cause = "pixels_too_large"
	// CausePixelsUnsupported: a pixel rule cannot fill an image of its
	// layout (dataset.Dataset.RedactPixelRegions says which it refuses).
	CausePixelsUnsupported Cause = "pixels_unsupported"
	// CausePixelsUndecodable: a frame of the image, or a JPEG segment of
	// it, does not decode; for an image filled as it is written, the
	// writer's error wraps ErrPixelsUndecodable instead.
	CausePixelsUndecodable Cause = "pixels_undecodable"
	// CausePixelRegionOutside: a redact rule's rectangle lies wholly
	// outside the image.
	CausePixelRegionOutside Cause = "pixel_region_outside"
	// CausePixelTextUnsearched: the image stays compressed in a syntax
	// whose segments are not searched, and the profile holds such images.
	CausePixelTextUnsearched Cause = "pixel_text_unsearched"
	// CauseFramesMismatch: native pixels hold more bytes than the frames
	// the data set states, so a fill would leave some frames as they are.
	CauseFramesMismatch Cause = "frames_mismatch"
	// CauseDetectImplausible: the detector found regions covering more of
	// the picture than text does (anonymize.ErrImplausibleRedaction).
	CauseDetectImplausible Cause = "detect_implausible"
	// CauseTooManyMatches: a redact rule would cut, or a value pattern
	// match, more than profile.MaxRedactMatches distinct texts in one
	// value: more needles than the bound, none of which may be dropped.
	CauseTooManyMatches Cause = "too_many_matches"
	// CauseRedactInvalid: the value a redact rule leaves is not a value of
	// its VR (PS3.5 6.2), such as a replacement making it too long. The
	// file is refused rather than the element removed, which no rule said.
	CauseRedactInvalid Cause = "redact_invalid"
)

// The reasons a quarantine records for a failure.
const (
	// ReasonFailed is the reason of every cause but a rule's quarantine.
	ReasonFailed = "deid_failed"
	// ReasonRefused is the reason of a rule's quarantine.
	ReasonRefused = "profile_refused"
	// ReasonBurnedIn is the reason of an image at risk of burned-in text
	// no pixel rule matches.
	ReasonBurnedIn = "burned_in_risk"
)

// Failure is a file the profile cannot process. It is never a fault of the
// server: the same file under the same profile fails the same way, and the
// pipeline holds it in quarantine with Reason and Detail. Its text names
// where in the data set and which rule, never a value.
type Failure struct {
	// Cause is why.
	Cause Cause
	// Path is the element where it failed, as Entry.Path writes it; empty
	// when the failure is of the whole file.
	Path string
	// Rule is the name of the rule whose action failed or quarantined,
	// or its position ("rule 3") when it has no name; empty for a default.
	Rule string
	// RiskCause is why an image refused with CauseBurnedInRisk is at risk:
	// "burned_in_annotation", "secondary_capture" or "modality".
	RiskCause string
}

// Error says what failed, where and by which rule.
func (f *Failure) Error() string {
	s := "deid: " + string(f.Cause)
	if f.Path != "" {
		s += " at " + f.Path
	}
	if f.Rule != "" {
		s += " (" + f.Rule + ")"
	}
	return s
}

// Reason returns the quarantine reason of the failure.
func (f *Failure) Reason() string {
	switch f.Cause {
	case CauseRuleQuarantine:
		return ReasonRefused
	case CauseBurnedInRisk:
		return ReasonBurnedIn
	}
	return ReasonFailed
}

// Detail returns the quarantine detail: the rule's name for a rule's
// quarantine, why the image is at risk for burned-in text, the cause
// otherwise.
func (f *Failure) Detail() string {
	switch f.Cause {
	case CauseRuleQuarantine:
		return f.Rule
	case CauseBurnedInRisk:
		return f.RiskCause
	}
	return string(f.Cause)
}

// Entry is what happened to one element of the data set, and why.
type Entry struct {
	// Path is where the element is: its tag as eight hexadecimal digits,
	// after the sequences and item numbers it is inside, such as
	// "00081115[0]/0020000E".
	Path string
	// Tag is the element's tag.
	Tag uint32
	// VR is the element's value representation as the engine read it.
	VR string
	// Action is what was done: keep, remove, empty, replace, set, hash,
	// uid, shift, generalize, lookup, clean, dummy, remove_unless_required,
	// redact, or create for an element a set rule made. An element the floor
	// covers and the profile would have kept has Rule -1 and a Reason that
	// says so.
	Action string
	// Rule is the position of the deciding rule in the expanded profile,
	// from 0; -1 when no rule decided.
	Rule int
	// RuleName is the deciding rule's name; empty when it has none or no
	// rule decided.
	RuleName string
	// Origin is the base the deciding rule came from; empty for the
	// profile's own rules and the defaults.
	Origin string
	// Reason is the rule's reason, or why the default or a fixed rule of
	// the engine applied.
	Reason string
	// Changed reports whether the element's value or presence changed.
	Changed bool
	// Redactions is the number of parts a redact action cut from the
	// value; zero for any other action.
	Redactions int
	// PatternMatches is the number of matches the deciding rule's value
	// pattern found in the value; zero when it has none.
	PatternMatches int
}

// NeedleKind is what a removed value was, which says how a later search
// for it may match its written forms.
type NeedleKind int

// The kinds of value.
const (
	// NeedleText is an identifier, as text.
	NeedleText NeedleKind = iota + 1
	// NeedleName is a person's name (VR PN), or one of its parts.
	NeedleName
	// NeedleDate is a date or a date and time (VR DA or DT).
	NeedleDate
	// NeedleUID is a UID (VR UI).
	NeedleUID
	// NeedleList is every term of the profile's text lists (Needle.Lists):
	// one needle in every file's record, whatever the file holds, so a
	// term found again in any value holds the file. It has no Value.
	NeedleList
)

// Needle is a value the profile removed or replaced that can identify
// someone by itself (needles.go), kept in memory so the check after can
// search the de-identified file for it.
type Needle struct {
	// Kind is what the value was.
	Kind NeedleKind
	// Value is the value as received, its padding removed.
	Value string
	// Path is the element it was removed from.
	Path string
	// Edge is where the value is taken for itself when searched for and
	// cleaned: EdgeAny for most, EdgeWord for a text a pattern took out
	// that begins or ends with a letter, so a cut "Smith" is not found in
	// "Smithson", and EdgeIdentifier for an identifier of words, not the
	// patient's (IdentifierEdge, wordEdgeAllowed).
	Edge Edge
	// Whole is set for a value searched for only whole, never split into
	// values at its backslashes: a text a pattern took out of one value,
	// in which a backslash is text (ST, LT, UT) or absent.
	Whole bool
	// Lists are the profile's text lists, for a needle of kind
	// NeedleList; nil for any other. They are compiled once per profile
	// version and shared by every file's record.
	Lists *profile.TextLists
}

// Needles is the values a profile removed from a file. It holds patient
// data, so it is never printed: its text is only its count.
type Needles []Needle

// String returns the count, never a value.
func (n Needles) String() string { return strconv.Itoa(len(n)) + " values" }

// GoString returns the count, so %#v prints no value either.
func (n Needles) GoString() string { return n.String() }

// Format prints the count for every verb, so no formatting of a Record or
// of its needles reaches a value.
func (n Needles) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(n.String())) }

// Record is what the engine did to a file: an entry per element, in the
// order the data set is walked, and the values it removed.
type Record struct {
	// Entries lists every element of the data set as received, with what
	// was done to it, then each element a set rule made.
	Entries []Entry
	// Needles are the values removed or replaced. They stay in memory for
	// the check after and are never stored, logged or answered.
	Needles Needles
	// DatesShifted reports whether any date was moved by the subject's
	// shift. The shift itself is in no field: it is a secret of the subject.
	DatesShifted bool
	// Pixels is what was done to the file's pixels, and why; nil when the
	// file has no Pixel Data (7FE0,0010).
	Pixels *PixelWork
	// Written is the record of the de-identification the engine wrote at
	// the data set's top level in this run, none of it read from the file.
	Written Written
	// Unnamed are the elements of the file no rule of the profile named,
	// each once with its count, in the order they were first visited, at
	// most MaxUnnamed (seen.go); no value.
	Unnamed []Unnamed
	// UnnamedOver counts the elements no rule named that were visited once
	// Unnamed held MaxUnnamed distinct ones and were not among them.
	UnnamedOver int
}

// Written is the record of the de-identification (PS3.15 E.1.1, E.3.6) an
// Engine wrote into one data set, exactly as it wrote it: the check after
// de-identification is told it so that the words of the record (a code
// meaning, the method's name, a version) are not taken for a removed value
// they happen to equal, as a patient named "NoPACs" or "Basic" would be in
// every file.
type Written struct {
	// Method is the value written in De-identification Method (0012,0063),
	// its values joined by backslashes; empty when none was written.
	Method string
	// MethodCodes are the codes written in De-identification Method Code
	// Sequence (0012,0064), one item each, in order, each with its Coding
	// Scheme Designator as written; nil when no sequence was written.
	MethodCodes []anonymize.MethodCode
}

// formatTag writes a tag as eight upper-case hexadecimal digits, the form a
// profile's selectors use.
func formatTag(t uint32) string { return fmt.Sprintf("%08X", t) }
