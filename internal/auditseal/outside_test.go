package auditseal

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// The variables that ask for the outside check, beside the interpreter
// (NOPACS_CHECK_AUDITCHAIN_PYTHON) and openssl (NOPACS_CHECK_AUDITCHAIN_OPENSSL)
// the chain's own outside check reads.
const (
	pythonEnv  = "NOPACS_CHECK_AUDITCHAIN_PYTHON"
	opensslEnv = "NOPACS_CHECK_AUDITCHAIN_OPENSSL"
	// sqlite3Env names the sqlite3 command-line shell that reads a SQLite
	// log's tables; unset, the one on the PATH.
	sqlite3Env = "NOPACS_CHECK_AUDITCHAIN_SQLITE3"
	// psqlEnv is the command line of the psql that reads a Postgres log's
	// tables, split at spaces (such as "docker exec -i NAME psql -U postgres
	// -d nopacs_test" for a server in a container); unset, psql on the PATH
	// with the test database's address.
	psqlEnv = "NOPACS_CHECK_AUDITCHAIN_PSQL"
)

// outsideEntry is an entry as scripts/check/auditchain.py reads it.
type outsideEntry struct {
	Seq        int64             `json:"seq"`
	ID         string            `json:"id"`
	AtUS       int64             `json:"at_us"`
	KeyID      string            `json:"key_id"`
	LocalUser  string            `json:"local_user"`
	RemoteAddr string            `json:"remote_addr"`
	Method     string            `json:"method"`
	TenantName string            `json:"tenant_name"`
	Object     string            `json:"object"`
	Outcome    string            `json:"outcome"`
	Reason     string            `json:"reason"`
	Details    map[string]string `json:"details"`
	RequestID  string            `json:"request_id"`
	Encoded    string            `json:"encoded"`
}

// outsideCheckpoint is a checkpoint as the script reads it.
type outsideCheckpoint struct {
	Installation string `json:"installation"`
	Log          string `json:"log"`
	From         int64  `json:"from"`
	To           int64  `json:"to"`
	Head         string `json:"head"`
	Previous     string `json:"previous"`
	TimeUS       int64  `json:"time_us"`
	KeyVersion   uint32 `json:"key_version"`
	Signature    string `json:"signature"`
}

// TestSealChecksOutside guards the seal against tools that are not NoPACs',
// on each database type: a master's and a tenant's log are sealed in two
// passes, the second under a newer key version, and exported with Export.
// scripts/check/auditchain.py then encodes every entry with Python's
// hashlib and struct and compares it with the export's, folds the chain to
// every checkpoint's head, checks the previous digests, and has openssl
// verify every signature with the exported PEM keys; the heads it prints
// are the database's. Then the database's own shell (sqlite3, psql) reads
// the tables directly: the seq of every entry and the checkpoints'
// ranges, heads and digests are what the export carried, and each log's
// anchor of the other is the other's latest checkpoint. It runs only when
// NOPACS_CHECK_AUDITCHAIN_PYTHON names an interpreter (make
// check-auditchain).
func TestSealChecksOutside(t *testing.T) {
	python, asked := os.LookupEnv(pythonEnv)
	if !asked {
		t.Skip(pythonEnv + " is not set; run make check-auditchain")
	}
	if python == "" {
		t.Fatal(pythonEnv + " is set but empty: name a Python interpreter")
	}
	openssl := os.Getenv(opensslEnv)
	if openssl == "" {
		openssl = "openssl"
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "check", "auditchain.py"))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			keys := keyFile(t, 2)
			pub, err := PublicKeys(keys, testInstallation)
			if err != nil {
				t.Fatal(err)
			}
			c := &clock{t: t0.Add(time.Minute)}
			write(t, b.master, 7, 0)
			write(t, b.tenants["lab"], 5, 0)
			b.sealer(t, signerOf(t, keys, 1), c, 0).Pass(ctx)
			write(t, b.master, 4, time.Second)
			write(t, b.tenants["lab"], 6, time.Second)
			c.set(t0.Add(2 * time.Minute))
			b.sealer(t, signerOf(t, keys, 2), c, 0).Pass(ctx)

			for _, log := range []string{master.MasterAuditLog, b.ids["lab"]} {
				st := b.logs()[log]
				dir := t.TempDir()
				if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit to be entered.
					t.Fatal(err)
				}
				entries, checkpoints := exportFiles(t, st, log, pub, dir)
				heads := runScript(t, python, script, dir, openssl)
				if len(heads) != len(checkpoints) {
					t.Fatalf("%s: Python printed %d heads, want %d", log, len(heads), len(checkpoints))
				}
				for _, cp := range checkpoints {
					if heads[cp.To] != cp.Head {
						t.Errorf("%s: Python's head at seq %d is %s, the database's %s", log, cp.To, heads[cp.To], cp.Head)
					}
				}
				rows := b.outsideRows(t, log)
				checkTables(t, log, rows, entries, checkpoints)
				other := master.MasterAuditLog
				if log == master.MasterAuditLog {
					other = b.ids["lab"]
				}
				l := latestOf(t, b.logs()[other])
				want := other + "|" + strconv.FormatInt(l.To, 10) + "|" + hex.EncodeToString(l.Digest[:])
				if !containsLine(rows.anchors, want) {
					t.Errorf("%s: the shell reads anchors %q, want one %q", log, rows.anchors, want)
				}
				t.Logf("%s on %s: %d entries, %d checkpoints checked by Python and openssl, and read back by the database's shell", log, b.name, len(entries), len(checkpoints))
			}
		})
	}
}

