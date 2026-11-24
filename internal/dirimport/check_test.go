package dirimport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCheckJob guards what an import job is checked for before it is made:
// a job over a directory with a map that parses passes; a path that is not
// there, a file, or a link, a map file missing or naming two values for one
// key, a map with no configuration, a job's map configuration filling
// another part than the receiver's, a key part the layout lacks, and a
// receiver of another type are refused, and no refusal's message holds the
// path, the map file's name or a cell of the map.
func TestCheckJob(t *testing.T) {
	tree := t.TempDir()
	for _, d := range []string{"proj1/sessA/ct2", "private-folder"} {
		if err := os.MkdirAll(filepath.Join(tree, filepath.FromSlash(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		if err := os.WriteFile(filepath.Join(tree, filepath.FromSlash(p)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("proj1/afile", "x")
	write("proj1/map.csv", testMap)
	write("proj1/twice.csv", "project,label,subject_label\nproj1,sessA,Secret-Subject\nproj1,sessA,Other-Subject\n")
	if err := os.Symlink(filepath.Join(tree, "proj1", "sessA"), filepath.Join(tree, "proj1", "link")); err != nil {
		t.Fatal(err)
	}
	roots := treeRoot{path: tree}
	rcv := testReceiver()
	ok := tenant.ImportJob{Receiver: rcv.Name, Path: "proj1", MapCSV: []byte(testMap)}
	if kept, err := CheckJob(t.Context(), roots, "lab", nil, rcv, ok); err != nil || string(kept) != testMap {
		t.Fatalf("a job that can run: %q, %v", kept, err)
	}
	fromFile := ok
	fromFile.MapCSV, fromFile.MapFile = nil, "proj1/map.csv"
	if kept, err := CheckJob(t.Context(), roots, "lab", nil, rcv, fromFile); err != nil || string(kept) != testMap {
		t.Fatalf("a job with its map in a file keeps the file as read: %q, %v", kept, err)
	}
	fromReceiver := rcv
	fromReceiver.Import.MapFile = "proj1/map.csv"
	if kept, err := CheckJob(t.Context(), roots, "lab", nil, fromReceiver, tenant.ImportJob{Receiver: rcv.Name, Path: "proj1"}); err != nil || string(kept) != testMap {
		t.Fatalf("a job using the receiver's map file keeps it as read: %q, %v", kept, err)
	}
	noMap := rcv
	noMap.Import.Map, noMap.Naming = tenant.ImportMap{}, nil
	other := rcv
	other.Type = tenant.ReceiverSTOW
	cases := []struct {
		name string
		rcv  tenant.Receiver
		job  func(j *tenant.ImportJob)
		kind fault.Kind
	}{
		{"a path not there", rcv, func(j *tenant.ImportJob) { j.Path = "private-folder/none" }, fault.Precondition},
		{"a file", rcv, func(j *tenant.ImportJob) { j.Path = "proj1/afile" }, fault.Precondition},
		{"a link", rcv, func(j *tenant.ImportJob) { j.Path = "proj1/link" }, fault.Precondition},
		{"a map file not there", rcv, func(j *tenant.ImportJob) { j.MapCSV, j.MapFile = nil, "private-folder/map.csv" }, fault.Precondition},
		{"a map with two values for a key", rcv, func(j *tenant.ImportJob) { j.MapCSV, j.MapFile = nil, "proj1/twice.csv" }, fault.Precondition},
		{"a map with no configuration", noMap, func(*tenant.ImportJob) {}, fault.Precondition},
		{"another part filled", rcv, func(j *tenant.ImportJob) {
			j.Map = tenant.ImportMap{KeyColumns: []string{"label"}, ValueColumn: "subject_label", KeyParts: []string{"session"}, Gives: "visit"}
		}, fault.Precondition},
		{"a key part the layout lacks", rcv, func(j *tenant.ImportJob) {
			j.Map = tenant.ImportMap{KeyColumns: []string{"label"}, ValueColumn: "subject_label", KeyParts: []string{"site"}, Gives: "subject"}
		}, fault.Precondition},
		{"not an import receiver", other, func(*tenant.ImportJob) {}, fault.Precondition},
	}
	for _, c := range cases {
		j := ok
		c.job(&j)
		_, err := CheckJob(t.Context(), roots, "lab", nil, c.rcv, j)
		if fault.KindOf(err) != c.kind {
			t.Errorf("%s: %v, want %v", c.name, err, c.kind)
			continue
		}
		for _, secret := range []string{"private-folder", "afile", "twice", "Secret-Subject", tree} {
			if strings.Contains(fault.Message(err), secret) {
				t.Errorf("%s: the message holds %q: %s", c.name, secret, fault.Message(err))
			}
		}
	}
}

// TestCheckJobMapFileBound guards the bound of a map read from a file: a
// map file past the 3 MiB a job keeps is refused, naming the bound, before
// it is read, so a 64 MiB file costs the check no more than a few MiB of
// memory; a file of exactly 3 MiB is read (and then refused only for what
// it holds).
func TestCheckJobMapFileBound(t *testing.T) {
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "proj1"), 0o700); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(tree, "proj1", "big.csv")
	if err := os.WriteFile(big, []byte(testMap), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sparse: 64 MiB to read, nothing written to the disk.
	if err := os.Truncate(big, 64<<20); err != nil {
		t.Fatal(err)
	}
	rcv := testReceiver()
	job := tenant.ImportJob{Receiver: rcv.Name, Path: "proj1", MapFile: "proj1/big.csv"}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := CheckJob(t.Context(), treeRoot{path: tree}, "lab", nil, rcv, job)
	runtime.ReadMemStats(&after)
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "3 MiB") {
		t.Fatalf("a 64 MiB map file: %v, want a precondition naming 3 MiB", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4<<20 {
		t.Errorf("the check of a 64 MiB map file allocated %d bytes, want under 4 MiB", grew)
	}
	if err := os.Truncate(big, tenant.MaxImportMapCSV); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckJob(t.Context(), treeRoot{path: tree}, "lab", nil, rcv, job); err == nil || strings.Contains(fault.Message(err), "3 MiB") {
		t.Errorf("a map file of exactly 3 MiB is read and refused for what it holds: %v", err)
	}
}

// TestCheckJobRootSwapped guards the identity of the root opened: a root
// directory renamed away and another made in its place between its check
// and its opening is refused, not read under the rules checked of the
// first.
func TestCheckJobRootSwapped(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "proj1"), 0o700); err != nil {
		t.Fatal(err)
	}
	rcv := testReceiver()
	job := tenant.ImportJob{Receiver: rcv.Name, Path: "proj1", MapCSV: []byte(testMap)}
	if _, err := checkJobBounded(t.Context(), treeRoot{path: tree}, "lab", nil, rcv, job, checkSeams{}); err != nil {
		t.Fatalf("a root left alone: %v", err)
	}
	swap := checkSeams{opened: func(dir string) {
		if err := os.Rename(dir, filepath.Join(base, "moved")); err != nil {
			t.Error(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "proj1"), 0o700); err != nil {
			t.Error(err)
		}
	}}
	_, err := checkJobBounded(t.Context(), treeRoot{path: tree}, "lab", nil, rcv, job, swap)
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "changed while it was opened") {
		t.Errorf("a root swapped before its opening: %v", err)
	}
}

// stallingRoots is a file system that does not answer: Root blocks, deaf to
// its context as a call into a hung mount is, until release is closed.
type stallingRoots struct{ release chan struct{} }

// Root blocks until release is closed, then answers no root.
func (r stallingRoots) Root(context.Context, string, tenant.Store, string) (string, error) {
	<-r.release
	return "", errors.New("released")
}

// TestCheckJobBounded guards the bound on a check whose file system does
// not answer: CheckJob returns an Unavailable failure, naming the root and
// no path, once its context ends, though the call into the file system has
// not returned.
func TestCheckJobBounded(t *testing.T) {
	roots := stallingRoots{release: make(chan struct{})}
	defer close(roots.release)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := CheckJob(ctx, roots, "lab", nil, testReceiver(), tenant.ImportJob{Receiver: "r", Path: "proj1", MapCSV: []byte(testMap)})
	if fault.KindOf(err) != fault.Unavailable || !strings.Contains(fault.Message(err), "did not answer in time") {
		t.Errorf("a check whose file system does not answer: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the check returned after %v, want soon after its context ended", took)
	}
}
