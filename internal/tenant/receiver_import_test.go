package tenant

import (
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
)

// importReceiver returns a valid import receiver with a pattern layout and
// a map, whose naming rule uses the parts both give.
func importReceiver() Receiver {
	return Receiver{Name: "old-import", Type: ReceiverImport, Enabled: true, Storage: "main", QuietPeriod: 5 * time.Second,
		MaxOpenTime: time.Minute, MaxFileBytes: 1 << 20,
		Naming: &naming.Spec{Subject: "{project}_{subject}", Session: "{project}_{session}", Scan: "{scan}"},
		Import: ImportReceiver{Root: "old", Layout: importpath.Pattern, UnitDepth: 2,
			Pattern: `^(?P<project>[^/]+)/(?P<session>[^/]+)/(?P<scan>[^/]+)/[^/]+$`,
			Map: ImportMap{KeyColumns: []string{"project", "label"}, ValueColumn: "subject_label", KeyParts: []string{"project", "session"},
				Gives: "subject"}}}
}

// TestAnImportReceiverIsCheckedWithTheParts guards an import receiver's
// rules: it is valid with a naming rule over the parts its layout and map
// give; a rule using a part neither gives, a map keyed by a part the layout
// lacks or giving one it has, a configuration of another type, a plain
// layout with a pattern, and a receiver of another type with an import
// configuration are refused, each with its kind.
func TestAnImportReceiverIsCheckedWithTheParts(t *testing.T) {
	if err := importReceiver().Validate(); err != nil {
		t.Fatalf("a valid import receiver was refused: %v", err)
	}
	if got := importReceiver().GivenParts(); !slices.Equal(got, []string{"project", "session", "scan", "subject"}) {
		t.Errorf("given parts %v", got)
	}
	plain := importReceiver()
	plain.Import = ImportReceiver{Root: "old", Layout: importpath.Plain, UnitDepth: 1}
	plain.Naming = nil
	if err := plain.Validate(); err != nil {
		t.Errorf("a plain import receiver of unit depth 1 was refused: %v", err)
	}
	for _, c := range []struct {
		name   string
		change func(*Receiver)
		kind   fault.Kind
	}{
		{"a part nothing gives", func(r *Receiver) { r.Naming.Scan = "{series}" }, fault.Invalid},
		{"a map keyed by a part the layout lacks", func(r *Receiver) { r.Import.Map.KeyParts = []string{"project", "visit"} }, fault.Precondition},
		{"a map giving a part the layout gives", func(r *Receiver) { r.Import.Map.Gives = "scan"; r.Naming.Subject = "{project}" }, fault.Precondition},
		{"a dimse configuration", func(r *Receiver) { r.DIMSE.Port = 104 }, fault.Invalid},
		{"no root", func(r *Receiver) { r.Import.Root = "" }, fault.Invalid},
		{"a plain layout with a pattern", func(r *Receiver) { r.Import.Layout = importpath.Plain }, fault.Invalid},
		{"a unit depth past the bound", func(r *Receiver) { r.Import.UnitDepth = 17 }, fault.Invalid},
		{"a map file breaking the path rule", func(r *Receiver) { r.Import.MapFile = "../map.csv" }, fault.Invalid},
		{"a dimse receiver with an import configuration", func(r *Receiver) {
			r.Type, r.Naming = ReceiverDIMSE, nil
			r.DIMSE = DIMSEReceiver{Port: 104, AETitle: "ARCHIVE", AnyCaller: true}
		}, fault.Invalid},
	} {
		r := importReceiver()
		n := *r.Naming
		r.Naming = &n
		c.change(&r)
		if err := r.Validate(); fault.KindOf(err) != c.kind {
			t.Errorf("%s: %v, want %v", c.name, err, c.kind)
		}
	}
	stored := importReceiver()
	change := Receiver{Import: stored.Import}
	change.Import.Root = "new"
	out, err := ApplyReceiver(stored, change, []ReceiverField{FieldImporter})
	if err != nil || out.Import.Root != "new" {
		t.Errorf("a change of the importer: %v, root %q", err, out.Import.Root)
	}
	dimse := Receiver{Name: "d", Type: ReceiverDIMSE, Enabled: true, Storage: "main", QuietPeriod: 5 * time.Second,
		MaxOpenTime: time.Minute, MaxFileBytes: 1 << 20, DIMSE: DIMSEReceiver{Port: 104, AETitle: "ARCHIVE", AnyCaller: true}}
	if _, err := ApplyReceiver(dimse, change, []ReceiverField{FieldImporter}); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a dimse receiver took an importer: %v", err)
	}
}
