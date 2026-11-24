package tenant

import (
	"errors"
	"slices"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
)

// ReceiverImport is a receiver that never listens: the files of a
// directory under one of the installation's import roots are handed to it
// by its import jobs (JobImport), each as a file that arrived.
const ReceiverImport ReceiverType = "import"

// FieldImporter sets an import receiver's Import, replaced whole.
const FieldImporter ReceiverField = "importer"

// ImportReceiver is an import receiver's configuration: the root its jobs
// read, how a file's path is read for labels, and the map a job uses when
// it names none of its own.
type ImportReceiver struct {
	// Root names the root, among the installation's import.roots, whose
	// directory its jobs read. That the root exists and is allotted to the
	// tenant is the caller's to check.
	Root string
	// Layout is how a file's path is read: importpath.Plain or
	// importpath.Pattern.
	Layout importpath.Kind
	// Pattern is a pattern layout's RE2 pattern over a file's path, empty
	// for plain (importpath.New).
	Pattern string
	// UnitDepth is how many segments from the root make a unit's folder:
	// 1 to importpath.MaxUnitDepth for a pattern layout, 0 or 1 for plain,
	// whose depth is fixed at 1.
	UnitDepth int
	// MapFile is the map's path relative to the root, or empty. It can
	// identify a person: kept in the tenant's database only.
	MapFile string
	// Map is the map configuration, zero for none; a job may give its own,
	// which replaces it whole.
	Map ImportMap
}

// isZero reports whether i holds nothing, as a receiver of another type
// keeps it.
func (i ImportReceiver) isZero() bool {
	return i.Root == "" && i.Layout == 0 && i.Pattern == "" && i.UnitDepth == 0 && i.MapFile == "" && i.Map.IsZero()
}

// ParsedLayout returns the receiver's layout, or an Invalid error for one
// that does not parse.
func (i ImportReceiver) ParsedLayout() (*importpath.Layout, error) {
	depth := i.UnitDepth
	if i.Layout == importpath.Plain && depth == importpath.PlainUnitDepth {
		depth = 0
	}
	l, err := importpath.New(i.Layout, i.Pattern, depth)
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "an import receiver's layout is not valid: "+err.Error())
	}
	return l, nil
}

// GivenParts returns the parts a file's path gives the naming rule: the
// layout's, and the part the map fills when the receiver has a map
// configuration; none for a layout that does not parse.
func (i ImportReceiver) GivenParts() []string {
	l, err := i.ParsedLayout()
	if err != nil {
		return nil
	}
	out := slices.Clone(l.Parts())
	if !i.Map.IsZero() && !slices.Contains(out, i.Map.Gives) {
		// A part the layout gives too is refused by validate, as a
		// precondition, not here as a rule naming one part twice.
		out = append(out, i.Map.Gives)
	}
	return out
}

// validate returns an Invalid error unless i can be stored, or a
// precondition failure for a map configuration that does not fit the
// layout.
func (i ImportReceiver) validate() error {
	if err := ValidateName("root", i.Root); err != nil {
		return err
	}
	if i.Layout == importpath.Plain && i.UnitDepth != 0 && i.UnitDepth != importpath.PlainUnitDepth {
		return invalid("a plain layout's unit depth is fixed at 1")
	}
	l, err := i.ParsedLayout()
	if err != nil {
		return err
	}
	if err := validateImportPath("map file", i.MapFile, true); err != nil {
		return err
	}
	if err := i.Map.Validate(); err != nil {
		return err
	}
	if !i.Map.IsZero() {
		err := l.CheckMap(importpath.MapUse{Key: i.Map.KeyParts, Gives: i.Map.Gives})
		if errors.Is(err, importpath.ErrMapParts) {
			return Precondition("receiver.importer: a map's key parts are parts of the layout, and the part it gives is not")
		}
		if err != nil {
			return fault.Wrap(fault.Invalid, err, "receiver.importer: "+err.Error())
		}
	}
	return nil
}

// cloneImport returns a copy of i sharing no slice with it.
func cloneImport(i ImportReceiver) ImportReceiver {
	i.Map.KeyColumns = slices.Clone(i.Map.KeyColumns)
	i.Map.KeyParts = slices.Clone(i.Map.KeyParts)
	return i
}

// GivenParts returns the parts the receiver's naming rule may use beside
// a file's attributes: an import receiver's (ImportReceiver.GivenParts),
// none for any other.
func (r Receiver) GivenParts() []string {
	if r.Type != ReceiverImport {
		return nil
	}
	return r.Import.GivenParts()
}

// NamingRule returns the receiver's naming rule, naming.Default when it has
// none, parsed with the parts its type gives (GivenParts).
func (r Receiver) NamingRule() (*naming.Rule, error) {
	if r.Naming == nil {
		return naming.Default(), nil
	}
	return naming.ParseWithParts(*r.Naming, r.GivenParts())
}
