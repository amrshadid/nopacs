package profile

import (
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/amrshadid/go-dicom/valuerep"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/pathname"
)

// Bounds of an action's text.
const (
	// MaxValueLength is the bound of a constant and of a template.
	MaxValueLength = 1024
	// MaxPlaceholders is the most placeholders of a template.
	MaxPlaceholders = 8
	// HashCodeLength is the length of the code hash writes, and a
	// template's placeholder: 80 bits in base32, the code of a label's
	// {#X} placeholder.
	HashCodeLength = 16
)

// The VRs each action fits. An action outside its list would write a value
// the element cannot hold (PS3.5 6.2), or would do nothing the person meant.
var (
	// textVRs hold text a keyed code can replace.
	textVRs = []string{"AE", "CS", "LO", "LT", "PN", "SH", "ST", "UC", "UT"}
	// cleanVRs are what clean is defined for: free text names can hide
	// in, a sequence (cleaned by its items' rules), and the bits of an
	// overlay or a curve (removed).
	cleanVRs = []string{"LO", "LT", "SH", "ST", "UC", "UT", "SQ", "OB", "OW"}
	// uidFallbackVRs are what remove_unless_required's UID fallback fits:
	// a UID, or a sequence holding UID references (PS3.15 X/Z/U*).
	uidFallbackVRs = []string{"SQ", "UI"}
	// stringVRs hold a value written as text, which a constant can be.
	stringVRs = []string{"AE", "AS", "CS", "DA", "DS", "DT", "IS", "LO", "LT", "PN", "SH", "ST", "TM", "UC", "UI", "UR", "UT"}
	uidVRs    = []string{"UI"}
	dateVRs   = []string{"DA", "DT"}
	coarseVRs = []string{"AS", "DA", "DT"}
)

// checkAction checks an action against what the selector says of the
// elements it selects, and returns its normalized form, in which a
// template's keywords are their tags, and for redact its pattern compiled
// for a search.
func checkAction(a at, act *v1.ProfileAction, sel selector) (*v1.ProfileAction, *regexp.Regexp) {
	a = a.in("action")
	norm, _ := proto.Clone(act).(*v1.ProfileAction)
	var redact *regexp.Regexp
	switch k := norm.GetKind().(type) {
	case nil:
		a.add(CodeRequired, "a rule needs an action")
	case *v1.ProfileAction_Keep, *v1.ProfileAction_Remove, *v1.ProfileAction_Empty, *v1.ProfileAction_Quarantine, *v1.ProfileAction_Dummy:
		// Fit any element.
	case *v1.ProfileAction_RemoveUnlessRequired:
		a := a.in("removeUnlessRequired").in("fallback")
		switch f := k.RemoveUnlessRequired.GetFallback(); f {
		case v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY, v1.RequiredFallback_REQUIRED_FALLBACK_DUMMY:
			// Fit any element.
		case v1.RequiredFallback_REQUIRED_FALLBACK_UID:
			fits(a, sel, uidFallbackVRs, "the fallback uid")
		case v1.RequiredFallback_REQUIRED_FALLBACK_UNSPECIFIED:
			a.add(CodeRequired, "say what an element its IOD requires with a value becomes: empty, dummy or uid")
		default:
			a.add(CodeBadEnum, "%d is not a value of RequiredFallback", f)
		}
	case *v1.ProfileAction_Replace:
		if t, ok := checkValue(a.in("replace"), sel, k.Replace.GetConstant(), k.Replace.GetTemplate(), k.Replace.GetValue() != nil, isTemplate(k.Replace.GetValue())); ok {
			k.Replace.Value = &v1.ReplaceAction_Template{Template: t}
		}
	case *v1.ProfileAction_Set:
		if sel.tier != TierTag {
			a.in("set").add(CodeActionSelector, "set makes an element where it is absent, so it needs one place: a tag or keyword selector")
			return norm, nil
		}
		if len(sel.vrs) != 1 {
			a.in("set").add(CodeActionVR, "set makes an element, so the data dictionary must give it one VR; %s has %q", formatTag(sel.tag), strings.Join(sel.vrs, " or "))
			return norm, nil
		}
		if t, ok := checkValue(a.in("set"), sel, k.Set.GetConstant(), k.Set.GetTemplate(), k.Set.GetValue() != nil, isTemplate(k.Set.GetValue())); ok {
			k.Set.Value = &v1.SetAction_Template{Template: t}
		}
	case *v1.ProfileAction_Hash:
		fits(a.in("hash"), sel, textVRs, "hash")
	case *v1.ProfileAction_Uid:
		fits(a.in("uid"), sel, uidVRs, "uid")
	case *v1.ProfileAction_Shift:
		fits(a.in("shift"), sel, dateVRs, "shift")
	case *v1.ProfileAction_Generalize:
		fits(a.in("generalize"), sel, coarseVRs, "generalize")
	case *v1.ProfileAction_Clean:
		fits(a.in("clean"), sel, cleanVRs, "clean")
	case *v1.ProfileAction_Redact:
		redact = checkRedact(a, k.Redact, sel)
	case *v1.ProfileAction_Lookup:
		a := a.in("lookup")
		fits(a, sel, textVRs, "lookup")
		if !pathname.ValidName(k.Lookup.GetTable()) {
			a.in("table").add(CodeBadName, "a lookup table's name is 1 to 63 of a-z, 0-9 and -, starting with a letter")
		}
		switch m := k.Lookup.GetOnMissing(); {
		case m == v1.LookupMissing_LOOKUP_MISSING_UNSPECIFIED && a.c.stored:
			a.in("onMissing").add(CodeBadEnum, "an expanded form writes every default out, and a lookup's onMissing is never unspecified in it")
		case m == v1.LookupMissing_LOOKUP_MISSING_UNSPECIFIED:
			// The default, written out so the expanded form says it.
			k.Lookup.OnMissing = v1.LookupMissing_LOOKUP_MISSING_QUARANTINE
		default:
			if _, known := v1.LookupMissing_name[int32(m)]; !known {
				a.in("onMissing").add(CodeBadEnum, "%d is not a value of LookupMissing", m)
			}
		}
	default:
		a.add(CodeBadEnum, "an action this version does not know")
	}
	return norm, redact
}

