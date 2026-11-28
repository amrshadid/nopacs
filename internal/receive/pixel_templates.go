package receive

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// templatesFor returns the engine's reach of the tenant's pixel templates:
// the store's, or nil when the store keeps none, which holds every image a
// template rule would have filled and that may carry burned-in text.
func templatesFor(store any) engine.PixelTemplates {
	if t, ok := store.(tenant.PixelTemplates); ok {
		return pixelTemplates{store: t}
	}
	return nil
}

// pixelTemplates reaches the tenant's pixel templates for the engine.
type pixelTemplates struct{ store tenant.PixelTemplates }

// Template implements engine.PixelTemplates.
func (p pixelTemplates) Template(ctx context.Context, q engine.TemplateQuery) ([]*v1.PixelRectangle, bool, error) {
	if q.Rows < 1 || q.Columns < 1 || q.Rows > tenant.MaxPixelSide || q.Columns > tenant.MaxPixelSide {
		return nil, false, nil
	}
	t, found, err := p.store.FindPixelTemplate(ctx, tenant.PixelTemplateQuery{Manufacturer: q.Manufacturer, Model: q.Model,
		Rows: int32(q.Rows), Columns: int32(q.Columns), SOPClass: q.SOPClass}) //nolint:gosec // G115: bounded above.
	if err != nil || !found {
		return nil, false, err
	}
	out := make([]*v1.PixelRectangle, 0, len(t.Rectangles))
	for _, r := range t.Rectangles {
		out = append(out, &v1.PixelRectangle{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height, FromRight: r.FromRight, FromBottom: r.FromBottom})
	}
	return out, true, nil
}
