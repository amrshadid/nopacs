package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The bounds of a template import, the server's (deid.proto,
// AddPixelTemplates).
const (
	maxTemplates = 1000
	// maxTemplateFile bounds the file read: a thousand templates of 64
	// rectangles each, every line at its longest.
	maxTemplateFile = 8 << 20
)

// newDeidTemplate builds "deid template".
func (a *app) newDeidTemplate() *cobra.Command {
	return group("template", "Keep the per-device pixel templates a profile's pixel rule fills by",
		`A pixel rule whose action is redact_by_template fills the rectangles of the
tenant's template for the device that made the image: its manufacturer and model
(case does not matter), its size in rows and columns (exactly) and, when the
template names one, its SOP class. An image at risk of burned-in text with no
template is held, with the cause no_pixel_template, never archived unfilled.

The server ships no template: which rectangles hold text is for a site to know
of its own devices. "import" loads a list from a JSON or CSV file, and "add"
one template from flags; "list" shows what the tenant holds and "remove" takes
one away. add, import and remove need the admin role on the tenant and are
refused to a masked key; list needs operator.`,
		a.newTemplateAdd(), a.newTemplateImport(), a.newTemplateList(), a.newTemplateRemove())
}

// parseRect reads a rectangle of the form x,y,width,height, with the words
// from-right and from-bottom after them, in either order, when x is measured
// from the right edge or y from the bottom.
func parseRect(s string) (*v1.PixelRectangle, error) {
	parts := strings.Split(s, ",")
	if len(parts) < 4 || len(parts) > 6 {
		return nil, fault.New(fault.Invalid, "a rectangle is x,y,width,height, then from-right and from-bottom when they apply")
	}
	var n [4]int32
	for i := range n {
		v, err := strconv.ParseInt(strings.TrimSpace(parts[i]), 10, 32)
		if err != nil {
			return nil, fault.New(fault.Invalid, "a rectangle's x, y, width and height are whole numbers")
		}
		n[i] = int32(v)
	}
	r := &v1.PixelRectangle{X: n[0], Y: n[1], Width: n[2], Height: n[3]}
	for _, w := range parts[4:] {
		switch strings.TrimSpace(w) {
		case "from-right":
			r.FromRight = true
		case "from-bottom":
			r.FromBottom = true
		default:
			return nil, fault.New(fault.Invalid, "after the four numbers a rectangle may say from-right or from-bottom, not "+strconv.Quote(output.Clean(w)))
		}
	}
	return r, nil
}

// newTemplateAdd builds "deid template add".
func (a *app) newTemplateAdd() *cobra.Command {
	var manufacturer, model, class string
	var rows, columns int32
	var rects []string
	cmd := &cobra.Command{
		Use:   "add <tenant> --manufacturer M --model M --rows N --columns N --rect x,y,w,h...",
		Short: "Add one pixel template",
		Long: `Add the template of one device and image size: the rectangles filled in every
image of that manufacturer, model and size (and SOP class, when given). Each
--rect is x,y,width,height in pixels from the image's top left corner; add
from-right and from-bottom after the four numbers to measure x from the right
edge and y from the bottom. A template holds 1 to 64 rectangles, each inside the
image. A device, size and SOP class the tenant already has a template for is
refused (exit 5): remove it first. Needs the admin role on the tenant.`,
		Example: `  nopacs deid template add lab1 --manufacturer "ACME Imaging" --model "Scan One" --rows 480 --columns 640 \
      --rect 0,0,640,40 --rect 10,10,120,20,from-right,from-bottom`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			t := &v1.PixelTemplate{Manufacturer: manufacturer, Model: model, Rows: rows, Columns: columns, SopClass: class}
			for _, s := range rects {
				r, err := parseRect(s)
				if err != nil {
					return err
				}
				t.Rectangles = append(t.Rectangles, r)
			}
			return a.addTemplates(ctx, cmd, args[0], []*v1.PixelTemplate{t})
		}),
	}
	cmd.Flags().StringVar(&manufacturer, "manufacturer", "", "the device's Manufacturer (0008,0070); case is ignored")
	cmd.Flags().StringVar(&model, "model", "", "the device's Manufacturer's Model Name (0008,1090); case is ignored")
	cmd.Flags().Int32Var(&rows, "rows", 0, "the image's Rows, matched exactly")
	cmd.Flags().Int32Var(&columns, "columns", 0, "the image's Columns, matched exactly")
	cmd.Flags().StringVar(&class, "sop-class", "", "the SOP Class UID the template is for; without it, any class")
	cmd.Flags().StringArrayVar(&rects, "rect", nil, "a rectangle to fill, x,y,width,height[,from-right][,from-bottom]; repeat for more")
	return cmd
}