// isTemplate reports whether a value choice is a template.
func isTemplate(v any) bool {
	switch v.(type) {
	case *v1.ReplaceAction_Template, *v1.SetAction_Template:
		return true
	}
	return false
}

// checkValue checks the value of a replace or set: a constant valid for the
// element's VR, or a template for a text element. For a valid template it
// returns the template normalized and true.
func checkValue(a at, sel selector, constant, template string, given, isTmpl bool) (string, bool) {
	if !given {
		a.add(CodeOneOf, "give the new value as constant or template")
		return "", false
	}
	if isTmpl {
		fieldA := a.in("template")
		if !fits(fieldA, sel, textVRs, "a template") {
			return "", false
		}
		norm, ok := checkTemplate(fieldA, template)
		if ok && !a.c.stored {
			// The longest output, each placeholder a code's full length,
			// must be a value of every VR it may be written into.
			for _, vr := range sel.vrs {
				if !checkWritten(fieldA, vr, renderLongest(norm, vr), "the template's longest output") {
					return "", false
				}
			}
		}
		return norm, ok
	}
	fieldA := a.in("constant")
	if len(constant) > MaxValueLength {
		fieldA.add(CodeTooLong, "a constant is at most %d bytes", MaxValueLength)
		return "", false
	}
	if !fits(fieldA, sel, stringVRs, "a constant") {
		return "", false
	}
	if !checkLiteral(fieldA, constant, "a constant") || a.c.stored || constant == "" {
		return "", false
	}
	// A constant is checked against each VR it may be written into: the
	// library's rules of PS3.5 6.2 for its characters and length. A stored
	// form is not checked again, so a later library cannot refuse it.
	for _, vr := range sel.vrs {
		if !checkWritten(fieldA, vr, constant, "the constant") {
			break
		}
	}
	return "", false
}

// checkLiteral checks text a person wrote into a constant or a template:
// printable ASCII, the default repertoire every character set of PS3.5
// 6.1 holds, so it fits every file; and no backslash, which would make one
// value several (PS3.5 6.4).
func checkLiteral(a at, s, what string) bool {
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c > 0x7E || c == '\\' {
			a.add(CodeBadValue, "%s is printable ASCII without a backslash; byte %d is 0x%02X", what, i+1, c)
			return false
		}
	}
	return true
}

// checkWritten checks a value a rule writes against a VR (PS3.5 6.2).
func checkWritten(a at, vr, value, what string) bool {
	if err := valuerep.ValidateValue(vr, value); err != nil {
		a.add(CodeBadValue, "%s, %q, is not a value of VR %s: %v", what, clipText(value), vr, err)
		return false
	}
	return true
}

