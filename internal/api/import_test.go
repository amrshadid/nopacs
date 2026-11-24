package api_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// importFixture is the place fixture with an import root "src" allotted to
// lab1 over a tree with one session folder, a map file, a storage "main"
// and an import receiver "from-tree" on it.
type importFixture struct {
	*placeFixture
	root string
}

// importMap is the map of the tree's one session.
const importMap = "project,label,subject_label\nproj1,sess1,subj1\n"

// newImportFixture makes the import fixture.
func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	f := &importFixture{placeFixture: newPlaceFixture(t)}
	f.root = filepath.Join(f.dir, "import")
	if err := os.MkdirAll(filepath.Join(f.root, "proj1", "sess1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "proj1", "map.csv"), []byte(importMap), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "proj1", "afile"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setSetting(installation(), "import.roots", "src="+f.root)
	f.setSetting(tenantScope("lab1"), "import.allowed-roots", "src")
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	mustCall[*v1.AddReceiverResponse](t, "add from-tree", addReceiver("lab1", importReceiverMsg("from-tree"))(f.as(local)))
	return f
}

// importReceiverMsg is an import receiver as a request gives it: the
// pattern layout over <project>/<session>/<file>, a map from the project
// and session to the subject, and a naming rule over the parts.
func importReceiverMsg(name string) *v1.Receiver {
	return &v1.Receiver{
		Name: name, Storage: "main", Enabled: true,
		Naming: &v1.NamingRule{Subject: "{project}_{subject}", Session: "{project}_{session}", Scan: "{#SeriesInstanceUID}"},
		Type: &v1.Receiver_Importer{Importer: &v1.ImportReceiver{
			Root: "src", Layout: v1.ImportLayout_IMPORT_LAYOUT_PATTERN,
			PathPattern: `^(?P<project>[^/]+)/(?P<session>[^/]+)/[^/]+$`, UnitDepth: 1, MapFile: "proj1/map.csv",
			MapKeyColumns: []string{"project", "label"}, MapKeyParts: []string{"project", "session"},
			MapValueColumn: "subject_label", MapGives: "subject",
		}},
		Deidentification: &v1.Receiver_AsReceived{AsReceived: &v1.AsReceived{}},
	}
}

// importJob is a CreateJob of an import of path through from-tree.
func importJob(path string, ij *v1.ImportJob) *v1.CreateJobRequest {
	if ij == nil {
		ij = &v1.ImportJob{}
	}
	ij.Receiver, ij.Path = "from-tree", path
	return &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_ImportJob{ImportJob: ij}}
}

// wantCode fails the test unless o failed with code and, when msg is not
// empty, a message holding it.
func wantCode(t *testing.T, what string, o outcome, code connect.Code, msg string) {
	t.Helper()
	if connect.CodeOf(o.err) != code || (msg != "" && !strings.Contains(o.err.Error(), msg)) {
		t.Errorf("%s: %v, want %v holding %q", what, o.err, code, msg)
	}
}

// TestImportRootsSettingRules guards where import.roots may point: inside a
// storage root, around it, at another import root or through a link into a
// storage root is refused; a relative path is invalid; storage.directory-
// roots may not come to hold an import root; a root an import receiver
// reads cannot be removed or moved, nor taken back from its tenant; and the
// value is kept with each path resolved.
func TestImportRootsSettingRules(t *testing.T) {
	f := newImportFixture(t)
	if err := os.Symlink(f.root, filepath.Join(f.dir, "import-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.placeFixture.root, filepath.Join(f.dir, "fast-alias")); err != nil {
		t.Fatal(err)
	}
	set := func(name, value string) outcome {
		return call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: name, Scope: installation(), Value: value}})
	}
	keep := "src=" + f.root + "\n"
	for what, c := range map[string]struct {
		value string
		code  connect.Code
	}{
		"inside a storage root":            {keep + "in=" + f.placeFixture.root + "/x", connect.CodeFailedPrecondition},
		"around a storage root":            {keep + "up=" + f.dir + "/roots", connect.CodeFailedPrecondition},
		"through a link to a storage root": {keep + "ln=" + f.dir + "/fast-alias", connect.CodeFailedPrecondition},
		"inside another import root":       {keep + "in=" + f.dir + "/import-alias/proj1", connect.CodeFailedPrecondition},
		"relative":                         {keep + "rel=relative/path", connect.CodeInvalidArgument},
		"the root removed":                 {"", connect.CodeFailedPrecondition},
		"the root moved":                   {"src=" + f.dir + "/elsewhere", connect.CodeFailedPrecondition},
	} {
		wantCode(t, what, set("import.roots", c.value), c.code, "")
	}
	wantCode(t, "a storage root at an import root", set("storage.directory-roots", "fast="+f.placeFixture.root+"\nbig="+f.dir+"/import-alias"),
		connect.CodeFailedPrecondition, "import root")
	wantCode(t, "the root taken back", call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{
		Name: "import.allowed-roots", Scope: tenantScope("lab1"), Value: ""}}), connect.CodeFailedPrecondition, "from-tree")

	// The same root written through a link is kept resolved, so the
	// receiver's root has not moved and the value is taken.
	got := mustCall[*v1.SetSettingResponse](t, "set through a link", set("import.roots", "src="+f.dir+"/import-alias"))
	want, _ := filepath.EvalSymlinks(f.root)
	if got.GetSetting().GetValue() != "src="+want {
		t.Errorf("the value kept is %q, want the path resolved", got.GetSetting().GetValue())
	}
}

