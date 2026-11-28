package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// validTemplate is a template of the device and size with a band across
// the top and a box in the bottom right corner.
func validTemplate(manufacturer, model string, rows, columns int32) *v1.PixelTemplate {
	return &v1.PixelTemplate{Manufacturer: manufacturer, Model: model, Rows: rows, Columns: columns, Rectangles: []*v1.PixelRectangle{
		{X: 0, Y: 0, Width: columns, Height: 4}, {X: 2, Y: 2, Width: 8, Height: 4, FromRight: true, FromBottom: true}}}
}

// addTemplates is an add of templates into lab1.
func addTemplates(ts ...*v1.PixelTemplate) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.deid.AddPixelTemplates, &v1.AddPixelTemplatesRequest{Tenant: "lab1", Templates: ts})
	}
}

// testAddPixelTemplates guards an add: admin on the tenant only; no
// template, a template with no manufacturer, a size of zero, no rectangle
// or one past the image, and a device given twice refused as invalid naming
// the template; a template the tenant holds already refused as a
// precondition with nothing of the request stored; the audit entry counts
// the templates and holds no name.
func testAddPixelTemplates(t *testing.T) {
	f := newFixture(t)
	past := validTemplate("Acme", "Past", 32, 32)
	past.Rectangles[0].Width = 33
	noName := validTemplate("", "Model", 32, 32)
	zero := validTemplate("Acme", "Zero", 0, 32)
	none := validTemplate("Acme", "None", 32, 32)
	none.Rectangles = nil
	badClass := validTemplate("Acme", "Class", 32, 32)
	badClass.SopClass = "not-a-uid"
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: addTemplates(validTemplate("Acme", "One", 32, 32)), code: connect.CodePermissionDenied},
		{name: "no template", as: f.lab1Admin, call: addTemplates(), code: connect.CodeInvalidArgument, msg: "templates"},
		{name: "no manufacturer", as: f.lab1Admin, call: addTemplates(noName), code: connect.CodeInvalidArgument, msg: "template 1: a template's manufacturer is required"},
		{name: "a size of zero", as: f.lab1Admin, call: addTemplates(validTemplate("Acme", "Ok", 32, 32), zero), code: connect.CodeInvalidArgument, msg: "template 2"},
		{name: "no rectangle", as: f.lab1Admin, call: addTemplates(none), code: connect.CodeInvalidArgument, msg: "rectangles"},
		{name: "a rectangle past the image", as: f.lab1Admin, call: addTemplates(past), code: connect.CodeInvalidArgument, msg: "rectangle 1 reaches past"},
		{name: "a SOP class that is no UID", as: f.lab1Admin, call: addTemplates(badClass), code: connect.CodeInvalidArgument, msg: "SOP class"},
		{name: "one device twice, one in upper case", as: f.lab1Admin,
			call: addTemplates(validTemplate("Acme", "Dup", 32, 32), validTemplate("ACME", "dup", 32, 32)), code: connect.CodeInvalidArgument, msg: "template 2 has the same device"},
		{name: "admin", as: f.lab1Admin, call: addTemplates(validTemplate("Acme", "One", 32, 32), validTemplate("Acme", "One", 64, 64)),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{tenant.DetailTemplates: "2"}},
			check: func(t *testing.T, o outcome) {
				if res := o.res.(*v1.AddPixelTemplatesResponse); res.GetAdded() != 2 {
					t.Errorf("added %d, want 2", res.GetAdded())
				}
			}},
		{name: "a template held already", as: f.lab1Admin, call: addTemplates(validTemplate("Acme", "Fresh", 32, 32), validTemplate("acme", "ONE", 32, 32)),
			code: connect.CodeFailedPrecondition, msg: "template 2 is already held",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed}},
	})
	list := f.as(f.lab1Operator)
	res := mustCall[*v1.ListPixelTemplatesResponse](t, "list", call(list.deid.ListPixelTemplates, &v1.ListPixelTemplatesRequest{Tenant: "lab1"}))
	if len(res.GetTemplates()) != 2 {
		t.Errorf("the tenant holds %d templates, want the two of the add that passed and none of a refused request", len(res.GetTemplates()))
	}
	f.flush()
	for _, en := range f.tenantLog("lab1") {
		for k, v := range en.Details {
			if v == "Acme" || v == "One" || k == "manufacturer" || k == "model" {
				t.Errorf("an audit entry holds a template's name: %s=%s", k, v)
			}
		}
	}
}

