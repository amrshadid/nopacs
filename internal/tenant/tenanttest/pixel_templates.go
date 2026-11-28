package tenanttest

import (
	"context"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ctClass is a SOP class used to tell a template for one class from one
// for any class.
const ctClass = "1.2.840.10008.5.1.4.1.1.2"

// pixelTemplate returns a valid template of the device and size.
func pixelTemplate(manufacturer, model string, rows, columns int32, class string) tenant.PixelTemplate {
	return tenant.PixelTemplate{Manufacturer: manufacturer, Model: model, Rows: rows, Columns: columns, SOPClass: class,
		Rectangles: []tenant.PixelRect{{X: 0, Y: 0, Width: columns, Height: 20}, {X: 4, Y: 6, Width: 30, Height: 10, FromRight: true, FromBottom: true}}}
}

// testPixelTemplates guards the table of per-device templates: a stored
// template reads back as it was written, is found with the manufacturer and
// model in any case and the padding of a DICOM value, is found by its exact
// size only, the template of the file's SOP class is found before the one
// for any class, a second template of one key is refused with nothing
// stored (a batch holding it stores none), the list pages in order of key,
// a removal is of one template and a second is NotFound, and a guard that
// refuses rolls the change back.
func testPixelTemplates(e *penv) {
	st, ok := e.p.(tenant.PixelTemplates)
	if !ok {
		e.Fatal("the store keeps no pixel templates")
	}
	any512 := pixelTemplate("ACME Imaging", "Scan One", 512, 512, "")
	ct512 := pixelTemplate("ACME Imaging", "Scan One", 512, 512, ctClass)
	ct512.Rectangles = []tenant.PixelRect{{X: 1, Y: 2, Width: 3, Height: 4}}
	other := pixelTemplate("Zeta", "Z1", 480, 640, "")
	e.ok("AddPixelTemplates", st.AddPixelTemplates(e.ctx, []tenant.PixelTemplate{any512, other}, e.audit(), nil))

	got, found, err := st.FindPixelTemplate(e.ctx, tenant.PixelTemplateQuery{Manufacturer: "acme imaging ", Model: "SCAN ONE\x00", Rows: 512, Columns: 512, SOPClass: ctClass})
	e.ok("FindPixelTemplate", err)
	if !found || got.Manufacturer != "ACME Imaging" || len(got.Rectangles) != 2 || got.Rectangles[1] != any512.Rectangles[1] || got.SOPClass != "" {
		e.Errorf("a template found with case and padding changed is %+v (found %v), want the one stored", got, found)
	}
	for _, q := range []tenant.PixelTemplateQuery{
		{Manufacturer: "ACME Imaging", Model: "Scan One", Rows: 512, Columns: 511},
		{Manufacturer: "ACME Imaging", Model: "Scan One", Rows: 511, Columns: 512},
		{Manufacturer: "ACME Imaging", Model: "Scan"},
		{Manufacturer: "ACME", Model: "Scan One", Rows: 512, Columns: 512},
	} {
		if _, found, err := st.FindPixelTemplate(e.ctx, q); err != nil || found {
			e.Errorf("a template found for %+v: %v (%v), want none", q, found, err)
		}
	}

	e.ok("AddPixelTemplates for a class", st.AddPixelTemplates(e.ctx, []tenant.PixelTemplate{ct512}, e.audit(), nil))
	if got, found, err = st.FindPixelTemplate(e.ctx, tenant.PixelTemplateQuery{Manufacturer: "acme imaging", Model: "scan one", Rows: 512, Columns: 512, SOPClass: ctClass}); err != nil ||
		!found || got.SOPClass != ctClass || len(got.Rectangles) != 1 {
		e.Errorf("the template of the file's SOP class is %+v (%v, found %v), want it before the one for any class", got, err, found)
	}
	if got, _, _ = st.FindPixelTemplate(e.ctx, tenant.PixelTemplateQuery{Manufacturer: "acme imaging", Model: "scan one", Rows: 512, Columns: 512, SOPClass: "1.2.3"}); got.SOPClass != "" {
		e.Errorf("a file of another class got the template of class %q, want the one for any class", got.SOPClass)
	}

	dup := pixelTemplate("acme IMAGING", " scan one", 512, 512, "")
	fresh := pixelTemplate("Fresh", "F", 100, 100, "")
	e.wantPrecondition("a key held already", st.AddPixelTemplates(e.ctx, []tenant.PixelTemplate{fresh, dup}, e.audit(), nil))
	if _, found, _ = st.FindPixelTemplate(e.ctx, tenant.PixelTemplateQuery{Manufacturer: "Fresh", Model: "F", Rows: 100, Columns: 100}); found {
		e.Errorf("a template of a refused batch was stored")
	}
	e.wantKind("one key twice in a batch", st.AddPixelTemplates(e.ctx, []tenant.PixelTemplate{fresh, fresh}, e.audit(), nil), fault.Invalid)
	bad := fresh
	bad.Rectangles = []tenant.PixelRect{{X: 90, Y: 0, Width: 20, Height: 5}}
	e.wantKind("a rectangle past the image", st.AddPixelTemplates(e.ctx, []tenant.PixelTemplate{bad}, e.audit(), nil), fault.Invalid)
	e.wantKind("no rectangle", st.AddPixelTemplates(e.ctx, []tenant.PixelTemplate{{Manufacturer: "A", Model: "B", Rows: 1, Columns: 1}}, e.audit(), nil), fault.Invalid)
	e.wantKind("an empty batch", st.AddPixelTemplates(e.ctx, nil, e.audit(), nil), fault.Invalid)

	first, next, err := st.ListPixelTemplates(e.ctx, master.Page{Limit: 2})
	e.ok("ListPixelTemplates", err)
	if len(first) != 2 || next == "" || first[0].Manufacturer != "ACME Imaging" || first[0].SOPClass != "" || first[1].SOPClass != ctClass {
		e.Fatalf("the first page is %+v (next %q), want the two ACME templates, the one for any class first", first, next)
	}
	rest, next, err := st.ListPixelTemplates(e.ctx, master.Page{Limit: 2, Cursor: next})
	e.ok("ListPixelTemplates, next page", err)
	if len(rest) != 1 || rest[0].Manufacturer != "Zeta" || next != "" {
		e.Errorf("the last page is %+v (next %q), want Zeta alone", rest, next)
	}

	refuse := func(context.Context) error { return fault.New(fault.Unavailable, "refused") }
	e.wantKind("a refused guard", st.RemovePixelTemplate(e.ctx, other.Key(), e.audit(), refuse), fault.Unavailable)
	if _, found, _ = st.FindPixelTemplate(e.ctx, tenant.PixelTemplateQuery{Manufacturer: "zeta", Model: "z1", Rows: 480, Columns: 640}); !found {
		e.Errorf("a removal whose guard refused took the template away")
	}
	e.ok("RemovePixelTemplate", st.RemovePixelTemplate(e.ctx, other.Key(), e.audit(), nil))
	e.wantKind("a second removal", st.RemovePixelTemplate(e.ctx, other.Key(), e.audit(), nil), fault.NotFound)
	if _, found, _ = st.FindPixelTemplate(e.ctx, tenant.PixelTemplateQuery{Manufacturer: "acme imaging", Model: "scan one", Rows: 512, Columns: 512}); !found {
		e.Errorf("removing one template took another")
	}
}
