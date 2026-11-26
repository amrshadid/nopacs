package api

import (
	"context"
	"slices"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The keys of the details an import receiver's and an import job's audit
// entries carry: the root by name and the layout, never a path.
const (
	detailRoot   = "root"
	detailLayout = "layout"
)

// importMaskFields are the paths of an import receiver's fields that
// SetReceiver's update_mask may name (receiver.proto,
// SetReceiverRequest.update_mask). The store replaces an import
// receiver's configuration whole (tenant.FieldImporter), so the handler
// sets the named fields on the stored configuration first.
var importMaskFields = []string{
	"importer.root", "importer.layout", "importer.path_pattern", "importer.unit_depth", "importer.map_file",
	"importer.map_key_columns", "importer.map_value_column", "importer.map_key_parts", "importer.map_gives",
}

// importLayouts maps the contract's layouts to the port's.
var importLayouts = map[v1.ImportLayout]importpath.Kind{
	v1.ImportLayout_IMPORT_LAYOUT_PLAIN:   importpath.Plain,
	v1.ImportLayout_IMPORT_LAYOUT_PATTERN: importpath.Pattern,
}

// importReceiverValues returns the import configuration a request gives,
// as given: a layout the contract does not define is kept as no layout, which
// the port refuses as invalid.
func importReceiverValues(m *v1.ImportReceiver) tenant.ImportReceiver {
	return tenant.ImportReceiver{
		Root: m.GetRoot(), Layout: importLayouts[m.GetLayout()], Pattern: m.GetPathPattern(), UnitDepth: int(m.GetUnitDepth()),
		MapFile: m.GetMapFile(),
		Map: tenant.ImportMap{KeyColumns: m.GetMapKeyColumns(), ValueColumn: m.GetMapValueColumn(),
			KeyParts: m.GetMapKeyParts(), Gives: m.GetMapGives()},
	}
}

// importReceiverMessage returns an import receiver's configuration as the
// contract writes it. Its map file is a path, which can name a person, so
// it is answered only when paths is true (mayReadPaths). A plain layout's
// unit depth is answered 1, the depth it is fixed at.
func importReceiverMessage(i tenant.ImportReceiver, paths bool) *v1.ImportReceiver {
	m := &v1.ImportReceiver{
		Root: i.Root, PathPattern: i.Pattern, UnitDepth: int32(i.UnitDepth), //nolint:gosec // G115: a depth of 1 to 16.
		MapKeyColumns: i.Map.KeyColumns, MapValueColumn: i.Map.ValueColumn, MapKeyParts: i.Map.KeyParts, MapGives: i.Map.Gives,
	}
	for k, v := range importLayouts {
		if v == i.Layout {
			m.Layout = k
		}
	}
	if i.Layout == importpath.Plain {
		m.UnitDepth = importpath.PlainUnitDepth
	}
	if paths {
		m.MapFile = i.MapFile
	}
	return m
}

// mergeImport returns stored with the fields of change that paths
// (importMaskFields) name set, for a SetReceiver of an import receiver.
func mergeImport(stored, change tenant.ImportReceiver, paths []string) tenant.ImportReceiver {
	out := stored
	out.Map.KeyColumns = slices.Clone(stored.Map.KeyColumns)
	out.Map.KeyParts = slices.Clone(stored.Map.KeyParts)
	for _, p := range paths {
		switch p {
		case "importer.root":
			out.Root = change.Root
		case "importer.layout":
			out.Layout = change.Layout
		case "importer.path_pattern":
			out.Pattern = change.Pattern
		case "importer.unit_depth":
			out.UnitDepth = change.UnitDepth
		case "importer.map_file":
			out.MapFile = change.MapFile
		case "importer.map_key_columns":
			out.Map.KeyColumns = slices.Clone(change.Map.KeyColumns)
		case "importer.map_value_column":
			out.Map.ValueColumn = change.Map.ValueColumn
		case "importer.map_key_parts":
			out.Map.KeyParts = slices.Clone(change.Map.KeyParts)
		case "importer.map_gives":
			out.Map.Gives = change.Map.Gives
		}
	}
	return out
}

// splitImportPaths returns the mask's paths with those of an import
// receiver's fields taken out, and those taken out.
func splitImportPaths(paths []string) (rest, imports []string) {
	for _, p := range paths {
		if strings.HasPrefix(p, "importer.") {
			imports = append(imports, p)
		} else {
			rest = append(rest, p)
		}
	}
	return rest, imports
}

// checkImportReceiver checks an import receiver against what the
// installation allows: a root import.roots names and import.allowed-roots
// allots to the tenant whose store is st. Each failure is a precondition:
// the same request may succeed once the installation allows it. Any other
// receiver passes.
func (c *core) checkImportReceiver(ctx context.Context, st tenant.Store, r tenant.Receiver) error {
	if r.Type != tenant.ReceiverImport {
		return nil
	}
	roots, err := c.values().ImportRoots(ctx)
	if err != nil {
		return err
	}
	if _, ok := roots.Lookup(r.Import.Root); !ok {
		return precondition("receiver.importer.root: the installation's setting import.roots names no root " + r.Import.Root)
	}
	allowed, err := c.values().OnTenant(st).ImportAllowedRoots(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(allowed, r.Import.Root) {
		return precondition("receiver.importer.root: the root " + r.Import.Root + " is not among those the installation allotted to this tenant (its setting import.allowed-roots)")
	}
	return nil
}

// importJobInProgress reports whether an import job of the receiver named
// name is queued or running in the tenant whose pipeline is p.
func importJobInProgress(ctx context.Context, p tenant.Pipeline, name string) (bool, error) {
	jobs, err := wholeList(ctx, "a tenant's jobs in progress", func(ctx context.Context, page master.Page) ([]tenant.Job, string, error) {
		return p.ListJobs(ctx, tenant.JobFilter{InProgress: true}, page)
	})
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.Kind == tenant.JobImport && j.Receiver == name {
			return true, nil
		}
	}
	return false, nil
}

// errImportJobRunning refuses a change or a removal of an import receiver
// while one of its jobs is queued or running: the job's cursor and counts
// are read by its configuration.
func errImportJobRunning(what string) error {
	return precondition("an import job of the receiver is queued or running; " + what + " once it has ended, or cancel it first (nopacs job cancel)")
}

// mayReadPaths reports whether the caller of d may read the paths under an
// import root and an import's map: an operator or more on the tenant the
// request is in, or on the installation (job.proto, "A path can hold what
// identifies a person"). A masked key never may: a path is an original
// value, not one the archive de-identified.
func mayReadPaths(d access.Decision) bool {
	if d.Masked() {
		return false
	}
	p := d.Principal()
	if p.Role(master.Installation).Includes(master.RoleOperator) {
		return true
	}
	ref, ok := d.Tenant()
	return ok && p.Role(master.TenantScope(ref.ID)).Includes(master.RoleOperator)
}

// importHealth is the health of an import receiver, which holds no
// listener: idle while enabled, down and disabled otherwise (receiver.proto,
// RECEIVER_STATE_IDLE).
func importHealth(r tenant.Receiver) Health {
	if !r.Enabled {
		return Health{State: HealthDown, Reason: DownDisabled, Detail: tenant.PhraseDisabled, Since: r.Updated}
	}
	return Health{State: HealthIdle, Since: r.Updated}
}

// checkImportLayoutGiven refuses an AddReceiver of an import receiver with
// no layout, naming the field, before the port's rule of a layout.
func checkImportLayoutGiven(field string, m *v1.Receiver) error {
	if im := m.GetImporter(); im != nil && im.GetLayout() == v1.ImportLayout_IMPORT_LAYOUT_UNSPECIFIED {
		return invalid(field+".importer.layout", "is required: IMPORT_LAYOUT_PLAIN or IMPORT_LAYOUT_PATTERN")
	}
	if im := m.GetImporter(); im != nil {
		if _, ok := importLayouts[im.GetLayout()]; !ok {
			return invalid(field+".importer.layout", "is a layout the contract defines: IMPORT_LAYOUT_PLAIN or IMPORT_LAYOUT_PATTERN")
		}
	}
	return nil
}
