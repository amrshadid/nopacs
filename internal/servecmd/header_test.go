package servecmd_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// headerPython is the interpreter, with pydicom, the outside check of a held
// file's header runs; the check is left out when NOPACS_PYTHON is unset.
const headerPython = "NOPACS_PYTHON"

// pydicomHeaderScript prints the held file as pydicom's to_json_dict gives
// it, without Pixel Data and without a value over 1 KiB. It leaves out
// Specific Character Set, which the server rewrites to UTF-8 ("ISO_IR 192")
// because it writes every text value in UTF-8 (PS3.18 F.2.1), while pydicom
// keeps the file's own.
const pydicomHeaderScript = `
import base64, json, sys, pydicom

def short(d):
    out = {}
    for k, v in d.items():
        if k in ("7FE00010", "00080005") or "BulkDataURI" in v:
            continue
        if len(base64.b64decode(v.get("InlineBinary", ""))) > 1024:
            continue
        vals = v.get("Value")
        if vals is not None and len(json.dumps(vals)) > 1024:
            continue
        if v.get("vr") == "SQ":
            v = dict(v, Value=[short(i) for i in vals or []])
        out[k] = v
    return out

ds = pydicom.dcmread(sys.argv[1])
print(json.dumps(short(ds.to_json_dict())))
`

// heldDifferentFile makes a tenant with a stow receiver, stores a corpus
// file, then a file with the same SOP Instance UID and another patient name
// of the same length, which the pipeline holds as different_file with its
// bytes kept. It returns the site, the held file's id and the held bytes.
func heldDifferentFile(t *testing.T) (*stowSite, string, []byte) {
	t.Helper()
	s := newSTOWSite(t, 1<<20)
	first := testdata(t, ctStudy[0])
	if status, body := s.store(s.key, "studies/"+ctStudyUID, stowPart{raw: first}); status != http.StatusOK {
		t.Fatalf("the first file: %d %s", status, body)
	}
	if n := s.waitArchived(); n != 1 {
		t.Fatalf("archived %d, want 1", n)
	}
	changed := bytes.Replace(first, []byte("Doe^Peter"), []byte("Doe^Paula"), 1)
	if bytes.Equal(changed, first) {
		t.Fatal("the corpus file holds no patient name to change")
	}
	if status, body := s.store(s.key, "studies/"+ctStudyUID, stowPart{raw: changed}); status != http.StatusOK {
		t.Fatalf("the second file: %d %s", status, body)
	}
	return s, s.waitHeld("QUARANTINE_REASON_DIFFERENT_FILE"), changed
}

// waitHeld waits for a held file with the reason and returns its id.
func (s *stowSite) waitHeld(reason string) string {
	s.t.Helper()
	var list struct {
		Files []struct {
			ID          string `json:"id"`
			Reason      string `json:"reason"`
			ContentKept bool   `json:"contentKept"`
		} `json:"files"`
	}
	id := ""
	waitWithin(s.t, "the file held as "+reason, 30*time.Second, func() bool {
		_ = json.Unmarshal(post(s.t, s.hc, "/nopacs.v1.QuarantineService/ListQuarantinedFiles", `{"tenant":"`+s.tenant+`"}`), &list)
		for _, f := range list.Files {
			if f.Reason == reason && f.ContentKept {
				id = f.ID
			}
		}
		return id != ""
	})
	return id
}

// getHeader asks for a held file with its header and its values and
// returns the answer's header text and too-large flag.
func (s *stowSite) getHeader(id string) (text string, tooLarge bool) {
	s.t.Helper()
	var got struct {
		HeaderJSON     string `json:"headerJson"`
		HeaderTooLarge bool   `json:"headerTooLarge"`
	}
	body := post(s.t, s.hc, "/nopacs.v1.QuarantineService/GetQuarantinedFile", `{"tenant":"`+s.tenant+`","id":"`+id+`","includeHeader":true,"showValues":true}`)
	if err := json.Unmarshal(body, &got); err != nil {
		s.t.Fatalf("%v: %s", err, body)
	}
	return got.HeaderJSON, got.HeaderTooLarge
}

