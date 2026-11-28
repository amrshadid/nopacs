package sqlkit

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ListPixelTemplates is the pixel templates' list name, the first field of
// its cursors.
const ListPixelTemplates = "tenant-pixel-templates"

// templateRect is a rectangle as the table's rectangles column stores it.
type templateRect struct {
	X          int32 `json:"x"`
	Y          int32 `json:"y"`
	Width      int32 `json:"w"`
	Height     int32 `json:"h"`
	FromRight  bool  `json:"r,omitempty"`
	FromBottom bool  `json:"b,omitempty"`
}

// rectanglesText returns the text a template's rectangles are stored as.
func rectanglesText(rs []tenant.PixelRect) (string, error) {
	out := make([]templateRect, len(rs))
	for i, r := range rs {
		out[i] = templateRect(r)
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// rectanglesOf reads the text rectanglesText made.
func rectanglesOf(text string) ([]tenant.PixelRect, error) {
	var in []templateRect
	if err := json.Unmarshal([]byte(text), &in); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "a stored pixel template's rectangles cannot be read")
	}
	out := make([]tenant.PixelRect, len(in))
	for i, r := range in {
		out[i] = tenant.PixelRect(r)
	}
	return out, nil
}

// AddPixelTemplates implements tenant.PixelTemplates: a point read of each
// key and an insert, in one transaction under the tenant's configuration
// lock, so two adds at once cannot pass the bound together.
func (k Deidentification) AddPixelTemplates(ctx context.Context, templates []tenant.PixelTemplate, audit master.AuditEntry, guard tenant.Guard) error {
	templates, err := tenant.ValidatePixelTemplates(templates)
	if err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	texts := make([]string, len(templates))
	for i, t := range templates {
		if texts[i], err = rectanglesText(t.Rectangles); err != nil {
			return fault.Wrap(fault.Internal, err, "a pixel template's rectangles cannot be stored")
		}
	}
	return k.Host.Write(ctx, "the pixel templates cannot be stored", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		var held int64
		if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM pixel_templates`, nil, &held); err != nil {
			return err
		}
		if held+int64(len(templates)) > tenant.MaxPixelTemplates {
			return fault.New(fault.Precondition, "the tenant holds "+strconv.FormatInt(held, 10)+" pixel templates, and at most "+
				strconv.Itoa(tenant.MaxPixelTemplates)+" are kept; remove some first")
		}
		for i, t := range templates {
			key := t.Key()
			taken, err := exists(ctx, tx, `SELECT 1 FROM pixel_templates WHERE manufacturer_key = ? AND model_key = ?
				AND image_rows = ? AND image_columns = ? AND sop_class = ?`,
				key.Manufacturer, key.Model, key.Rows, key.Columns, key.SOPClass)
			if err != nil {
				return err
			}
			if taken {
				return fault.New(fault.Precondition, "template "+strconv.Itoa(i+1)+" is already held for this device, size and SOP class; remove it first")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO pixel_templates (manufacturer_key, model_key, image_rows, image_columns, sop_class,
				manufacturer, model, rectangles) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				key.Manufacturer, key.Model, key.Rows, key.Columns, key.SOPClass, t.Manufacturer, t.Model, texts[i]); err != nil {
				return err
			}
		}
		return tx.Audited(ctx, audit.WithDetails(tenant.DetailTemplates, strconv.Itoa(len(templates))), guard)
	})
}

// RemovePixelTemplate implements tenant.PixelTemplates.
func (k Deidentification) RemovePixelTemplate(ctx context.Context, key tenant.PixelTemplateKey, audit master.AuditEntry, guard tenant.Guard) error {
	if err := tenant.ValidatePixelTemplateKey(key); err != nil {
		return err
	}
	if err := audit.Validate(); err != nil {
		return err
	}
	return k.Host.Write(ctx, "the pixel template cannot be removed", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		n, err := tx.Exec(ctx, `DELETE FROM pixel_templates WHERE manufacturer_key = ? AND model_key = ?
			AND image_rows = ? AND image_columns = ? AND sop_class = ?`,
			key.Manufacturer, key.Model, key.Rows, key.Columns, key.SOPClass)
		if err != nil {
			return err
		}
		if n == 0 {
			return fault.New(fault.NotFound, "the tenant has no such pixel template")
		}
		return tx.Audited(ctx, audit, guard)
	})
}

// ListPixelTemplates implements tenant.PixelTemplates: a range of the
// table's primary key.
func (k Deidentification) ListPixelTemplates(ctx context.Context, page master.Page) ([]tenant.PixelTemplate, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	// The cursor holds the key of the last template sent, as the table
	// orders it.
	after := [5]any{"", "", int64(0), int64(0), ""}
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListPixelTemplates, k.Host.Binding(), 5)
		if err != nil {
			return nil, "", err
		}
		rows, err := DecodeCursorInt(f[2])
		if err != nil {
			return nil, "", err
		}
		cols, err := DecodeCursorInt(f[3])
		if err != nil {
			return nil, "", err
		}
		after = [5]any{f[0], f[1], rows, cols, f[4]}
	}
	var out []tenant.PixelTemplate
	err := k.Host.Read(ctx, "the pixel templates cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT manufacturer, model, image_rows, image_columns, sop_class, rectangles FROM pixel_templates
			WHERE (manufacturer_key, model_key, image_rows, image_columns, sop_class) > (?, ?, ?, ?, ?)
			ORDER BY manufacturer_key, model_key, image_rows, image_columns, sop_class LIMIT ?`,
			append(after[:], page.Limit+1), func(r Row) error {
				var t tenant.PixelTemplate
				var rects string
				if err := r.Scan(&t.Manufacturer, &t.Model, &t.Rows, &t.Columns, &t.SOPClass, &rects); err != nil {
					return err
				}
				var err error
				if t.Rectangles, err = rectanglesOf(rects); err != nil {
					return err
				}
				out = append(out, t)
				return nil
			})
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	key := last.Key()
	return out, EncodeCursor(ListPixelTemplates, k.Host.Binding(), key.Manufacturer, key.Model,
		strconv.Itoa(int(key.Rows)), strconv.Itoa(int(key.Columns)), key.SOPClass), nil
}

