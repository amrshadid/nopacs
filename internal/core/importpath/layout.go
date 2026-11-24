package importpath

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// Kind is how a layout reads a file's path.
type Kind int

// The kinds of layout.
const (
	// Plain reads nothing of the path: labels come from the naming rule
	// over the headers alone. A unit is one folder at the first level.
	Plain Kind = iota + 1
	// Pattern reads the path by a person's own pattern, with its own unit
	// depth.
	Pattern
)

// String returns the kind's name, as a person writes it.
func (k Kind) String() string {
	switch k {
	case Plain:
		return "plain"
	case Pattern:
		return "pattern"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// The bounds of a layout, and a plain layout's fixed unit depth.
const (
	MaxPatternLength = 1024
	MaxUnitDepth     = 16
	PlainUnitDepth   = 1
)

// ConfigError is a layout, or the map given with it, refused as written.
// Its text may show the pattern or a part's name, which are
// configuration, never a path read from a tree.
type ConfigError struct {
	// Field is "kind", "pattern", "unit depth" or "map".
	Field string
	// Msg says what is wrong.
	Msg string
}

// Error returns the field and the fault.
func (e *ConfigError) Error() string { return "importpath: the layout's " + e.Field + " " + e.Msg }

// Layout is a parsed layout. It is not changed once made, so it is safe
// for concurrent use.
type Layout struct {
	kind  Kind
	re    *regexp.Regexp // nil for Plain
	parts []string
	depth int
}

// New returns the layout of kind with its pattern and unit depth, or a
// *ConfigError. Pattern takes a pattern of at most MaxPatternLength bytes
// with at least one named part, each named by a lower case letter followed
// by lower case letters, digits and "_", used once, which must match a
// whole path (the anchors are added), and a unit depth from 1 to
// MaxUnitDepth. Plain takes neither.
func New(kind Kind, pattern string, unitDepth int) (*Layout, error) {
	switch kind {
	case Plain:
		switch {
		case pattern != "":
			return nil, &ConfigError{Field: "pattern", Msg: "is given, and a plain layout takes none"}
		case unitDepth != 0:
			return nil, &ConfigError{Field: "unit depth", Msg: "is given, and a plain layout's is fixed"}
		}
		return &Layout{kind: Plain, depth: PlainUnitDepth}, nil
	case Pattern:
		switch {
		case pattern == "":
			return nil, &ConfigError{Field: "pattern", Msg: "is empty"}
		case len(pattern) > MaxPatternLength:
			return nil, &ConfigError{Field: "pattern", Msg: "is longer than " + strconv.Itoa(MaxPatternLength) + " bytes"}
		case unitDepth < 1 || unitDepth > MaxUnitDepth:
			return nil, &ConfigError{Field: "unit depth", Msg: "is not from 1 to " + strconv.Itoa(MaxUnitDepth)}
		}
		// Through the naming rule's own compiler, so a layout's parts and a
		// header pattern's are named and anchored by one rule.
		re, parts, err := naming.CompilePattern(pattern)
		if err != nil {
			return nil, &ConfigError{Field: "pattern", Msg: err.Error()}
		}
		return &Layout{kind: Pattern, re: re, parts: parts, depth: unitDepth}, nil
	}
	return nil, &ConfigError{Field: "kind", Msg: "is not plain or pattern"}
}

// Kind returns the layout's kind.
func (l *Layout) Kind() Kind { return l.kind }

// UnitDepth returns how many segments from the root make a unit's folder.
func (l *Layout) UnitDepth() int { return l.depth }

// Parts returns the parts the layout reads from a path, in the pattern's
// order; none for Plain. The caller must not change the slice.
func (l *Layout) Parts() []string { return l.parts }

// Unit returns the folder of the unit a file at path p belongs to: its
// first UnitDepth segments, and true; or "" and false for a file no deeper
// than that, which the listing unit meets on its way down.
func (l *Layout) Unit(p string) (string, bool) {
	i := 0
	for range l.depth {
		j := strings.IndexByte(p[i:], '/')
		if j < 0 {
			return "", false
		}
		i += j + 1
	}
	return p[:i-1], true
}

// ErrMapParts is a map whose key names a part the layout does not give,
// or whose value fills a part the layout already gives: the first could
// never be looked up, the second would give one part two sources.
var ErrMapParts = errors.New("importpath: the map's key parts must be parts of the layout, and the part it gives must not be")

// KeyedMap gives one part's value by the values of other parts, in the
// order of its key. *mapping.Map of package dirimport/mapping implements
// it.
type KeyedMap interface {
	// Lookup returns the value listed for key, and whether the map lists
	// it.
	Lookup(key []string) (string, bool)
}

// MapUse is how a resolver reads its map: the layout's parts whose values
// form the key, in the map's key order, and the part the value fills.
type MapUse struct {
	// Key names the layout's parts that form the key, at least one, each
	// once.
	Key []string
	// Gives names the part the map's value fills.
	Gives string
}

// CheckMap returns nil when a map used as u fits the layout, or
// ErrMapParts, or a *ConfigError for a use that names no key, a part
// twice, or a name that is not a part's.
func (l *Layout) CheckMap(u MapUse) error {
	if len(u.Key) == 0 {
		return &ConfigError{Field: "map", Msg: "names no key part"}
	}
	for i, k := range u.Key {
		if slices.Contains(u.Key[:i], k) {
			return &ConfigError{Field: "map", Msg: "names the key part " + strconv.Quote(k) + " twice"}
		}
		if !slices.Contains(l.parts, k) {
			return ErrMapParts
		}
	}
	if !naming.ValidPartName(u.Gives) {
		return &ConfigError{Field: "map", Msg: "gives a part whose name is not a lower case letter followed by lower case letters, digits and '_'"}
	}
	if slices.Contains(l.parts, u.Gives) || slices.Contains(u.Key, u.Gives) {
		return ErrMapParts
	}
	return nil
}

// Resolver reads files' paths for the parts a naming rule is given: a
// layout, and optionally a map that fills one more part.
type Resolver struct {
	layout *Layout
	m      KeyedMap
	use    MapUse
}

// Resolver returns the layout's resolver. m is its map, nil for none (a
// nil pointer in the interface is a map, so pass nil itself), read as u
// says; u is ignored without a map. A use that does not fit the layout is
// refused as CheckMap says.
func (l *Layout) Resolver(m KeyedMap, u MapUse) (*Resolver, error) {
	if m == nil {
		return &Resolver{layout: l}, nil
	}
	if err := l.CheckMap(u); err != nil {
		return nil, err
	}
	u.Key = slices.Clone(u.Key)
	return &Resolver{layout: l, m: m, use: u}, nil
}

// Given returns the parts a naming rule may use with this resolver, for
// naming.ParseWithParts: the layout's, and the part the map fills when
// there is a map. A rule using any other part is refused when it is set.
func (r *Resolver) Given() []string {
	out := slices.Clone(r.layout.parts)
	if r.m != nil {
		out = append(out, r.use.Gives)
	}
	return out
}

// Skip is why a file's path gives no parts. Its text is the contract's
// name for it.
type Skip int

// The skips a path can cause.
const (
	// NoSkip is a path that gave its parts.
	NoSkip Skip = iota
	// OutsideLayout is a path the layout does not match.
	OutsideLayout
	// UnmappedSession is a path whose key the map does not list: a folder
	// the source system no longer knows is not brought back.
	UnmappedSession
)

// String returns the skip's code as the contract names it.
func (s Skip) String() string {
	switch s {
	case NoSkip:
		return "none"
	case OutsideLayout:
		return "outside_layout"
	case UnmappedSession:
		return "unmapped_session"
	}
	return "Skip(" + strconv.Itoa(int(s)) + ")"
}

// Resolve returns the parts the file at path p gives, by name, for
// naming.Rule.LabelsWithParts, or a skip. p is the file's path relative to
// the root as the walk found it: names from a file system, which this
// does not judge beyond its shape (no leading, trailing or doubled "/");
// each value still passes the label rule when a label is made, or the file
// is quarantined. A part a pattern's optional group did not match is
// absent; one that matched nothing is "".
func (r *Resolver) Resolve(p string) (map[string]string, Skip) {
	if p == "" || p[0] == '/' || p[len(p)-1] == '/' || strings.Contains(p, "//") {
		return nil, OutsideLayout
	}
	l := r.layout
	if l.re == nil {
		return map[string]string{}, NoSkip
	}
	m := l.re.FindStringSubmatchIndex(p)
	if m == nil {
		return nil, OutsideLayout
	}
	parts := make(map[string]string, len(l.parts)+1)
	for _, name := range l.parts {
		i := l.re.SubexpIndex(name)
		if m[2*i] >= 0 {
			parts[name] = p[m[2*i]:m[2*i+1]]
		}
	}
	if r.m != nil {
		key := make([]string, len(r.use.Key))
		for i, k := range r.use.Key {
			v, ok := parts[k]
			if !ok {
				return nil, UnmappedSession
			}
			key[i] = v
		}
		v, ok := r.m.Lookup(key)
		if !ok {
			return nil, UnmappedSession
		}
		parts[r.use.Gives] = v
	}
	return parts, NoSkip
}
