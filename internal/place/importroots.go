package place

import (
	"context"
	"os"
	"path/filepath"
	"slices"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ImportValues answers the installation's settings an import root is
// checked against: the import roots themselves and the storage roots, which
// an import root may not touch.
type ImportValues interface {
	// ImportRoots returns import.roots.
	ImportRoots(ctx context.Context) (settings.DirectoryRoots, error)
	// DirectoryRoots returns storage.directory-roots.
	DirectoryRoots(ctx context.Context) (settings.DirectoryRoots, error)
}

// CheckImportRoots applies the rules of import.roots that need this
// server's file system, beside the rule of its text
// (settings.ParseImportRoots): each root's path is absolute on this server's
// system (Invalid), and, once every symbolic link in it is followed, it is
// not, does not contain and does not lie inside a storage root, one of the
// guarded directories (the server's data directory, its incoming area, the
// directory of the installation key file), or another import root
// (Precondition, as it depends on what is on the disk). So an import can
// never read the archive's own files or its secrets. A root that does not
// exist yet is compared where it would be once made, as CheckRoots does.
// Two directories are compared by name and by identity (overlap), so a
// second spelling of a directory (a decomposed accent on a file system that
// ignores normalization) or a bind mount of it is that directory. No
// message holds a path: a root's path is the installation's.
func CheckImportRoots(roots, storage settings.DirectoryRoots, guarded []string) error {
	resolved := make([]string, len(roots))
	for i, r := range roots {
		if !filepath.IsAbs(r.Path) {
			return fault.New(fault.Invalid, "has a path for the root "+r.Name+" that is not absolute on this server's system")
		}
		real, err := resolveLoose(r.Path)
		if err != nil {
			return fault.Wrap(fault.Precondition, pathless(err), "has a path for the root "+r.Name+" that the server cannot examine, so where it leads cannot be known")
		}
		resolved[i] = real
		for j := range i {
			hit, err := overlap(resolved[j], real)
			if err != nil {
				return fault.Wrap(fault.Precondition, pathless(err), "has the import roots "+roots[j].Name+" and "+r.Name+", which the server cannot examine, so whether they lie apart cannot be known")
			}
			if hit {
				return fault.New(fault.Precondition, "has the import roots "+roots[j].Name+" and "+r.Name+" in one directory or one inside the other once their symbolic links are followed on this server")
			}
		}
		if err := apart(r.Name, real, storage, guarded); err != nil {
			return err
		}
	}
	return nil
}

// ResolveImportRoots returns roots with each path replaced by where it
// leads now, every symbolic link in it followed (a part that does not
// exist yet is kept as it is written, since it is not a link), which is how
// import.roots is stored: so a root whose path later resolves elsewhere,
// through a link made or changed since, is told at its use (ImportRoots.Root)
// and refused. It is called after CheckImportRoots.
func ResolveImportRoots(roots settings.DirectoryRoots) (settings.DirectoryRoots, error) {
	out := make(settings.DirectoryRoots, len(roots))
	for i, r := range roots {
		real, err := resolveLoose(r.Path)
		if err != nil {
			return nil, fault.Wrap(fault.Precondition, pathless(err), "has a path for the root "+r.Name+" that the server cannot examine, so where it leads cannot be known")
		}
		out[i] = settings.DirectoryRoot{Name: r.Name, Path: real}
	}
	return out, nil
}

// apart refuses an import root, resolved to mine, that is, contains or lies
// inside a storage root or a guarded directory once their links are
// followed too, by name or by identity (overlap).
func apart(name, mine string, storage settings.DirectoryRoots, guarded []string) error {
	for _, s := range storage {
		if !filepath.IsAbs(s.Path) {
			// A storage root whose path is not absolute here is never opened
			// here (resolveRoot), so it holds nothing an import could read.
			continue
		}
		real, err := resolveLoose(s.Path)
		var hit bool
		if err == nil {
			hit, err = overlap(mine, real)
		}
		if err != nil {
			return fault.Wrap(fault.Precondition, pathless(err), "has the import root "+name+" beside the storage root "+s.Name+", which the server cannot examine, so whether they lie apart cannot be known")
		}
		if hit {
			return fault.New(fault.Precondition, "has the import root "+name+" in one directory with the storage root "+s.Name+", or one inside the other, once their symbolic links are followed on this server; an import never reads the archive's own files")
		}
	}
	for _, g := range guarded {
		if g == "" {
			continue
		}
		real, err := resolveLoose(g)
		var hit bool
		if err == nil {
			hit, err = overlap(mine, real)
		}
		if err != nil {
			return fault.Wrap(fault.Precondition, pathless(err), "has the import root "+name+" beside a directory of the server's own that the server cannot examine")
		}
		if hit {
			return fault.New(fault.Precondition, "has the import root "+name+" in one directory with the server's data directory, its incoming area or the directory of its key file, or one inside the other, once their symbolic links are followed on this server; an import never reads the server's own files or its secrets")
		}
	}
	return nil
}

// ImportRoots finds the directory of an import root for a tenant, by the
// settings that name and allot it (import.roots, import.allowed-roots), and
// checks it again at every use: a root's path, or a directory above it, can
// have come to pass through a new symbolic link since the setting was
// written, so it is resolved now and refused when it now touches a storage
// root, a guarded directory or another import root.
type ImportRoots struct {
	// Values reads import.roots and storage.directory-roots.
	Values ImportValues
	// Allotted returns import.allowed-roots as it applies on the tenant
	// whose database is st.
	Allotted func(ctx context.Context, st tenant.Store) ([]string, error)
	// Guarded are the server's own directories no import root may touch:
	// the data directory, the incoming area, the key file's directory.
	Guarded []string
}

// Root returns the absolute path of the directory the root named name stands
// for now, every link in it followed, for the tenant whose database is st;
// a precondition failure, naming the root and never its path, when the
// installation no longer names it, the tenant is not allotted it, it is not
// an existing directory, or it now touches what an import root may not.
// tenantName is not read: the allotment is the tenant's database's.
func (r ImportRoots) Root(ctx context.Context, _ string, st tenant.Store, name string) (string, error) {
	roots, err := r.Values.ImportRoots(ctx)
	if err != nil {
		return "", err
	}
	root, ok := roots.Lookup(name)
	if !ok {
		return "", fault.New(fault.Precondition, "import.roots does not name the root "+name)
	}
	if r.Allotted != nil {
		allowed, err := r.Allotted(ctx, st)
		if err != nil {
			return "", err
		}
		if !slices.Contains(allowed, name) {
			return "", fault.New(fault.Precondition, "import.allowed-roots does not allot the root "+name+" to the tenant")
		}
	}
	if !filepath.IsAbs(root.Path) {
		return "", fault.New(fault.Precondition, "the import root "+name+" is not an absolute path on this server")
	}
	real, err := filepath.EvalSymlinks(root.Path)
	if err != nil {
		return "", fault.Wrap(fault.Precondition, pathless(err), "the directory of the import root "+name+" is not there on this server")
	}
	if real != filepath.Clean(root.Path) {
		// The setting keeps each root resolved as it was when set
		// (ResolveImportRoots): a path that resolves elsewhere now passes
		// through a link made or changed since.
		return "", fault.New(fault.Precondition, "the import root "+name+" now resolves to another directory than when import.roots was set (a symbolic link in its path changed); the installation must set import.roots again")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", fault.New(fault.Precondition, "the import root "+name+" is not a directory on this server")
	}
	storage, err := r.Values.DirectoryRoots(ctx)
	if err != nil {
		return "", err
	}
	if err := apart(name, real, storage, r.Guarded); err != nil {
		return "", fault.Wrap(fault.Precondition, err, "import.roots "+fault.Message(err)+"; the installation must correct import.roots")
	}
	for _, o := range roots {
		if o.Name == name || !filepath.IsAbs(o.Path) {
			continue
		}
		other, err := resolveLoose(o.Path)
		var hit bool
		if err == nil {
			hit, err = overlap(real, other)
		}
		if err != nil {
			return "", fault.Wrap(fault.Precondition, pathless(err), "the import root "+o.Name+" cannot be examined on this server, so whether the root "+name+" lies apart from it cannot be known")
		}
		if hit {
			return "", fault.New(fault.Precondition, "the import roots "+name+" and "+o.Name+" are one directory or one inside the other once their symbolic links are followed on this server; the installation must correct import.roots")
		}
	}
	return real, nil
}