// exportFiles exports a whole log and writes it as the script reads it.
func exportFiles(t *testing.T, st master.AuditSealReads, log string, pub []PublicKey, dir string) ([]outsideEntry, []outsideCheckpoint) {
	t.Helper()
	var entries []outsideEntry
	var checkpoints []outsideCheckpoint
	for _, it := range exportAll(t, st, ExportOptions{Installation: testInstallation, Log: log, Keys: pub}) {
		switch {
		case it.Header != nil:
			for _, k := range it.Header.Keys {
				pem, err := k.PEM()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "public-"+strconv.FormatUint(uint64(k.Version), 10)+".pem"), pem, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		case it.Entry != nil:
			e := it.Entry.Entry
			details := e.Details
			if details == nil {
				details = map[string]string{}
			}
			entries = append(entries, outsideEntry{
				Seq: it.Entry.Seq, ID: e.ID, AtUS: e.Time.UnixMicro(), KeyID: e.KeyID, LocalUser: e.LocalUser,
				RemoteAddr: e.RemoteAddress, Method: e.Method, TenantName: e.TenantName, Object: e.Object,
				Outcome: string(e.Outcome), Reason: e.Reason, Details: details, RequestID: e.RequestID,
				Encoded: hex.EncodeToString(it.Entry.Encoded),
			})
		case it.Checkpoint != nil:
			c := it.Checkpoint
			checkpoints = append(checkpoints, outsideCheckpoint{
				Installation: c.Installation, Log: c.Log, From: c.From, To: c.To,
				Head: hex.EncodeToString(c.Head[:]), Previous: hex.EncodeToString(c.Previous[:]), TimeUS: c.Time.UnixMicro(),
				KeyVersion: c.KeyVersion, Signature: hex.EncodeToString(c.Signature),
			})
		}
	}
	writeLines(t, filepath.Join(dir, "entries.jsonl"), entries)
	writeLines(t, filepath.Join(dir, "checkpoints.jsonl"), checkpoints)
	return entries, checkpoints
}

// writeLines writes one JSON value per line.
func writeLines[T any](t *testing.T, path string, values []T) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, v := range values {
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runScript runs scripts/check/auditchain.py on dir and returns the heads it
// printed, by seq.
func runScript(t *testing.T, python, script, dir, openssl string) map[int64]string {
	t.Helper()
	cmd := exec.Command(python, "-I", script, dir, openssl) //nolint:gosec // G702: the interpreter and openssl are the ones the person running the test named.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		t.Logf("python: %s", line)
	}
	if err != nil {
		t.Fatalf("auditchain.py: %v\n%s", err, stderr.String())
	}
	heads := map[int64]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != "head" {
			continue
		}
		seq, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		heads[seq] = f[2]
	}
	return heads
}

// tableRows are a log's tables as the database's own shell prints them, a
// row a line, fields joined by "|".
type tableRows struct {
	entries, checkpoints, anchors []string
}

