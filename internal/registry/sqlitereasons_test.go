package registry

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestSQLiteReasonsHideThePath guards, through the real SQLite tenant
// driver, that no state reason, refusal, creation error or log line the
// registry builds from a driver failure holds the database's path: a tenant
// is created, its file is then damaged in each way the driver refuses, and
// the registry is started again with its data directory under a path that
// holds a recognizable string.
func TestSQLiteReasonsHideThePath(t *testing.T) {
	const secret = "SECRET-PATH-k3Vw" //nolint:gosec // G101: a marker searched for, not a credential.
	damage := map[string]func(t *testing.T, tenants, path string){
		"a file that is not a database": func(t *testing.T, _, path string) {
			if err := os.WriteFile(path, []byte(strings.Repeat("not a database\n", 400)), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a directory at the path": func(t *testing.T, _, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a tenants directory that is a file": func(t *testing.T, tenants, _ string) {
			if err := os.RemoveAll(tenants); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tenants, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	if runtime.GOOS != "windows" {
		damage["a file others can read"] = func(t *testing.T, _, path string) {
			if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: the loose mode is what is tested.
				t.Fatal(err)
			}
		}
		damage["a symbolic link at the path"] = func(t *testing.T, tenants, path string) {
			target := filepath.Join(tenants, "elsewhere.db")
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, spoil := range damage {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			e.data = filepath.Join(e.data, secret)
			if err := os.Mkdir(e.data, 0o700); err != nil {
				t.Fatal(err)
			}
			setup := e.newRegistry(e.config())
			e.create(setup, "id-lab", "lab", driver.SQLiteDriver, "")
			if err := setup.Close(ctx); err != nil {
				t.Fatal(err)
			}
			tenants := filepath.Join(e.data, driver.TenantsDir)
			spoil(t, tenants, filepath.Join(tenants, "lab.db"))

			r := e.newRegistry(e.config())
			if err := r.Start(ctx); err != nil {
				t.Fatal(err)
			}
			st, _ := r.State("lab")
			if st.State == master.StateReady {
				t.Fatal("the damaged tenant is ready")
			}
			_, serr := r.Store("lab")
			_, _, cerr := r.Create(ctx, NewTenant{ID: "id-lab2", Name: "lab2", Driver: driver.SQLiteDriver}, e.audit("CreateTenant"), master.LocalOperator())
			texts := []string{st.Reason, e.masterTenant("lab").StateReason, serr.Error(), e.logBuf.String()}
			if cerr != nil {
				texts = append(texts, cerr.Error())
			}
			for _, text := range texts {
				mustNotContain(t, "a reason, error or log line", text, secret, e.data)
			}
			t.Logf("%s: %s", st.State, st.Reason)
		})
	}
}