// FindPixelTemplate implements tenant.PixelTemplates: one read of the
// table's primary key, the template of the file's SOP class first.
func (k Deidentification) FindPixelTemplate(ctx context.Context, q tenant.PixelTemplateQuery) (tenant.PixelTemplate, bool, error) {
	var out tenant.PixelTemplate
	var found bool
	manufacturer, model := tenant.FoldTemplateText(q.Manufacturer), tenant.FoldTemplateText(q.Model)
	if manufacturer == "" || model == "" || q.Rows < 1 || q.Columns < 1 {
		return out, false, nil
	}
	err := k.Host.Read(ctx, "the pixel template cannot be read", func(tx SendingTx) error {
		var rects string
		var err error
		// "" sorts before any UID, so the descending order puts the SOP
		// class's own template before the one for any class.
		found, err = tx.QueryRow(ctx, `SELECT manufacturer, model, image_rows, image_columns, sop_class, rectangles FROM pixel_templates
			WHERE manufacturer_key = ? AND model_key = ? AND image_rows = ? AND image_columns = ? AND sop_class IN (?, '')
			ORDER BY sop_class DESC LIMIT 1`, []any{manufacturer, model, q.Rows, q.Columns, q.SOPClass},
			&out.Manufacturer, &out.Model, &out.Rows, &out.Columns, &out.SOPClass, &rects)
		if err != nil || !found {
			return err
		}
		out.Rectangles, err = rectanglesOf(rects)
		return err
	})
	if err != nil || !found {
		return tenant.PixelTemplate{}, false, err
	}
	return out, true, nil
}
