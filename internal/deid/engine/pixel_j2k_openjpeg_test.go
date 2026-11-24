package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/compress"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"
)

// The variables of the outside check of the JPEG 2000 walk.
const (
	// openJPEGEnv names the directory holding OpenJPEG's opj_compress,
	// opj_decompress and opj_dump.
	openJPEGEnv = "NOPACS_CHECK_J2K_OPENJPEG"
	// j2kScript has pydicom decode each file the check de-identifies,
	// from this package's directory.
	j2kScript = "../../../scripts/check/deid-j2k-pydicom.py"
)

// openJPEG runs one of OpenJPEG's programs and returns what it printed.
func openJPEG(t *testing.T, dir, program string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), filepath.Join(dir, program), args...) //nolint:gosec // G204: the program is OpenJPEG's, from the directory the test runner names.
	return cmd.CombinedOutput()
}

// opjDecode has opj_decompress decode a codestream to its samples, a PGX
// file a component (which keeps each component's own size and depth), and
// returns them one after the other, or nil and what it printed when it
// cannot.
func opjDecode(t *testing.T, dir, work, name string, f []byte) ([]byte, []byte) {
	t.Helper()
	in := filepath.Join(work, name+".j2k")
	if err := os.WriteFile(in, f, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := openJPEG(t, dir, "opj_decompress", "-i", in, "-o", filepath.Join(work, name+".pgx"))
	if err != nil || bytes.Contains(report, []byte("ERROR")) {
		return nil, report
	}
	var out []byte
	for c := 0; ; c++ {
		b, err := os.ReadFile(filepath.Join(work, fmt.Sprintf("%s_%d.pgx", name, c))) //nolint:gosec // G304: the test's own temporary file.
		if err != nil {
			break
		}
		out = append(out, b...)
	}
	if len(out) == 0 {
		return nil, report
	}
	return out, nil
}

// j2kEncodings are opj_compress's options for the codestreams the check
// writes, each with a comment holding a name: tiles and tile-parts with
// PLT and TLM; SOP and EPH with precincts in RPCL order; a progression
// change; an upshifted component; irreversible and lossy; no
// decomposition at all.
var j2kEncodings = [][]string{
	{},
	{"-t", "16,16", "-n", "3", "-TP", "R", "-PLT", "-TLM"},
	{"-SOP", "-EPH", "-p", "RPCL", "-n", "3", "-c", "[16,16],[16,16]"},
	{"-n", "3", "-POC", "T1=0,0,1,3,1,CPRL"},
	{"-ROI", "c=0,U=3"},
	{"-I", "-r", "10"},
	{"-n", "1"},
	{"-t", "24,20", "-n", "3", "-TP", "L", "-r", "20,10,1", "-TLM"},
}

// TestJ2KRewriteDecodesAlikeWithOpenJPEG is the outside check of the JPEG
// 2000 walk, run with openJPEGEnv set (and pythonEnv for pydicom). Over
// the codestreams of j2kTestdata, the frames of the JPEG 2000 and HTJ2K
// files (pydicom's corpus with corpusEnv set), and codestreams
// opj_compress writes with a name in a comment: a comment holding a name
// is planted in the main header and in every tile-part header (Psot and
// TLM raised), which OpenJPEG decodes as the codestream before; the walk
// removes every comment, and OpenJPEG decodes the result to the very
// samples of the codestream received and opj_dump reads its header. The
// images opj_compress writes are then wrapped as DICOM files and
// de-identified by the engine; pydicom decodes each result to the
// samples it decodes the file received to, and finds no name in it.
func TestJ2KRewriteDecodesAlikeWithOpenJPEG(t *testing.T) {
	dir := os.Getenv(openJPEGEnv)
	if dir == "" {
		t.Skipf("%s is not set", openJPEGEnv)
	}
	work := t.TempDir()
	streams := j2kCodestreams(t)
	paths, _ := filepath.Glob(filepath.Join(j2kTestdata, "*.dcm"))
	paths = append(paths, filepath.Join(receiveTestdata, "MR_small_jp2klossless.dcm"))
	if c := os.Getenv(corpusEnv); c != "" {
		paths = append(paths, corpusFiles(t, c)...)
	}
	for _, path := range paths {
		ds, err := readPath(t, path)
		if err != nil || !j2kSyntax(ds.TransferSyntaxUID()) {
			continue
		}
		if _, held := heldJ2KCorpus[filepath.Base(path)]; held {
			continue
		}
		streams[filepath.Base(path)] = oneFrame(t, ds)
	}
	const rows, cols = 48, 64
	pgm := fmt.Appendf(nil, "P5\n%d %d\n255\n", cols, rows)
	for y := range rows {
		for x := range cols {
			pgm = append(pgm, byte((x*3+y*5+(x*y)%7)%256))
		}
	}
	src := filepath.Join(work, "image.pgm")
	if err := os.WriteFile(src, pgm, 0o600); err != nil {
		t.Fatal(err)
	}
	var written [][]byte
	for i, opts := range j2kEncodings {
		out := filepath.Join(work, fmt.Sprintf("opj-%d.j2k", i))
		args := append([]string{"-i", src, "-o", out, "-C", j2kName}, opts...)
		if report, err := openJPEG(t, dir, "opj_compress", args...); err != nil {
			t.Fatalf("opj_compress %s: %v\n%s", strings.Join(opts, " "), err, report)
		}
		b, err := os.ReadFile(out) //nolint:gosec // G304: the test's own temporary file.
		if err != nil || !bytes.Contains(b, []byte("DOE^JOHN")) {
			t.Fatalf("opj_compress %s wrote no comment: %v", strings.Join(opts, " "), err)
		}
		streams[filepath.Base(out)] = b
		written = append(written, b)
	}
	decodedAlike := 0
	for name, f := range streams {
		in := planted(t, f, j2kName)
		want, report := opjDecode(t, dir, work, name+"-received", f)
		if want == nil {
			// OpenJPEG cannot decode it as received (an HT codestream of
			// several quality layers): the walk must still take it.
			t.Logf("%s: OpenJPEG does not decode it as received: %s", name, lastLine(report))
			if _, _, ok := cleanJ2K(in); !ok {
				t.Errorf("%s: held", name)
			}
			continue
		}
		if got, _ := opjDecode(t, dir, work, name+"-planted", in); !bytes.Equal(got, want) {
			t.Fatalf("%s: OpenJPEG decodes the planted codestream otherwise; the check is not sound", name)
		}
		decodedAlike++
		for which, frame := range map[string][]byte{"received": f, "planted": in} {
			out, removed, ok := cleanJ2K(frame)
			if !ok {
				t.Errorf("%s %s: held", name, which)
				continue
			}
			if bytes.Contains(out, []byte("DOE^JOHN")) || (which == "planted" && len(removed) < 2) {
				t.Errorf("%s %s: %v removed, the name left: %v", name, which, removed, bytes.Contains(out, []byte("DOE^JOHN")))
			}
			if got, _ := opjDecode(t, dir, work, name+"-"+which+"-cleaned", out); !bytes.Equal(got, want) {
				t.Errorf("%s %s: OpenJPEG decodes the cleaned codestream otherwise", name, which)
			}
			if report, err := openJPEG(t, dir, "opj_dump", "-i", filepath.Join(work, name+"-"+which+"-cleaned.j2k")); err != nil ||
				bytes.Contains(report, []byte("ERROR")) || !bytes.Contains(report, []byte("Codestream info from main header")) {
				t.Errorf("%s %s: opj_dump: %v\n%s", name, which, err, report)
			}
		}
	}
	t.Logf("%d of %d codestreams decoded alike by OpenJPEG, received, planted and cleaned", decodedAlike, len(streams))
	if decodedAlike < len(streams)-1 {
		t.Errorf("OpenJPEG decoded only %d of %d codestreams as received", decodedAlike, len(streams))
	}

	py, asked := os.LookupEnv(pythonEnv)
	if !asked || py == "" {
		return
	}
	en := pixelEngine(t, nil)
	var pairs []map[string]string
	for i, f := range written {
		syntax := compress.JPEG2000Lossless
		if strings.Contains(strings.Join(j2kEncodings[i], " "), "-r") {
			syntax = compress.JPEG2000Lossy
		}
		ds := j2kFile(t, rows, cols, syntax, planted(t, f, j2kName))
		input := filepath.Join(work, fmt.Sprintf("in-%d.dcm", i))
		if err := os.WriteFile(input, writeFile(t, ds), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := apply(t, en, ds)
		if rec.Pixels == nil || len(rec.Pixels.SegmentsRemoved) < 2 {
			t.Errorf("file %d: pixel work %+v", i, rec.Pixels)
		}
		output := filepath.Join(work, fmt.Sprintf("out-%d.dcm", i))
		if err := os.WriteFile(output, writeFile(t, ds), 0o600); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, map[string]string{"input": input, "output": output})
	}
	spec, err := json.Marshal(map[string]any{"name": "DOE^JOHN", "files": pairs})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(work, "j2k.json")
	if err := os.WriteFile(manifest, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), py, "-I", j2kScript, manifest) //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
	report, err := cmd.CombinedOutput()
	t.Logf("pydicom: %s", report)
	if err != nil {
		t.Fatalf("pydicom: %v", err)
	}
	if !bytes.Contains(report, fmt.Appendf(nil, "%d files decoded alike", len(pairs))) {
		t.Errorf("pydicom did not decode every file: %s", report)
	}
}

