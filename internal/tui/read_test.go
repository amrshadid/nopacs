package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// seedReading puts one subject with one session and one scan of two
// instances in tenant t000: the first with a header, the second whose header
// is too large to show.
func seedReading(f *fakeserver.Server) []byte {
	addTenants(f, 1)
	data := bytes.Repeat([]byte("DICM"), 300)
	f.SetChunkSize(500)
	at := timestamppb.New(fixedNow.Add(-time.Hour))
	f.AddSubject("t000", &v1.Subject{Label: "sub-a", SessionCount: 1, InstanceCount: 2, SizeBytes: 2400, ArchiveTime: at})
	f.AddSession("t000", &v1.Session{SubjectLabel: "sub-a", Label: "ses-1", StudyDate: "20240105", Modalities: []string{"MR"}, ScanCount: 1, InstanceCount: 2, SizeBytes: 2400, ArchiveTime: at})
	seven := int32(7)
	f.AddScan("t000", &v1.Scan{SubjectLabel: "sub-a", SessionLabel: "ses-1", Label: "scan-1", Modality: "MR", SeriesNumber: &seven, SeriesDescription: "T1 axial", InstanceCount: 2, SizeBytes: 2400, ArchiveTime: at},
		&fakeserver.Instance{ID: "1.2.3.1", Data: data, TransferSyntax: "1.2.840.10008.1.2.1",
			JSON: `{"00080060":{"vr":"CS","Value":["MR"]},"00100010":{"vr":"PN","Value":[{"Alphabetic":"Doe^Jane"}]},"00082112":{"vr":"SQ","Value":[{},{}]},"7FE00010":{"vr":"OB","BulkDataURI":"x"}}`},
		&fakeserver.Instance{ID: "1.2.3.2", Data: data, TransferSyntax: "1.2.840.10008.1.2.1", TooLarge: true})
	return data
}

// TestBrowseScansAndHeaders guards the walk from a tenant to a scan's headers:
// subjects, then the sessions of one, then its scans, then the scan's catalog
// row, then its instances and one instance's header with each attribute on
// its own line in tag order, a header too large said so rather than cut, and
// esc leaving one level at a time.
func TestBrowseScansAndHeaders(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) { seedReading(f) })
		r.key("enter", "enter", "c")
		r.expect("Subjects of t000", "sub-a", "SESSIONS", "2.3 KiB")
		r.key("enter")
		r.expect("Sessions of sub-a", "ses-1", "20240105", "MR")
		r.key("enter")
		r.expect("Scans of sub-a/ses-1", "scan-1", "T1 axial")
		r.key("enter")
		r.expect("Scan sub-a/ses-1/scan-1", "Series number:", "7", "Instances:", "m headers", "d download")
		r.key("m")
		r.expect("Headers of scan-1", "1.2.3.1", "whole", "1.2.3.2", "too large")
		r.key("enter")
		r.expect("Header of 1.2.3.1", "(0008,0060)", "CS: MR", "(0010,0010)", "PN: Doe^Jane", "(0008,2112)", "SQ: 2 item(s)", "(7FE0,0010)", "OB: bulk data")
		lines := r.lines()
		var tags []string
		for _, l := range lines {
			if i := indexOf(l, "(0"); i >= 0 {
				tags = append(tags, l[i:i+11])
			}
		}
		if len(tags) < 3 || tags[0] != "(0008,0060)" || tags[1] != "(0008,2112)" || tags[2] != "(0010,0010)" {
			t.Errorf("the attributes are not in tag order: %v", tags)
		}
		r.key("esc", "down", "enter")
		r.expect("Header of 1.2.3.2", "not shown: it is longer than 1 MiB")
		r.key("esc", "esc")
		r.expect("Scan sub-a/ses-1/scan-1")
		r.key("esc", "esc", "esc")
		r.expect("Subjects of t000")
	})
}

// indexOf is strings.Index without the import's name in the test body.
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestDownloadScreen guards the download from the interactive program: the
// key asks for a directory, the files arrive there under the layout the
// command uses, each equal to the server's bytes, the screen says how many
// files and bytes came and that it is done, and a second download into the
// same directory fetches nothing it holds whole.
func TestDownloadScreen(t *testing.T) {
	var data []byte
	r := newRig(t, 100, 30, func(f *fakeserver.Server) { data = seedReading(f) })
	dir := filepath.Join(t.TempDir(), "out")
	r.key("enter", "enter", "c")
	r.key("d")
	r.expect("Download sub-a", "Directory", "./nopacs-download")
	for range len("./nopacs-download") {
		r.key("backspace")
	}
	r.typeText(dir)
	r.key("enter")
	r.expect("Download sub-a", "2 files fetched, 0 already complete, 2.3 KiB received", "done")
	for _, id := range []string{"1.2.3.1", "1.2.3.2"} {
		got, err := os.ReadFile(filepath.Join(dir, "sub-a", "ses-1", "scan-1", id+".dcm"))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s: %v, %d bytes, want %d", id, err, len(got), len(data))
		}
	}
	r.key("esc")
	r.expect("Subjects of t000")
	r.key("d")
	for range len("./nopacs-download") {
		r.key("backspace")
	}
	r.typeText(dir)
	r.key("enter")
	r.expect("0 files fetched, 2 already complete, 0 B received", "done")

	// An empty directory is refused before anything is asked of the server.
	r.key("esc", "d")
	for range len("./nopacs-download") {
		r.key("backspace")
	}
	before := r.calls("ReadFiles")
	r.key("enter")
	r.expect("say which directory")
	if r.calls("ReadFiles") != before {
		t.Error("a download with no directory was started")
	}
}

// TestKeyScreensShowRules guards what a key's screens show of its rules: the
// networks it is limited to, the client certificate it needs, its successor,
// the address it was last used from, and a warning once it is about to
// expire; a key far from its end has no warning, and the list says expiring.
func TestKeyScreensShowRules(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) {
			f.AddKey(&v1.Key{Id: "k00000000100", Holder: "bridge", CreateTime: timestamp(-100), ExpireTime: timestamp(24 * 3), LastUseTime: timestamp(-2), LastUseAddress: "10.20.0.7",
				Networks: []string{"10.20.0.0/16", "192.0.2.0/24"}, ClientCertSha256: "ab" + string(bytes.Repeat([]byte("cd"), 31)), SuccessorId: "k00000000101"})
			f.AddKey(&v1.Key{Id: "k00000000101", Holder: "bridge 2", CreateTime: timestamp(-1), ExpireTime: timestamp(24 * 60)})
		})
		r.key("down", "enter")
		r.expect("Keys", "k00000000100", "expiring", "k00000000101", "active")
		r.key("enter")
		r.expect("Key k00000000100", "Last used:", "from 10.20.0.7", "Warning:", "expires in 3 days", "nopacs key rotate",
			"Networks:", "10.20.0.0/16, 192.0.2.0/24", "Client cert:", "sha256", "abcdcd", "Successor:", "k00000000101")
		r.key("esc", "down", "enter")
		r.expect("Key k00000000101")
		r.refuse("Warning:", "Networks:", "Successor:", "Client cert:")
	})
}
