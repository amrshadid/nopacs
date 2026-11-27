package download

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestReadRefsAcceptsTheListFormat guards the one input form of a chosen
// set: one reference a line, comments and blank lines ignored, a bad line
// refused by its number, nothing guessed.
func TestReadRefsAcceptsTheListFormat(t *testing.T) {
	in := "# a set\n\nsub-a\n  sub-b/ses-1  \nsub-c/ses-2/scan-9\r\n#sub-x\n"
	refs, err := ReadRefs(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range refs {
		got = append(got, refText(r))
	}
	if strings.Join(got, ",") != "sub-a,sub-b/ses-1,sub-c/ses-2/scan-9" {
		t.Errorf("read %q", got)
	}
	for in, want := range map[string]string{
		"sub-a\n../x\n":           "line 2",
		"a/b/c/d\n":               "line 1",
		"sub-a//scan\n":           "a part is empty",
		"sub-a/ses\x00\n":         "not a label",
		"sub a\n":                 "not a label",
		strings.Repeat("a", 2000): "line 1",
	} {
		_, err := ReadRefs(strings.NewReader(in))
		if err == nil || !strings.Contains(fault.Message(err)+err.Error(), want) {
			t.Errorf("%q: error %v, want one saying %q", in, err, want)
		}
		if err != nil && fault.KindOf(err) != fault.Invalid {
			t.Errorf("%q: kind %v, want invalid", in, fault.KindOf(err))
		}
	}
}

// TestSelectionSavesAtomicallyAndResumes guards the selection file: what is
// recorded is read back, a file as recorded is vouched for without being
// read, a file whose size or time differs is not, the file is replaced with
// no half-written state left behind, and a file of another tenant, layout or
// format is refused instead of overwritten.
func TestSelectionSavesAtomicallyAndResumes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sel", "selection.json")
	data := filepath.Join(dir, "a.dcm")
	if err := os.WriteFile(data, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(data)
	sum := sha256.Sum256([]byte("hello"))
	sha := hex.EncodeToString(sum[:])

	s, err := loadSelection(path, "lab1", "", []string{"sub-a", "sub-b/ses-1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.vouches("a.dcm", sha, 5, st) {
		t.Error("an empty selection vouched for a file")
	}
	s.record("a.dcm", sha, 5, st)
	s.record("gone.dcm", sha, 5, st)
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*"))
	if len(left) != 0 {
		t.Errorf("a temporary file was left beside the selection: %v", left)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 != 0 && !isWindows() {
		t.Errorf("the selection file is not private: %v %v", info, err)
	}

	back, err := loadSelection(path, "lab1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !back.vouches("a.dcm", sha, 5, st) {
		t.Error("a recorded file in its recorded state was not vouched for")
	}
	if back.vouches("a.dcm", strings.Repeat("0", 64), 5, st) {
		t.Error("a file was vouched for under another checksum than the announced one")
	}
	if back.vouches("a.dcm", sha, 6, st) {
		t.Error("a file was vouched for under another length")
	}
	if err := os.WriteFile(data, []byte("HELLO"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := st.ModTime().Add(time.Hour)
	if err := os.Chtimes(data, later, later); err != nil {
		t.Fatal(err)
	}
	st2, _ := os.Stat(data)
	if back.vouches("a.dcm", sha, 5, st2) {
		t.Error("a file changed on disk (same size, later time) was vouched for")
	}
	back.prune(map[string]bool{"a.dcm": true})
	if got := back.entries(); len(got) != 1 || got[0].Path != "a.dcm" {
		t.Errorf("after pruning the entries are %v, want only a.dcm", got)
	}

	tenant, layout, refs, found, err := SelectionRefs(path)
	if err != nil || !found || tenant != "lab1" || layout != "" || len(refs) != 2 || refText(refs[1]) != "sub-b/ses-1" {
		t.Errorf("SelectionRefs: %q %q %v %v %v", tenant, layout, refs, found, err)
	}
	if _, _, _, found, err := SelectionRefs(filepath.Join(dir, "none.json")); found || err != nil {
		t.Errorf("a missing file: found %v, error %v", found, err)
	}

	if _, err := loadSelection(path, "lab2", "", nil); err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Errorf("another tenant: %v", err)
	}
	if _, err := loadSelection(path, "lab1", LayoutBagIt, nil); err == nil || !strings.Contains(err.Error(), "layout") {
		t.Errorf("another layout: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	for _, content := range []string{"not json", `{"format":"something-else/9","tenant":"lab1"}`, ""} {
		if err := os.WriteFile(bad, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSelection(bad, "lab1", "", nil); err == nil || !strings.Contains(err.Error(), "not a selection file") {
			t.Errorf("%q: %v, want a refusal", content, err)
		}
		if b, _ := os.ReadFile(bad); string(b) != content {
			t.Errorf("a refused selection file was changed: %q", b)
		}
	}
}

// isWindows says whether mode bits mean nothing here.
func isWindows() bool { return filepath.Separator == '\\' }

// TestWriteBagMatchesRFC8493 guards the bag's files: the version and encoding
// of bagit.txt, the payload sum in bag-info.txt, one manifest line per file in
// path order with the checksum then two spaces then the path, and a tag
// manifest whose checksums are those of the tag files as written.
func TestWriteBagMatchesRFC8493(t *testing.T) {
	dir := t.TempDir()
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	files := []BagFile{
		{Path: "data/sub-b/ses-1/scan-1/2.dcm", SHA256: sum("two"), Bytes: 3},
		{Path: "data/sub-a/ses-1/scan-1/1.dcm", SHA256: sum("one!"), Bytes: 4},
		{Path: "data/sub-a/odd%name\nx.dcm", SHA256: sum("x"), Bytes: 1},
	}
	when := time.Date(2026, 11, 27, 23, 59, 0, 0, time.UTC)
	if err := writeBag(dir, files, bagInfo{date: when, description: "line one\nline two"}); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read("bagit.txt"); got != "BagIt-Version: 1.0\nTag-File-Character-Encoding: UTF-8\n" {
		t.Errorf("bagit.txt:\n%s", got)
	}
	info := read("bag-info.txt")
	for _, want := range []string{"Bagging-Date: 2026-11-27\n", "Payload-Oxum: 8.3\n", "Bag-Software-Agent: nopacs\n", "External-Description: line one line two\n"} {
		if !strings.Contains(info, want) {
			t.Errorf("bag-info.txt lacks %q:\n%s", want, info)
		}
	}
	wantManifest := sum("x") + "  data/sub-a/odd%25name%0Ax.dcm\n" +
		sum("one!") + "  data/sub-a/ses-1/scan-1/1.dcm\n" +
		sum("two") + "  data/sub-b/ses-1/scan-1/2.dcm\n"
	if got := read("manifest-sha256.txt"); got != wantManifest {
		t.Errorf("manifest-sha256.txt:\n%s\nwant\n%s", got, wantManifest)
	}
	wantTags := sum(read("bag-info.txt")) + "  bag-info.txt\n" + sum(read("bagit.txt")) + "  bagit.txt\n" + sum(read("manifest-sha256.txt")) + "  manifest-sha256.txt\n"
	if got := read("tagmanifest-sha256.txt"); got != wantTags {
		t.Errorf("tagmanifest-sha256.txt:\n%s\nwant\n%s", got, wantTags)
	}
	if strings.Contains(read("tagmanifest-sha256.txt"), "tagmanifest") {
		t.Error("the tag manifest lists itself")
	}
	if err := writeBag(dir, []BagFile{{Path: "elsewhere/x.dcm"}}, bagInfo{date: when}); err == nil {
		t.Error("a payload file outside data/ was accepted")
	}
	// The same input gives the same bytes.
	again := t.TempDir()
	if err := writeBag(again, files, bagInfo{date: when, description: "line one\nline two"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bagit.txt", "bag-info.txt", "manifest-sha256.txt", "tagmanifest-sha256.txt"} {
		b, _ := os.ReadFile(filepath.Join(again, name))
		if !bytes.Equal(b, []byte(read(name))) {
			t.Errorf("%s differs between two writes of the same bag", name)
		}
	}
}