// addTemplates sends templates and says how many were stored.
func (a *app) addTemplates(ctx context.Context, cmd *cobra.Command, tenantName string, templates []*v1.PixelTemplate) error {
	c, closeClient, err := a.open(cmd)
	if err != nil {
		return err
	}
	defer closeClient()
	res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.AddPixelTemplatesResponse, error) {
		return c.Deid.AddPixelTemplates(ctx, &v1.AddPixelTemplatesRequest{Tenant: tenantName, Templates: templates})
	})
	if err != nil {
		return err
	}
	return messageOut(a, res, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%d pixel templates added to tenant %s\n", res.GetAdded(), output.Clean(tenantName))
		return err
	})
}

// templateJSON is a template in an import's JSON file.
type templateJSON struct {
	Manufacturer string     `json:"manufacturer"`
	Model        string     `json:"model"`
	Rows         int32      `json:"rows"`
	Columns      int32      `json:"columns"`
	SOPClass     string     `json:"sop_class"`
	Rectangles   []rectJSON `json:"rectangles"`
}

// rectJSON is a rectangle in an import's JSON file.
type rectJSON struct {
	X          int32 `json:"x"`
	Y          int32 `json:"y"`
	Width      int32 `json:"width"`
	Height     int32 `json:"height"`
	FromRight  bool  `json:"from_right"`
	FromBottom bool  `json:"from_bottom"`
}

// readTemplateFile reads an import's file: JSON, when its first character is
// [ or {, else CSV.
func readTemplateFile(path string) ([]*v1.PixelTemplate, error) {
	f, err := os.Open(path) // #nosec G304 -- the person names the file to import.
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read "+path)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxTemplateFile+1))
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read "+path)
	}
	if len(b) > maxTemplateFile {
		return nil, fault.New(fault.Invalid, fmt.Sprintf("%s is larger than %d bytes, the most an import reads; split it", path, maxTemplateFile))
	}
	var out []*v1.PixelTemplate
	if t := bytes.TrimSpace(b); len(t) > 0 && (t[0] == '[' || t[0] == '{') {
		out, err = templatesFromJSON(path, t)
	} else {
		out, err = templatesFromCSV(path, b)
	}
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Invalid, path+" holds no template")
	}
	if len(out) > maxTemplates {
		return nil, fault.New(fault.Invalid, fmt.Sprintf("%s holds more than %d templates, the most one import takes; split it", path, maxTemplates))
	}
	return out, nil
}

// templatesFromJSON reads {"templates": [...]} or a bare list, refusing a
// field it does not know.
func templatesFromJSON(path string, b []byte) ([]*v1.PixelTemplate, error) {
	var list []templateJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var err error
	if b[0] == '[' {
		err = dec.Decode(&list)
	} else {
		var doc struct {
			Templates []templateJSON `json:"templates"`
		}
		err = dec.Decode(&doc)
		list = doc.Templates
	}
	if err == nil && dec.More() {
		err = errors.New("more than one value")
	}
	if err != nil {
		return nil, fault.New(fault.Invalid, path+" is not a JSON list of templates: "+err.Error())
	}
	out := make([]*v1.PixelTemplate, 0, len(list))
	for _, t := range list {
		p := &v1.PixelTemplate{Manufacturer: t.Manufacturer, Model: t.Model, Rows: t.Rows, Columns: t.Columns, SopClass: t.SOPClass}
		for _, r := range t.Rectangles {
			p.Rectangles = append(p.Rectangles, &v1.PixelRectangle{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height, FromRight: r.FromRight, FromBottom: r.FromBottom})
		}
		out = append(out, p)
	}
	return out, nil
}