// TestImportReceiverRules guards the rules of an import receiver: a root
// not allotted, no layout, a naming rule using a part nothing gives and a
// map key part the layout lacks are refused; it is idle while enabled; its
// map file reaches an operator and not a reader; its fields change one by
// one and never as a whole message; and while one of its jobs is queued it
// can be neither changed in its import fields nor removed.
func TestImportReceiverRules(t *testing.T) {
	f := newImportFixture(t)
	add := func(mut func(r *v1.Receiver)) outcome {
		r := importReceiverMsg("other")
		mut(r)
		return addReceiver("lab1", r)(f.as(local))
	}
	wantCode(t, "a root not named", add(func(r *v1.Receiver) { r.GetImporter().Root = "nowhere" }), connect.CodeFailedPrecondition, "import.roots")
	wantCode(t, "no layout", add(func(r *v1.Receiver) { r.GetImporter().Layout = v1.ImportLayout_IMPORT_LAYOUT_UNSPECIFIED }),
		connect.CodeInvalidArgument, "receiver.importer.layout")
	wantCode(t, "a part nothing gives", add(func(r *v1.Receiver) { r.Naming.Scan = "{visit}" }), connect.CodeInvalidArgument, "naming")
	wantCode(t, "a key part the layout lacks", add(func(r *v1.Receiver) { r.GetImporter().MapKeyParts = []string{"project", "site"} }),
		connect.CodeFailedPrecondition, "")
	f.setSetting(installation(), "import.roots", "src="+f.root+"\nother="+f.dir+"/other")
	wantCode(t, "a root not allotted", add(func(r *v1.Receiver) { r.GetImporter().Root = "other" }), connect.CodeFailedPrecondition, "import.allowed-roots")

	asOp := mustCall[*v1.GetReceiverResponse](t, "get as an operator", call(f.as(f.lab1Operator).receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: "from-tree"}))
	asReader := mustCall[*v1.GetReceiverResponse](t, "get as a reader", call(f.as(f.lab1Reader).receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: "from-tree"}))
	if asOp.GetReceiver().GetImporter().GetMapFile() != "proj1/map.csv" || asReader.GetReceiver().GetImporter().GetMapFile() != "" {
		t.Errorf("map file: operator %q, reader %q", asOp.GetReceiver().GetImporter().GetMapFile(), asReader.GetReceiver().GetImporter().GetMapFile())
	}
	if asOp.GetReceiver().GetHealth().GetState() != v1.ReceiverState_RECEIVER_STATE_IDLE {
		t.Errorf("health %v, want idle", asOp.GetReceiver().GetHealth())
	}

	setReq := func(paths ...string) *v1.SetReceiverRequest {
		return &v1.SetReceiverRequest{Tenant: "lab1", Receiver: &v1.Receiver{Name: "from-tree", Type: &v1.Receiver_Importer{Importer: &v1.ImportReceiver{
			PathPattern: `^(?P<project>[^/]+)/(?P<session>[^/]+)/.+$`}}}, UpdateMask: mask(paths...)}
	}
	wantCode(t, "the whole message", call(f.as(local).receivers.SetReceiver, setReq("importer")), connect.CodeInvalidArgument, "importer")
	changed := mustCall[*v1.SetReceiverResponse](t, "change the pattern", call(f.as(local).receivers.SetReceiver, setReq("importer.path_pattern")))
	if im := changed.GetReceiver().GetImporter(); im.GetPathPattern() != `^(?P<project>[^/]+)/(?P<session>[^/]+)/.+$` || im.GetRoot() != "src" || im.GetMapGives() != "subject" {
		t.Errorf("after the change: %v", im)
	}

	mustCall[*v1.CreateJobResponse](t, "a queued job", call(f.as(f.lab1Operator).jobs.CreateJob, importJob("proj1", nil)))
	wantCode(t, "a change while a job is queued", call(f.as(local).receivers.SetReceiver, setReq("importer.path_pattern")),
		connect.CodeFailedPrecondition, "import job")
	wantCode(t, "a removal while a job is queued", call(f.as(local).receivers.RemoveReceiver, &v1.RemoveReceiverRequest{Tenant: "lab1", Name: "from-tree"}),
		connect.CodeFailedPrecondition, "import job")
}

