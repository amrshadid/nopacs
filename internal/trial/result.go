package trial

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/receive"
)

// MaxElements is the most elements answered for one file.
const MaxElements = 2000

// AnswerBudget is about how many bytes of elements one answer carries, so it
// stays within the 4 MiB message: past it, elements are counted, not
// answered.
const AnswerBudget = 3 << 20

// Result returns one file's trial as the API answers it, its
// elements' values included, spending budget (bytes of the answer left);
// elements past it, or past MaxElements, are counted, not answered.
func Result(t receive.Trial, budget *int) *v1.TrialResult {
	r := &v1.TrialResult{Verdict: v1.TrialVerdict_TRIAL_VERDICT_PASSED, Receiver: t.Receiver, DeidentificationOnly: t.Receiver == ""}
	if t.Undecided {
		r.Verdict, r.Cause = v1.TrialVerdict_TRIAL_VERDICT_UNDECIDED, receive.TrialCauseLookupTable
	}
	if t.Hold != nil {
		r.Verdict = v1.TrialVerdict_TRIAL_VERDICT_HELD
		r.Reason, r.Cause = string(t.Hold.Reason), HoldCause(t.Hold)
		r.Path, r.Rule = t.Hold.Details["path"], t.Hold.Details["rule"]
	}
	r.ValuesSearched = int32(t.Report.Searched) //nolint:gosec // G115: bounded by the check's needles.
	r.ValuesSkipped = int32(t.Report.Skipped)   //nolint:gosec // G115: bounded by the check's needles.
	r.Pixels = pixels(t.Pixels)
	if t.Record == nil {
		return r
	}
	for i, e := range t.Record.Entries {
		el := &v1.TrialElement{
			Path: e.Path, Keyword: KeywordOf(e.Tag), Vr: e.VR, Action: e.Action, Rule: int32(e.Rule + 1), //nolint:gosec // G115: a position under profile.MaxExpandedRules.
			RuleName: e.RuleName, Origin: e.Origin, Reason: e.Reason, Changed: e.Changed,
			Redactions: int32(e.Redactions), PatternMatches: int32(e.PatternMatches), //nolint:gosec // G115: at most profile.MaxRedactMatches.
		}
		el.Before, _ = valueAt(t.Before, e.Path)
		if t.After != nil {
			el.After, el.PresentAfter = valueAt(t.After, e.Path)
		}
		size := len(el.Path) + len(el.Keyword) + len(el.Action) + len(el.RuleName) + len(el.Origin) + len(el.Reason) + len(el.Before) + len(el.After) + 32
		if i >= MaxElements || size > *budget {
			r.ElementsOmitted = int32(len(t.Record.Entries) - i) //nolint:gosec // G115: bounded by engine.MaxElements.
			break
		}
		*budget -= size
		r.Elements = append(r.Elements, el)
	}
	return r
}

// pixels returns the work on a file's pixels as the API answers it:
// positions, counts and names, never a value of the file; nil for none.
func pixels(w *engine.PixelWork) *v1.TrialPixels {
	if w == nil {
		return nil
	}
	out := &v1.TrialPixels{
		AtRisk: w.AtRisk, RiskCause: w.RiskCause, Rule: w.Rule, Origin: w.Origin, Action: w.Action,
		Frames: int32(w.Frames), DecodedBytes: w.DecodedBytes, Decoded: w.Decoded, //nolint:gosec // G115: frames are an IS of the file, bounded by its size.
		SegmentsRemoved: slices.Clone(w.SegmentsRemoved),
	}
	for _, g := range w.Regions {
		out.Regions = append(out.Regions, &v1.PixelRectangle{
			X: int32(g.X), Y: int32(g.Y), Width: int32(g.Width), Height: int32(g.Height), //nolint:gosec // G115: inside an image of at most 65535 a side.
		})
	}
	return out
}

// HoldCause is the fixed cause of a hold: the record's cause, or for a
// value found the kind of value, or for a rule's refusal "rule".
func HoldCause(h *receive.TrialHold) string {
	if c := h.Details["cause"]; c != "" {
		return c
	}
	if k := h.Details["kind"]; k != "" {
		return k
	}
	return "rule"
}

