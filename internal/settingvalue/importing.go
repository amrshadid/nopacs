package settingvalue

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/settings"
)

// ImportRoots returns the installation's import.roots: the directories an
// import may read, by name.
func (r Reader) ImportRoots(ctx context.Context) (settings.DirectoryRoots, error) {
	v, err := r.Installation(ctx, settings.ImportRoots)
	return typed(settings.ImportRoots, v, err, settings.ParseImportRoots)
}

// ImportServerMaxUnits returns the installation's import.server-max-units:
// the most import units of every tenant the server works at once.
func (r Reader) ImportServerMaxUnits(ctx context.Context) (int, error) {
	v, err := r.Installation(ctx, settings.ImportServerMaxUnits)
	return typed(settings.ImportServerMaxUnits, v, err, settings.ParseImportServerMaxUnits)
}

// ImportMaxDirEntries returns the installation's import.max-dir-entries:
// the most names one directory an import reads may hold.
func (r Reader) ImportMaxDirEntries(ctx context.Context) (int, error) {
	v, err := r.Installation(ctx, settings.ImportMaxDirEntries)
	return typed(settings.ImportMaxDirEntries, v, err, settings.ParseMaxDirEntries)
}

// ImportAllowedRoots returns import.allowed-roots as it applies on the
// tenant: the names of the roots its import receivers may read.
func (t Tenant) ImportAllowedRoots(ctx context.Context) ([]string, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ImportAllowedRoots)
	return typed(settings.ImportAllowedRoots, v, err, settings.ParseAllowedRoots)
}

// ImportMaxUnits returns import.max-units as it applies on the tenant: the
// most of its import units the server works at once.
func (t Tenant) ImportMaxUnits(ctx context.Context) (int, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.ImportMaxUnits)
	return typed(settings.ImportMaxUnits, v, err, settings.ParseImportMaxUnits)
}
