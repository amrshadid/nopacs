package dir

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestRemoveEmptyDirs guards that only empty directories go: under a
// purged scan's directory every empty one is removed, then each one above
// it left empty, up to but not the top level; a directory still holding an
// object stays with the object, and one beside it is not touched; a Put
// into a removed directory makes it again; the storage's own directories
// are refused.
func TestRemoveEmptyDirs(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for _, k := range []string{"data/sub/ses/a/1.dcm", "data/sub/ses/b/2.dcm", "data/other/ses/c/3.dcm"} {
		if _, err := s.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"data/sub/ses/a/1.dcm"} {
		if err := s.Delete(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	root := s.root.Name()
	if err := os.MkdirAll(filepath.Join(root, "data", "sub", "ses", "a", "deeper", "still"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveEmptyDirs(ctx, "data/sub/ses/a"); err != nil {
		t.Fatal(err)
	}
	exists := func(rel string) bool {
		_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	if exists("data/sub/ses/a") {
		t.Fatal("the emptied scan's directory stays")
	}
	if !exists("data/sub/ses/b/2.dcm") || !exists("data/sub/ses") {
		t.Fatal("a directory still holding an object went")
	}
	if err := s.Delete(ctx, "data/sub/ses/b/2.dcm"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveEmptyDirs(ctx, "data/sub/ses/b"); err != nil {
		t.Fatal(err)
	}
	if exists("data/sub") || !exists("data") || !exists("data/other/ses/c/3.dcm") {
		t.Fatal("the subject's directories were not removed up to the top level alone")
	}
	if _, err := s.Put(ctx, "data/sub/ses/b/4.dcm", strings.NewReader("y")); err != nil {
		t.Fatalf("a Put into a removed directory: %v", err)
	}
	if !exists("data/sub/ses/b/4.dcm") {
		t.Fatal("the Put's object is not there")
	}
	if err := s.RemoveEmptyDirs(ctx, tempDir); fault.KindOf(err) != fault.Invalid {
		t.Fatalf("pruning the storage's own directory answered %v, want Invalid", err)
	}
}
