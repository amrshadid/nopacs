package contexts

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// loc is a location in a private directory of the test's own.
func loc(t *testing.T) Location {
	t.Helper()
	return Location{Path: filepath.Join(t.TempDir(), "contexts.json")}
}

// TestLocate guards the places the file is looked for: NOPACS_CONFIG, then
// XDG_CONFIG_HOME, then ~/.config on Unix systems, macOS included.
func TestLocate(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := t.TempDir()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
		own  bool
	}{
		{"override", map[string]string{"NOPACS_CONFIG": "/x/c.json", "XDG_CONFIG_HOME": "/y"}, "/x/c.json", false},
		{"xdg", map[string]string{"XDG_CONFIG_HOME": "/y", "HOME": home}, "/y/nopacs/contexts.json", true},
		{"home", map[string]string{"HOME": home}, filepath.Join(home, ".config", "nopacs", "contexts.json"), true},
		{"relative xdg is ignored", map[string]string{"XDG_CONFIG_HOME": "y", "HOME": home}, filepath.Join(home, ".config", "nopacs", "contexts.json"), true},
	} {
		if runtime.GOOS == "windows" {
			t.Skip("paths differ on Windows")
		}
		got, err := Locate(env(tc.env))
		if err != nil || got.Path != tc.want || got.Own != tc.own {
			t.Errorf("%s: %+v, %v; want %s, own %v", tc.name, got, err, tc.want, tc.own)
		}
	}
}

// TestSaveAndLoad guards the round trip, the mode of the file and of a
// directory made for it, and that Save leaves no temporary file behind.
func TestSaveAndLoad(t *testing.T) {
	l := Location{Path: filepath.Join(t.TempDir(), "new", "nopacs", "contexts.json"), Own: true}
	f := &File{Current: "a", Contexts: map[string]Context{"a": {Server: "https://h:1", KeyFile: "/k", Tenant: "lab1"}}}
	if err := f.Save(l); err != nil {
		t.Fatal(err)
	}
	got, err := Load(l)
	if err != nil || got.Current != "a" || got.Contexts["a"].Tenant != "lab1" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(l.Path)
		di, _ := os.Stat(filepath.Dir(l.Path))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Errorf("modes: file %v, directory %v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(l.Path))
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries after Save, want only the file", len(entries))
	}
}

