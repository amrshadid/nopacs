package api

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/dirimport"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// createImportJob makes an import job (job.proto, ImportJob): the files of
// a directory under an import receiver's root handed to it. The receiver,
// its root, the path and the map are checked before the job is made
// (dirimport.CheckJob), and the map is kept with the job as read now, so a
// map file changed later changes nothing of the job. Its role, audit and
// errors are CreateJob's; the audit entry names the receiver and its root,
// never a path.
func (s jobService) createImportJob(ctx context.Context, d access.Decision, p tenant.Pipeline, st tenant.Store, req *v1.CreateJobRequest) (*v1.CreateJobResponse, error) {
	c := s.c
	ij := req.GetImportJob()
	if ij.GetCounts() != nil {
		return nil, c.failIn(d, st, invalid("import_job.counts", "is answered by the server and is absent here"))
	}
	if err := itemName("import_job.receiver", ij.GetReceiver()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	id := c.newID()
	if master.ValidateID("job id", id) != nil {
		return nil, c.failIn(d, st, fault.New(fault.Internal, "api: the id source made a job id that is not valid"))
	}
	kept, err := c.values().OnTenant(st).FailuresKept(ctx)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	nj := tenant.NewImportJob{
		ID: id, Receiver: ij.GetReceiver(), Path: ij.GetPath(), MapFile: ij.GetMapFile(), MapCSV: ij.GetMapCsv(),
		Map: tenant.ImportMap{KeyColumns: ij.GetMapKeyColumns(), ValueColumn: ij.GetMapValueColumn(),
			KeyParts: ij.GetMapKeyParts(), Gives: ij.GetMapGives()},
		FailuresKept: kept, Created: c.now(),
	}
	if err := nj.Validate(); err != nil {
		return nil, c.failIn(d, st, fieldOf("import_job", err))
	}
	// The check reads the file system, which may not answer (a share gone,
	// a hung mount), so it runs outside the lock that orders the changes of
	// receivers and of the installation's places, under a bound: a stalled
	// check never holds those changes back. What it was checked against,
	// the receiver and its root's path, is read again under the lock below,
	// and a change meanwhile refuses the job.
	rcv, rootPath, err := c.importJobPlace(ctx, p, nj.Receiver)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, c.importCheckBound())
	mapCSV, err := dirimport.CheckJob(checkCtx, c.jobRoots(), req.GetTenant(), st, rcv,
		tenant.ImportJob{Receiver: nj.Receiver, Path: nj.Path, MapFile: nj.MapFile, MapCSV: nj.MapCSV, Map: nj.Map})
	cancel()
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if len(nj.MapCSV) == 0 && len(mapCSV) > 0 {
		// A map read from a file is kept with the job as read now, in the
		// place of an inline map, so its units never read the file again.
		// CheckJob bounded it to tenant.MaxImportMapCSV.
		nj.MapCSV, nj.MapFile = mapCSV, ""
	}
	// Under the lock a change or removal of an import receiver, and a
	// change of import.roots, takes, so neither passes unseen between the
	// reading again below and the job's making.
	c.places.RLock()
	defer c.places.RUnlock()
	now, nowPath, err := c.importJobPlace(ctx, p, nj.Receiver)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if !now.Updated.Equal(rcv.Updated) || nowPath != rootPath {
		return nil, c.failIn(d, st, fault.New(fault.Unavailable, "import_job: the receiver "+nj.Receiver+
			" or its root changed while the job was checked; ask for the job again"))
	}
	details := maps.Clone(createJobDetails(req))
	details[detailRoot] = rcv.Import.Root
	out, err := p.CreateImportJob(ctx, nj, changeEntry(d, access.Facts{Object: id, Details: details}), c.roleGuard(d, master.RoleOperator))
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("import_job", err))
	}
	c.sendersChanged(req.GetTenant())
	return &v1.CreateJobResponse{Job: c.newJobViews(p, req.GetTenant()).message(ctx, out)}, nil
}

// importCheckTimeout bounds the check of an import job (dirimport.CheckJob):
// opening the root, the folders of its path and its map file, and reading
// the map, at most 3 MiB. A healthy disk or network share does all of that
// in well under a second; thirty seconds is past any of them, and short
// enough that the caller is told rather than left waiting.
const importCheckTimeout = 30 * time.Second

