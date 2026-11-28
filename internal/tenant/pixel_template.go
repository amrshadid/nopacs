package tenant

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The bounds of a tenant's pixel templates.
const (
	// MaxPixelTemplateBatch is the most templates one AddPixelTemplates
	// takes: a site's own list in one transaction, all or none.
	MaxPixelTemplateBatch = 1000
	// MaxPixelTemplates is the most templates a tenant holds. A file's match
	// is one point read of the table's key, so the bound is for the list a
	// person reads, not for speed.
	MaxPixelTemplates = 10000
	// MaxPixelTemplateRectangles is the most rectangles one template fills,
	// the bound of a pixel rule's own rectangles.
	MaxPixelTemplateRectangles = 64
	// MaxPixelTemplateText is the longest manufacturer or model: the longest
	// LO value is 64 characters (PS3.5 6.2), four bytes each at most.
	MaxPixelTemplateText = 256
	// MaxPixelSide is the largest Rows or Columns of an image (PS3.5 6.2: US
	// is 16 bits).
	MaxPixelSide = 65535
)

// The details a store adds to the audit entry of a change of pixel
// templates: counts and the match keys, which are a device's, never an
// image's.
const (
	// DetailTemplates is how many templates an add stored.
	DetailTemplates = "templates"
)

// PixelRect is a rectangle of an image, in pixels, measured from the edges
// the fields name (the same meaning as a profile's pixel rectangle).
type PixelRect struct {
	// X and Y are the distance of the rectangle's left side and top from the
	// left and top edges, or, with FromRight and FromBottom, of its right
	// side and bottom from the right and bottom edges.
	X, Y int32
	// Width and Height are the rectangle's size.
	Width, Height int32
	// FromRight and FromBottom say which edge X and Y are measured from.
	FromRight, FromBottom bool
}

// PixelTemplate is the part of an image of one device and size to fill:
// burned-in text a model draws in the same place on every image of its
// size. It matches an image by its manufacturer and model (compared with
// case folded), its Rows and Columns (exactly) and, when SOPClass is not
// empty, its SOP class.
type PixelTemplate struct {
	// Manufacturer and Model are Manufacturer (0008,0070) and Manufacturer's
	// Model Name (0008,1090), as the person wrote them.
	Manufacturer, Model string
	// Rows and Columns are the image's size: Rows (0028,0010) and Columns
	// (0028,0011), 1 to MaxPixelSide.
	Rows, Columns int32
	// SOPClass is the SOP Class UID the template is for; empty for any
	// class.
	SOPClass string
	// Rectangles are what is filled, 1 to MaxPixelTemplateRectangles.
	Rectangles []PixelRect
}

// PixelTemplateKey is what makes two templates the same: the folded
// manufacturer and model, the size and the SOP class ("" for any).
type PixelTemplateKey struct {
	// Manufacturer and Model are FoldTemplateText of the given text.
	Manufacturer, Model string
	// Rows and Columns are the image's size.
	Rows, Columns int32
	// SOPClass is the SOP Class UID, or "" for any.
	SOPClass string
}

// String is the key as a person reads it, for an audit entry's object.
func (k PixelTemplateKey) String() string {
	s := k.Manufacturer + "|" + k.Model + "|" + strconv.Itoa(int(k.Rows)) + "x" + strconv.Itoa(int(k.Columns))
	if k.SOPClass != "" {
		s += "|" + k.SOPClass
	}
	return s
}

// FoldTemplateText returns what a manufacturer or model is compared by:
// without the padding of a DICOM text value (spaces and NULs at either
// end), in lower case.
func FoldTemplateText(s string) string {
	return strings.ToLower(strings.Trim(s, " \x00"))
}

// Key returns the template's key.
func (t PixelTemplate) Key() PixelTemplateKey {
	return PixelTemplateKey{Manufacturer: FoldTemplateText(t.Manufacturer), Model: FoldTemplateText(t.Model),
		Rows: t.Rows, Columns: t.Columns, SOPClass: t.SOPClass}
}

// PixelTemplateQuery is an image's facts, as received, a template is looked
// up by.
type PixelTemplateQuery struct {
	// Manufacturer and Model are as the file gave them; they are folded
	// for the match.
	Manufacturer, Model string
	// Rows and Columns are the image's size.
	Rows, Columns int32
	// SOPClass is the file's SOP Class UID.
	SOPClass string
}

