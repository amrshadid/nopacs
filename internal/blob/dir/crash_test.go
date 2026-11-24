package dir

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
)

// crashRootEnv names the variable that makes this test binary, run again by
// TestCrash, the child that puts until it is killed.
const crashRootEnv = "NOPACS_BLOB_CRASH_ROOT"

// crashGenEnv names the variable that gives the child its first
// generation, higher each round, so a later round's objects are told from
// an earlier one's.
const crashGenEnv = "NOPACS_BLOB_CRASH_GENERATION"

// crashStallEnv names the variable that tells the child, in some rounds,
// after how many finished Puts to stop halfway through the next one and
// say so, so the kill is certain to land inside a Put.
const crashStallEnv = "NOPACS_BLOB_CRASH_STALL"

// crashRounds is how many times the child is started and killed, each time
// after a different number of finished Puts.
const crashRounds = 8

// crashKeys is how many keys the child keeps replacing.
const crashKeys = 12

// crashObject returns the bytes the child puts at key k in generation g: the
// generation as 8 bytes, then a pattern whose seed and length both follow
// from k and g, up to 2 MB, so that a reader can tell from the first 8
// bytes alone what the whole object must be. It is a stream, so the child
// spends its time inside Put, making the bytes as they are written.
func crashObject(k, g int) io.Reader {
	seed := uint64(k)<<32 | uint64(g)       // #nosec G115 -- small, non-negative test counters.
	n := int64(seed*2654435761%(2<<20)) + 1 // #nosec G115 -- below 2 MB.
	var head [8]byte
	binary.LittleEndian.PutUint64(head[:], uint64(g)) // #nosec G115 -- as above.
	return io.MultiReader(bytes.NewReader(head[:]), blobtest.Pattern(seed, n))
}

func crashKey(k int) string { return fmt.Sprintf("crash/k%02d/object", k) }