// TestAHeldFilesHeaderIsAnsweredByTheRealServer guards GetQuarantinedFile
// with include_header on the real server, which answered the record alone:
// the held file's header is returned as DICOM JSON without Pixel Data or a
// value over 1 KiB and equals pydicom's to_json_dict of the held bytes for
// every attribute it holds (checked when NOPACS_PYTHON names an interpreter
// with pydicom); a file whose bytes were removed by its rejection answers
// no header.
func TestAHeldFilesHeaderIsAnsweredByTheRealServer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process; left out under -short")
	}
	s, id, held := heldDifferentFile(t)
	text, tooLarge := s.getHeader(id)
	if text == "" || tooLarge {
		t.Fatalf("the header of a held file: %q, too large %v", text, tooLarge)
	}
	var ours map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &ours); err != nil {
		t.Fatal(err)
	}
	if _, ok := ours["7FE00010"]; ok {
		t.Error("the header holds Pixel Data")
	}
	if !bytes.Contains([]byte(text), []byte("Doe^Paula")) {
		t.Errorf("the header does not hold the held file's patient name: %s", text)
	}
	if py := os.Getenv(headerPython); py != "" {
		file := filepath.Join(t.TempDir(), "held.dcm")
		if err := os.WriteFile(file, held, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(t.Context(), py, "-I", "-c", pydicomHeaderScript, file).Output() //nolint:gosec // G702: the interpreter is the test runner's own NOPACS_PYTHON and the script a constant.
		if err != nil {
			t.Fatalf("pydicom: %v", err)
		}
		var want, got any
		if err := json.Unmarshal(out, &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatal(err)
		}
		delete(got.(map[string]any), "00080005")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the header differs from pydicom's:\n got %s\nwant %s", text, out)
		}
	}
	post(t, s.hc, "/nopacs.v1.QuarantineService/RejectQuarantinedFile", `{"tenant":"`+s.tenant+`","id":"`+id+`","reason":"a test"}`)
	if text, tooLarge := s.getHeader(id); text != "" || tooLarge {
		t.Errorf("a rejected file answered a header: %q, too large %v", text, tooLarge)
	}
}

// TestAHeaderPastTheReceiversLimitIsAnsweredWithoutOne guards a file held as
// header_too_large, whose bytes are kept but whose header cannot be read
// within the bound: its record is answered with neither a header nor the
// too-large flag, which is the contract's answer for a header that could not
// be read.
func TestAHeaderPastTheReceiversLimitIsAnsweredWithoutOne(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process; left out under -short")
	}
	s := newSTOWSite(t, 8<<20)
	post(t, s.hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"receiver.header-limit","scope":{"installation":{}},"value":"1MiB"}}`)
	// A private OB element of 1.5 MiB placed before Pixel Data (explicit VR
	// little endian, PS3.5 7.1.2) makes the header longer than the limit.
	base := testdata(t, ctStudy[0])
	at := bytes.Index(base, []byte{0xE0, 0x7F, 0x10, 0x00})
	if at < 0 {
		t.Fatal("the corpus file has no Pixel Data")
	}
	big := []byte{0x49, 0x00, 0x99, 0x10, 'O', 'B', 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(big[8:], 3<<19)
	big = append(big, make([]byte, 3<<19)...)
	long := append(append(append([]byte{}, base[:at]...), big...), base[at:]...)
	if status, body := s.store(s.key, "studies/"+ctStudyUID, stowPart{raw: long}); status != http.StatusOK {
		t.Fatalf("the file: %d %s", status, body)
	}
	id := s.waitHeld("QUARANTINE_REASON_HEADER_TOO_LARGE")
	if text, tooLarge := s.getHeader(id); text != "" || tooLarge {
		t.Errorf("a header past the limit was answered: %q, too large %v", text, tooLarge)
	}
}
