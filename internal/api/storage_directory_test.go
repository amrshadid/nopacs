package api_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestRootThroughLinkIntoAnotherTenant guards two roots that are apart in
// their text and not on the disk: a root whose path is a symbolic link into
// one tenant's directory under another root would let a tenant named like a
// directory there share it. Such a value is refused when it is set; a root
// that becomes such a link after the value was set is refused when it is
// used, and so is every root it nests with, so no storage of either is
// added or opened while the two are not apart. No message names a path.
func TestRootThroughLinkIntoAnotherTenant(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "lab2 add", addStorage("lab2", dirStorage("main", "fast", "data"))(f.as(f.lab2Admin)))
	link := filepath.Join(f.dir, "roots", "slow")
	if err := os.Symlink(filepath.Join(f.root, "lab2"), link); err != nil {
		t.Fatal(err)
	}
	value := "fast=" + f.root + "\nslow=" + link
	o := call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.StorageDirectoryRoots, Scope: installation(), Value: value}})
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "symbolic links") || strings.Contains(o.message(), f.dir) {
		t.Fatalf("a root through a link into another root: %v %q", o.code(), o.message())
	}

	// The link made only after the value was set: the value is accepted,
	// since the path of slow leads nowhere yet, and then refused on use.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	f.setSetting(installation(), settings.StorageDirectoryRoots, value)
	if err := os.Symlink(filepath.Join(f.root, "lab2"), link); err != nil {
		t.Fatal(err)
	}
	f.tenant("data")
	dataAdmin, _ := f.key(f.onTenant("data", master.RoleAdmin))
	o = addStorage("data", dirStorage("s", "slow", ""))(f.as(dataAdmin))
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "symbolic links") || strings.Contains(o.message(), f.dir) {
		t.Errorf("a storage on a root that became a link into another: %v %q", o.code(), o.message())
	}
	if _, err := os.Stat(filepath.Join(f.root, "lab2", "data", "data")); err == nil {
		t.Error("a directory was made through the link")
	}
	res := mustCall[*v1.TestStorageResponse](t, "test", call(f.as(local).storages.TestStorage, &v1.TestStorageRequest{Tenant: "lab2", Name: "main"}))
	if res.GetPassed() || res.GetResults()[0].GetFault() != v1.StorageFault_STORAGE_FAULT_REFUSED {
		t.Errorf("a storage on the root the other now nests in was opened: %v", res)
	}
}

// TestDirectoryPathBound guards a directory storage's path that the
// contract's rule of the path accepts but whose directory, under its root,
// is longer than every system can open: it is refused as a precondition
// before anything is made or stored, never answered as the database not
// answering, and the message names no path, nor the directory's length,
// from which the root's could be told.
func TestDirectoryPathBound(t *testing.T) {
	f := newPlaceFixture(t)
	parts := make([]string, 16)
	for i := range parts {
		parts[i] = strings.Repeat("a", 62)
	}
	o := addStorage("lab1", dirStorage("long", "fast", strings.Join(parts, "/")))(f.as(f.lab1Admin))
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "1023") || strings.Contains(o.message(), f.dir) {
		t.Errorf("a path too long for the server: %v %q", o.code(), o.message())
	}
	if _, err := os.Stat(filepath.Join(f.root, "lab1", parts[0])); err == nil {
		t.Error("a directory was made for a storage that was refused")
	}
	list := mustCall[*v1.ListStoragesResponse](t, "list", call(f.as(local).storages.ListStorages, &v1.ListStoragesRequest{Tenant: "lab1"}))
	if len(list.GetStorages()) != 0 {
		t.Errorf("a refused storage was stored: %v", list.GetStorages())
	}
	// A path of the same contract that fits is a storage that opens.
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage("short", "fast", strings.Join(parts[:2], "/")))(f.as(f.lab1Admin)))
	res := mustCall[*v1.TestStorageResponse](t, "test", call(f.as(f.lab1Admin).storages.TestStorage, &v1.TestStorageRequest{Tenant: "lab1", Name: "short"}))
	if !res.GetPassed() {
		t.Errorf("a storage that was accepted does not open: %v", res)
	}
}

// TestDirectoryPathWindowsSegments guards the segments of a path that
// Windows reads as another name: a final dot, which it drops, so "mri." is
// the directory of the storage at "mri" there, and a device name with or
// without an extension. Each is refused as invalid on every system.
func TestDirectoryPathWindowsSegments(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add mri", addStorage("lab1", dirStorage("a", "fast", "mri"))(f.as(f.lab1Admin)))
	for i, p := range []string{"mri.", "mri..", "con", "nul.txt", "com1", "x/AUX"} {
		o := addStorage("lab1", dirStorage("s"+string(rune('a'+i)), "fast", p))(f.as(f.lab1Admin))
		if o.code() != connect.CodeInvalidArgument || !strings.Contains(o.message(), "directory storage's path") {
			t.Errorf("path %q: %v %q", p, o.code(), o.message())
		}
	}
}

// TestSetStorageInsideItself guards SetStorage moving a directory storage:
// into its own former directory it may, since that directory is no longer
// another storage's; into another storage's directory, or around it, it may
// not.
func TestSetStorageInsideItself(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add a", addStorage("lab1", dirStorage("a", "fast", "mri"))(f.as(f.lab1Admin)))
	mustCall[*v1.AddStorageResponse](t, "add b", addStorage("lab1", dirStorage("b", "fast", "ct"))(f.as(f.lab1Admin)))
	set := func(name, path string) outcome {
		return call(f.as(f.lab1Admin).storages.SetStorage, &v1.SetStorageRequest{Tenant: "lab1", Storage: dirStorage(name, "fast", path), UpdateMask: mask("directory.path")})
	}
	res := mustCall[*v1.SetStorageResponse](t, "into itself", set("a", "mri/sub"))
	if got := res.GetStorage().GetDirectory().GetPath(); got != "mri/sub" {
		t.Errorf("moved to %q", got)
	}
	if info, err := os.Stat(filepath.Join(f.root, "lab1", "mri", "sub")); err != nil || !info.IsDir() {
		t.Errorf("the new directory was not made: %v", err)
	}
	for _, p := range []string{"ct/sub", "CT"} {
		if o := set("a", p); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "storage b") {
			t.Errorf("into b's directory, %q: %v %q", p, o.code(), o.message())
		}
	}
}

// TestRootsAbsoluteOnThisSystem guards the server's own check of a root's
// path: the rule of the text takes the forms of Unix and of Windows, and a
// path of the other system's form is refused as invalid, not stored to be
// found unusable later.
func TestRootsAbsoluteOnThisSystem(t *testing.T) {
	f := newPlaceFixture(t)
	other := "D:\\srv"
	if runtime.GOOS == "windows" {
		other = "/srv"
	}
	o := call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.StorageDirectoryRoots, Scope: installation(), Value: "fast=" + f.root + "\nother=" + other}})
	if o.code() != connect.CodeInvalidArgument || !strings.Contains(o.message(), "not absolute on this server's system") {
		t.Errorf("a root of the other system's form: %v %q", o.code(), o.message())
	}
}