// KeywordOf is a standard element's keyword in the data dictionary; empty
// for a private or unknown one.
func KeywordOf(t uint32) string {
	tt := tag.Tag(t)
	if tt.Group()%2 == 1 {
		return ""
	}
	return tt.GetKeyword()
}

// textVRs are the VRs whose value is text, which a trial run answers.
var textVRs = map[string]bool{
	"AE": true, "AS": true, "CS": true, "DA": true, "DS": true, "DT": true, "IS": true, "LO": true, "LT": true,
	"PN": true, "SH": true, "ST": true, "TM": true, "UC": true, "UI": true, "UR": true, "UT": true,
}

// valueAt returns the text of the element at path (a record's path, such
// as "00081115[0]/0020000E") in ds, its padding removed, and whether ds
// holds the element there. A value that is not text, or not in memory, is
// answered empty.
func valueAt(ds *dataset.Dataset, path string) (string, bool) {
	steps := strings.Split(path, "/")
	cur := ds
	for i, s := range steps {
		if cur == nil {
			return "", false
		}
		ts, index, item := strings.Cut(s, "[")
		t, err := strconv.ParseUint(ts, 16, 32)
		if err != nil || len(ts) != 8 {
			return "", false
		}
		elem, ok := cur.Get(tag.Tag(t))
		if !ok {
			return "", false
		}
		if i == len(steps)-1 {
			if item {
				return "", false
			}
			vr := string(elem.GetVR())
			if !textVRs[vr] {
				return "", true
			}
			b, ok := elem.Bytes()
			if !ok {
				return "", true
			}
			return naming.TrimValue(vr, string(b)), true
		}
		n, err := strconv.Atoi(strings.TrimSuffix(index, "]"))
		seq, isSeq := elem.GetValue().(*sequence.Sequence)
		if !item || err != nil || !isSeq || n < 0 || n >= seq.Length() {
			return "", false
		}
		it, err := seq.Get(n)
		if err != nil {
			return "", false
		}
		cur, _ = it.(*dataset.Dataset)
	}
	return "", false
}

// Summary counts a trial run: files, verdicts, the elements each rule
// decided and the files held for each reason. It holds no value.
type Summary struct {
	// Files, Passed, Held and Undecided count the files and their verdicts.
	Files, Passed, Held, Undecided int
	// ByRule counts the elements each rule decided, and ByReason the files
	// held for each reason and cause.
	ByRule, ByReason map[string]int
}

// NewSummary returns an empty summary.
func NewSummary() *Summary {
	return &Summary{ByRule: map[string]int{}, ByReason: map[string]int{}}
}

// Add counts one file.
func (s *Summary) Add(t receive.Trial, r *v1.TrialResult) {
	s.Files++
	switch r.GetVerdict() {
	case v1.TrialVerdict_TRIAL_VERDICT_HELD:
		s.Held++
		s.ByReason[r.GetReason()+"/"+r.GetCause()]++
	case v1.TrialVerdict_TRIAL_VERDICT_UNDECIDED:
		s.Undecided++
	default:
		s.Passed++
	}
	if t.Record == nil {
		return
	}
	for _, e := range t.Record.Entries {
		switch {
		case e.Rule < 0:
			s.ByRule["default"]++
		case e.RuleName != "":
			s.ByRule[e.RuleName]++
		default:
			s.ByRule["rule "+strconv.Itoa(e.Rule+1)]++
		}
	}
}

// Counts returns m by count, most first, then by key.
func Counts(m map[string]int) []*v1.TrialCount {
	out := make([]*v1.TrialCount, 0, len(m))
	for k, n := range m {
		out = append(out, &v1.TrialCount{Key: k, Count: int32(n)}) //nolint:gosec // G115: bounded by the files' elements.
	}
	slices.SortFunc(out, func(a, b *v1.TrialCount) int {
		return cmp.Or(cmp.Compare(b.GetCount(), a.GetCount()), cmp.Compare(a.GetKey(), b.GetKey()))
	})
	return out
}

// Proto returns the summary as the API answers it.
func (s *Summary) Proto() *v1.TrialSummary {
	return &v1.TrialSummary{Files: int32(s.Files), Passed: int32(s.Passed), Held: int32(s.Held), Undecided: int32(s.Undecided), //nolint:gosec // G115: at most the files of one trial run.
		ByRule: Counts(s.ByRule), ByReason: Counts(s.ByReason)}
}