// TestSaveIsAtomic guards that a Save that cannot finish leaves the old file
// as it was and no temporary file: the target is made a directory so the
// final rename fails.
func TestSaveIsAtomic(t *testing.T) {
	l := loc(t)
	if err := (&File{Contexts: map[string]Context{"a": {Server: "https://h:1", KeyFile: "/k"}}}).Save(l); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(l.Path)
	// A second file whose rename fails: the target path is a non-empty directory.
	bad := Location{Path: filepath.Join(filepath.Dir(l.Path), "dir")}
	if err := os.MkdirAll(filepath.Join(bad.Path, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (&File{Contexts: map[string]Context{}}).Save(bad); err == nil {
		t.Fatal("Save over a directory succeeded")
	}
	after, _ := os.ReadFile(l.Path)
	if string(before) != string(after) {
		t.Error("the old file changed")
	}
	entries, _ := os.ReadDir(filepath.Dir(l.Path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".contexts-") {
			t.Errorf("a temporary file is left: %s", e.Name())
		}
	}
}

// TestLoadRefusals guards the files Load refuses: another version, trailing
// documents, a current context that is not defined, a bad name, not JSON.
func TestLoadRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"version":  `{"version":2,"contexts":{}}`,
		"trailing": `{"version":1,"contexts":{}} {}`,
		"current":  `{"version":1,"current":"x","contexts":{}}`,
		"name":     `{"version":1,"contexts":{"a b":{"server":"https://h","keyFile":"/k"}}}`,
		"notjson":  `current: a`,
		"type":     `{"version":1,"contexts":{"a":{"server":3}}}`,
	} {
		l := loc(t)
		if err := os.WriteFile(l.Path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(l); err == nil || !strings.Contains(err.Error(), l.Path) {
			t.Errorf("%s: %v, want an error that names the file", name, err)
		}
	}
	l := loc(t)
	if f, err := Load(l); err != nil || len(f.Contexts) != 0 {
		t.Errorf("a missing file: %+v, %v", f, err)
	}
}

// TestLoadRefusesAKeyWhereANameBelongs guards that a key typed into a context
// file, as a name or as any value, is called a key whatever its id starts
// with: ids are random, so only the prefix common to every key tells them.
func TestLoadRefusesAKeyWhereANameBelongs(t *testing.T) {
	for _, id := range []string{"kAbc", "7Xyz", "Zq9"} {
		key := "nopacs_" + id + "_secretsecretsecret"
		for field, body := range map[string]string{
			"name":    `{"version":1,"contexts":{"` + key + `":{"server":"https://h","keyFile":"/k"}}}`,
			"tenant":  `{"version":1,"contexts":{"a":{"server":"https://h","tenant":"` + key + `"}}}`,
			"keyFile": `{"version":1,"contexts":{"a":{"server":"https://h","keyFile":"` + key + `"}}}`,
		} {
			l := loc(t)
			if err := os.WriteFile(l.Path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(l)
			if err == nil {
				t.Errorf("%s %s: loaded, want a refusal", id, field)
			} else if strings.Contains(err.Error(), "secretsecret") {
				t.Errorf("%s %s: the error quotes the value: %v", id, field, err)
			}
		}
	}
}

// TestSaveRefusesALostUpdate guards that a Save does not replace a file that
// changed since it was read: two commands that read the same file and save in
// turn must not silently drop the first one's change.
func TestSaveRefusesALostUpdate(t *testing.T) {
	l := loc(t)
	first := &File{Contexts: map[string]Context{"a": {Server: "https://h:1", KeyFile: "/k"}}}
	if err := first.Save(l); err != nil {
		t.Fatal(err)
	}
	one, err := Load(l)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Load(l)
	if err != nil {
		t.Fatal(err)
	}
	one.Contexts["b"] = Context{Server: "https://h:2", KeyFile: "/k"}
	if err := one.Save(l); err != nil {
		t.Fatal(err)
	}
	two.Contexts["c"] = Context{Server: "https://h:3", KeyFile: "/k"}
	err = two.Save(l)
	if err == nil || !strings.Contains(err.Error(), "changed while this command ran") || !strings.Contains(err.Error(), l.Path) {
		t.Fatalf("the second Save: %v", err)
	}
	got, _ := Load(l)
	if _, ok := got.Contexts["b"]; !ok || len(got.Contexts) != 2 {
		t.Errorf("the file lost the first change: %v", got.Names())
	}
	// A Save that follows its own Save is not a conflict.
	one.Contexts["d"] = Context{Server: "https://h:4", KeyFile: "/k"}
	if err := one.Save(l); err != nil {
		t.Errorf("a second Save of the same File: %v", err)
	}
	// A file created by someone else since a missing file was read.
	l2 := loc(t)
	empty, _ := Load(l2)
	if err := first.Save(l2); err != nil {
		t.Fatal(err)
	}
	if err := empty.Save(l2); err == nil {
		t.Error("a Save over a file made since the read succeeded")
	}
}

// TestSavedFilePassesTheCheck guards that the file Save writes is one Load
// accepts: on Windows the temporary file must not inherit the directory's
// access list, which Load would refuse, so every save would break the next
// command. On Unix it guards the mode.
func TestSavedFilePassesTheCheck(t *testing.T) {
	l := loc(t)
	f := &File{Contexts: map[string]Context{"a": {Server: "https://h:1", KeyFile: "/k"}}}
	for range 2 {
		if err := f.Save(l); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(l); err != nil {
			t.Fatalf("Load refused the file Save wrote: %v", err)
		}
	}
}
