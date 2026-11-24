package profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"

	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// Expand checks a document against a catalog and returns it expanded: its
// bases' rules copied in, each base's own bases first, then the document's
// rules; every keyword its tag; the defaults explicit. Problems lists every
// problem found, in the order of the document; the expanded form is nil
// when there is one.
//
// A base named again, directly or inside another base, is used once, where
// it first comes: naming the basic profile after an option that is built on
// it must not let the basic profile override the option.
func Expand(doc *v1.ProfileDocument, cat *Catalog) (*v1.ExpandedProfile, Problems) {
	if doc == nil {
		return nil, Problems{{Code: CodeRequired, Message: "a profile needs a document"}}
	}
	chk := newChecker(false)
	top := at{c: chk}
	if len(doc.GetBases()) > MaxBases {
		top.in("bases").add(CodeTooMany, "a document names at most %d bases", MaxBases)
	}
	var order []*catalogBase
	named := map[string]bool{}
	seen := map[string]bool{}
	for i, n := range doc.GetBases() {
		if i >= MaxBases {
			break
		}
		a := top.index("bases", i)
		cb, ok := cat.bases[n]
		switch {
		case named[n]:
			a.add(CodeDuplicateBase, "%s is named twice", clipText(n))
			continue
		case !ok:
			a.add(CodeUnknownBase, "%q is not a base this server ships; ListProfileBases names them", clipText(n))
			continue
		case cb.reason != "":
			a.add(CodeBaseUnavailable, "%s is not available: %s", n, cb.reason)
			continue
		}
		named[n] = true
		order = cat.flatten(cb, seen, order)
	}
	rules, pixels := checkDocument(chk, doc)
	if len(chk.list) > 0 {
		return nil, chk.list
	}

	e := &v1.ExpandedProfile{Unnamed: v1.UnnamedElements_UNNAMED_ELEMENTS_KEEP, DateShiftDays: DefaultDateShiftDays, LanguageEdition: LanguageEdition}
	cost := chk.cost
	for _, cb := range order {
		e.Bases = append(e.Bases, cb.base.Name)
		for _, r := range cb.rules {
			e.Rules = append(e.Rules, expandedRule(r, cb.base.Name))
			cost += r.cost
		}
		for _, p := range cb.pixels {
			e.PixelRules = append(e.PixelRules, &v1.ExpandedPixelRule{Rule: clonePixelRule(p.norm), Origin: cb.base.Name})
			cost += p.cost
		}
		takeDefaults(e, cb.base.Document)
	}
	for _, r := range rules {
		e.Rules = append(e.Rules, expandedRule(r, ""))
	}
	for _, p := range pixels {
		e.PixelRules = append(e.PixelRules, &v1.ExpandedPixelRule{Rule: clonePixelRule(p.norm)})
	}
	takeDefaults(e, doc)
	e.LanguageEdition = editionOf(e)
	switch {
	case len(e.Rules) > MaxExpandedRules:
		top.in("bases").add(CodeTooMany, "with its bases the profile has %d rules, more than %d", len(e.Rules), MaxExpandedRules)
	case cost > MaxPatternBudget:
		top.in("bases").add(CodeTooLarge, "with its bases the profile's regular expressions compile to %d instructions and ranges, more than %d", cost, MaxPatternBudget)
	default:
		if n, err := answerBytes(doc, e); err != nil || n > MaxVersionBytes {
			top.add(CodeTooLarge, "the document and its expanded form come to %d bytes, more than the %d one answer may carry with them; make the document smaller", n, MaxVersionBytes)
		}
	}
	if len(chk.list) > 0 {
		return nil, chk.list
	}
	return e, nil
}

// LanguageEdition is the edition of the profile language this build
// writes into an expanded form: what each selector selects and each
// action does. A build runs only the editions it knows, and keeps the
// meaning of every edition it has shipped, so a stored version never
// changes what it does when the language grows.
//
// Edition 2 adds value patterns and redact. A profile that uses neither,
// its bases included, is written as edition 1, so its expanded text and
// checksum are what they were before edition 2 existed, and a build of
// edition 1 still runs it.
const LanguageEdition = 2

