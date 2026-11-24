package servecmd_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/servecmd"
)

// TestAnOnlineBackupRestoresLikeAnOfflineOne guards "nopacs backup create"
// over the API of a real server process: taken while a reception of the CT
// study sent by dcmtk's storescu is in progress, it writes a file whose size
// the summary (--json) states and which nopacs-server backup verify finds
// whole. With the server stopped, an offline backup is made by
// nopacs-server backup create; each is restored into a directory of its
// own, and read by the sqlite3 program, outside nopacs, the two restored
// installations have the same tables with the same columns, the same
// configuration rows and the same catalog: the restore rebuilt each from
// the storage, so the reception left half done in the online backup costs
// no instance.
func TestAnOnlineBackupRestoresLikeAnOfflineOne(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process and sends a study over the network; left out under -short")
	}
	scu := storescu(t)
	sqlite3, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("the sqlite3 program, which reads the restored databases, is not installed")
	}
	dataDir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dataDir); err != nil {
		t.Fatal(err)
	}
	child := startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dataDir)
	waitServing(t, dataDir, child)
	hc := socketClient(dataDir)
	roots := filepath.Join(filepath.Dir(dataDir), "roots")
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	const tn = "lab1"
	port := freePort(t)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tn+`","databaseType":"DATABASE_TYPE_SQLITE"}`)
	for name, value := range map[string]string{"receiver.ports": strconv.Itoa(port), "receiver.allow-as-received": "true"} {
		post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"`+name+`","scope":{"tenant":"`+tn+`"},"value":"`+value+`"}}`)
	}
	post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tn+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
	post(t, hc, "/nopacs.v1.ReceiverService/AddReceiver", `{"tenant":"`+tn+`","receiver":{"name":"scanner","enabled":true,"storage":"main",`+
		`"quietPeriod":"5s","dimse":{"port":`+strconv.Itoa(port)+`,"aeTitle":"ARCHIVE","allowedCallers":{"rules":[{"callingAeTitle":"STORESCU"}]}}}}`)
	waitWithin(t, "the receiver to be up", 10*time.Second, func() bool {
		return bytes.Contains(post(t, hc, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+tn+`"}`), []byte(`"RECEIVER_STATE_UP"`))
	})

	// The study is sent; once its reception is in progress (it stays so for
	// the 5-second quiet period after the last file), the backup is taken.
	args := []string{"-v", "-aet", "STORESCU", "-aec", "ARCHIVE", "127.0.0.1", strconv.Itoa(port)}
	for _, f := range ctStudy {
		args = append(args, filepath.Join("..", "receive", "testdata", f))
	}
	sent := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, scu, args...).CombinedOutput() //nolint:gosec // G204: a test's own tool and files.
		if err == nil && bytes.Count(out, []byte("Received Store Response (Success)")) != len(ctStudy) {
			err = fmt.Errorf("storescu stored fewer files than sent:\n%s", out)
		}
		sent <- err
	}()
	waitWithin(t, "a reception to be in progress", time.Minute, func() bool {
		return bytes.Contains(post(t, hc, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+tn+`","inProgress":true}`), []byte(`"id"`))
	})
	online := filepath.Join(t.TempDir(), "online.backup")
	sum := cliJSON(t, dataDir, "backup", "create", "--out", online)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(online)
	if err != nil {
		t.Fatal(err)
	}
	sections, _ := field(sum, "sections").([]any)
	if str(sum, "sizeBytes") != strconv.FormatInt(info.Size(), 10) || str(sum, "format") != "nopacs-backup" || len(sections) != 2 {
		t.Fatalf("the online backup's summary %v, for a file of %d bytes", sum, info.Size())
	}
	// A file at --out is never replaced; the summary for a person names each
	// section and the digest.
	if code, _, errOut := nopacs(t.Context(), dataDir, "backup", "create", "--out", online); code != 5 || !strings.Contains(errOut, "never replaces a file") {
		t.Fatalf("a backup onto an existing file: exit %d\n%s", code, errOut)
	}
	if after, err := os.Stat(online); err != nil || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		t.Fatalf("the existing backup was touched: %v %v", after, err)
	}
	code, out, errOut := nopacs(t.Context(), dataDir, "backup", "create", "--out", filepath.Join(t.TempDir(), "second.backup"))
	if code != 0 || !strings.Contains(out, "SHA-256") || !strings.Contains(out, "Tenant lab1") || !strings.Contains(out, "nopacs-server backup verify") {
		t.Fatalf("a backup's summary for a person: exit %d\n%s\n%s", code, out, errOut)
	}
	waitBody(t, "the reception to be done", func() bool {
		body := post(t, hc, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+tn+`"}`)
		return bytes.Contains(body, []byte("RECEPTION_STEP_DONE")) && !bytes.Contains(post(t, hc, "/nopacs.v1.ReceptionService/ListReceptions",
			`{"tenant":"`+tn+`","inProgress":true}`), []byte(`"id"`))
	})
	hc.CloseIdleConnections()
	stopServer(t, child)

	offline := filepath.Join(t.TempDir(), "offline.backup")
	if _, err := run(t, nil, "backup", "create", "--data-dir", dataDir, "--out", offline); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dataDir, servecmd.KeyFileName)
	text, err := run(t, nil, "backup", "verify", online, "--key-file", keyFile)
	// It was taken with the reception not done: verify names what a restore
	// of it cannot bring back of that reception (files in incoming).
	if err != nil || !strings.Contains(text, "is whole") || !strings.Contains(text, "of unfinished receptions") {
		t.Fatalf("verify of the online backup: %v\n%s", err, text)
	}
	fromOnline, fromOffline := shortDir(t), shortDir(t)
	if text, err := run(t, nil, "backup", "restore", online, "--data-dir", fromOnline, "--key-file", keyFile); err != nil {
		t.Fatalf("restore of the online backup: %v\n%s", err, text)
	}
	// The first restore took the storage's owner record one generation
	// further; the second, of a backup made before it, is told to take the
	// storage over.
	if text, err := run(t, nil, "backup", "restore", offline, "--data-dir", fromOffline, "--key-file", keyFile, "--take-over"); err != nil {
		t.Fatalf("restore of the offline backup: %v\n%s", err, text)
	}
	for _, db := range []string{"master.db", filepath.Join("tenants", tn+".db")} {
		a, b := readDB(t, sqlite3, filepath.Join(fromOnline, db)), readDB(t, sqlite3, filepath.Join(fromOffline, db))
		if len(a.schema) == 0 || (db == "master.db" && len(a.rows["tenants"]) == 0) {
			t.Fatalf("%s: read nothing: %+v", db, a)
		}
		if !slices.Equal(a.schema, b.schema) {
			t.Errorf("%s: the restored tables differ:\n%v\n%v", db, a.schema, b.schema)
		}
		for table, rows := range a.rows {
			if !slices.Equal(rows, b.rows[table]) {
				t.Errorf("%s: the restored %s differ:\n%v\n%v", db, table, rows, b.rows[table])
			}
		}
	}
	if n := readDB(t, sqlite3, filepath.Join(fromOnline, "tenants", tn+".db")).rows["instances"]; len(n) != len(ctStudy) {
		t.Fatalf("the catalog restored from the online backup holds %d instances, want %d", len(n), len(ctStudy))
	}
}