// TestJ2KFreeWordsDecodeAlikeWithOpenJPEG is the outside check of the
// removal of CPF and CRG, run with openJPEGEnv set: over the codestreams
// of j2kTestdata OpenJPEG decodes, a CPF and a CRG whose words spell a
// name are planted after SIZ, which OpenJPEG decodes to the samples of the
// codestream before; the walk removes both, the name is gone, and OpenJPEG
// decodes the result to those samples again.
func TestJ2KFreeWordsDecodeAlikeWithOpenJPEG(t *testing.T) {
	dir := os.Getenv(openJPEGEnv)
	if dir == "" {
		t.Skipf("%s is not set", openJPEGEnv)
	}
	work := t.TempDir()
	checked := 0
	for name, f := range j2kCodestreams(t) {
		want, _ := opjDecode(t, dir, work, name+"-received", f)
		if want == nil {
			continue
		}
		in := withFreeWords(f)
		if got, report := opjDecode(t, dir, work, name+"-planted", in); !bytes.Equal(got, want) {
			t.Fatalf("%s: OpenJPEG decodes the planted codestream otherwise; the check is not sound: %s", name, lastLine(report))
		}
		out, removed, ok := cleanJ2K(in)
		if !ok || bytes.Contains(out, []byte("DOE^")) || !slices.Contains(removed, "CPF") || !slices.Contains(removed, "CRG") {
			t.Errorf("%s: taken %v, removed %v, the name left %v", name, ok, removed, bytes.Contains(out, []byte("DOE^")))
			continue
		}
		if got, report := opjDecode(t, dir, work, name+"-cleaned", out); !bytes.Equal(got, want) {
			t.Errorf("%s: OpenJPEG decodes the cleaned codestream otherwise: %s", name, lastLine(report))
		}
		checked++
	}
	t.Logf("%d codestreams decoded alike by OpenJPEG with CPF and CRG planted and removed", checked)
	if checked < 20 {
		t.Errorf("only %d codestreams checked", checked)
	}
}

// lastLine returns the last line of what a program printed that is not
// empty.
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// j2kFile returns an 8-bit MONOCHROME2 MR image of rows by cols whose one
// frame is the codestream f, in syntax.
func j2kFile(t *testing.T, rows, cols uint16, syntax string, f []byte) *dataset.Dataset {
	t.Helper()
	ds := image(t, "MR", "1.2.840.10008.5.1.4.1.1.4", "", rows, cols, 1)
	if len(f)%2 == 1 {
		f = append(f, 0)
	}
	value, err := compress.EncapsulateFrames([][]byte{f}, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := ds.Get(tag.Tag(tagPixelData))
	e.SetVR(dataelem.OB)
	if err := ds.SetValue(tag.Tag(tagPixelData), value); err != nil {
		t.Fatal(err)
	}
	ds.SetTransferSyntaxUID(syntax)
	return ds
}
