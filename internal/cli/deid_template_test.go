package cli_test

import (
	"strings"
	"testing"
)

// TestDeidTemplateCommands guards "deid template": add sends one template
// with its rectangles as written (a measured-from-the-right rectangle
// among them) and says how many were stored; a template the tenant holds
// already is refused with exit 5; a malformed rectangle is refused at once
// (exit 2) with nothing sent; list shows each template with its size and
// rectangles, and in JSON; remove takes one away by its key, in another
// case, and a second removal is exit 4.
func TestDeidTemplateCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	add := []string{"deid", "template", "add", "lab1", "--manufacturer", "ACME Imaging", "--model", "Scan One", "--rows", "480", "--columns", "640",
		"--rect", "0,0,640,40", "--rect", "10,10,120,20,from-right,from-bottom"}
	if res := r.run(add...); res.code != 0 || !strings.Contains(res.stdout, "1 pixel templates added to tenant lab1") {
		t.Fatalf("add:\n%+v", res)
	}
	sent := r.fake.PixelTemplateRequests()
	if len(sent) != 1 || len(sent[0].GetTemplates()) != 1 {
		t.Fatalf("requests sent: %v", sent)
	}
	got := sent[0].GetTemplates()[0]
	if got.GetManufacturer() != "ACME Imaging" || got.GetRows() != 480 || got.GetColumns() != 640 || len(got.GetRectangles()) != 2 ||
		got.GetRectangles()[0].GetWidth() != 640 || !got.GetRectangles()[1].GetFromRight() || !got.GetRectangles()[1].GetFromBottom() {
		t.Errorf("the template sent: %v", got)
	}
	if res := r.run(add...); res.code != 5 {
		t.Errorf("a template held already: %+v", res)
	}
	badRect := []string{"deid", "template", "add", "lab1", "--manufacturer", "A", "--model", "B", "--rows", "8", "--columns", "8", "--rect", "0,0,8"}
	if res := r.run(badRect...); res.code != 2 || len(r.fake.PixelTemplateRequests()) != 2 {
		t.Errorf("a rectangle of three numbers: %+v", res)
	}
	badWord := append(append([]string{}, badRect[:len(badRect)-1]...), "0,0,8,8,sideways")
	if res := r.run(badWord...); res.code != 2 || len(r.fake.PixelTemplateRequests()) != 2 {
		t.Errorf("a rectangle with an unknown word: %+v", res)
	}
	list := r.run("deid", "template", "list", "lab1")
	if list.code != 0 || !strings.Contains(list.stdout, "ACME Imaging") || !strings.Contains(list.stdout, "480x640") ||
		!strings.Contains(list.stdout, "0,0,640,40 10,10,120,20,from-right,from-bottom") || !strings.Contains(list.stdout, "any") {
		t.Errorf("list:\n%+v", list)
	}
	if d := document(t, r.run("deid", "template", "list", "lab1", "--json")); len(d["templates"].([]any)) != 1 {
		t.Errorf("list --json: %v", d)
	}
	remove := []string{"deid", "template", "remove", "lab1", "--manufacturer", "acme imaging", "--model", "SCAN ONE", "--rows", "480", "--columns", "640"}
	if res := r.run(remove...); res.code != 0 || !strings.Contains(res.stdout, "removed from tenant lab1") {
		t.Errorf("remove:\n%+v", res)
	}
	if res := r.run(remove...); res.code != 4 {
		t.Errorf("a second removal: %+v", res)
	}
}

// TestDeidTemplateImport guards "deid template import": a JSON file (a list
// and an object with the list in it) and a CSV whose consecutive lines of one
// device are one template's rectangles are sent as templates; a CSV that is
// ragged, has a number that is none, a header that is not the columns', a
// JSON field it does not know or no template at all is refused with exit 2,
// naming the line and never quoting it, and nothing is sent.
func TestDeidTemplateImport(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	dir := t.TempDir()
	csv := writeFile(t, dir, "t.csv", "manufacturer,model,rows,columns,sop_class,x,y,width,height,from_right,from_bottom\n"+
		"ACME,One,64,64,,0,0,64,8,,\n"+
		"ACME,One,64,64,,4,4,10,10,true,true\n"+
		"ACME,Two,32,32,1.2.840.10008.5.1.4.1.1.6.1,0,0,32,4,false,false\n")
	if res := r.run("deid", "template", "import", "lab1", csv); res.code != 0 || !strings.Contains(res.stdout, "2 pixel templates added") {
		t.Fatalf("a CSV import:\n%+v", res)
	}
	sent := r.fake.PixelTemplateRequests()[0].GetTemplates()
	if len(sent) != 2 || len(sent[0].GetRectangles()) != 2 || !sent[0].GetRectangles()[1].GetFromRight() ||
		sent[1].GetSopClass() != "1.2.840.10008.5.1.4.1.1.6.1" || len(sent[1].GetRectangles()) != 1 {
		t.Errorf("the templates sent: %v", sent)
	}
	list := writeFile(t, dir, "list.json", `[{"manufacturer":"Beta","model":"B","rows":16,"columns":16,"rectangles":[{"x":0,"y":0,"width":16,"height":2}]}]`)
	if res := r.run("deid", "template", "import", "lab1", list); res.code != 0 {
		t.Errorf("a JSON list: %+v", res)
	}
	object := writeFile(t, dir, "object.json", `{"templates":[{"manufacturer":"Gamma","model":"G","rows":16,"columns":16,"sop_class":"1.2.3",`+
		`"rectangles":[{"x":1,"y":1,"width":4,"height":4,"from_right":true}]}]}`)
	if res := r.run("deid", "template", "import", "lab1", object); res.code != 0 {
		t.Errorf("a JSON object: %+v", res)
	}
	n := len(r.fake.PixelTemplateRequests())
	head := "manufacturer,model,rows,columns,sop_class,x,y,width,height\n"
	for _, c := range []struct{ name, file, line, secret string }{
		{"a ragged CSV", head + "SECRETCO,One,64,64,,0,0,64\n", "line 2", "SECRETCO"},
		{"a number that is none", head + "SECRETCO,One,sixty,64,,0,0,64,8\n", "line 2", "SECRETCO"},
		{"a wrong header", "make,model,rows,columns,sop_class,x,y,width,height\nSECRETCO,One,64,64,,0,0,64,8\n", "column 1", "SECRETCO"},
		{"a field JSON does not know", `[{"manufacturer":"SECRETCO","model":"M","rows":8,"columns":8,"color":"red","rectangles":[]}]`, "JSON", ""},
		{"no template", head, "no template", ""},
	} {
		path := writeFile(t, dir, "bad.txt", c.file)
		res := r.run("deid", "template", "import", "lab1", path)
		if res.code != 2 || !strings.Contains(res.stderr, c.line) || (c.secret != "" && strings.Contains(res.stderr, c.secret)) {
			t.Errorf("%s: %+v", c.name, res)
		}
	}
	if got := len(r.fake.PixelTemplateRequests()); got != n {
		t.Errorf("a refused file sent a request: %d requests, want %d", got, n)
	}
}