// editionOf returns the oldest language edition that has everything an
// expanded profile's rules use.
func editionOf(e *v1.ExpandedProfile) int32 {
	for _, er := range e.GetRules() {
		if usesValuePatterns(er.GetRule()) {
			return valuePatternEdition
		}
	}
	return 1
}

// MaxVersionBytes is the bound of a version's document and expanded form
// together, each as the most bytes its JSON may take in an answer: the
// API's message limit of 4 MiB less 256 KiB for the other fields (a name,
// a note, a checksum, the warnings). So a version the API accepts can
// always be answered whole, in either codec.
const MaxVersionBytes = 4<<20 - 256<<10

// answerBytes returns the most bytes the document and the expanded form
// take together in an answer: their canonical JSON, and one more byte for
// each comma, since the protobuf JSON encoder may write a space after any
// comma. Binary protobuf is never larger than the JSON.
func answerBytes(doc *v1.ProfileDocument, e *v1.ExpandedProfile) (int, error) {
	n := 0
	for _, m := range []proto.Message{doc, e} {
		c, err := Canonical(m)
		if err != nil {
			return 0, err
		}
		n += len(c) + bytes.Count(c, []byte{','})
	}
	return n, nil
}

// expandedRule returns a compiled rule as an expanded form holds it: the
// normalized rule, where it came from, and the VRs validation decided.
func expandedRule(r rule, origin string) *v1.ExpandedRule {
	return &v1.ExpandedRule{Rule: cloneRule(r.norm), Origin: origin, Vrs: slices.Clone(r.sel.vrs)}
}

// takeDefaults sets what doc says of the defaults over what came before
// it, so the document's own word wins over its bases' and a later base's
// over an earlier's.
func takeDefaults(e *v1.ExpandedProfile, doc *v1.ProfileDocument) {
	if u := doc.GetUnnamed(); u != v1.UnnamedElements_UNNAMED_ELEMENTS_UNSPECIFIED {
		e.Unnamed = u
	}
	if d := doc.GetDateShiftDays(); d != 0 {
		e.DateShiftDays = d
	}
	// Holding is written as unspecified, which holds, so the expanded form
	// of a document that says nothing, or says hold, is what it was before
	// the field existed, and so is its checksum.
	switch doc.GetUnsearchedSegments() {
	case v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_HOLD:
		e.UnsearchedSegments = v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_UNSPECIFIED
	case v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_KEEP:
		e.UnsearchedSegments = v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_KEEP
	}
}

// flatten appends cb to order after the bases it is built on, each base
// once, where it first comes.
func (c *Catalog) flatten(cb *catalogBase, seen map[string]bool, order []*catalogBase) []*catalogBase {
	if seen[cb.base.Name] {
		return order
	}
	seen[cb.base.Name] = true
	for _, n := range cb.base.Document.GetBases() {
		order = c.flatten(c.bases[n], seen, order)
	}
	return append(order, cb)
}

// Checksum returns an expanded profile's canonical text and its SHA-256 in
// lowercase hexadecimal.
func Checksum(e *v1.ExpandedProfile) (text []byte, sum string, err error) {
	if e == nil {
		return nil, "", errors.New("profile: no expanded profile")
	}
	text, err = Canonical(e)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(text)
	return text, hex.EncodeToString(h[:]), nil
}

// cloneRule returns a deep copy of a rule.
func cloneRule(r *v1.ProfileRule) *v1.ProfileRule {
	c, _ := proto.Clone(r).(*v1.ProfileRule)
	return c
}

// clonePixelRule returns a deep copy of a pixel rule.
func clonePixelRule(r *v1.PixelRule) *v1.PixelRule {
	c, _ := proto.Clone(r).(*v1.PixelRule)
	return c
}