// renderLongest renders a normalized template with each placeholder at a
// code's full length, in the case hash writes for vr: upper case in CS,
// which holds no lower-case letter (PS3.5 6.2), lower case elsewhere.
func renderLongest(tmpl, vr string) string {
	code := strings.Repeat("a", HashCodeLength)
	if vr == "CS" {
		code = strings.ToUpper(code)
	}
	var out strings.Builder
	for i := 0; i < len(tmpl); i++ {
		switch {
		case strings.HasPrefix(tmpl[i:], "{{"), strings.HasPrefix(tmpl[i:], "}}"):
			out.WriteByte(tmpl[i])
			i++
		case tmpl[i] == '{':
			out.WriteString(code)
			i += strings.IndexByte(tmpl[i:], '}')
		default:
			out.WriteByte(tmpl[i])
		}
	}
	return out.String()
}

// fits reports whether every VR the selector may select is in allowed,
// recording a problem when not.
func fits(a at, sel selector, allowed []string, what string) bool {
	if sel.vrs == nil {
		a.add(CodeActionVR, "%s needs to know the VR of what the rule selects, and a group, a private element, a tag the data dictionary does not know or a content item without a value type says nothing of it: such a rule may keep, remove, empty, dummy, remove unless required with the fallback empty or dummy, or quarantine", what)
		return false
	}
	for _, vr := range sel.vrs {
		if !slices.Contains(allowed, vr) {
			a.add(CodeActionVR, "%s fits VR %s, and the rule selects VR %s", what, strings.Join(allowed, ", "), strings.Join(sel.vrs, " or "))
			return false
		}
	}
	return true
}

// checkTemplate checks a template's grammar: literal text, "{{" and "}}"
// for a brace, and "{#Keyword}" or "{#GGGGEEEE}" for the keyed hash of a
// top-level text element. It returns the template with each placeholder's
// keyword written as its tag, and whether it is valid.
func checkTemplate(a at, s string) (string, bool) {
	if len(s) > MaxValueLength {
		a.add(CodeTooLong, "a template is at most %d bytes", MaxValueLength)
		return "", false
	}
	var out strings.Builder
	n := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '}':
			if i+1 < len(s) && s[i+1] == '}' {
				out.WriteString("}}")
				i++
				continue
			}
			a.add(CodeBadTemplate, "a lone } at byte %d; write }} for a brace", i+1)
			return "", false
		case '{':
			if i+1 < len(s) && s[i+1] == '{' {
				out.WriteString("{{")
				i++
				continue
			}
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				a.add(CodeBadTemplate, "a { at byte %d is not closed", i+1)
				return "", false
			}
			inner := s[i+1 : i+end]
			name, hashed := strings.CutPrefix(inner, "#")
			if !hashed {
				a.add(CodeBadTemplate, "{%s} is not a placeholder: a template holds {#Keyword}, the keyed hash of an element, and nothing read as received", clipText(inner))
				return "", false
			}
			t, ok := checkPlaceholder(a, name)
			if !ok {
				return "", false
			}
			out.WriteString("{#" + formatTag(t) + "}")
			n++
			i += end
		default:
			if c := s[i]; c < 0x20 || c > 0x7E || c == '\\' {
				a.add(CodeBadValue, "a template's text is printable ASCII without a backslash; byte %d is 0x%02X", i+1, c)
				return "", false
			}
			out.WriteByte(s[i])
		}
	}
	if n > MaxPlaceholders {
		a.add(CodeTooMany, "a template has at most %d placeholders", MaxPlaceholders)
		return "", false
	}
	return out.String(), true
}

// checkPlaceholder checks the element of a placeholder: a standard element
// whose value is text, and returns its tag. A stored form names it by tag,
// checked when it was validated, and the dictionary is not read again.
func checkPlaceholder(a at, name string) (uint32, bool) {
	var t uint32
	var ok bool
	if len(name) == 8 && isHex(name) || a.c.stored {
		t, ok = parseTag(a, name)
	} else {
		t, ok = keywordTag(a, name)
	}
	if !ok || !checkStandard(a, t) {
		return 0, false
	}
	if a.c.stored {
		return t, true
	}
	vrs := dictVRs(t)
	if len(vrs) != 1 || !slices.Contains(textVRs, vrs[0]) {
		a.add(CodeBadTemplate, "{#%s} is not a text element: a placeholder hashes an element of VR %s", clipText(name), strings.Join(textVRs, ", "))
		return 0, false
	}
	return t, true
}