// TestImportJobRules guards CreateJob of an import: the map file is read
// when the job is made and kept with it, so a later change of the file
// changes nothing; a path that is not a directory, a map file and an inline
// map together, an inline map past 3 MiB, counts given, a receiver that is
// not an import receiver and a path overlapping a queued job's are refused;
// a retention start is answered unimplemented; a reader reads no path.
func TestImportJobRules(t *testing.T) {
	f := newImportFixture(t)
	op := f.as(f.lab1Operator)
	made := mustCall[*v1.CreateJobResponse](t, "an import using the receiver's map", call(op.jobs.CreateJob, importJob("proj1", nil)))
	id := made.GetJob().GetId()
	if err := os.WriteFile(filepath.Join(f.root, "proj1", "map.csv"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := mustCall[*v1.GetJobResponse](t, "get as an operator", call(op.jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: id}))
	if string(got.GetJob().GetImportJob().GetMapCsv()) != importMap || got.GetJob().GetImportJob().GetPath() != "proj1" {
		t.Errorf("the job keeps the map %q and the path %q, want the map as read when it was made", got.GetJob().GetImportJob().GetMapCsv(), got.GetJob().GetImportJob().GetPath())
	}
	asReader := mustCall[*v1.GetJobResponse](t, "get as a reader", call(f.as(f.lab1Reader).jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: id}))
	if ij := asReader.GetJob().GetImportJob(); ij.GetPath() != "" || len(ij.GetMapCsv()) != 0 || ij.GetReceiver() != "from-tree" {
		t.Errorf("a reader reads %v", ij)
	}

	create := func(path string, ij *v1.ImportJob) outcome { return call(op.jobs.CreateJob, importJob(path, ij)) }
	wantCode(t, "a path not there", create("proj9", &v1.ImportJob{MapCsv: []byte(importMap)}), connect.CodeFailedPrecondition, "import_job.path")
	wantCode(t, "a file", create("proj1/afile", &v1.ImportJob{MapCsv: []byte(importMap)}), connect.CodeFailedPrecondition, "import_job.path")
	wantCode(t, "both maps", create("other", &v1.ImportJob{MapCsv: []byte(importMap), MapFile: "proj1/map.csv"}), connect.CodeInvalidArgument, "")
	wantCode(t, "an inline map too large", create("other", &v1.ImportJob{MapCsv: bytes.Repeat([]byte("a"), 3<<20+1)}), connect.CodeInvalidArgument, "3 MiB")
	wantCode(t, "counts given", create("other", &v1.ImportJob{Counts: &v1.ImportCounts{}}), connect.CodeInvalidArgument, "import_job.counts")
	wantCode(t, "an overlapping path", create("proj1/sess1", &v1.ImportJob{MapCsv: []byte(importMap)}), connect.CodeFailedPrecondition, "")
	wantCode(t, "a retention start", create("proj1", &v1.ImportJob{RetentionStart: timestamppb.Now()}), connect.CodeUnimplemented, "retention")
	mustCall[*v1.AddReceiverResponse](t, "add scanner1", addReceiver("lab1", dimseReceiver("scanner1", "main", 11112))(f.as(local)))
	wantCode(t, "not an import receiver", call(op.jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_ImportJob{
		ImportJob: &v1.ImportJob{Receiver: "scanner1", Path: "proj1"}}}), connect.CodeFailedPrecondition, "not an import receiver")
}