// restoredDB is what the test compares of a restored SQLite database.
type restoredDB struct {
	// schema is each table's statement, in order of name.
	schema []string
	// rows holds, by table, the rows of the tables that must come back the
	// same whatever moment the backup stood for, each as text, in order.
	rows map[string][]string
}

// comparedTables are the tables, with the columns of each, whose rows two
// restores of the same installation hold the same: configuration, and the
// catalog the restore rebuilt from the storage (its labels and identifiers,
// not its row ids, which the rebuild gives anew).
var comparedTables = map[string]string{
	"tenants":   "id, name, driver",
	"grants":    "key_id, scope, role",
	"storages":  "name, type, dir_root, dir_path",
	"receivers": "name, type, enabled, storage",
	"subjects":  "label",
	"sessions":  "label, study_uid",
	"scans":     "label, series_uid",
	"instances": "sop_instance_uid, hex(sha256)",
}

// sqliteLines runs one statement with the sqlite3 program on the database
// at path, read only, and returns its output's lines.
func sqliteLines(t *testing.T, sqlite3, path, statement string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, sqlite3, "-readonly", "-batch", path, statement).CombinedOutput() //nolint:gosec // G204: a test's own tool, file and statement.
	if err != nil {
		t.Fatalf("sqlite3 %s %q: %v\n%s", path, statement, err, out)
	}
	text := strings.TrimSuffix(string(out), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// readDB reads a restored SQLite database with the sqlite3 program.
func readDB(t *testing.T, sqlite3, path string) restoredDB {
	t.Helper()
	out := restoredDB{rows: map[string][]string{}}
	present := map[string]bool{}
	for _, name := range sqliteLines(t, sqlite3, path, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`) {
		present[name] = true
	}
	// Each statement on one line, so a line is a table.
	out.schema = sqliteLines(t, sqlite3, path, `SELECT replace(replace(sql, char(10), ' '), char(13), ' ') FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	for table, cols := range comparedTables {
		if present[table] {
			out.rows[table] = sqliteLines(t, sqlite3, path, `SELECT json_array(`+cols+`) FROM `+table+` ORDER BY 1`)
		}
	}
	return out
}
