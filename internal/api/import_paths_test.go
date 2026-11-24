package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// importPathFields are the fields that carry a path under an import root,
// or an import's map: what a folder named after a patient, or a subject's
// label as the source system kept it, can reach a client through.
var importPathFields = map[protoreflect.FullName]bool{
	"nopacs.v1.ImportReceiver.map_file":     true,
	"nopacs.v1.ImportJob.path":              true,
	"nopacs.v1.ImportJob.map_file":          true,
	"nopacs.v1.ImportJob.map_csv":           true,
	"nopacs.v1.QuarantinedFile.import_path": true,
}

// importFolder is the folder of the test tree, named as a site might name
// a folder after a patient, so its text is looked for in every answer.
const importFolder = "Smith_John"

// TestReaderAnswersCarryNoImportPaths guards that the paths under an
// import root and an import's map reach operators only: a reader's answers
// of receivers, jobs and quarantined files hold none of them, and no text
// of the folder's name anywhere, while an operator's answer of the same
// job holds its path, so the test cannot pass on answers that hold none.
// The import is carried out by the real worker, its files held as they
// arrive (holdingSink), so the held file names the job and its path.
func TestReaderAnswersCarryNoImportPaths(t *testing.T) {
	f := newPlaceFixture(t)
	f.runImports(t)
	root := filepath.Join(f.dir, "import")
	folder := filepath.Join(root, importFolder, "sess1")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	// One file that is not DICOM (skipped) and one that starts as Part 10
	// and does not parse (quarantined, so a held file names the import).
	if err := os.WriteFile(filepath.Join(folder, "notes.txt"), []byte("not dicom"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := append(make([]byte, 128), []byte("DICMbroken")...)
	if err := os.WriteFile(filepath.Join(folder, "image.dcm"), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	mapCSV := "project,label,subject_label\n" + importFolder + ",sess1,subj1\n"
	if err := os.WriteFile(filepath.Join(root, importFolder, "map.csv"), []byte(mapCSV), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setSetting(installation(), "import.roots", "src="+root)
	f.setSetting(tenantScope("lab1"), "import.allowed-roots", "src")
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	mustCall[*v1.AddReceiverResponse](t, "add from-tree", addReceiver("lab1", &v1.Receiver{
		Name: "from-tree", Storage: "main", Enabled: true,
		Type: &v1.Receiver_Importer{Importer: &v1.ImportReceiver{
			Root: "src", Layout: v1.ImportLayout_IMPORT_LAYOUT_PATTERN,
			PathPattern: `^(?P<project>[^/]+)/(?P<session>[^/]+)/[^/]+$`, UnitDepth: 1,
			MapFile:        importFolder + "/map.csv",
			MapKeyColumns:  []string{"project", "label"},
			MapKeyParts:    []string{"project", "session"},
			MapValueColumn: "subject_label",
			MapGives:       "subject",
		}},
		Deidentification: &v1.Receiver_AsReceived{AsReceived: &v1.AsReceived{}},
	})(f.as(local)))
	made := mustCall[*v1.CreateJobResponse](t, "create the import", call(f.as(f.lab1Operator).jobs.CreateJob, &v1.CreateJobRequest{
		Tenant: "lab1",
		Kind: &v1.CreateJobRequest_ImportJob{ImportJob: &v1.ImportJob{
			Receiver: "from-tree", Path: importFolder, MapCsv: []byte(mapCSV),
		}},
	}))
	id := made.GetJob().GetId()

	// Wait for the job to end and its one held file to be recorded.
	deadline := time.Now().Add(30 * time.Second)
	for {
		got := mustCall[*v1.GetJobResponse](t, "get the import", call(f.as(f.lab1Operator).jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: id}))
		if got.GetJob().GetEndTime() != nil && got.GetJob().GetImportJob().GetCounts().GetProcessing() == 0 {
			if p := got.GetJob().GetImportJob().GetPath(); p != importFolder {
				t.Fatalf("an operator reads the job's path as %q, want %q", p, importFolder)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the import did not settle: %v", got.GetJob())
		}
		time.Sleep(50 * time.Millisecond)
	}

	reader := f.as(f.lab1Reader)
	answers := []proto.Message{
		mustCall[*v1.GetReceiverResponse](t, "get the receiver", call(reader.receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: "from-tree"})),
		mustCall[*v1.ListReceiversResponse](t, "list receivers", call(reader.receivers.ListReceivers, &v1.ListReceiversRequest{Tenant: "lab1"})),
		mustCall[*v1.GetJobResponse](t, "get the job", call(reader.jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: id})),
		mustCall[*v1.ListJobsResponse](t, "list jobs", call(reader.jobs.ListJobs, &v1.ListJobsRequest{Tenant: "lab1"})),
	}
	held := mustCall[*v1.ListQuarantinedFilesResponse](t, "list held files", call(reader.quarantine.ListQuarantinedFiles, &v1.ListQuarantinedFilesRequest{Tenant: "lab1"}))
	if len(held.GetFiles()) == 0 {
		t.Fatal("the import held no file, so the quarantine's answers were not checked")
	}
	answers = append(answers, held)
	for _, q := range held.GetFiles() {
		answers = append(answers, mustCall[*v1.GetQuarantinedFileResponse](t, "get a held file", call(reader.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: q.GetId()})))
	}
	for _, a := range answers {
		checkNoImportPath(t, a.ProtoReflect())
	}

	// The files and units are listed with their paths to an operator, and
	// to a reader not at all.
	op := f.as(f.lab1Operator)
	files := mustCall[*v1.ListImportFilesResponse](t, "list the files", call(op.jobs.ListImportFiles, &v1.ListImportFilesRequest{Tenant: "lab1", Id: id}))
	var listed []string
	for _, fl := range files.GetFiles() {
		listed = append(listed, fl.GetPath()+" "+fl.GetKind().String()+" "+fl.GetSkipCode().String()+" "+fl.GetQuarantineReason().String())
	}
	want := []string{
		importFolder + "/map.csv IMPORT_FILE_KIND_SKIPPED IMPORT_SKIP_CODE_OUTSIDE_LAYOUT QUARANTINE_REASON_UNSPECIFIED",
		importFolder + "/sess1/image.dcm IMPORT_FILE_KIND_QUARANTINED IMPORT_SKIP_CODE_UNSPECIFIED QUARANTINE_REASON_BAD_LABEL",
		importFolder + "/sess1/notes.txt IMPORT_FILE_KIND_SKIPPED IMPORT_SKIP_CODE_NOT_PART10 QUARANTINE_REASON_UNSPECIFIED",
	}
	if strings.Join(listed, "\n") != strings.Join(want, "\n") {
		t.Errorf("an operator's files:\n%s\nwant\n%s", strings.Join(listed, "\n"), strings.Join(want, "\n"))
	}
	units := mustCall[*v1.ListImportUnitsResponse](t, "list the units", call(op.jobs.ListImportUnits, &v1.ListImportUnitsRequest{Tenant: "lab1", Id: id}))
	if len(units.GetUnits()) != 1 || units.GetUnits()[0].GetFolder() != importFolder || units.GetUnits()[0].GetCounts().GetFound() != 3 {
		t.Errorf("an operator's units: %v", units.GetUnits())
	}
	for name, o := range map[string]outcome{
		"files": call(reader.jobs.ListImportFiles, &v1.ListImportFilesRequest{Tenant: "lab1", Id: id}),
		"units": call(reader.jobs.ListImportUnits, &v1.ListImportUnitsRequest{Tenant: "lab1", Id: id}),
	} {
		if connect.CodeOf(o.err) != connect.CodePermissionDenied {
			t.Errorf("a reader's list of the %s: %v, want permission denied", name, o.err)
		}
	}
}

// checkNoImportPath fails the test when m, or a message within it, sets one
// of importPathFields, or holds the test folder's name in any text field.
func checkNoImportPath(t *testing.T, m protoreflect.Message) {
	t.Helper()
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if importPathFields[fd.FullName()] {
			t.Errorf("a reader's %s answers %s", m.Descriptor().FullName(), fd.FullName())
		}
		switch {
		case fd.IsList():
			l := v.List()
			for i := range l.Len() {
				checkValue(t, fd, l.Get(i))
			}
		case fd.IsMap():
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if strings.Contains(k.String(), importFolder) {
					t.Errorf("a reader's %s holds the folder's name in a key", fd.FullName())
				}
				checkValue(t, fd.MapValue(), mv)
				return true
			})
		default:
			checkValue(t, fd, v)
		}
		return true
	})
}

// checkValue checks one value of a field: a message within, or text.
func checkValue(t *testing.T, fd protoreflect.FieldDescriptor, v protoreflect.Value) {
	t.Helper()
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		checkNoImportPath(t, v.Message())
	case protoreflect.StringKind:
		if strings.Contains(v.String(), importFolder) {
			t.Errorf("a reader's %s holds the folder's name", fd.FullName())
		}
	case protoreflect.BytesKind:
		if strings.Contains(string(v.Bytes()), importFolder) {
			t.Errorf("a reader's %s holds the folder's name", fd.FullName())
		}
	}
}
