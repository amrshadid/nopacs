package profile

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/amrshadid/go-dicom/anonymize"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// baseName is the rule of a base's name: a family, a name and an edition,
// such as "annex-e/basic:2026d". A new edition of a base is a new name, so
// a stored version that names one never changes meaning.
var baseName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}/[a-z][a-z0-9-]{0,47}:[a-z0-9][a-z0-9.]{0,15}$`)

// Base is a profile the server ships, which a document may name among its
// bases.
type Base struct {
	// Name is the base's name, such as "annex-e/basic:2026d".
	Name string
	// Description says what the base does, for a person.
	Description string
	// Document is the base as a document; its bases name other bases of the
	// same catalog. Nil for a base that is not available.
	Document *v1.ProfileDocument
	// Unavailable, when set, says why this build cannot expand the base: a
	// document that names it is refused with this reason.
	Unavailable string
}

// Catalog is a set of bases, checked whole when it is made: every base's
// document is valid, every base it names is in the catalog, and no base
// names itself through others. It is never changed after it is made, so
// one catalog is shared by every expansion.
type Catalog struct {
	bases map[string]*catalogBase
	names []string
}

// catalogBase is a base with its compiled rules, and why it cannot be used
// when it, or a base it names, is unavailable.
type catalogBase struct {
	base   Base
	rules  []rule
	pixels []pixelRule
	reason string
}

// NewCatalog checks bases and returns them as a catalog.
func NewCatalog(bases ...Base) (*Catalog, error) {
	c := &Catalog{bases: map[string]*catalogBase{}}
	for _, b := range bases {
		if !baseName.MatchString(b.Name) {
			return nil, fmt.Errorf("profile: %q is not a base's name: family/name:edition", b.Name)
		}
		if _, dup := c.bases[b.Name]; dup {
			return nil, fmt.Errorf("profile: the base %s is given twice", b.Name)
		}
		cb := &catalogBase{base: b, reason: b.Unavailable}
		switch {
		case b.Unavailable == "" && b.Document == nil:
			return nil, fmt.Errorf("profile: the base %s has neither a document nor a reason it is unavailable", b.Name)
		case b.Unavailable != "" && b.Document != nil:
			return nil, fmt.Errorf("profile: the base %s has a document and is unavailable", b.Name)
		case b.Document != nil:
			chk := newChecker(false)
			cb.rules, cb.pixels = checkDocument(chk, b.Document)
			if len(chk.list) > 0 {
				return nil, fmt.Errorf("profile: the base %s: %w", b.Name, chk.list)
			}
			if len(b.Document.GetBases()) > MaxBases {
				return nil, fmt.Errorf("profile: the base %s names more than %d bases", b.Name, MaxBases)
			}
		}
		c.bases[b.Name] = cb
		c.names = append(c.names, b.Name)
	}
	slices.Sort(c.names)
	for _, n := range c.names {
		if err := c.resolve(c.bases[n], nil); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// errCycle is the cause of a catalog whose bases name each other in a
// circle.
var errCycle = errors.New("profile: bases name each other in a circle")

// resolve checks that every base cb names exists and that none leads back
// to a base on path, and sets cb's reason when a base it names cannot be
// used.
func (c *Catalog) resolve(cb *catalogBase, path []string) error {
	if slices.Contains(path, cb.base.Name) {
		return fmt.Errorf("%w: %v", errCycle, append(path, cb.base.Name))
	}
	path = append(path, cb.base.Name)
	for _, n := range cb.base.Document.GetBases() {
		inner, ok := c.bases[n]
		if !ok {
			return fmt.Errorf("profile: the base %s names %s, which the catalog does not hold", cb.base.Name, n)
		}
		if err := c.resolve(inner, path); err != nil {
			return err
		}
		if inner.reason != "" && cb.reason == "" {
			cb.reason = fmt.Sprintf("it is built on %s, which is not available: %s", n, inner.reason)
		}
	}
	return nil
}

// Names returns the names of the catalog's bases, in byte order.
func (c *Catalog) Names() []string { return slices.Clone(c.names) }

// Lookup returns a base by name, whether the catalog holds it, and why a
// document may not name it: empty when it may, else the reason it, or a
// base it is built on, is unavailable.
func (c *Catalog) Lookup(name string) (b Base, unavailable string, found bool) {
	cb, ok := c.bases[name]
	if !ok {
		return Base{}, "", false
	}
	return cb.base, cb.reason, true
}

// Proto returns the catalog as the API answers it, by name in byte order.
func (c *Catalog) Proto() []*v1.ProfileBase {
	out := make([]*v1.ProfileBase, 0, len(c.names))
	for _, n := range c.names {
		cb := c.bases[n]
		pb := &v1.ProfileBase{Name: n, Available: cb.reason == "", UnavailableReason: cb.reason, Description: cb.base.Description}
		if cb.reason == "" {
			pb.Document = cloneDocument(cb.base.Document)
		}
		out = append(out, pb)
	}
	return out
}

// The bases of PS3.15 Annex E: the basic profile and its options (PS3.15
// E.3). They are generated from the standard's tables, which the DICOM
// library holds: each column of Table E.1-1 by annexEDocument, and the
// Retain Safe Private option from Table E.3.10-1 (safePrivateDocument).
// NoPACs keeps no copy of either, which would drift from the library's.
//
// A base is named with the edition of the standard its tables are, and is
// frozen with it: a version stores its rules expanded, so a later build
// never changes what a stored version does, and a later edition of the
// tables is a new base name beside this one. Should the library's tables
// be of another edition than AnnexEEdition, every Annex E base is refused
// as unavailable rather than built from tables that are not the edition
// its name says; the package's tests hold each base's checksum, so a
// change of the library's table under the same edition fails the build.
//
// A name of an earlier edition (2026c) is not in the catalog and is
// refused as unknown: no build that could store a version naming one was
// ever released, so no stored version names one.
const (
	// AnnexEEdition is the edition of PS3.15 the Annex E bases follow.
	AnnexEEdition = "2026d"
	// reasonVisualFeatures is why Clean Recognizable Visual Features is
	// not available as a base: it removes faces from the pixels (defacing),
	// which this build does not do.
	reasonVisualFeatures = "it removes recognizable visual features such as faces from the pixels, which this build does not do"
)

// annexEKind is where an Annex E base's rules come from.
type annexEKind int

// The kinds of Annex E base.
const (
	// annexEColumn: a column of Table E.1-1.
	annexEColumn annexEKind = iota + 1
	// annexESafePrivate: Table E.3.10-1.
	annexESafePrivate
	// annexECleanPixels: Clean Pixel Data, which has no column in Table
	// E.1-1 and so no rules: its work is the profile's pixel rules and the
	// gate that holds an image at risk no pixel rule matches, which every
	// profile has. Its code is recorded only in a file whose pixels a
	// pixel rule filled, by the engine, never for the base alone.
	annexECleanPixels
	// annexEVisualFeatures: Clean Recognizable Visual Features, not
	// available (reasonVisualFeatures).
	annexEVisualFeatures
)

// annexE lists each Annex E base: its name without the edition, what it
// is, where its rules come from (and the column of Table E.1-1 for a
// column), and its code in PS3.16 CID 7050, which De-identification Method
// Code Sequence (0012,0064) records (PS3.15 E.1.1).
var annexE = []struct {
	name, description string
	kind              annexEKind
	column            anonymize.ProfileColumn
	code              anonymize.MethodCodeValue
}{
	{"basic", "PS3.15 E.2, the Basic Application Level Confidentiality Profile: the actions of Table E.1-1's basic column.", annexEColumn, anonymize.ColumnBasic, anonymize.MethodBasicProfile},
	{"clean-pixel-data", "PS3.15 E.3.1, Clean Pixel Data: burned-in identifying information is removed from the pixels.", annexECleanPixels, 0, anonymize.MethodCleanPixelData},
	{"clean-recognizable-visual-features", "PS3.15 E.3.2, Clean Recognizable Visual Features.", annexEVisualFeatures, 0, anonymize.MethodCleanRecognizableVisualFeatures},
	{"clean-graphics", "PS3.15 E.3.3, Clean Graphics: identifying text in graphics and overlays is removed.", annexEColumn, anonymize.ColumnCleanGraphics, anonymize.MethodCleanGraphics},
	{"clean-structured-content", "PS3.15 E.3.4, Clean Structured Content: identifying content items of structured reports are cleaned.", annexEColumn, anonymize.ColumnCleanStructuredContent, anonymize.MethodCleanStructuredContent},
	{"clean-descriptors", "PS3.15 E.3.5, Clean Descriptors: free-text descriptions are cleaned rather than kept or removed.", annexEColumn, anonymize.ColumnCleanDescriptors, anonymize.MethodCleanDescriptors},
	{"retain-longitudinal-full-dates", "PS3.15 E.3.6, Retain Longitudinal Temporal Information with Full Dates.", annexEColumn, anonymize.ColumnRetainLongitudinalFullDates, anonymize.MethodRetainLongitudinalFullDates},
	{"retain-longitudinal-modified-dates", "PS3.15 E.3.6, Retain Longitudinal Temporal Information with Modified Dates: dates are shifted, not removed.", annexEColumn, anonymize.ColumnRetainLongitudinalModifiedDates, anonymize.MethodRetainLongitudinalModifiedDates},
	{"retain-patient-characteristics", "PS3.15 E.3.7, Retain Patient Characteristics: sex, age, size, weight and the like are kept.", annexEColumn, anonymize.ColumnRetainPatientCharacteristics, anonymize.MethodRetainPatientCharacteristics},
	{"retain-device-identity", "PS3.15 E.3.8, Retain Device Identity.", annexEColumn, anonymize.ColumnRetainDeviceIdentity, anonymize.MethodRetainDeviceIdentity},
	{"retain-uids", "PS3.15 E.3.9, Retain UIDs: original UIDs are kept.", annexEColumn, anonymize.ColumnRetainUIDs, anonymize.MethodRetainUIDs},
	{"retain-safe-private", "PS3.15 E.3.10, Retain Safe Private: the private attributes Table E.3.10-1 lists as safe are kept.", annexESafePrivate, anonymize.ColumnRetainSafePrivate, anonymize.MethodRetainSafePrivate},
	{"retain-institution-identity", "PS3.15 E.3.11, Retain Institution Identity.", annexEColumn, anonymize.ColumnRetainInstitutionIdentity, anonymize.MethodRetainInstitutionIdentity},
}

// AnnexE returns the name of an Annex E base of this edition, such as
// AnnexE("basic") for "annex-e/basic:2026d".
func AnnexE(name string) string { return "annex-e/" + name + ":" + AnnexEEdition }

// AnnexEName splits the name of an Annex E base, "annex-e/<option>:<edition>",
// into its option and edition; ok is false for a name of another family
// or form.
func AnnexEName(name string) (option, edition string, ok bool) {
	rest, found := strings.CutPrefix(name, "annex-e/")
	if !found {
		return "", "", false
	}
	option, edition, found = strings.Cut(rest, ":")
	return option, edition, found && option != "" && edition != ""
}

// MethodCodes returns the PS3.16 CID 7050 codes of the Annex E bases among
// bases (an expanded version's, each base once), in the order of the list
// of Annex E bases: the basic profile, then each option. A base is matched
// by its option, whatever edition its stored name carries, so what a
// stored version records never depends on the edition this build ships. A
// base of another family has none. The engine records them in
// De-identification Method Code Sequence (0012,0064) (PS3.15 E.1.1). Clean
// Pixel Data's code is not among them: it says the pixels of the file were
// cleaned, which is true only of a file a pixel rule filled, and the engine
// adds it to that file alone (CleanPixelDataCode).
func MethodCodes(bases []string) []anonymize.MethodCode {
	have := map[string]bool{}
	for _, b := range bases {
		if option, _, ok := AnnexEName(b); ok {
			have[option] = true
		}
	}
	var out []anonymize.MethodCode
	for _, b := range annexE {
		if have[b.name] && b.kind != annexECleanPixels {
			out = append(out, b.code.Code())
		}
	}
	return out
}

// CleanPixelDataCode is the PS3.16 CID 7050 code of the Clean Pixel Data
// Option (PS3.15 E.3.1), which the engine records in a file whose pixels a
// pixel rule filled.
func CleanPixelDataCode() anonymize.MethodCode { return anonymize.MethodCleanPixelData.Code() }

// KeepsBaseChange reports whether a rule of a version's own keeps elements
// a base of it changes or removes (Warnings, CodeKeepsBaseChange), or
// whether it cannot tell because the warnings were cut at MaxWarnings. A
// file made by such a version is not what its bases' codes would claim,
// so the engine records no codes for it.
func KeepsBaseChange(e *v1.ExpandedProfile) (bool, error) {
	ws, err := Warnings(e)
	if err != nil {
		return false, err
	}
	for _, w := range ws {
		if w.Code == CodeKeepsBaseChange || w.Code == CodeMoreWarnings {
			return true, nil
		}
	}
	return false, nil
}

// annexEBase builds one Annex E base from the library's tables, or says
// why it cannot be.
func annexEBase(name, description string, kind annexEKind, column anonymize.ProfileColumn, modifiedDates bool) Base {
	b := Base{Name: AnnexE(name), Description: description}
	switch {
	case kind == annexEVisualFeatures:
		b.Unavailable = reasonVisualFeatures
		return b
	case kind == annexECleanPixels:
		b.Document = &v1.ProfileDocument{}
		return b
	case anonymize.ProfileTableEdition != AnnexEEdition:
		b.Unavailable = fmt.Sprintf("the DICOM library's Table E.1-1 is of edition %s, not %s", anonymize.ProfileTableEdition, AnnexEEdition)
		return b
	case kind == annexESafePrivate && anonymize.SafePrivateEdition != AnnexEEdition:
		b.Unavailable = fmt.Sprintf("the DICOM library's Table E.3.10-1 is of edition %s, not %s", anonymize.SafePrivateEdition, AnnexEEdition)
		return b
	}
	doc, err := annexEDocument(annexERows(column), column.String(), modifiedDates)
	if err != nil {
		// The table is fixed in the library and every code is checked by
		// the package's tests; a failure here is a defect of the build.
		panic(err)
	}
	if kind == annexESafePrivate {
		doc.Rules = append(doc.Rules, safePrivateDocument().GetRules()...)
	}
	b.Document = doc
	return b
}

// shippedBases is every base the binary ships.
func shippedBases() []Base {
	var out []Base
	for _, b := range annexE {
		out = append(out, annexEBase(b.name, b.description, b.kind, b.column, b.column == anonymize.ColumnRetainLongitudinalModifiedDates))
	}
	// The data set as The Cancer Imaging Archive publishes it: the basic
	// profile with its six options, dates shifted so intervals between
	// studies survive.
	out = append(out, Base{
		Name:        "nopacs/longitudinal:1",
		Description: "The basic profile with clean pixel data, clean descriptors, retain longitudinal temporal information with modified dates, retain patient characteristics, retain device identity and retain safe private: dates are shifted per subject, so intervals between studies survive.",
		Document: &v1.ProfileDocument{Bases: []string{
			AnnexE("basic"),
			AnnexE("clean-pixel-data"),
			AnnexE("clean-descriptors"),
			AnnexE("retain-longitudinal-modified-dates"),
			AnnexE("retain-patient-characteristics"),
			AnnexE("retain-device-identity"),
			AnnexE("retain-safe-private"),
		}},
	})
	// The least that leaves a file useful: the cleaning options, every
	// date reduced to its year, and every element no rule names removed.
	// A VR rule is the least specific, so a date the bases name keeps the
	// bases' action; a date they leave is reduced to its year.
	out = append(out, Base{
		Name:        "nopacs/strict:1",
		Description: "The basic profile with clean descriptors, clean structured content and clean graphics; dates the bases leave are reduced to their year, private elements are removed, and an element no rule names is removed.",
		Document: &v1.ProfileDocument{
			Bases: []string{
				AnnexE("basic"),
				AnnexE("clean-descriptors"),
				AnnexE("clean-structured-content"),
				AnnexE("clean-graphics"),
			},
			Unnamed: v1.UnnamedElements_UNNAMED_ELEMENTS_REMOVE,
			Rules: []*v1.ProfileRule{
				{Name: "dates-to-year", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Vr{Vr: "DA"}}, Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Generalize{Generalize: &v1.GeneralizeAction{}}}, Reason: "a date the bases leave is reduced to its year"},
				{Name: "datetimes-to-year", Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Vr{Vr: "DT"}}, Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Generalize{Generalize: &v1.GeneralizeAction{}}}, Reason: "a date and time the bases leave is reduced to its year"},
			},
		},
	})
	return out
}

// Shipped returns the catalog of the bases this binary ships.
var Shipped = sync.OnceValue(func() *Catalog {
	c, err := NewCatalog(shippedBases()...)
	if err != nil {
		// The shipped bases are fixed in the binary and checked by the
		// package's tests; a failure here is a defect of the build.
		panic(err)
	}
	return c
})