// importCheckBound returns importCheckTimeout, or the bound a test set.
func (c *core) importCheckBound() time.Duration {
	if c.seams.importCheck > 0 {
		return c.seams.importCheck
	}
	return importCheckTimeout
}

// jobRoots returns the import roots an import job is checked against: the
// installation's (core.importRoots), or those a test set.
func (c *core) jobRoots() dirimport.Roots {
	if c.seams.importRoots != nil {
		return c.seams.importRoots
	}
	return c.importRoots()
}

// importJobPlace returns the receiver named name, as an import job's check
// reads it, and the path import.roots gives its root ("" for a receiver of
// another type, or a root import.roots does not name, which the check
// refuses). A receiver the tenant does not have is a precondition failure.
func (c *core) importJobPlace(ctx context.Context, p tenant.Pipeline, name string) (tenant.Receiver, string, error) {
	rcv, err := p.Receiver(ctx, name)
	if fault.KindOf(err) == fault.NotFound {
		return tenant.Receiver{}, "", precondition("import_job.receiver: the tenant has no receiver " + name)
	}
	if err != nil {
		return tenant.Receiver{}, "", err
	}
	if rcv.Type != tenant.ReceiverImport {
		return rcv, "", nil
	}
	roots, err := c.values().ImportRoots(ctx)
	if err != nil {
		return tenant.Receiver{}, "", err
	}
	root, _ := roots.Lookup(rcv.Import.Root)
	return rcv, root.Path, nil
}

// importJobMessage returns an import job's part as the contract writes it,
// its paths included and its inline map left out: GetJob adds the map for
// an operator, and GetJob and ListJobs take the paths out for a reader
// (hideImportPaths).
func importJobMessage(j tenant.Job) *v1.ImportJob {
	if j.Import == nil {
		return &v1.ImportJob{Receiver: j.Receiver}
	}
	i := j.Import
	return &v1.ImportJob{
		Receiver: i.Receiver, Path: i.Path, MapFile: i.MapFile, Counts: importCountsMessage(i.Counts),
		MapKeyColumns: i.Map.KeyColumns, MapValueColumn: i.Map.ValueColumn, MapKeyParts: i.Map.KeyParts, MapGives: i.Map.Gives,
	}
}

// importCountsMessage returns an import's counts as the contract writes
// them, each list most first, then in the contract's order.
func importCountsMessage(c tenant.ImportCounts) *v1.ImportCounts {
	m := &v1.ImportCounts{
		Found: c.Found, HandedIn: c.HandedIn, HandedAgain: c.HandedAgain, Archived: c.Archived, AlreadyHeld: c.AlreadyHeld,
		Lost: c.Lost, Processing: c.Processing, BytesHandedIn: c.BytesHandedIn, LabelDiffers: c.LabelDiffers,
		Units: c.Units, UnitsWalked: c.UnitsWalked, Failed: c.Failed, Discarded: c.Discarded,
	}
	for code, n := range c.Skipped {
		if n > 0 {
			m.Skipped = append(m.Skipped, &v1.ImportSkipCount{Code: importSkipCode(code), Count: n})
		}
	}
	slices.SortFunc(m.Skipped, func(a, b *v1.ImportSkipCount) int {
		return cmp.Or(cmp.Compare(b.GetCount(), a.GetCount()), cmp.Compare(a.GetCode(), b.GetCode()))
	})
	for reason, n := range c.Quarantined {
		if n > 0 {
			m.Quarantined = append(m.Quarantined, &v1.ImportQuarantineCount{Reason: reasons[reason], Count: n})
		}
	}
	slices.SortFunc(m.Quarantined, func(a, b *v1.ImportQuarantineCount) int {
		return cmp.Or(cmp.Compare(b.GetCount(), a.GetCount()), cmp.Compare(a.GetReason(), b.GetReason()))
	})
	return m
}

// importSkipCode returns a skip code as the contract names it.
func importSkipCode(c tenant.ImportSkipCode) v1.ImportSkipCode {
	return enumOf[v1.ImportSkipCode](v1.ImportSkipCode_value, "IMPORT_SKIP_CODE_", string(c))
}

// hideImportPaths takes out of m, a job's message, the paths an import job
// answers, for a caller who may not read paths (mayReadPaths): a path can
// name a person.
func hideImportPaths(d access.Decision, m *v1.Job) *v1.Job {
	if ij := m.GetImportJob(); ij != nil && !mayReadPaths(d) {
		ij.Path, ij.MapFile, ij.MapCsv = "", "", nil
	}
	return m
}

