package rebuild

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The variables of the outside check: the Python that runs
// scripts/prove/catalog_dump.py (standard library only), and, for the
// Postgres part, the command line of the psql it reads with.
const (
	checkPythonEnv = "NOPACS_CHECK_REBUILD_PYTHON"
	checkPsqlEnv   = "NOPACS_CHECK_REBUILD_PSQL"
)

// dumpTool is scripts/prove/catalog_dump.py, run by the Python checkPythonEnv
// names.
type dumpTool struct {
	t      *testing.T
	python string
	psql   string
}

// newDumpTool returns the tool, skipping the test when checkPythonEnv is
// not set; set, even to nothing, the check was asked for, and a Python that
// is not there fails it.
func newDumpTool(t *testing.T) dumpTool {
	t.Helper()
	python, asked := os.LookupEnv(checkPythonEnv)
	if !asked {
		t.Skip(checkPythonEnv + " is not set: the outside check of rebuilt catalogs was not asked for")
	}
	if python == "" {
		python = "python3"
	}
	if _, err := exec.LookPath(python); err != nil {
		t.Fatalf("%s names no Python: %v", checkPythonEnv, err)
	}
	return dumpTool{t: t, python: python, psql: os.Getenv(checkPsqlEnv)}
}

// dump runs the tool on the SQLite file at path, or on the Postgres schema
// named (address set), with the flags given, and returns its output.
func (d dumpTool) dump(path, address, schema string, flags ...string) string {
	d.t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "prove", "catalog_dump.py"))
	if err != nil {
		d.t.Fatal(err)
	}
	args := []string{"-I", script}
	cmd := exec.Command(d.python) // #nosec G204 -- the Python the person running the check named.
	if address != "" {
		u, err := url.Parse(address)
		if err != nil {
			d.t.Fatal(err)
		}
		args = append(args, "--postgres", strings.TrimPrefix(u.Path, "/"), "--schema", schema)
		cmd.Env = append(os.Environ(), "PSQL="+d.psql)
	} else {
		args = append(args, "--sqlite", path)
	}
	cmd.Args = append([]string{d.python}, append(args, flags...)...)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		d.t.Fatalf("catalog_dump.py %v: %v %s", flags, err, stderr)
	}
	return string(out)
}

// catalogRows returns the rows of one table of a dump.
func catalogRows(t *testing.T, dump, table string) []map[string]any {
	t.Helper()
	var all map[string][]map[string]any
	if err := json.Unmarshal([]byte(dump), &all); err != nil {
		t.Fatalf("the dump is not JSON: %v", err)
	}
	return all[table]
}

// TestRebuildChecksOutside checks the rebuild with a tool that is not
// NoPACs: catalog_dump.py reads the tenant's catalog and the shadow with the
// sqlite3 module or psql, matching rows by labels and UIDs with integer ids
// dropped. The corpus is archived through the real pipeline; a compare's
// shadow, read before it is dropped, equals the catalog in use (whole, and
// without times and receptions as the tool's documentation offers); every
// catalog row is then removed, the storage kept, and a replace makes a
// catalog equal to the dump taken before; last, a scan a prepared delete's
// correction record names is absent from a compare's shadow and nothing
// else is.
func TestRebuildChecksOutside(t *testing.T) {
	tool := newDumpTool(t)
	drivers(t, func(t *testing.T, postgres bool) {
		if postgres && tool.psql == "" {
			t.Skip(checkPsqlEnv + " is not set: no psql to read a Postgres catalog with")
		}
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		tenantSchema := ""
		if postgres {
			u, err := url.Parse(r.address)
			if err != nil {
				t.Fatal(err)
			}
			tenantSchema = u.Query().Get("search_path")
		}
		dumps := func(path, schema string) [2]string {
			return [2]string{tool.dump(path, r.address, schema), tool.dump(path, r.address, schema, "--skip-times", "--skip-receptions")}
		}
		live := dumps(r.dbPath, tenantSchema)
		if len(catalogRows(t, live[0], "instances")) != len(studyFiles) {
			t.Fatalf("the live dump holds %d instances, want %d", len(catalogRows(t, live[0], "instances")), len(studyFiles))
		}
		var shadow [2]string
		var writes atomic.Int64
		var job tenant.Job
		cfg := withBeforeDrop(r.config(&writes), func(id string) {
			j, err := r.p.Job(r.ctx, id)
			if err != nil {
				t.Error(err)
				return
			}
			job = j
			shadow = dumps(r.dbPath+".rebuild", j.Rebuild.Scratch)
		})
		end, _ := r.run(cfg, tenant.RebuildCompare)
		if end.State != tenant.JobDone || job.ID != end.ID {
			t.Fatalf("the compare ended %+v", end)
		}
		for i, what := range []string{"whole", "without times and receptions"} {
			if shadow[i] != live[i] {
				t.Errorf("the compare's shadow differs from the live catalog (%s):\n%s\nwant\n%s", what, shadow[i], live[i])
			}
		}
		r.forgetCatalog()
		if n := len(catalogRows(t, tool.dump(r.dbPath, r.address, tenantSchema), "scans")); n != 0 {
			t.Fatalf("the catalog removed still dumps %d scans", n)
		}
		r.run(r.config(&writes), tenant.RebuildReplace)
		for i, what := range []string{"whole", "without times and receptions"} {
			if got := dumps(r.dbPath, tenantSchema)[i]; got != live[i] {
				t.Errorf("the replaced catalog differs from the dump before (%s):\n%s\nwant\n%s", what, got, live[i])
			}
		}
		// A prepared delete of the first scan: the shadow lacks it.
		paths := r.scanPaths()
		gone, err := r.p.CatalogScan(r.ctx, paths[0])
		if err != nil {
			t.Fatal(err)
		}
		r.putRecord(r.recordOf("01j00000000000000000000001", paths[0], record.LevelScan, record.StatePrepared, nil))
		r.run(cfg, tenant.RebuildCompare)
		var series []string
		for _, row := range catalogRows(t, shadow[0], "scans") {
			series = append(series, row["series_uid"].(string))
		}
		liveScans := catalogRows(t, live[0], "scans")
		if len(series) != len(liveScans)-1 {
			t.Errorf("the shadow holds %d scans, want %d", len(series), len(liveScans)-1)
		}
		for _, s := range series {
			if s == gone.SeriesUID {
				t.Errorf("the shadow holds the scan the delete's record names")
			}
		}
		if writes.Load() != 0 {
			t.Errorf("a rebuild wrote %d times to a storage", writes.Load())
		}
	})
}
