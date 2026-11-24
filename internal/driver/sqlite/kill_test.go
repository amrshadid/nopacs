package sqlite

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// The environment of the child process TestKillDuringChanges starts: the
// database it writes, set only in the child.
const killChildEnv = "NOPACS_KILL_CHILD_DB"

// killAnswered is how many changes the child must have answered before it is
// killed: enough that it is killed among many changes in flight, whatever
// the disk's speed, since it is a count and not a time.
const killAnswered = 400

// killDupID is an audit id taken before the child starts, which every
// seventh change of the child reuses, so that changes failing inside a
// shared transaction are killed along with the others.
const killDupID = "kill-dup"

// TestKillDuringChanges guards durability and atomicity across a crash: a
// process making changes from many goroutines at once, so that they share
// commits, is killed (SIGKILL on Unix, TerminateProcess on Windows) without
// a chance to close anything. The database is then reopened, every change
// the child was answered for is read back through the driver, and the
// sqlite3 command line tool, which is SQLite's own and not this package's,
// checks the file's integrity, that every answered change is there with its
// audit entry, that every refused one is absent, and that no change is
// there without its entry or an entry without its change.
//
// The test is skipped without the sqlite3 tool (SQLITE3 names one, or it is
// looked up on PATH), unless NOPACS_SQLITE3_REQUIRED is set, as the
// outside check sets it, which makes a missing tool a failure.
func TestKillDuringChanges(t *testing.T) {
	if path := os.Getenv(killChildEnv); path != "" {
		killChild(path)
		return
	}
	sqlite3 := os.Getenv("SQLITE3")
	if sqlite3 == "" {
		sqlite3 = "sqlite3"
	}
	sqlite3, err := exec.LookPath(sqlite3)
	if err != nil {
		if os.Getenv("NOPACS_SQLITE3_REQUIRED") != "" {
			t.Fatalf("the sqlite3 tool is required: %v", err)
		}
		t.Skipf("the sqlite3 tool is not here: %v", err)
	}
	if testing.Short() {
		t.Skip("starts a process")
	}

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "master.db")
	m, err := OpenMaster(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSetting(ctx, "seed", "v", entry(killDupID), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestKillDuringChanges$", "-test.count=1") // #nosec G204 -- this test binary itself.
	cmd.Env = append(os.Environ(), killChildEnv+"="+path)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Every line the child printed was printed after its change was
	// answered, so every one read, before or after the kill, counts.
	ok, refused := map[string]bool{}, map[string]bool{}
	reached := make(chan struct{})
	read := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			word, n, _ := strings.Cut(sc.Text(), " ")
			switch word {
			case "ok":
				ok[n] = true
				if len(ok) == killAnswered {
					close(reached)
				}
			case "refused":
				refused[n] = true
			}
		}
		read <- sc.Err()
	}()
	select {
	case <-reached:
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("the child did not answer %d changes", killAnswered)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("the child ended on its own, not by the kill")
	}
	t.Logf("killed after %d changes answered and %d refused", len(ok), len(refused))
	if len(refused) == 0 {
		t.Error("no change was refused: the failures inside a shared transaction were not exercised")
	}

	// Reopened through the driver, which recovers the write-ahead log.
	m, err = OpenMaster(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for n := range ok {
		if v, err := m.Setting(ctx, "kill."+n); err != nil || v != n {
			t.Errorf("answered change %s after the kill: %q, %v", n, v, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	q := func(query string) string {
		t.Helper()
		b, err := exec.Command(sqlite3, "-batch", "-noheader", path, query).CombinedOutput() //nolint:gosec // G204, G702: the tool and the database are this test's own.
		if err != nil {
			t.Fatalf("sqlite3 %q: %v: %s", query, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	if got := q("PRAGMA integrity_check"); got != "ok" {
		t.Errorf("PRAGMA integrity_check: %s", got)
	}
	// A change is a setting kill.N and its audit entry kill-N, in one
	// transaction: one without the other is a change half there.
	if got := q(`SELECT count(*) FROM settings WHERE name LIKE 'kill.%'
		AND 'kill-' || substr(name, 6) NOT IN (SELECT id FROM audit)`); got != "0" {
		t.Errorf("%s settings without their audit entry", got)
	}
	if got := q(`SELECT count(*) FROM audit WHERE id LIKE 'kill-%' AND id <> '` + killDupID + `'
		AND 'kill.' || substr(id, 6) NOT IN (SELECT name FROM settings)`); got != "0" {
		t.Errorf("%s audit entries without their setting", got)
	}
	there := map[string]bool{}
	for _, name := range strings.Fields(q(`SELECT name FROM settings WHERE name LIKE 'kill.%'`)) {
		there[strings.TrimPrefix(name, "kill.")] = true
	}
	for n := range ok {
		if !there[n] {
			t.Errorf("answered change %s is not in the file", n)
		}
	}
	for n := range refused {
		if there[n] {
			t.Errorf("refused change %s is in the file", n)
		}
	}
	t.Logf("sqlite3: integrity ok, %d changes in the file, %d answered before the kill", len(there), len(ok))
}

// killChild makes changes from 32 goroutines until it is killed, printing
// "ok N" once change N is answered as written and "refused N" once it is
// answered as refused. Every seventh change reuses a taken audit id, so it
// fails inside a transaction it shares with others.
func killChild(path string) {
	ctx := context.Background()
	m, err := OpenMaster(ctx, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var mu sync.Mutex
	say := func(word string, n int64) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintf(os.Stdout, "%s %d\n", word, n)
	}
	var seq atomic.Int64
	for range 32 {
		go func() {
			for {
				n := seq.Add(1)
				id := "kill-" + strconv.FormatInt(n, 10)
				if n%7 == 0 {
					id = killDupID
				}
				err := m.SetSetting(ctx, "kill."+strconv.FormatInt(n, 10), strconv.FormatInt(n, 10),
					master.AuditEntry{ID: id, Time: time.Now(), Method: "Kill", Outcome: master.OutcomeDone}, master.LocalOperator())
				switch {
				case err == nil:
					say("ok", n)
				case n%7 == 0:
					say("refused", n)
				default:
					fmt.Fprintln(os.Stderr, "change", n, err)
					os.Exit(3)
				}
			}
		}()
	}
	select {}
}
