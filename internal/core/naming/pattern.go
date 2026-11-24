package naming

import (
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
)

// CompilePattern compiles a pattern with named parts to match a whole
// value, and returns it with its part names in the order they appear. Each
// part's name is a lower case letter followed by lower case letters,
// digits and '_', used once, and there is at least one. The error says in
// plain words why the pattern is refused; it holds the pattern, which is
// configuration, never a value it is to read. The caller bounds the
// pattern's length.
func CompilePattern(pattern string) (*regexp.Regexp, []string, error) {
	// Compiled as written first, so its errors are the ones a person
	// writing a Go regular expression expects.
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, n := range re.SubexpNames()[1:] {
		switch {
		case n == "":
			continue
		case !partName(n):
			return nil, nil, errorString("group name " + strconv.Quote(n) + " is not a lower case letter followed by lower case letters, digits and '_'")
		case slices.Contains(names, n):
			return nil, nil, errorString("group name " + strconv.Quote(n) + " is used twice")
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return nil, nil, errorString("has no named group (?P<name>...)")
	}
	// The anchors are added to the parsed tree, not to the text: joined as
	// text, a pattern ending in \Q would quote them and one such as
	// "a)|(b" would escape them. \A and \z, not ^ and $, so a flag such as
	// (?m) inside the pattern cannot turn them into line anchors. The tree
	// is parsed with the flags regexp.Compile uses, and the result must
	// have the same groups as the pattern alone, or it is refused.
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, nil, err
	}
	anchored := &syntax.Regexp{Op: syntax.OpConcat, Sub: []*syntax.Regexp{
		{Op: syntax.OpBeginText}, tree, {Op: syntax.OpEndText},
	}}
	full, err := regexp.Compile(anchored.String())
	if err != nil || !slices.Equal(full.SubexpNames(), re.SubexpNames()) {
		return nil, nil, errorString("cannot be made to match a whole value")
	}
	return full, names, nil
}
