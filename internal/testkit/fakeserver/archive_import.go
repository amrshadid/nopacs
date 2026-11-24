package fakeserver

import (
	"context"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// checkImporter holds an import receiver to what the commands must send: a
// root and a layout.
func checkImporter(im *v1.ImportReceiver) error {
	if im.GetRoot() == "" || im.GetLayout() == v1.ImportLayout_IMPORT_LAYOUT_UNSPECIFIED {
		return fault.New(fault.Invalid, "receiver.importer: a root and a layout are required")
	}
	return nil
}

// setImportField sets the field of an import receiver a mask path names,
// and reports whether p is one.
func setImportField(next *v1.Receiver, in *v1.ImportReceiver, p string) bool {
	im := next.GetImporter()
	if im == nil {
		return false
	}
	switch p {
	case "importer.root":
		im.Root = in.GetRoot()
	case "importer.layout":
		im.Layout = in.GetLayout()
	case "importer.path_pattern":
		im.PathPattern = in.GetPathPattern()
	case "importer.unit_depth":
		im.UnitDepth = in.GetUnitDepth()
	case "importer.map_file":
		im.MapFile = in.GetMapFile()
	case "importer.map_key_columns":
		im.MapKeyColumns = in.GetMapKeyColumns()
	case "importer.map_key_parts":
		im.MapKeyParts = in.GetMapKeyParts()
	case "importer.map_value_column":
		im.MapValueColumn = in.GetMapValueColumn()
	case "importer.map_gives":
		im.MapGives = in.GetMapGives()
	default:
		return false
	}
	return true
}

// createImportJob makes an import job of an import receiver the tenant has,
// queued with no counts yet. The caller holds the lock.
func (s *Server) createImportJob(tenant string, in *v1.ImportJob) (*v1.CreateJobResponse, error) {
	r, ok := s.arch.receivers[key(tenant, in.GetReceiver())]
	if !ok || r.GetImporter() == nil {
		return nil, fault.New(fault.Precondition, "import_job.receiver: the tenant has no import receiver of that name")
	}
	if in.GetCounts() != nil || (in.GetMapFile() != "" && len(in.GetMapCsv()) > 0) {
		return nil, fault.New(fault.Invalid, "import_job: counts are the server's, and a map is a file or inline, not both")
	}
	ij := proto.Clone(in).(*v1.ImportJob)
	ij.Counts = &v1.ImportCounts{Units: 1}
	j := &v1.Job{Id: s.newJobID(tenant), Kind: &v1.Job_ImportJob{ImportJob: ij}, State: v1.JobState_JOB_STATE_QUEUED,
		Counts: &v1.JobCounts{Scans: 1}, CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
	s.arch.jobs[key(tenant, j.GetId())] = j
	return &v1.CreateJobResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}

// AddImportFile lists a file of an import job, for ListImportFiles.
func (s *Server) AddImportFile(tenant, job string, f *v1.ImportFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	s.arch.importFiles[key(tenant, job)] = append(s.arch.importFiles[key(tenant, job)], f)
}

// AddImportUnit lists a unit of an import job, for ListImportUnits.
func (s *Server) AddImportUnit(tenant, job string, u *v1.ImportUnit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	s.arch.importUnits[key(tenant, job)] = append(s.arch.importUnits[key(tenant, job)], u)
}

// importJob returns a stored import job, or the error the real server
// answers for a job not there or of another kind.
func (s *Server) importJob(tenant, id string) error {
	j, err := s.job(tenant, id)
	if err != nil {
		return err
	}
	if j.GetImportJob() == nil {
		return fault.New(fault.Precondition, "the job is not an import job")
	}
	return nil
}

// ListImportFiles answers a page of an import job's listed files, of the
// kinds asked for.
func (h jobHandler) ListImportFiles(_ context.Context, req *v1.ListImportFilesRequest) (*v1.ListImportFilesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.importJob(req.GetTenant(), req.GetId()); err != nil {
		return nil, err
	}
	var all []*v1.ImportFile
	for _, f := range s.arch.importFiles[key(req.GetTenant(), req.GetId())] {
		kept := len(req.GetKinds()) == 0
		for _, k := range req.GetKinds() {
			kept = kept || k == f.GetKind()
		}
		if kept && (req.GetUnit() == "" || req.GetUnit() == f.GetUnit()) {
			all = append(all, f)
		}
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListImportFilesResponse{Files: all[from:to], NextPageToken: next}, nil
}

// ListImportUnits answers a page of an import job's units, in the states
// asked for.
func (h jobHandler) ListImportUnits(_ context.Context, req *v1.ListImportUnitsRequest) (*v1.ListImportUnitsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.importJob(req.GetTenant(), req.GetId()); err != nil {
		return nil, err
	}
	var all []*v1.ImportUnit
	for _, u := range s.arch.importUnits[key(req.GetTenant(), req.GetId())] {
		kept := len(req.GetStates()) == 0
		for _, st := range req.GetStates() {
			kept = kept || st == u.GetState()
		}
		if kept {
			all = append(all, u)
		}
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListImportUnitsResponse{Units: all[from:to], NextPageToken: next}, nil
}
