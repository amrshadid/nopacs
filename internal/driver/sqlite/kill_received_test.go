package sqlite

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The environment of the child process TestKillDuringReceivedBatches
// starts: the tenant database it writes, set only in the child.
const killReceivedEnv = "NOPACS_KILL_RECEIVED_DB"

// killReceivers is how many receptions the child records files into at
// once, each from a goroutine of its own, so their batches share commits.
const killReceivers = 16

// killBatch is the files in each batch the child records.
const killBatch = 10

// TestKillDuringReceivedBatches guards durability on reception's own path:
// a process recording received files from killReceivers goroutines at once,
// so that their batches are written in shared transactions and one sync, is
// killed without a chance to close anything. Reopened, every batch the child
// was answered for is there, each of its files with its row, and each
// reception's received count equals its rows, so no batch is there in part
// and none acknowledged is lost; the sqlite3 tool checks the file's
// integrity. It is skipped without the sqlite3 tool unless
// NOPACS_SQLITE3_REQUIRED is set.
func TestKillDuringReceivedBatches(t *testing.T) {
	if path := os.Getenv(killReceivedEnv); path != "" {
		killReceivedChild(path)
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
	path := filepath.Join(t.TempDir(), "tenant.db")
	s, err := OpenTenant(ctx, path, "kill")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: now, Method: "Kill", Outcome: master.OutcomeDone}
	}
	if _, err := s.AddStorage(ctx, tenant.Storage{Name: "store", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "root", Path: "kill"}}, now, audit("a1"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddReceiver(ctx, tenant.Receiver{Name: "recv", Type: tenant.ReceiverDIMSE, Storage: "store",
		DIMSE:       tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE"},
		QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes},
		now, audit("a2"), nil); err != nil {
		t.Fatal(err)
	}
	for g := range killReceivers {
		if _, _, err := s.OpenReception(ctx, "recv", killStudy(g), killReception(g), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestKillDuringReceivedBatches$", "-test.count=1") // #nosec G204 -- this test binary itself.
	cmd.Env = append(os.Environ(), killReceivedEnv+"="+path)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Every line the child printed was printed after its batch was
	// answered, so every one read, before or after the kill, counts.
	acked := map[string]bool{}
	reached := make(chan struct{})
	read := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if batch, ok := strings.CutPrefix(sc.Text(), "ok "); ok {
				acked[batch] = true
				if len(acked) == killAnswered {
					close(reached)
				}
			}
		}
		read <- sc.Err()
	}()
	select {
	case <-reached:
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("the child did not answer %d batches", killAnswered)
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

	s, err = OpenTenant(ctx, path, "kill")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	there := map[string]bool{}
	for g := range killReceivers {
		page := master.Page{Limit: master.MaxPageLimit}
		rows := 0
		for {
			files, next, err := s.ReceivedFiles(ctx, killReception(g), page)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				there[f.SOPInstanceUID] = true
			}
			rows += len(files)
			if next == "" {
				break
			}
			page.Cursor = next
		}
		r, _, err := s.Reception(ctx, killReception(g))
		if err != nil {
			t.Fatal(err)
		}
		if r.Counts.FilesReceived != int64(rows) || rows%killBatch != 0 {
			t.Errorf("reception %d counts %d files received and has %d rows: a batch is there in part", g, r.Counts.FilesReceived, rows)
		}
	}
	for batch := range acked {
		g, n, _ := strings.Cut(batch, " ")
		gi, _ := strconv.Atoi(g)
		ni, _ := strconv.Atoi(n)
		for i := range killBatch {
			if uid := killUID(gi, ni, i); !there[uid] {
				t.Errorf("file %s of answered batch %s is not in the file after the kill", uid, batch)
			}
		}
	}
	got, err := exec.Command(sqlite3, "-batch", "-noheader", path, "PRAGMA integrity_check").CombinedOutput() //nolint:gosec // G204: the tool and the database are this test's own.
	if err != nil || strings.TrimSpace(string(got)) != "ok" {
		t.Errorf("sqlite3 PRAGMA integrity_check: %s %v", got, err)
	}
	t.Logf("killed after %d batches answered; %d files in the file", len(acked), len(there))
}

// killStudy, killReception and killUID name the child's studies,
// receptions and files.
func killStudy(g int) string           { return "2.25.1." + strconv.Itoa(g+1) }
func killReception(g int) string       { return fmt.Sprintf("kill%02d", g) }
func killUID(g, n, i int) string       { return fmt.Sprintf("2.25.3.%d.%d.%d", g+1, n+1, i+1) }
func killSum(uid string) tenant.SHA256 { return sha256.Sum256([]byte(uid)) }

// killReceivedChild records batches of killBatch received files into each
// of killReceivers receptions, one goroutine each, until it is killed,
// printing "ok G N" once batch N of reception G is answered as written.
func killReceivedChild(path string) {
	ctx := context.Background()
	s, err := OpenTenant(ctx, path, "kill")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var mu sync.Mutex
	for g := range killReceivers {
		go func() {
			for n := 0; ; n++ {
				files := make([]tenant.ReceivedFile, killBatch)
				for i := range files {
					uid := killUID(g, n, i)
					files[i] = tenant.ReceivedFile{ReceptionID: killReception(g), SOPInstanceUID: uid, StudyUID: killStudy(g),
						SeriesUID: "2.25.2." + strconv.Itoa(g+1), SOPClassUID: "1.2.840.10008.5.1.4.1.1.4",
						TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: 1000, SHA256: killSum(uid), Incoming: "recv/" + uid,
						Association: "assoc"}
				}
				if _, err := s.AddReceivedFiles(ctx, files, time.Now()); err != nil {
					fmt.Fprintln(os.Stderr, "batch", g, n, err)
					os.Exit(3)
				}
				mu.Lock()
				_, _ = fmt.Fprintf(os.Stdout, "ok %d %d\n", g, n)
				mu.Unlock()
			}
		}()
	}
	select {}
}