// The statements the shells run: the same on both, but for hex.
const (
	sqliteEntries     = `SELECT seq, id FROM audit WHERE seq IS NOT NULL ORDER BY seq`
	sqliteCheckpoints = `SELECT from_seq, to_seq, lower(hex(head)), lower(hex(digest)), key_version, pruned FROM audit_checkpoints ORDER BY to_seq`
	sqliteAnchors     = `SELECT log, to_seq, lower(hex(digest)) FROM audit_anchors ORDER BY log`
	pgEntries         = `SELECT seq, id FROM audit WHERE seq IS NOT NULL ORDER BY seq`
	pgCheckpoints     = `SELECT from_seq, to_seq, encode(head, 'hex'), encode(digest, 'hex'), key_version, CASE WHEN pruned THEN 1 ELSE 0 END FROM audit_checkpoints ORDER BY to_seq`
	pgAnchors         = `SELECT log, to_seq, encode(digest, 'hex') FROM audit_anchors ORDER BY log`
)

// outsideRows reads a log's tables with sqlite3 or psql.
func (b *backend) outsideRows(t *testing.T, log string) tableRows {
	t.Helper()
	where := b.where[log]
	if b.name == "sqlite" {
		shell := os.Getenv(sqlite3Env)
		if shell == "" {
			shell = "sqlite3"
		}
		run := func(q string) []string {
			return lines(t, exec.Command(shell, "-readonly", "-batch", "-separator", "|", where, q)) //nolint:gosec // G204: the shell the person running the test named.
		}
		return tableRows{entries: run(sqliteEntries), checkpoints: run(sqliteCheckpoints), anchors: run(sqliteAnchors)}
	}
	u, err := url.Parse(where)
	if err != nil {
		t.Fatal(err)
	}
	schema := u.Query().Get("search_path")
	command := strings.Fields(os.Getenv(psqlEnv))
	if len(command) == 0 {
		q := u.Query()
		q.Del("search_path")
		q.Del("pool_max_conns")
		u.RawQuery = q.Encode()
		command = []string{"psql", u.String()}
	}
	run := func(q string) []string {
		args := append(append([]string{}, command[1:]...), "-X", "-q", "-A", "-t", "-F", "|",
			"-c", "SET search_path TO "+schema, "-c", q)
		return lines(t, exec.Command(command[0], args...)) //nolint:gosec // G204: the psql the person running the test named.
	}
	return tableRows{entries: run(pgEntries), checkpoints: run(pgCheckpoints), anchors: run(pgAnchors)}
}

// lines runs cmd and returns its output's lines.
func lines(t *testing.T, cmd *exec.Cmd) []string {
	t.Helper()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd.Path, err, stderr.String())
	}
	var rows []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			rows = append(rows, l)
		}
	}
	return rows
}

// checkTables compares what the shell read with what the export carried.
func checkTables(t *testing.T, log string, rows tableRows, entries []outsideEntry, checkpoints []outsideCheckpoint) {
	t.Helper()
	if len(rows.entries) != len(entries) {
		t.Fatalf("%s: the shell reads %d sealed entries, the export carried %d", log, len(rows.entries), len(entries))
	}
	for i, e := range entries {
		if want := strconv.FormatInt(e.Seq, 10) + "|" + e.ID; rows.entries[i] != want {
			t.Errorf("%s: the shell reads %q, the export carried %q", log, rows.entries[i], want)
		}
	}
	if len(rows.checkpoints) != len(checkpoints) {
		t.Fatalf("%s: the shell reads %d checkpoints, the export carried %d", log, len(rows.checkpoints), len(checkpoints))
	}
	for i, c := range checkpoints {
		f := strings.Split(rows.checkpoints[i], "|")
		if len(f) != 6 || f[0] != strconv.FormatInt(c.From, 10) || f[1] != strconv.FormatInt(c.To, 10) || f[2] != c.Head ||
			f[4] != strconv.FormatUint(uint64(c.KeyVersion), 10) || f[5] != "0" {
			t.Errorf("%s: the shell reads checkpoint %q, the export carried %+v", log, rows.checkpoints[i], c)
		}
		if i+1 < len(checkpoints) && f[3] != checkpoints[i+1].Previous {
			t.Errorf("%s: checkpoint %d's digest in the table is not what the next one names as previous", log, c.To)
		}
	}
}

// containsLine reports whether rows holds want.
func containsLine(rows []string, want string) bool {
	for _, r := range rows {
		if r == want {
			return true
		}
	}
	return false
}
