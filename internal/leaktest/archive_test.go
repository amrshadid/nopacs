package leaktest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkArchive requires every file the de-identifying receivers archived to
// hold none of the originals the profile removes, and returns the needles
// with those the archive legitimately keeps (a date, a UID) left out: what
// the archive holds is not a surface.
func (s *site) checkArchive(t *testing.T, w *world, all []seedFile, needles []needle) []needle {
	t.Helper()
	kept := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(s.roots, w.tenant, "main"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 G122 -- the test's own storage.
		if err != nil {
			return err
		}
		low := strings.ToLower(string(b))
		for _, n := range needles {
			if strings.Contains(low, n.form) {
				if mustNotBeArchived[n.value] {
					t.Fatalf("the archive holds the original %q (as %q) in %s: the receiver did not de-identify", n.value, n.form, p)
				}
				kept[n.value] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) > 0 {
		t.Logf("the de-identified archive keeps %v: not searched for", keysOf(boolInts(kept)))
	}
	var out []needle
	for _, n := range needles {
		if !kept[n.value] {
			out = append(out, n)
		}
	}
	return out
}

func boolInts(m map[string]bool) map[string]int {
	out := map[string]int{}
	for k := range m {
		out[k] = 1
	}
	return out
}

// control shows the search finds what is there: the storage the receiver
// that keeps files as received wrote holds the originals, and the search
// finds them. Nothing of it is a surface.
func control(t *testing.T, s *site, w *world, needles []needle) {
	t.Helper()
	found := 0
	err := filepath.WalkDir(filepath.Join(s.roots, w.tenant, "raw"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 G122 -- the test's own storage.
		if err != nil {
			return err
		}
		found += len(search([]surface{{label: "control", text: string(b)}}, needles))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("the control found no original in the storage of the receiver that keeps files as received: the search proves nothing")
	}
}

// addLog records the server's log by line: a line that carries a request or
// an answer in full (written at the most detailed level) apart from the
// others, and, of those, the lines written before the keys' calls began (the
// setup's, made as the local operator) apart from the keys'.
func addLog(rec *recorder, log string, keysFrom int) {
	off := 0
	var plain, setup, keys strings.Builder
	for _, line := range strings.SplitAfter(log, "\n") {
		switch {
		case !strings.Contains(line, `"msg":"message received"`) && !strings.Contains(line, `"msg":"message sent"`):
			plain.WriteString(line)
		case off < keysFrom:
			setup.WriteString(line)
		default:
			keys.WriteString(line)
		}
		off += len(line)
	}
	rec.add("log", "server", plain.String())
	rec.add("log messages in setup", "server", setup.String())
	rec.add("log messages of keys", "server", keys.String())
}
