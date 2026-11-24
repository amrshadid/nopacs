package secret

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestOpenPrivate guards OpenPrivate: a file only its owner can open is
// returned open and readable; a file that others can open is refused as
// invalid with the way to fix it and none of its content; a missing path is
// not found; a directory is refused. It uses the same private file as
// LoadKeyFile's tests (writePrivate), and for the wide one a file written the
// ordinary way, which is open to others on Linux and macOS (mode 0644) and
// inherits the directory's wider access list on Windows.
func TestOpenPrivate(t *testing.T) {
	dir := privateDir(t)

	private := filepath.Join(dir, "private")
	writePrivate(t, private, []byte("a credential\n"))
	f, err := OpenPrivate(private)
	if err != nil {
		t.Fatalf("OpenPrivate of a private file: %v", err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(got) != "a credential\n" {
		t.Errorf("read %q, %v", got, err)
	}

	wide := filepath.Join(dir, "wide")
	if err := os.WriteFile(wide, []byte("content-that-must-not-be-quoted"), 0o644); err != nil { //nolint:gosec // G306: the mode is the point of the test.
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o644); err != nil { //nolint:gosec // G302: the mode is the point of the test.
		t.Fatal(err)
	}
	f, err = OpenPrivate(wide)
	if err == nil {
		_ = f.Close()
		t.Fatal("a file other users can open was accepted")
	}
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(err.Error(), "make it private to") {
		t.Errorf("refusal: kind %v, %v; want invalid with the fix", fault.KindOf(err), err)
	}
	if strings.Contains(err.Error(), "content-that-must-not-be-quoted") {
		t.Errorf("the refusal quotes the file: %v", err)
	}

	if _, err := OpenPrivate(filepath.Join(dir, "missing")); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a missing file: %v, want not found", err)
	}
	if f, err := OpenPrivate(dir); err == nil {
		_ = f.Close()
		t.Error("a directory was accepted")
	}
}