// templateColumns are the columns of an import's CSV, in order. A line is one
// rectangle; the lines that follow one another with the same manufacturer,
// model, rows, columns and SOP class are one template's rectangles.
var templateColumns = []string{"manufacturer", "model", "rows", "columns", "sop_class", "x", "y", "width", "height", "from_right", "from_bottom"}

// templatesFromCSV reads an import's CSV: a header of templateColumns (the
// last two optional), then a line for each rectangle.
func templatesFromCSV(path string, b []byte) ([]*v1.PixelTemplate, error) {
	notCSV := func(why string) error {
		return fault.New(fault.Invalid, fmt.Sprintf("%s is not a CSV of pixel templates: %s", path, why))
	}
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil, notCSV(csvProblem(err))
	}
	if len(head) < 9 || len(head) > len(templateColumns) {
		return nil, notCSV("the first line names 9 to 11 columns: " + strings.Join(templateColumns, ","))
	}
	for i, h := range head {
		if !strings.EqualFold(strings.TrimSpace(h), templateColumns[i]) {
			return nil, notCSV("column " + strconv.Itoa(i+1) + " of the first line is not " + templateColumns[i])
		}
	}
	var out []*v1.PixelTemplate
	var last [5]string
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, notCSV(csvProblem(err))
		}
		if len(rec) != len(head) {
			return nil, notCSV("line " + strconv.Itoa(line) + " has " + strconv.Itoa(len(rec)) + " columns, the first line's " + strconv.Itoa(len(head)))
		}
		at := "line " + strconv.Itoa(line)
		var n [6]int32
		for i, col := range []int{2, 3, 5, 6, 7, 8} {
			v, perr := strconv.ParseInt(strings.TrimSpace(rec[col]), 10, 32)
			if perr != nil {
				return nil, notCSV(at + ": " + templateColumns[col] + " is not a whole number")
			}
			n[i] = int32(v)
		}
		rect := &v1.PixelRectangle{X: n[2], Y: n[3], Width: n[4], Height: n[5]}
		for i, flag := range []*bool{&rect.FromRight, &rect.FromBottom} {
			if len(rec) > 9+i && strings.TrimSpace(rec[9+i]) != "" {
				v, perr := strconv.ParseBool(strings.TrimSpace(rec[9+i]))
				if perr != nil {
					return nil, notCSV(at + ": " + templateColumns[9+i] + " is true or false")
				}
				*flag = v
			}
		}
		key := [5]string{rec[0], rec[1], rec[2], rec[3], strings.TrimSpace(rec[4])}
		if len(out) > 0 && key == last {
			t := out[len(out)-1]
			t.Rectangles = append(t.Rectangles, rect)
			continue
		}
		last = key
		out = append(out, &v1.PixelTemplate{Manufacturer: rec[0], Model: rec[1], Rows: n[0], Columns: n[1], SopClass: key[4],
			Rectangles: []*v1.PixelRectangle{rect}})
	}
	return out, nil
}

// newTemplateImport builds "deid template import".
func (a *app) newTemplateImport() *cobra.Command {
	return &cobra.Command{
		Use:   "import <tenant> FILE",
		Short: "Import pixel templates from a JSON or CSV file",
		Long: `Import a list of pixel templates, all or none, at most 1000 at a time. The file
is JSON when it starts with [ or {, and CSV otherwise.

JSON is a list of templates, or an object with the list as "templates":

  [{"manufacturer": "ACME Imaging", "model": "Scan One", "rows": 480, "columns": 640,
    "sop_class": "", "rectangles": [{"x": 0, "y": 0, "width": 640, "height": 40},
    {"x": 10, "y": 10, "width": 120, "height": 20, "from_right": true, "from_bottom": true}]}]

CSV has a header line and a line for each rectangle; lines that follow one another
with the same manufacturer, model, rows, columns and sop_class are one template.
The columns are manufacturer, model, rows, columns, sop_class (empty for any
class), x, y, width, height, and optionally from_right and from_bottom (true or
false):

  manufacturer,model,rows,columns,sop_class,x,y,width,height
  ACME Imaging,Scan One,480,640,,0,0,640,40

A line or field that is not what a column holds, or a field JSON does not know,
refuses the whole file naming the line before anything is sent. The server then
refuses a template with a rectangle past its image, two templates of one
device, size and SOP class, or one the tenant has already (exit 5: remove it
first). Needs the admin role on the tenant.`,
		Example: `  nopacs deid template import lab1 templates.json
  nopacs deid template import lab1 templates.csv`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			templates, err := readTemplateFile(args[1])
			if err != nil {
				return err
			}
			return a.addTemplates(ctx, cmd, args[0], templates)
		}),
	}
}

