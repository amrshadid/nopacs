package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// importLayoutPrefix is what every name of an import layout starts with.
const importLayoutPrefix = "IMPORT_LAYOUT_"

// importFlags are the flags of an import receiver's own fields.
type importFlags struct {
	root           string
	layout         string
	pattern        string
	unitDepth      int
	mapFile        string
	mapKeyColumns  []string
	mapKeyParts    []string
	mapValueColumn string
	mapGives       string
}

// importFlagNames are the flags of an import receiver, each with the mask
// path of its field.
var importFlagNames = [][2]string{
	{"root", "importer.root"}, {"layout", "importer.layout"}, {"pattern", "importer.path_pattern"},
	{"unit-depth", "importer.unit_depth"}, {"map-file", "importer.map_file"},
	{"map-key-column", "importer.map_key_columns"}, {"map-key-part", "importer.map_key_parts"},
	{"map-value-column", "importer.map_value_column"}, {"map-gives", "importer.map_gives"},
}

// add registers the flags on a command.
func (f *importFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.root, "root", "", "import: the name of the import root, among those the installation allotted to the tenant (the setting import.allowed-roots), whose directory its jobs read")
	fl.StringVar(&f.layout, "layout", "", "import: how a file's path is read for labels: plain (not read) or pattern (--pattern and --unit-depth)")
	fl.StringVar(&f.pattern, "pattern", "", "import: the RE2 pattern over a file's path under the root with named parts the naming templates use, such as ^(?P<project>[^/]+)/(?P<session>[^/]+)/.+$")
	fl.IntVar(&f.unitDepth, "unit-depth", 0, "import: how many folders from the root make one unit of work, 1 to 16 (the pattern layout's)")
	fl.StringVar(&f.mapFile, "map-file", "", "import: the map file, a CSV under the root, by its path under the root (shown to operators only)")
	fl.StringArrayVar(&f.mapKeyColumns, "map-key-column", nil, "import: a column of the map's header that forms a row's key; repeat, in order, 1 to 8")
	fl.StringArrayVar(&f.mapKeyParts, "map-key-part", nil, "import: a part of the layout that forms a file's key, in the order of --map-key-column; repeat")
	fl.StringVar(&f.mapValueColumn, "map-value-column", "", "import: the map's column whose value fills --map-gives")
	fl.StringVar(&f.mapGives, "map-gives", "", "import: the part the map fills, such as subject, which the naming templates use as {subject}")
	enumCompletion(cmd, "layout", v1.ImportLayout_name, importLayoutPrefix)
}

// given reports whether any flag of an import receiver was given.
func (f *importFlags) given(cmd *cobra.Command) bool {
	for _, n := range importFlagNames {
		if cmd.Flags().Changed(n[0]) {
			return true
		}
	}
	return false
}

// importer returns the import configuration the flags give, and the mask
// paths of the fields given.
func (f *importFlags) importer(cmd *cobra.Command) (*v1.ImportReceiver, []string, error) {
	m := &v1.ImportReceiver{Root: f.root, PathPattern: f.pattern, MapFile: f.mapFile, MapKeyColumns: nonEmpty(f.mapKeyColumns),
		MapKeyParts: nonEmpty(f.mapKeyParts), MapValueColumn: f.mapValueColumn, MapGives: f.mapGives}
	if cmd.Flags().Changed("layout") {
		l, err := parseEnum[v1.ImportLayout]("layout", f.layout, v1.ImportLayout_name, importLayoutPrefix)
		if err != nil {
			return nil, nil, err
		}
		m.Layout = l
	}
	if f.unitDepth < 0 || f.unitDepth > 16 {
		return nil, nil, fault.New(fault.Invalid, fmt.Sprintf("--unit-depth %d is not from 1 to 16", f.unitDepth))
	}
	m.UnitDepth = int32(f.unitDepth) // #nosec G115 -- checked above, 0 to 16
	var paths []string
	for _, n := range importFlagNames {
		if cmd.Flags().Changed(n[0]) {
			paths = append(paths, n[1])
		}
	}
	return m, paths, nil
}

// importDetails returns the labeled values of an import receiver.
func importDetails(m *v1.ImportReceiver) []output.Pair {
	layout := strings.ToLower(strings.TrimPrefix(m.GetLayout().String(), importLayoutPrefix))
	pairs := []output.Pair{
		{Label: "Root", Value: m.GetRoot()},
		{Label: "Layout", Value: layout},
	}
	if m.GetPathPattern() != "" {
		pairs = append(pairs, output.Pair{Label: "Pattern", Value: m.GetPathPattern()})
	}
	pairs = append(pairs, output.Pair{Label: "Unit depth", Value: strconv.Itoa(int(m.GetUnitDepth()))})
	if m.GetMapFile() != "" {
		pairs = append(pairs, output.Pair{Label: "Map file", Value: m.GetMapFile()})
	}
	if m.GetMapGives() != "" {
		pairs = append(pairs, output.Pair{Label: "Map", Value: fmt.Sprintf("%s from %s by columns %s of parts %s",
			m.GetMapGives(), m.GetMapValueColumn(), strings.Join(m.GetMapKeyColumns(), ", "), strings.Join(m.GetMapKeyParts(), ", "))})
	}
	return pairs
}
