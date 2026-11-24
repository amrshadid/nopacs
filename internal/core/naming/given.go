package naming

import (
	"slices"
	"strconv"
	"strings"
)

// GivenSource is how an error and Plain name where a given part came from:
// an import reads its parts from the file's path relative to its root, and
// a part its map gives is looked up by others of them.
const GivenSource = "the file's path"

// ValidPartName reports whether s may name a part: a lower case letter,
// then lower case letters, digits and '_', so it never reads as an
// attribute's keyword, which starts with an upper case letter.
func ValidPartName(s string) bool { return partName(s) }

// givenSet checks the names of the given parts and returns them as a set.
// A name that is not a part's name, is given twice, or is also a group of
// the rule's own pattern is refused.
func givenSet(given []string, pattern map[string]bool) (map[string]bool, error) {
	if len(given) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(given))
	for _, n := range given {
		switch {
		case !partName(n):
			return nil, &SyntaxError{Field: "pattern", Offset: -1, Msg: "the given part " + strconv.Quote(n) +
				" is not a lower case letter followed by lower case letters, digits and '_'"}
		case set[n]:
			return nil, &SyntaxError{Field: "pattern", Offset: -1, Msg: "the part " + strconv.Quote(n) + " is given twice"}
		case pattern[n]:
			return nil, &SyntaxError{Field: "pattern", Offset: -1, Msg: "group name " + strconv.Quote(n) +
				" is also a part " + GivenSource + " gives, so {" + n + "} would have two sources"}
		}
		set[n] = true
	}
	return set, nil
}

// givenHint completes the refusal of a template part that neither the
// pattern nor the given parts hold, by naming the parts that are given.
func givenHint(given []string) string {
	if len(given) == 0 {
		return ""
	}
	return "; " + GivenSource + " gives " + strings.Join(given, ", ")
}

// GivenParts returns the given parts the rule's templates use, each once,
// in the order of the labels: the parts a caller must put in
// LabelsWithParts's map.
func (r *Rule) GivenParts() []string {
	var out []string
	for _, t := range r.templates {
		for _, p := range t.pieces {
			if p.given && !slices.Contains(out, p.part) {
				out = append(out, p.part)
			}
		}
	}
	return out
}