// withImportMap adds to m, GetJob's message of the import job j, its kept
// map, for an operator or more only.
func (s jobService) withImportMap(ctx context.Context, d access.Decision, p tenant.Pipeline, j tenant.Job, m *v1.Job) error {
	if j.Kind != tenant.JobImport || !mayReadPaths(d) {
		return nil
	}
	full, err := p.ImportJob(ctx, j.ID)
	if err != nil {
		return err
	}
	if full.Import != nil && m.GetImportJob() != nil {
		m.GetImportJob().MapCsv = full.Import.MapCSV
	}
	return nil
}

// ListImportFiles answers a page of the files of an import job that did not
// reach the archive, each by its path, to an operator or more.
func (s jobService) ListImportFiles(ctx context.Context, req *v1.ListImportFilesRequest) (*v1.ListImportFilesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.JobServiceListImportFilesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	filter := tenant.ImportFileFilter{Unit: req.GetUnit()}
	if len(req.GetKinds()) > tenant.MaxImportFilters {
		return nil, c.failIn(d, st, invalid("kinds", "names at most 16 kinds"))
	}
	for _, k := range req.GetKinds() {
		name, ok := strings.CutPrefix(k.String(), "IMPORT_FILE_KIND_")
		if !ok || k == v1.ImportFileKind_IMPORT_FILE_KIND_UNSPECIFIED {
			return nil, c.failIn(d, st, invalid("kinds", "names only kinds the contract defines, never IMPORT_FILE_KIND_UNSPECIFIED"))
		}
		filter.Kinds = append(filter.Kinds, tenant.ImportFileKind(strings.ToLower(name)))
	}
	if filter.Unit != "" {
		if err := serverID("unit", filter.Unit); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	if err := filter.Validate(); err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListImportFiles(ctx, req.GetId(), filter, page)
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoJob)
	}
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListImportFilesResponse{Files: make([]*v1.ImportFile, 0, len(items)), NextPageToken: next}
	for _, f := range items {
		out.Files = append(out.Files, &v1.ImportFile{
			Path: f.Path, Kind: enumOf[v1.ImportFileKind](v1.ImportFileKind_value, "IMPORT_FILE_KIND_", string(f.Kind)),
			QuarantinedFile: f.QuarantinedFile, Unit: f.Unit, SizeBytes: f.Size, Detail: f.Detail, Time: timestamp(f.Time),
			Reception: f.Reception, SkipCode: importSkipCode(f.SkipCode), QuarantineReason: reasons[f.QuarantineReason],
		})
	}
	return out, nil
}

// ListImportUnits answers a page of the units of an import job, each with
// its folder, state and counts, to an operator or more.
func (s jobService) ListImportUnits(ctx context.Context, req *v1.ListImportUnitsRequest) (*v1.ListImportUnitsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.JobServiceListImportUnitsProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := serverID("id", req.GetId()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	var filter tenant.ImportUnitFilter
	if len(req.GetStates()) > tenant.MaxImportFilters {
		return nil, c.failIn(d, st, invalid("states", "names at most 16 states"))
	}
	for _, us := range req.GetStates() {
		name, ok := strings.CutPrefix(us.String(), "IMPORT_UNIT_STATE_")
		if !ok || us == v1.ImportUnitState_IMPORT_UNIT_STATE_UNSPECIFIED {
			return nil, c.failIn(d, st, invalid("states", "names only states the contract defines, never IMPORT_UNIT_STATE_UNSPECIFIED"))
		}
		filter.States = append(filter.States, tenant.ImportUnitState(strings.ToLower(name)))
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListImportUnits(ctx, req.GetId(), filter, page)
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoJob)
	}
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListImportUnitsResponse{Units: make([]*v1.ImportUnit, 0, len(items)), NextPageToken: next}
	for _, u := range items {
		out.Units = append(out.Units, &v1.ImportUnit{
			Id: u.ID, Folder: u.Folder, State: enumOf[v1.ImportUnitState](v1.ImportUnitState_value, "IMPORT_UNIT_STATE_", string(u.State)),
			Counts: importCountsMessage(u.Counts), Reason: u.Reason, Attempts: int32(u.Attempts), //nolint:gosec // G115: at most 30 attempts.
			WalkEndTime: timestamp(u.WalkEnded),
		})
	}
	return out, nil
}
