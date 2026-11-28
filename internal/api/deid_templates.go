package api

import (
	"context"
	"strconv"
	"unicode/utf8"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// templatesOf returns the tenant's pixel templates port.
func templatesOf(st tenant.Store) (tenant.PixelTemplates, error) {
	p, ok := st.(tenant.PixelTemplates)
	if !ok {
		return nil, fault.New(fault.Internal, "api: the tenant's store keeps no pixel templates")
	}
	return p, nil
}

// inTemplates is inTenant for the pixel template methods: the tenant's store
// and its templates' port, or the error already recorded.
func (c *core) inTemplates(ctx context.Context, d access.Decision, name string, details map[string]string) (tenant.PixelTemplates, tenant.Store, error) {
	_, st, err := c.inTenant(ctx, d, name, details)
	if err != nil {
		return nil, nil, err
	}
	p, err := templatesOf(st)
	if err != nil {
		return nil, nil, c.failIn(d, st, err)
	}
	return p, st, nil
}

// templateOf returns the tenant's form of a template of a request.
func templateOf(t *v1.PixelTemplate) tenant.PixelTemplate {
	out := tenant.PixelTemplate{Manufacturer: t.GetManufacturer(), Model: t.GetModel(), Rows: t.GetRows(), Columns: t.GetColumns(),
		SOPClass: t.GetSopClass(), Rectangles: make([]tenant.PixelRect, 0, len(t.GetRectangles()))}
	for _, r := range t.GetRectangles() {
		out.Rectangles = append(out.Rectangles, tenant.PixelRect{X: r.GetX(), Y: r.GetY(), Width: r.GetWidth(), Height: r.GetHeight(),
			FromRight: r.GetFromRight(), FromBottom: r.GetFromBottom()})
	}
	return out
}

// templateView returns the API's form of a stored template.
func templateView(t tenant.PixelTemplate) *v1.PixelTemplate {
	out := &v1.PixelTemplate{Manufacturer: t.Manufacturer, Model: t.Model, Rows: t.Rows, Columns: t.Columns, SopClass: t.SOPClass,
		Rectangles: make([]*v1.PixelRectangle, 0, len(t.Rectangles))}
	for _, r := range t.Rectangles {
		out.Rectangles = append(out.Rectangles, &v1.PixelRectangle{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height,
			FromRight: r.FromRight, FromBottom: r.FromBottom})
	}
	return out
}

// templateKeyOf returns the key a removal names.
func templateKeyOf(r *v1.RemovePixelTemplateRequest) tenant.PixelTemplateKey {
	return tenant.PixelTemplateKey{Manufacturer: tenant.FoldTemplateText(r.GetManufacturer()), Model: tenant.FoldTemplateText(r.GetModel()),
		Rows: r.GetRows(), Columns: r.GetColumns(), SOPClass: r.GetSopClass()}
}

// templateObject returns the audit object of a removal: the template's key,
// or invalidValue when the key could not be written in an audit entry.
func templateObject(r *v1.RemovePixelTemplateRequest) string {
	s := templateKeyOf(r).String()
	if len(s) > master.MaxAuditFieldLength || !utf8.ValidString(s) {
		return invalidValue
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7F {
			return invalidValue
		}
	}
	return s
}

// AddPixelTemplates stores pixel templates, all or none.
func (s deidService) AddPixelTemplates(ctx context.Context, req *v1.AddPixelTemplatesRequest) (*v1.AddPixelTemplatesResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.DeidServiceAddPixelTemplatesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTemplates(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	facts := access.Facts{Details: map[string]string{tenant.DetailTemplates: strconv.Itoa(len(req.GetTemplates()))}}
	if n := len(req.GetTemplates()); n == 0 || n > tenant.MaxPixelTemplateBatch {
		return nil, c.failNow(ctx, d, st, facts, invalid("templates", "holds 1 to "+strconv.Itoa(tenant.MaxPixelTemplateBatch)+" templates"))
	}
	templates := make([]tenant.PixelTemplate, len(req.GetTemplates()))
	for i, t := range req.GetTemplates() {
		templates[i] = templateOf(t)
	}
	templates, err = tenant.ValidatePixelTemplates(templates)
	if err != nil {
		return nil, c.failNow(ctx, d, st, facts, fieldOf("templates", err))
	}
	if err := p.AddPixelTemplates(ctx, templates, changeEntry(d, access.Facts{}), c.adminGuard(d)); err != nil {
		if _, refusal := access.RefusedAtChange(err); refusal != nil {
			return nil, c.failWith(d, st, facts, err)
		}
		return nil, c.failNow(ctx, d, st, facts, fieldOf("templates", err))
	}
	return &v1.AddPixelTemplatesResponse{Added: int32(len(templates))}, nil //nolint:gosec // G115: at most tenant.MaxPixelTemplateBatch.
}

// RemovePixelTemplate removes the template of one key.
func (s deidService) RemovePixelTemplate(ctx context.Context, req *v1.RemovePixelTemplateRequest) (*v1.RemovePixelTemplateResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.DeidServiceRemovePixelTemplateProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTemplates(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	key := templateKeyOf(req)
	if err := tenant.ValidatePixelTemplateKey(key); err != nil {
		return nil, c.failNow(ctx, d, st, access.Facts{}, fieldOf("manufacturer", err))
	}
	if err := p.RemovePixelTemplate(ctx, key, changeEntry(d, access.Facts{}), c.adminGuard(d)); err != nil {
		if _, refusal := access.RefusedAtChange(err); refusal != nil {
			return nil, c.failWith(d, st, access.Facts{}, err)
		}
		return nil, c.failNow(ctx, d, st, access.Facts{}, err)
	}
	return &v1.RemovePixelTemplateResponse{}, nil
}

// ListPixelTemplates answers a page of the tenant's pixel templates.
func (s deidService) ListPixelTemplates(ctx context.Context, req *v1.ListPixelTemplatesRequest) (*v1.ListPixelTemplatesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.DeidServiceListPixelTemplatesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTemplates(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListPixelTemplates(ctx, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListPixelTemplatesResponse{Templates: make([]*v1.PixelTemplate, 0, len(items)), NextPageToken: next}
	for _, t := range items {
		out.Templates = append(out.Templates, templateView(t))
	}
	return out, nil
}