// TestCrash guards that a process killed while it puts leaves every key
// whole or absent: each object found afterwards is, byte for byte, one the
// process put, and each Put it saw return is there. The killed process is
// this test binary run again as a child, which replaces a few keys over and
// over with objects of up to 2 MB, so that it is almost always inside a Put
// when the kill comes. Each round kills it at a different point; every
// other round, the child stops halfway through a Put and says so first, so
// that at least those kills land inside a Put for certain, and the test
// checks that a temporary file was left and the key kept its old object.
//
// A kill does not lose what the system has cached but not yet written to
// the disk, so this guards the temporary file and the rename; TestPutSteps
// guards the syncs that a power cut would test.
func TestCrash(t *testing.T) {
	if root := os.Getenv(crashRootEnv); root != "" {
		crashChild(root, os.Getenv(crashGenEnv))
		return
	}
	if testing.Short() {
		t.Skip("starts a child process several times")
	}
	root := t.TempDir()
	done := map[int]int{} // key -> highest generation whose Put returned
	temps := 0
	for round := range crashRounds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrash$") // #nosec G204 G702 -- this test binary itself.
		after := 3 + round*2
		stall := round%2 == 0
		cmd.Env = append(os.Environ(), crashRootEnv+"="+root, crashGenEnv+"="+strconv.Itoa(round*1000000))
		if stall {
			cmd.Env = append(cmd.Env, crashStallEnv+"="+strconv.Itoa(after))
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Wait for a number of finished Puts that differs by round, or for
		// the child to say it stopped inside a Put, then kill.
		lines := bufio.NewScanner(out)
		stalledKey, stalledGen := -1, -1
		for seen := 0; (stall || seen < after) && lines.Scan(); seen++ {
			var k, g int
			if _, err := fmt.Sscanf(lines.Text(), "midway %d %d", &k, &g); err == nil {
				stalledKey, stalledGen = k, g
				break
			}
			if _, err := fmt.Sscanf(lines.Text(), "put %d %d", &k, &g); err != nil {
				t.Fatalf("the child said %q", lines.Text())
			}
			done[k] = max(done[k], g)
		}
		if stall && stalledKey < 0 {
			t.Fatalf("round %d: the child ended without stopping inside a Put", round)
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		// Puts that returned between the last line read and the kill.
		for lines.Scan() {
			var k, g int
			if _, err := fmt.Sscanf(lines.Text(), "put %d %d", &k, &g); err == nil {
				done[k] = max(done[k], g)
			}
		}
		_ = cmd.Wait() // Killed: its exit status says so and nothing more.
		left := countTemps(t, root)
		temps += left
		if stall && left == 0 {
			t.Fatalf("round %d: killed inside a Put, but no temporary file was left", round)
		}

		s, err := Open(root)
		if err != nil {
			t.Fatalf("round %d: Open after the kill: %v", round, err)
		}
		if n := countTemps(t, root); n != 0 {
			t.Fatalf("round %d: %d temporary files left after Open", round, n)
		}
		checkCrashObjects(t, s, done)
		if stall {
			r, err := s.Read(context.Background(), crashKey(stalledKey), 0, 8)
			if err == nil {
				b, _ := io.ReadAll(r)
				_ = r.Close()
				if len(b) == 8 && int(binary.LittleEndian.Uint64(b)) == stalledGen { // #nosec G115 -- a test generation number.
					t.Fatalf("round %d: %q holds the generation whose Put was killed halfway", round, crashKey(stalledKey))
				}
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("temporary files the kills left, all removed by Open: %d in %d rounds", temps, crashRounds)
}

// checkCrashObjects checks every key under crash/ against what the child
// could have put there, and that every Put the child saw return is there.
func checkCrashObjects(t *testing.T, s *Store, done map[int]int) {
	t.Helper()
	ctx := context.Background()
	p, err := s.List(ctx, blob.ListOptions{Prefix: "crash/"})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, key := range p.Keys {
		found[key] = true
		r, err := s.Read(ctx, key, 0, -1)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		k, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSuffix(key, "/object"), "crash/k"))
		if err != nil || len(b) < 8 {
			t.Fatalf("%q holds %d bytes, not an object the child put", key, len(b))
		}
		g := int(binary.LittleEndian.Uint64(b)) // #nosec G115 -- a test generation number.
		want, err := io.ReadAll(crashObject(k, g))
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(b) != sha256.Sum256(want) {
			t.Fatalf("%q holds %d bytes that are not generation %d whole (%d bytes)", key, len(b), g, len(want))
		}
		if g < done[k] {
			t.Fatalf("%q holds generation %d, but the Put of generation %d had returned", key, g, done[k])
		}
	}
	for k := range done {
		if !found[crashKey(k)] {
			t.Fatalf("%q is absent, but a Put to it had returned", crashKey(k))
		}
	}
}

// countTemps counts the temporary files under root: the files in its
// temporary directory, and any file a Put might have left elsewhere, found
// by walking the whole tree, which only a test can afford.
func countTemps(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && (filepath.Base(filepath.Dir(p)) == tempDir || strings.HasPrefix(d.Name(), blob.ReservedPrefix)) {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// crashChild puts generation after generation of each key, saying on
// standard output each Put that returned, until it is killed. It gives up
// after a minute, so a parent that died cannot leave it running.
//
// Given a number in crashStallEnv, it stops after that many finished Puts,
// halfway through the next one, says "midway" with its key and generation,
// and waits there to be killed.
func crashChild(root, generation string) {
	start, err := strconv.Atoi(generation)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child: no first generation:", err)
		os.Exit(2)
	}
	s, err := Open(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child:", err)
		os.Exit(2)
	}
	stallAfter := -1
	if v := os.Getenv(crashStallEnv); v != "" {
		if stallAfter, err = strconv.Atoi(v); err != nil {
			fmt.Fprintln(os.Stderr, "child: bad stall count:", err)
			os.Exit(2)
		}
	}
	ctx := context.Background()
	deadline := time.Now().Add(time.Minute)
	finished := 0
	for g := start; time.Now().Before(deadline); g++ {
		for k := range crashKeys {
			r := crashObject(k, g)
			if finished == stallAfter {
				r = io.MultiReader(io.LimitReader(r, 4096), &stallReader{k: k, g: g})
			}
			finished++
			if _, err := s.Put(ctx, crashKey(k), r); err != nil {
				fmt.Fprintln(os.Stderr, "child:", err)
				os.Exit(2)
			}
			fmt.Printf("put %d %d\n", k, g)
		}
	}
	os.Exit(0)
}

// stallReader says "midway" once it is reached, then never returns, so the
// Put reading it stays inside its copy until the process is killed.
type stallReader struct{ k, g int }

func (r *stallReader) Read([]byte) (int, error) {
	fmt.Printf("midway %d %d\n", r.k, r.g)
	time.Sleep(time.Hour) // Until the kill.
	return 0, io.ErrUnexpectedEOF
}