// newTemplateList builds "deid template list".
func (a *app) newTemplateList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use:     "list <tenant>",
		Short:   "List a tenant's pixel templates",
		Long:    `List a tenant's pixel templates in order of manufacturer, model, rows, columns and SOP class, each with its rectangles. Needs the operator role on the tenant.`,
		Example: `  nopacs deid template list lab1`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			items, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.PixelTemplate, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListPixelTemplatesResponse, error) {
					return c.Deid.ListPixelTemplates(ctx, &v1.ListPixelTemplatesRequest{Tenant: args[0], PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetTemplates(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "templates", items, more, func() error {
				rows := make([][]string, 0, len(items))
				for _, t := range items {
					class := t.GetSopClass()
					if class == "" {
						class = "any"
					}
					rows = append(rows, []string{output.Clean(t.GetManufacturer()), output.Clean(t.GetModel()),
						strconv.Itoa(int(t.GetRows())) + "x" + strconv.Itoa(int(t.GetColumns())), class, rectsText(t.GetRectangles())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"MANUFACTURER", "MODEL", "ROWSxCOLUMNS", "SOP CLASS", "RECTANGLES"}, rows)
				})
			})
		}),
	}
	l.add(cmd)
	return cmd
}

// rectsText writes rectangles as --rect takes them, joined by spaces.
func rectsText(rs []*v1.PixelRectangle) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		s := fmt.Sprintf("%d,%d,%d,%d", r.GetX(), r.GetY(), r.GetWidth(), r.GetHeight())
		if r.GetFromRight() {
			s += ",from-right"
		}
		if r.GetFromBottom() {
			s += ",from-bottom"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// newTemplateRemove builds "deid template remove".
func (a *app) newTemplateRemove() *cobra.Command {
	var manufacturer, model, class string
	var rows, columns int32
	cmd := &cobra.Command{
		Use:   "remove <tenant> --manufacturer M --model M --rows N --columns N",
		Short: "Remove one pixel template",
		Long: `Remove the template of one device, size and SOP class, named as "add" gave
them (case of the manufacturer and model does not matter). Images that matched it
and are at risk of burned-in text are held, with the cause no_pixel_template,
from the next file on. Needs the admin role on the tenant; refused to a masked
key.`,
		Example: `  nopacs deid template remove lab1 --manufacturer "ACME Imaging" --model "Scan One" --rows 480 --columns 640`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RemovePixelTemplateResponse, error) {
				return c.Deid.RemovePixelTemplate(ctx, &v1.RemovePixelTemplateRequest{Tenant: args[0], Manufacturer: manufacturer, Model: model,
					Rows: rows, Columns: columns, SopClass: class})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "the pixel template of %s %s removed from tenant %s\n", output.Clean(manufacturer), output.Clean(model), output.Clean(args[0]))
				return err
			})
		}),
	}
	cmd.Flags().StringVar(&manufacturer, "manufacturer", "", "the template's manufacturer")
	cmd.Flags().StringVar(&model, "model", "", "the template's model")
	cmd.Flags().Int32Var(&rows, "rows", 0, "the template's rows")
	cmd.Flags().Int32Var(&columns, "columns", 0, "the template's columns")
	cmd.Flags().StringVar(&class, "sop-class", "", "the template's SOP class; without it, the template for any class")
	return cmd
}
