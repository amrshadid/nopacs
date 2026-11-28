package engine

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// CausePixelNoTemplate is the cause of an image held because a pixel rule
// fills by the tenant's template, its pixels may carry burned-in text, and
// no template fits its device, size and SOP class. The file is never
// archived unfilled; it is released once a template is added.
const CausePixelNoTemplate Cause = "no_pixel_template"

// TemplateQuery is what an image's header, as received, says that a pixel
// template is looked up by.
type TemplateQuery struct {
	// Manufacturer (0008,0070) and Model (0008,1090) as the file gave them;
	// the implementation compares them with case folded.
	Manufacturer, Model string
	// Rows and Columns are the image's size.
	Rows, Columns int
	// SOPClass is the file's SOP Class UID (0008,0016).
	SOPClass string
}

// PixelTemplates reaches a tenant's pixel templates.
type PixelTemplates interface {
	// Template returns the rectangles of the template that fits the
	// image, the one of its SOP class before one for any class, and
	// whether one does.
	Template(ctx context.Context, q TemplateQuery) (rectangles []*v1.PixelRectangle, found bool, err error)
}

// redactByTemplate finds the template of the image by its header as
// received and keeps its rectangles for filling, as a redact rule's are.
// An image with no template is held when its pixels may carry burned-in
// text, and left as it is otherwise: a template is for a device known to
// draw text, and a file that may carry none is not one.
func (r *run) redactByTemplate(w *PixelWork, img Image, infoErr error, name string) error {
	w.Action = pixelRedact
	if infoErr != nil {
		return &Failure{Cause: CausePixelsUnsupported, Path: formatTag(tagPixelData), Rule: name}
	}
	var rects []*v1.PixelRectangle
	if r.en.templates != nil {
		info, _ := r.top.GetPixelDataInfo()
		var found bool
		var err error
		rects, found, err = r.en.templates.Template(r.ctx, TemplateQuery{
			Manufacturer: r.facts[v1.ConditionSubject_CONDITION_SUBJECT_MANUFACTURER],
			Model:        r.facts[v1.ConditionSubject_CONDITION_SUBJECT_MODEL_NAME],
			Rows:         info.Rows, Columns: info.Columns,
			SOPClass: r.facts[v1.ConditionSubject_CONDITION_SUBJECT_SOP_CLASS_UID],
		})
		if err != nil {
			return err
		}
		if !found {
			rects = nil
		}
	}
	if len(rects) == 0 {
		if w.AtRisk {
			return &Failure{Cause: CausePixelNoTemplate, Path: formatTag(tagPixelData), Rule: name}
		}
		return nil
	}
	if err := r.withinLimit(w, img, name); err != nil {
		return err
	}
	if err := r.framesMatch(name); err != nil {
		return err
	}
	info, _ := r.top.GetPixelDataInfo()
	for _, rect := range rects {
		reg := placeRectangle(rect, info.Columns, info.Rows)
		if reg.X >= info.Columns || reg.Y >= info.Rows || reg.X+reg.Width <= 0 || reg.Y+reg.Height <= 0 {
			return &Failure{Cause: CausePixelRegionOutside, Path: formatTag(tagPixelData), Rule: name}
		}
		w.Regions = append(w.Regions, reg)
	}
	return nil
}
