package profile

import (
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/tag"
	"google.golang.org/protobuf/reflect/protoreflect"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// MaxDescribedElements is the most elements one effect of a description
// names; the rest are counted.
const MaxDescribedElements = 64

// Describe says what an expanded profile does to each class of element, in
// plain terms: for each class (ProfileElementClass), each action given to
// it and what it is given to, in the order of the rules. Among the rules
// whose selector names the same thing with no condition the later
// decides, as the precedence says, so an element a base removes and the
// profile's own rule keeps is listed once, as kept; a rule with conditions
// is listed beside it, marked conditional. A class is judged from the
// selector and the VRs validation stored, never from a file, so a rule
// over a group, a VR or a value pattern is listed by what it selects.
func Describe(e *v1.ExpandedProfile) []*v1.ProfileClassEffects {
	type item struct {
		class       v1.ProfileElementClass
		action      string
		order       int // the action's field number, for the order of effects
		what        string
		conditional bool
	}
	var items []item
	last := map[string]int{} // an unconditional selector's text, to its item
	for _, er := range e.GetRules() {
		r := er.GetRule()
		what, class := describeSelector(r.GetSelector(), er.GetVrs())
		action, order := oneofName(r.GetAction().ProtoReflect(), "kind")
		it := item{class: class, action: action, order: order, what: what, conditional: len(r.GetWhen()) > 0}
		if it.conditional {
			items = append(items, it)
			continue
		}
		if i, ok := last[what]; ok {
			items[i] = it
			continue
		}
		last[what] = len(items)
		items = append(items, it)
	}
	for i, pr := range e.GetPixelRules() {
		r := pr.GetRule()
		action, order := oneofName(r.ProtoReflect(), "action")
		what := r.GetName()
		if what == "" {
			what = "pixel rule " + strconv.Itoa(i+1)
		}
		items = append(items, item{class: v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_PIXELS, action: action, order: order, what: what,
			conditional: len(r.GetWhen()) > 0 || r.GetRows() > 0 || r.GetColumns() > 0})
	}
	var out []*v1.ProfileClassEffects
	for class := v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_NAMES; class <= v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_OTHER; class++ {
		var effects []*v1.ProfileClassEffect
		var orders []int
		for _, it := range items {
			if it.class != class {
				continue
			}
			i := slices.IndexFunc(effects, func(x *v1.ProfileClassEffect) bool { return x.GetAction() == it.action })
			if i < 0 {
				i = len(effects)
				effects = append(effects, &v1.ProfileClassEffect{Action: it.action})
				orders = append(orders, it.order)
			}
			x := effects[i]
			x.Conditional = x.Conditional || it.conditional
			if slices.Contains(x.Elements, it.what) {
				continue
			}
			if len(x.Elements) < MaxDescribedElements {
				x.Elements = append(x.Elements, it.what)
			} else {
				x.ElementsOmitted++
			}
		}
		if len(effects) == 0 {
			continue
		}
		at := make([]int, len(effects))
		for i := range at {
			at[i] = i
		}
		slices.SortStableFunc(at, func(a, b int) int { return orders[a] - orders[b] })
		sorted := make([]*v1.ProfileClassEffect, len(at))
		for i, j := range at {
			sorted[i] = effects[j]
		}
		out = append(out, &v1.ProfileClassEffects{Class: class, Effects: sorted})
	}
	return out
}

// DescribeNotes returns the sentences that hold for a whole expanded
// profile, whatever its rules name: the defaults, the date shift, and
// what the engine and the pipeline do in every file.
func DescribeNotes(e *v1.ExpandedProfile) []string {
	unnamed := "kept"
	if e.GetUnnamed() == v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE {
		unnamed = "removed"
	}
	out := []string{
		"A standard element no rule names is " + unnamed + "; a private element no rule names is removed.",
		"Whatever the rules say, the attributes that name or identify the person (names of persons, patient IDs, " +
			"other patient IDs, birth date, addresses, telephone numbers) are never kept, at any depth.",
		"Every file is searched after de-identification for each identifying value removed from it (names, " +
			"identifiers, dates, UIDs); a file where one is found again is held, never archived.",
	}
	if usesAction(e, "shift") {
		out = append(out, "A shifted date is moved back by one offset per subject, 1 to "+strconv.Itoa(int(e.GetDateShiftDays()))+
			" days, kept with the tenant's key and never written in a file; times of day are kept, so ages and intervals survive.")
	}
	if usesAction(e, "uid") {
		out = append(out, "A remapped UID is derived from the original under the tenant's key, the same every time, so references "+
			"between files still match; a UID the standard registers is kept.")
	}
	if usesAction(e, "hash") || usesAction(e, "lookup") {
		out = append(out, "A hashed value is the keyed hash of the original under the tenant's naming key, the code a subject's label "+
			"takes; a looked-up value is the code the tenant's table gives it. Neither can be reversed from the archive.")
	}
	out = append(out, "An image at risk of burned-in text (Burned In Annotation YES, or absent on a modality or SOP class commonly "+
		"drawn on) is held unless a pixel rule covers it.")
	out = append(out, "Comment and application segments of JPEG, and comment segments of JPEG 2000 and HTJ2K, are removed from "+
		"compressed pixel data; a frame holding any other segment that could carry text is held.")
	if e.GetUnsearchedSegments() == v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_KEEP {
		out = append(out, "Compressed pixel data whose segments are not searched for text (JPEG 2000 Part 2, MPEG-2, H.264, HEVC, "+
			"JPEG XL) is kept as received.")
	} else {
		out = append(out, "Compressed pixel data whose segments are not searched for text (JPEG 2000 Part 2, MPEG-2, H.264, HEVC, "+
			"JPEG XL) is held.")
	}
	return out
}

// usesAction reports whether a rule of e has the action of that field name.
func usesAction(e *v1.ExpandedProfile, name string) bool {
	for _, er := range e.GetRules() {
		if n, _ := oneofName(er.GetRule().GetAction().ProtoReflect(), "kind"); n == name {
			return true
		}
	}
	return false
}

// oneofName returns the field name set in m's oneof of that name, and its
// number, or "none" and 0.
func oneofName(m protoreflect.Message, oneof string) (string, int) {
	od := m.Descriptor().Oneofs().ByName(protoreflect.Name(oneof))
	if od == nil {
		return "none", 0
	}
	fd := m.WhichOneof(od)
	if fd == nil {
		return "none", 0
	}
	return string(fd.Name()), int(fd.Number())
}

// classOfVR is the class of an element of a VR, and false for a VR that
// says nothing of it.
func classOfVR(vr string) (v1.ProfileElementClass, bool) {
	switch vr {
	case "PN":
		return v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_NAMES, true
	case "UI":
		return v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_UIDS, true
	case "DA", "DT", "TM", "AS":
		return v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_DATES, true
	}
	return 0, false
}

// classOfTag is the class of a standard element by its VRs, then by its
// keyword (identifiers, addresses, telephones), else other; a private one
// is private.
func classOfTag(t uint32, vrs []string) v1.ProfileElementClass {
	if (t>>16)%2 == 1 {
		return v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_PRIVATE
	}
	for _, vr := range vrs {
		if c, ok := classOfVR(vr); ok {
			return c
		}
	}
	if identifierKeyword(tag.Tag(t).GetKeyword()) {
		return v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_IDENTIFIERS
	}
	return v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_OTHER
}

// identifierKeyword reports whether a PS3.6 keyword names an identifier,
// an address or a telephone: "ID" as a word of it (PatientID,
// OtherPatientIDsSequence, StudyID), or Identifier, AccessionNumber,
// Address, Telephone, Telecom.
func identifierKeyword(kw string) bool {
	for _, w := range []string{"Identifier", "AccessionNumber", "Address", "Telephone", "Telecom"} {
		if strings.Contains(kw, w) {
			return true
		}
	}
	for i := strings.Index(kw, "ID"); i >= 0; {
		rest := kw[i+2:]
		if rest == "" || rest[0] == 's' || (rest[0] >= 'A' && rest[0] <= 'Z') {
			return true
		}
		j := strings.Index(rest, "ID")
		if j < 0 {
			break
		}
		i += 2 + j
	}
	return false
}

// describeSelector returns what a selector selects in a few words and the
// class it belongs to.
func describeSelector(s *v1.ProfileSelector, vrs []string) (string, v1.ProfileElementClass) {
	other := v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_OTHER
	byVRs := func() v1.ProfileElementClass {
		var found v1.ProfileElementClass
		for _, vr := range vrs {
			c, ok := classOfVR(vr)
			if !ok || (found != 0 && c != found) {
				return other
			}
			found = c
		}
		if found == 0 {
			return other
		}
		return found
	}
	withPrivate := ""
	if s.GetIncludePrivate() {
		withPrivate = ", private ones too"
	}
	switch k := s.GetKind().(type) {
	case *v1.ProfileSelector_Tag:
		t, err := strconv.ParseUint(k.Tag, 16, 32)
		if err != nil {
			return k.Tag, other
		}
		return tagName(uint32(t)), classOfTag(uint32(t), vrs)
	case *v1.ProfileSelector_Keyword:
		return k.Keyword, classOfTag(uint32(tag.GlobalDictionary().GetByKeyword(k.Keyword)), vrs)
	case *v1.ProfileSelector_Group:
		if g, err := strconv.ParseUint(k.Group, 16, 16); err == nil && g%2 == 1 {
			return "group " + k.Group, v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_PRIVATE
		}
		return "group " + k.Group + withPrivate, other
	case *v1.ProfileSelector_Vr:
		c, ok := classOfVR(k.Vr)
		if !ok {
			c = other
		}
		return "every " + k.Vr + " element" + withPrivate, c
	case *v1.ProfileSelector_PrivateElement:
		p := k.PrivateElement
		what := "private '" + p.GetCreator() + "' " + p.GetGroup()
		if p.GetElement() != "" {
			what += "," + p.GetElement()
		}
		return what, v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_PRIVATE
	case *v1.ProfileSelector_Path:
		steps := strings.Split(k.Path, ".")
		lastStep, _, _ := strings.Cut(steps[len(steps)-1], "[")
		if t, err := strconv.ParseUint(lastStep, 16, 32); err == nil && len(lastStep) == 8 {
			return pathName(k.Path), classOfTag(uint32(t), vrs)
		}
		return pathName(k.Path), byVRs()
	case *v1.ProfileSelector_ContentItem:
		c := k.ContentItem
		return "content item " + c.GetScheme() + " " + c.GetValue(), byVRs()
	case *v1.ProfileSelector_ValuePattern:
		return "values matching a pattern among " + strings.Join(vrs, ", "), byVRs()
	}
	return "nothing", other
}

// tagName writes a tag as its keyword, or its eight digits where it has
// none.
func tagName(t uint32) string {
	if kw := tag.Tag(t).GetKeyword(); kw != "" && (t>>16)%2 == 0 {
		return kw
	}
	return formatTag(t)
}

// pathName writes a path with each tag step as its keyword.
func pathName(p string) string {
	steps := strings.Split(p, ".")
	for i, s := range steps {
		step, rest, _ := strings.Cut(s, "[")
		if len(step) != 8 {
			continue
		}
		if t, err := strconv.ParseUint(step, 16, 32); err == nil {
			steps[i] = tagName(uint32(t))
			if rest != "" {
				steps[i] += "[" + rest
			}
		}
	}
	return strings.Join(steps, ".")
}