// PixelTemplates keeps a tenant's pixel templates, and finds the one that
// fits an image. A pixel rule that fills by template reads it; an
// administrator changes it.
type PixelTemplates interface {
	// AddPixelTemplates stores templates, all in one transaction or none,
	// with the audit entry (the details gain DetailTemplates), under guard.
	// templates is a batch ValidatePixelTemplates returned. A template whose
	// key the tenant already holds is a Precondition failure naming its
	// position, from 1, and nothing is stored; so is a batch that takes the
	// tenant past MaxPixelTemplates.
	AddPixelTemplates(ctx context.Context, templates []PixelTemplate, audit master.AuditEntry, guard Guard) error
	// RemovePixelTemplate removes the template of the key, with the audit
	// entry in the same transaction, under guard; NotFound when there is
	// none.
	RemovePixelTemplate(ctx context.Context, key PixelTemplateKey, audit master.AuditEntry, guard Guard) error
	// ListPixelTemplates returns a page of the templates in order of key.
	ListPixelTemplates(ctx context.Context, page master.Page) ([]PixelTemplate, string, error)
	// FindPixelTemplate returns the template that fits the image: the one of
	// its SOP class when there is one, else the one for any class; false
	// when none fits.
	FindPixelTemplate(ctx context.Context, q PixelTemplateQuery) (PixelTemplate, bool, error)
}

// Validate returns an Invalid error unless t can be stored.
func (t PixelTemplate) Validate() error {
	for _, f := range []struct{ what, v string }{{"manufacturer", t.Manufacturer}, {"model", t.Model}} {
		switch folded := FoldTemplateText(f.v); {
		case folded == "":
			return invalid("a template's " + f.what + " is required")
		case len(f.v) > MaxPixelTemplateText:
			return invalid("a template's " + f.what + " is at most " + strconv.Itoa(MaxPixelTemplateText) + " bytes")
		case !validTemplateText(f.v):
			return invalid("a template's " + f.what + " is UTF-8 text without control characters")
		}
	}
	if t.Rows < 1 || t.Rows > MaxPixelSide || t.Columns < 1 || t.Columns > MaxPixelSide {
		return invalid("a template's rows and columns are 1 to " + strconv.Itoa(MaxPixelSide))
	}
	if t.SOPClass != "" {
		if err := validateUID("SOP class", t.SOPClass); err != nil {
			return err
		}
	}
	if n := len(t.Rectangles); n == 0 || n > MaxPixelTemplateRectangles {
		return invalid("a template fills 1 to " + strconv.Itoa(MaxPixelTemplateRectangles) + " rectangles")
	}
	for i, r := range t.Rectangles {
		at := "rectangle " + strconv.Itoa(i+1)
		switch {
		case r.X < 0 || r.Y < 0:
			return invalid(at + ": x and y are at least 0")
		case r.Width < 1 || r.Height < 1 || r.Width > MaxPixelSide || r.Height > MaxPixelSide:
			return invalid(at + ": width and height are 1 to " + strconv.Itoa(MaxPixelSide))
		case int64(r.X)+int64(r.Width) > int64(t.Columns):
			return invalid(at + " reaches past the image's " + strconv.Itoa(int(t.Columns)) + " columns")
		case int64(r.Y)+int64(r.Height) > int64(t.Rows):
			return invalid(at + " reaches past the image's " + strconv.Itoa(int(t.Rows)) + " rows")
		}
	}
	return nil
}

// validTemplateText reports whether s, without its padding, is UTF-8 with
// no control character.
func validTemplateText(s string) bool {
	s = strings.Trim(s, " \x00")
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			return false
		}
	}
	return true
}

// ValidatePixelTemplates returns templates, or an Invalid error unless they
// are 1 to MaxPixelTemplateBatch templates that each validate and no two
// have one key. An error names a template by its position, from 1.
func ValidatePixelTemplates(templates []PixelTemplate) ([]PixelTemplate, error) {
	if len(templates) == 0 || len(templates) > MaxPixelTemplateBatch {
		return nil, invalid("a batch holds 1 to " + strconv.Itoa(MaxPixelTemplateBatch) + " templates")
	}
	seen := make(map[PixelTemplateKey]int, len(templates))
	for i, t := range templates {
		at := "template " + strconv.Itoa(i+1)
		if err := t.Validate(); err != nil {
			return nil, fault.Wrap(fault.Invalid, err, at+": "+fault.Message(err))
		}
		k := t.Key()
		if first, dup := seen[k]; dup {
			return nil, invalid(at + " has the same device, size and SOP class as template " + strconv.Itoa(first))
		}
		seen[k] = i + 1
	}
	return templates, nil
}

// ValidatePixelTemplateKey returns an Invalid error unless k can name a
// template.
func ValidatePixelTemplateKey(k PixelTemplateKey) error {
	if k.Manufacturer == "" || k.Model == "" || k.Rows < 1 || k.Columns < 1 || k.Rows > MaxPixelSide || k.Columns > MaxPixelSide {
		return invalid("a template is named by its manufacturer, model, rows and columns")
	}
	if k.SOPClass != "" {
		return validateUID("SOP class", k.SOPClass)
	}
	return nil
}
