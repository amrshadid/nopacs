//go:build unix

package incoming

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The child process's environment: the area's root, the step to stop at,
// and on which hit of it.
const (
	crashRootEnv  = "NOPACS_INCOMING_CRASH_ROOT"
	crashPointEnv = "NOPACS_INCOMING_CRASH_POINT"
	crashHitEnv   = "NOPACS_INCOMING_CRASH_HIT"
)

// crashPoints are the durable steps of a landing.
var crashPoints = []string{"land.before-sync", "land.before-rename", "land.before-dirsync", "land.done"}

// TestCrashChild is the child process of TestLandSurvivesAKill: it lands
// files of known content and prints each one Land returned, then is killed
// with signal 9 at the step it was told. Run alone, it does nothing.
func TestCrashChild(t *testing.T) {
	root := os.Getenv(crashRootEnv)
	if root == "" {
		t.Skip("the child of a crash test; run by TestLandSurvivesAKill")
	}
	a, err := Open(root, "lab")
	if err != nil {
		t.Fatal(err)
	}
	point := os.Getenv(crashPointEnv)
	hit, _ := strconv.Atoi(os.Getenv(crashHitEnv))
	n := 0
	SetHook(a, func(p string) {
		if p == point {
			n++
			if n == hit {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
	})
	out := bufio.NewWriter(os.Stdout)
	for i := 0; ; i++ {
		data := content(i)
		f, err := a.Land(context.Background(), "dimse", bytes.NewReader(data), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(out, "%s %x\n", f.ID, f.SHA256)
		_ = out.Flush()
	}
}

// content returns the bytes of the i-th file a child lands.
func content(i int) []byte {
	return bytes.Repeat([]byte("file "+strconv.Itoa(i)+" "), 2000+i*37)
}

// TestLandSurvivesAKill guards the order of a landing: a process killed with
// signal 9 at every durable step, on many hits, leaves every file whose
// Land returned under its landed name with its bytes whole, and nothing else
// but files being written, which a restart removes.
func TestLandSurvivesAKill(t *testing.T) {
	if testing.Short() {
		t.Skip("starts child processes")
	}
	rounds := 0
	for _, point := range crashPoints {
		for _, hit := range []int{1, 2, 5, 13} {
			rounds++
			root := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.count=1") // #nosec G204 G702 -- the test binary itself.
			cmd.Env = append(os.Environ(), crashRootEnv+"="+root, crashPointEnv+"="+point, crashHitEnv+"="+strconv.Itoa(hit))
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("%s hit %d: the child was not killed: %v\n%s", point, hit, err, stdout.String())
			}
			acked := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
				if id, sum, ok := strings.Cut(line, " "); ok {
					acked[id] = sum
				}
			}
			a, err := Open(root, "lab")
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			if _, err := a.Scan(func(e Entry) error {
				switch e.State {
				case Temp:
					if _, ok := acked[e.ID]; ok {
						t.Errorf("%s hit %d: an acknowledged file is still being written", point, hit)
					}
					return a.RemoveTemp(e)
				case Landed:
					found[e.ID] = true
					if want, ok := acked[e.ID]; ok {
						f, _, err := a.Open(e.Path)
						if err != nil {
							return err
						}
						h := sha256.New()
						_, _ = io.Copy(h, f)
						_ = f.Close()
						if hex.EncodeToString(h.Sum(nil)) != want {
							t.Errorf("%s hit %d: file %s is not whole", point, hit, e.ID)
						}
					}
				default:
					t.Errorf("%s hit %d: a file is recorded that nothing recorded", point, hit)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for id := range acked {
				if !found[id] {
					t.Errorf("%s hit %d: acknowledged file %s is lost", point, hit, id)
				}
			}
			if len(acked) != hit-1 {
				t.Errorf("%s hit %d: %d files acknowledged, want %d", point, hit, len(acked), hit-1)
			}
			_ = a.Close()
		}
	}
	t.Logf("%d rounds, each killed at a durable step of a landing", rounds)
}
