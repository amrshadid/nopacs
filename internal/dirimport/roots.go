package dirimport

import (
	"context"
	"os"
	"path/filepath"
	"slices"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// SettingRoots finds a root by the settings that name and allot it:
// import.roots on the installation and import.allowed-roots on the tenant.
// The root's path is resolved through its links every time a unit starts,
// and must be an absolute path to a directory. Where a root may not lie
// (a storage's root, the data directory, the incoming area, the key file's
// directory, another import root) is checked when the setting is written;
// a server that checks it again here gives its own Roots.
type SettingRoots struct {
	// Values reads the settings.
	Values settingvalue.Reader
}

// Root implements Roots.
func (s SettingRoots) Root(ctx context.Context, _ string, st tenant.Store, name string) (string, error) {
	roots, err := s.Values.ImportRoots(ctx)
	if err != nil {
		return "", err
	}
	root, ok := roots.Lookup(name)
	if !ok {
		return "", fault.New(fault.Precondition, "import.roots no longer names the root "+name)
	}
	allowed, err := s.Values.OnTenant(st).ImportAllowedRoots(ctx)
	if err != nil {
		return "", err
	}
	if !slices.Contains(allowed, name) {
		return "", fault.New(fault.Precondition, "import.allowed-roots no longer allots the root "+name+" to the tenant")
	}
	return resolveDir(root.Path, name)
}

// resolveDir returns p with every link in it followed, or a precondition
// failure naming the root name (never the path) when p is not absolute or
// does not lead to a directory.
func resolveDir(p, name string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fault.New(fault.Precondition, "the root "+name+" is not an absolute path on this server")
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fault.New(fault.Precondition, "the directory of the root "+name+" is not there on this server")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", fault.New(fault.Precondition, "the root "+name+" is not a directory on this server")
	}
	return real, nil
}
