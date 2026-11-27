package profile

import (
	"context"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// listEdition is the language edition that has text lists: a version
// with a list, or a rule naming one, is of it, and an older edition's
// version holding either does not compile.
const listEdition = 3

// useLists compiles a document's or an expanded form's text lists into the
// checker, for the rules that name them, recording their problems at top.
func useLists(c *checker, top at, lists []*v1.TextList) {
	if len(lists) == 0 {
		return
	}
	c.listNames = map[string]bool{}
	for _, l := range lists {
		c.listNames[l.GetName()] = true
	}
	if c.shared {
		c.lists = sharedTextLists(top, lists)
		return
	}
	c.lists = compileTextLists(top, textListsOf(lists), true)
}

// checkListNames checks the lists a clean or redact action names: at most
// MaxRuleLists, each a list of the document, each once. It returns their
// bits (TextLists.Mask), zero when there is a problem or no list.
func checkListNames(a at, names []string) uint32 {
	if len(names) == 0 {
		return 0
	}
	a = a.in("lists")
	if len(names) > MaxRuleLists {
		a.add(CodeTooMany, "a rule names at most %d lists", MaxRuleLists)
		return 0
	}
	seen := map[string]bool{}
	ok := true
	for i, n := range names {
		la := a
		la.field = a.field + "[" + strconv.Itoa(i+1) + "]"
		switch {
		case !a.c.listNames[n]:
			la.add(CodeUnknownList, "the profile has no list %q", clipText(n))
			ok = false
		case seen[n]:
			la.add(CodeDuplicateName, "list %q is named twice", n)
			ok = false
		}
		seen[n] = true
	}
	if !ok || a.c.lists == nil {
		return 0
	}
	mask, _ := a.c.lists.Mask(names)
	return mask
}

// replacementHoldsTerm reports whether a text a rule writes (a redact's
// replacement, a replace or set constant, a template's text) holds a term
// of the lists in mask standing as a word: what it writes would be held
// by the check after, or cut again.
func replacementHoldsTerm(lists *TextLists, mask uint32, repl string) bool {
	if repl == "" || lists == nil || mask == 0 {
		return false
	}
	budget := int64(len(repl)) * MaxTermBytes
	spans, err := lists.Find(context.Background(), []rune(NFC(repl)), mask, &budget)
	return err != nil || len(spans) > 0
}

// usesLists reports whether a rule names a text list.
func usesLists(r *v1.ProfileRule) bool {
	return len(r.GetAction().GetClean().GetLists()) > 0 || len(r.GetAction().GetRedact().GetLists()) > 0
}

// TextLists returns the profile's text lists; nil when it has none.
func (p *Program) TextLists() *TextLists { return p.lists }