// testRemovePixelTemplate guards a removal: admin on the tenant only; the
// template of the key given with its case changed is removed and the
// others stay; a second removal is not found; a key that names no template
// is invalid; the audit entry names the key.
func testRemovePixelTemplate(t *testing.T) {
	f := newFixture(t)
	mustCall[*v1.AddPixelTemplatesResponse](t, "add", addTemplates(validTemplate("Acme", "One", 32, 32), validTemplate("Acme", "Two", 32, 32))(f.as(f.lab1Admin)))
	remove := func(model string, rows int32) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.deid.RemovePixelTemplate, &v1.RemovePixelTemplateRequest{Tenant: "lab1", Manufacturer: "ACME", Model: model, Rows: rows, Columns: 32})
		}
	}
	f.run(t, []tcase{
		{name: "operator", as: f.lab1Operator, call: remove("one", 32), code: connect.CodePermissionDenied},
		{name: "no model", as: f.lab1Admin, call: remove("", 32), code: connect.CodeInvalidArgument, msg: "manufacturer"},
		{name: "another size", as: f.lab1Admin, call: remove("one", 33), code: connect.CodeNotFound},
		{name: "admin, in other case", as: f.lab1Admin, call: remove("one", 32),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "acme|one|32x32"}},
		{name: "a second removal", as: f.lab1Admin, call: remove("one", 32), code: connect.CodeNotFound},
	})
	res := mustCall[*v1.ListPixelTemplatesResponse](t, "list", call(f.as(f.lab1Operator).deid.ListPixelTemplates, &v1.ListPixelTemplatesRequest{Tenant: "lab1"}))
	if len(res.GetTemplates()) != 1 || res.GetTemplates()[0].GetModel() != "Two" {
		t.Errorf("after the removal the tenant holds %v, want only Two", res.GetTemplates())
	}
}

// testListPixelTemplates guards the list: operator on the tenant (a reader
// is refused); templates as written, in order of manufacturer, model and
// size, with their rectangles; a page of one carries a token for the next.
func testListPixelTemplates(t *testing.T) {
	f := newFixture(t)
	mustCall[*v1.AddPixelTemplatesResponse](t, "add", addTemplates(validTemplate("Zeta", "Z", 16, 16), validTemplate("Acme", "Beta", 16, 16),
		validTemplate("Acme", "Alpha", 32, 32))(f.as(f.lab1Admin)))
	list := func(size int32) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.deid.ListPixelTemplates, &v1.ListPixelTemplatesRequest{Tenant: "lab1", PageSize: size})
		}
	}
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: list(0), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: list(0), audit: inTenant("lab1", master.OutcomeDone), check: func(t *testing.T, o outcome) {
			ts := o.res.(*v1.ListPixelTemplatesResponse).GetTemplates()
			if len(ts) != 3 || ts[0].GetModel() != "Alpha" || ts[1].GetModel() != "Beta" || ts[2].GetManufacturer() != "Zeta" {
				t.Fatalf("templates %v, want Alpha, Beta, Zeta in order", ts)
			}
			if r := ts[0].GetRectangles(); len(r) != 2 || r[0].GetWidth() != 32 || !r[1].GetFromRight() || !r[1].GetFromBottom() {
				t.Errorf("the rectangles are %v, want the two written", r)
			}
		}},
		{name: "a page of one", as: f.lab1Operator, call: list(1), check: func(t *testing.T, o outcome) {
			if res := o.res.(*v1.ListPixelTemplatesResponse); len(res.GetTemplates()) != 1 || res.GetNextPageToken() == "" {
				t.Errorf("a page of one: %v", res)
			}
		}},
	})
}
