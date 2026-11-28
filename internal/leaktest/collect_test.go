package leaktest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// readDatabases records what the server stored once it has stopped, read
// through the database ports: every entry of the master's audit log, and,
// of each tenant, every entry of its audit log and every event of its feed.
func readDatabases(t *testing.T, s *site, rec *recorder) {
	t.Helper()
	ctx := context.Background()
	m, err := driver.OpenMaster(ctx, driver.SQLiteScheme+filepath.Join(s.dataDir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	audit := func(label, who string, list func(master.Page) ([]master.AuditEntry, string, error)) int {
		cursor, n := "", 0
		for {
			entries, next, err := list(master.Page{Cursor: cursor, Limit: master.MaxPageLimit})
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				b, _ := json.Marshal(e)
				rec.add(label, who, string(b))
				n++
			}
			if next == "" {
				return n
			}
			cursor = next
		}
	}
	if n := audit("audit-db", "master", func(p master.Page) ([]master.AuditEntry, string, error) {
		return m.ListAudit(ctx, master.AuditFilter{}, p)
	}); n == 0 {
		t.Fatal("the master's audit log is empty")
	}
	events := 0
	for _, name := range []string{"lab1", "lab2"} {
		row, err := m.TenantByName(ctx, name)
		if err != nil {
			continue
		}
		st, err := (driver.SQLiteTenants{DataDir: s.dataDir}).Open(ctx, row.ID, name, "")
		if err != nil {
			t.Fatal(err)
		}
		audit("audit-db", name, func(p master.Page) ([]master.AuditEntry, string, error) {
			return st.ListAudit(ctx, master.AuditFilter{}, p)
		})
		feed, ok := st.(tenant.Events)
		if !ok {
			t.Fatal("the tenant's store keeps no event feed")
		}
		cursor := ""
		for {
			page, err := feed.ReadEvents(ctx, cursor, nil, tenant.MaxEventPage)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range page.Events {
				b, _ := json.Marshal(e)
				rec.add("events-db", name, string(e.Type)+" "+string(b))
				events++
			}
			if len(page.Events) == 0 || page.Cursor == cursor {
				break
			}
			cursor = page.Cursor
		}
		_ = st.Close()
	}
	if events == 0 {
		t.Fatal("the tenants stored no event")
	}
}

// checkReached requires that the run reached what it means to search: the
// calls were answered (a reader of the tenant was told a scan's labels), the
// log holds the receptions, the audit log holds the keys' calls, and the
// command line printed something.
func checkReached(t *testing.T, items []surface) {
	t.Helper()
	has := func(label, who, text string) bool {
		for _, it := range items {
			if strings.HasPrefix(it.label, label) && (who == "" || it.who == who) && strings.Contains(it.text, text) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct{ label, who, text string }{
		{"api/GetScan", "tenant-reader", `"subjectLabel"`},
		{"api/GetScanMetadata", "tenant-reader", `dicomJson`},
		{"api/ListQuarantinedFiles", "tenant-operator", `"files"`},
		{"api/GetQuarantinedFile", "tenant-operator", `"reason"`},
		{"audit-api", "installation-admin", `"entries"`},
		{"events-feed", "installation-admin", `"type"`},
		{"audit-db", "", `GetScan`},
		{"events-db", "", `reception_step`},
		{"cli/catalog scans", "tenant-reader", "sub-"},
		{"cli/quarantine list --json", "tenant-operator", `"schemaVersion"`},
		{"log", "", `"msg"`},
	} {
		if !has(c.label, c.who, c.text) {
			t.Errorf("no surface %q seen by %q holds %q: the run did not reach what it searches", c.label, c.who, c.text)
		}
	}
}

// dump writes every surface to the file surfaces.txt in the directory named
// by NOPACS_LEAKTEST_DUMP, when it is set, to read what a run saw.
func dump(t *testing.T, items []surface) {
	t.Helper()
	dir := os.Getenv("NOPACS_LEAKTEST_DUMP")
	if dir == "" {
		return
	}
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "=== %s | %s\n%s\n", it.label, it.who, it.text)
	}
	if err := os.WriteFile(filepath.Join(dir, "surfaces.txt"), []byte(b.String()), 0o600); err != nil { // #nosec G703 -- a directory the person running the test names.
		t.Fatal(err)
	}
}
