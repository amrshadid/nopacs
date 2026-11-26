package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The identifiers planted in the corpus file for the seeded-identifier
// test of the command line: made up, a name and a Patient ID of the length
// of the values they replace, and a birth date the file did not have.
const (
	plantedName  = "Qvrnx^Yla"
	plantedID    = "ZK4471QX"
	plantedBirth = "19370521"
)

// plantedFile returns the corpus file ct5n-2062.dcm with the planted
// identifiers: Patient's Name and Patient ID replaced, and a Patient's Birth
// Date written into its empty element (Explicit VR Little Endian, no group
// length in group 0010).
func plantedFile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "receive", "testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte("Doe^Peter"), []byte(plantedName), 1)
	b = bytes.Replace(b, []byte("98890234"), []byte(plantedID), 1)
	emptyBirth := []byte{0x10, 0x00, 0x30, 0x00, 'D', 'A', 0x00, 0x00}
	if !bytes.Contains(b, []byte(plantedName)) || !bytes.Contains(b, []byte(plantedID)) || bytes.Count(b, emptyBirth) != 1 {
		t.Fatal("the corpus file is not the one the planting expects")
	}
	return bytes.Replace(b, emptyBirth, append([]byte{0x10, 0x00, 0x30, 0x00, 'D', 'A', 0x08, 0x00}, plantedBirth...), 1)
}

// plantedIn reports which planted identifier text holds, or "".
func plantedIn(text string) string {
	for _, p := range []string{plantedName, plantedID, plantedBirth, "Qvrnx", "ZK4471"} {
		if strings.Contains(text, p) {
			return p
		}
	}
	return ""
}

// TestPlantedIdentifiersAreNotPrinted is the seeded-identifier test of the
// command line: a trial of a file with planted identifiers prints none of
// them, in its table or its --json, on standard output or standard error,
// unless --show-values asks; with it, they are printed, so the search is
// shown to find what is there.
func TestPlantedIdentifiersAreNotPrinted(t *testing.T) {
	dir := t.TempDir()
	dcm := writeFile(t, dir, "planted.dcm", string(plantedFile(t)))
	doc := writeFile(t, dir, "basic.json", basicJSON)
	none := map[string]string{}
	for _, args := range [][]string{
		{"profile", "try", "--local", "--document", doc, dcm},
		{"profile", "try", "--local", "--document", doc, dcm, "--json"},
		{"profile", "try", "--local", "--document", doc, dcm, "--changed-only"},
	} {
		res := runWith(none, args...)
		if res.code != 0 {
			t.Fatalf("%v: %+v", args, res)
		}
		if p := plantedIn(res.stdout + res.stderr); p != "" {
			t.Errorf("%v printed the planted %q:\n%s", args, p, res.stdout)
		}
	}
	shown := runWith(none, "profile", "try", "--local", "--document", doc, dcm, "--show-values")
	if shown.code != 0 || !strings.Contains(shown.stdout, plantedName) || !strings.Contains(shown.stdout, plantedID) {
		t.Errorf("--show-values printed no planted identifier:\n%s", shown.stdout)
	}
}
