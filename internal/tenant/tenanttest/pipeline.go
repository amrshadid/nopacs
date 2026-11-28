package tenanttest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// OpenPipeline is Open for a store that implements tenant.Pipeline.
type OpenPipeline func(tb testing.TB, tenantID string) tenant.Pipeline

// OpenPipelinePair is OpenPair for stores that implement tenant.Pipeline.
type OpenPipelinePair func(tb testing.TB, tenantID string) (first, second tenant.Pipeline)

// runPipeline runs the contract suite's pipeline groups (tenant.Pipeline)
// against the stores open makes, each test on its own store, in parallel. Run
// runs it.
func runPipeline(t *testing.T, open OpenPipeline) {
	tests := []struct {
		name string
		fn   func(*penv)
	}{
		{"Storages", testStorages},
		{"StorageSetPartial", testStorageSetPartial},
		{"StorageInUse", testStorageInUse},
		{"Receivers", testReceivers},
		{"ReceiverNameHeld", testReceiverNameHeld},
		{"ChangeGuards", testChangeGuards},
		{"IdentifyStorage", testIdentifyStorage},
		{"BindAddressClash", testBindAddressClash},
		{"STOWReceivers", testSTOWReceivers},
		{"ReceiverDeidentification", testReceiverDeidentification},
		{"DeidentifiedRows", testDeidentifiedRows},
		{"MustDeidentifyRows", testMustDeidentifyRows},
		{"ReleasedProfile", testReleasedProfile},
		{"EndpointSpelling", testEndpointSpelling},
		{"ConfigPagingWithDeletes", testConfigPagingWithDeletes},
		{"OpenReception", testOpenReception},
		{"OpenReceptionConcurrent", testOpenReceptionConcurrent},
		{"OpenReceptionSameID", testOpenReceptionSameID},
		{"ReceptionSteps", testReceptionSteps},
		{"ReceptionStepRefusals", testReceptionStepRefusals},
		{"StepHook", testStepHook},
		{"ReceptionList", testReceptionList},
		{"ReceptionPagingWithInserts", testReceptionPagingWithInserts},
		{"RetryReception", testRetryReception},
		{"DiscardReception", testDiscardReception},
		{"ReceivedFiles", testReceivedFiles},
		{"ReceivedBatchRepeated", testReceivedBatchRepeated},
		{"ReceivedFilesClosed", testReceivedFilesClosed},
		{"ReceivedBatchOrder", testReceivedBatchOrder},
		{"ReceivedFilesAtEnd", testReceivedFilesAtEnd},
		{"Claims", testClaims},
		{"ClaimsOutliveTheirReception", testClaimsOutliveTheirReception},
		{"CommitScan", testCommitScan},
		{"InstanceUIDOncePerTenant", testInstanceUIDOncePerTenant},
		{"ReleaseIntoSameReception", testReleaseIntoSameReception},
		{"DiscardKeepsQuarantined", testDiscardKeepsQuarantined},
		{"DiscardLeavesLivePlaces", testDiscardLeavesLivePlaces},
		{"HoldCountsReceived", testHoldCountsReceived},
		{"CountIdentity", testCountIdentity},
		{"QuarantineHold", testQuarantineHold},
		{"QuarantineDetails", testQuarantineDetails},
		{"QuarantineRelease", testQuarantineRelease},
		{"QuarantineReject", testQuarantineReject},
		{"QuarantineTotals", testQuarantineTotals},
		{"QuarantineList", testQuarantineList},
		{"WorkClaim", testWorkClaim},
		{"WorkUniqueKey", testWorkUniqueKey},
		{"WorkUniqueKeyFirst", testWorkUniqueKeyFirst},
		{"WorkFencing", testWorkFencing},
		{"WorkRetries", testWorkRetries},
		{"WorkConcurrentClaims", testWorkConcurrentClaims},
		{"WorkChain", testWorkChain},
		{"WorkGivesUpItsReception", testWorkGivesUpItsReception},
		{"ReceptionsWithoutWork", testReceptionsWithoutWork},
		{"CommitSharesSubject", testCommitSharesSubject},
		{"CreatedIsEarliestHeld", testCreatedIsEarliestHeld},
		{"PipelineBounds", testPipelineBounds},
		{"StorageSecret", testStorageSecret},
		{"Secrets", testTenantSecrets},
		{"ReleaseReason", testReleaseReason},
		{"FencedFiles", testFencedFiles},
		{"IncomingNamed", testIncomingNamed},
		{"ClaimNextWork", testClaimNextWork},
		{"ReclaimWorkOfKinds", testReclaimWorkOfKinds},
		{"ScanInstances", testScanInstances},
		{"NewQuarantineReasons", testNewQuarantineReasons},
		{"PipelineTenantsApart", testPipelineTenantsApart},
		{"CatalogGets", testCatalogGets},
		{"CatalogLists", testCatalogLists},
		{"CatalogRefusals", testCatalogRefusals},
		{"CatalogCursors", testCatalogCursors},
		{"CatalogInstances", testCatalogInstances},
		{"CatalogValuesKept", testCatalogValuesKept},
		{"CatalogValuesFilled", testCatalogValuesFilled},
		{"CatalogTotals", testCatalogTotals},
		{"CatalogLargest", testCatalogLargest},
		{"CatalogOriginals", testCatalogOriginals},
		{"SearchMatchesRouting", testSearchMatchesRouting},
		{"SearchWritten", testSearchWritten},
		{"SearchRefusals", testSearchRefusals},
		{"SearchCountsAndFacets", testSearchCountsAndFacets},
		{"SearchHidesOriginals", testSearchHidesOriginals},
		{"SearchPagesHoldWhileArchiving", testSearchPagesHoldWhileArchiving},
		{"SearchStudyScope", testSearchStudyScope},
		{"SearchTextWithNUL", testSearchTextWithNUL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := open(t, SuiteTenant)
			tt.fn(&penv{env: &env{T: t, s: p, ctx: context.Background()}, p: p, open: open})
		})
	}
}

// runPipelineShared runs the pipeline's tests that need two instances of
// the store on one database.
func runPipelineShared(t *testing.T, open OpenPipelinePair) {
	tests := []struct {
		name string
		fn   func(e *penv, other tenant.Pipeline)
	}{
		{"ClaimsAcrossInstances", testClaimsShared},
		{"OpenAcrossInstances", testOpenShared},
		{"BatchRepeatedAcrossInstances", testBatchShared},
		{"InstanceClaimsAcrossInstances", testInstanceClaimsShared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first, second := open(t, SuiteTenant)
			tt.fn(&penv{env: &env{T: t, s: first, ctx: context.Background()}, p: first}, second)
		})
	}
}

// penv is a pipeline test's store and its helpers; it has env's too.
type penv struct {
	*env
	p    tenant.Pipeline
	open OpenPipeline
	// row is the search row the next commits of the catalog fixtures give
	// their scans; the zero row gives none.
	row search.Row
}

// at returns T0 plus seconds.
func at(seconds int) time.Time { return T0.Add(time.Duration(seconds) * time.Second) }

// uidOf returns a valid UID unique to n and kind (1 for studies, 2 for
// series, 3 for instances), under the 2.25 root of UUID-derived UIDs
// (PS3.5 B.2), so no real UID can match it.
func uidOf(kind, n int) string { return fmt.Sprintf("2.25.%d.%d", kind, n+1) }

// sum returns a checksum made from n, never zero.
func sum(n int) tenant.SHA256 {
	var s tenant.SHA256
	for i := range s {
		s[i] = byte((n + i + 1) & 0xff)
	}
	s[0] = byte((n>>8)&0x7f) | 0x80
	return s
}

// wantIs fails unless err holds target in its chain and has the kind.
func (e *penv) wantIs(what string, err error, target error, kind fault.Kind) {
	e.Helper()
	if !errors.Is(err, target) || fault.KindOf(err) != kind {
		e.Errorf("%s: %v (kind %v), want %v of kind %v", what, err, fault.KindOf(err), target, kind)
	}
}

// wantPrecondition fails unless err is a precondition failure: its kind is
// fault.Precondition, which is what a caller answers by, whatever sentinel
// names the case.
func (e *penv) wantPrecondition(what string, err error) {
	e.Helper()
	e.wantKind(what, err, fault.Precondition)
}

// dirStorage returns a directory storage named name under root "fast", of
// the online tier, as a store answers it.
func dirStorage(name, path string) tenant.Storage {
	return tenant.Storage{Name: name, Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: path},
		Tier: tenant.TierOnline}
}

// s3Storage returns an S3 storage with a sealed secret, of the online tier.
func s3Storage(name, prefix string) tenant.Storage {
	return tenant.Storage{Name: name, Type: tenant.StorageS3, Tier: tenant.TierOnline, S3: tenant.S3Storage{
		Endpoint: "https://s3.example.org", Region: "us-east-1", Bucket: "lab-archive", Prefix: prefix,
		PathStyle: true, AccessKeyID: "AKIDEXAMPLE", SealedSecret: []byte("sealed-secret-bytes"),
	}}
}

// receiverOf returns a DIMSE receiver with the contract's defaults.
func receiverOf(name, storage string, port int) tenant.Receiver {
	return tenant.Receiver{
		Name: name, Type: tenant.ReceiverDIMSE, Storage: storage,
		DIMSE:       tenant.DIMSEReceiver{Port: port, AETitle: "ARCHIVE"},
		QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes,
	}
}

// addStorage adds a directory storage, failing the test on an error.
func (e *penv) addStorage(name string) tenant.Storage {
	e.Helper()
	s, err := e.p.AddStorage(e.ctx, dirStorage(name, name), T0, e.audit(), nil)
	e.ok("AddStorage", err)
	return s
}

// addReceiver adds a receiver on a storage it adds first when missing.
func (e *penv) addReceiver(name, storage string, port int) tenant.Receiver {
	e.Helper()
	if _, err := e.p.Storage(e.ctx, storage); fault.KindOf(err) == fault.NotFound {
		e.addStorage(storage)
	}
	r, err := e.p.AddReceiver(e.ctx, receiverOf(name, storage, port), T0, e.audit(), nil)
	e.ok("AddReceiver", err)
	return r
}

// openReception opens a reception of study n at the receiver.
func (e *penv) openReception(receiver string, study int, when time.Time) tenant.Reception {
	e.Helper()
	r, _, err := e.p.OpenReception(e.ctx, receiver, uidOf(1, study), e.id("rc"), when)
	e.ok("OpenReception", err)
	return r
}

// file returns received file n of study for the reception.
func file(r tenant.Reception, study, n int) tenant.ReceivedFile {
	return tenant.ReceivedFile{
		ReceptionID: r.ID, SOPInstanceUID: uidOf(3, n), SeriesUID: uidOf(2, study), StudyUID: r.StudyUID,
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1",
		Size: int64(1000 + n), SHA256: sum(n), Incoming: fmt.Sprintf("%s/f%06d.dcm", r.ID, n),
		Caller:      tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"},
		Association: "assoc1",
	}
}

// path is the main line of a reception's steps after receiving: each event
// and the state it leads to.
var path = []struct {
	event reception.Event
	to    reception.State
}{
	{reception.Quiet, reception.Complete}, {reception.Identify, reception.Identified},
	{reception.Archive, reception.Archived}, {reception.Verify, reception.Verified}, {reception.Finish, reception.Done},
}

// stepTo moves a reception along the main line until it is in state to,
// identifying it with storage.
func (e *penv) stepTo(r tenant.Reception, to reception.State, storage string) tenant.Reception {
	e.Helper()
	from := reception.Receiving
	for _, s := range path {
		if r.State == to {
			return r
		}
		if r.State != from {
			from = s.to
			continue
		}
		from = s.to
		step := tenant.ReceptionStep{ID: r.ID, From: r.State, Event: s.event, At: r.LastFile.Add(time.Minute)}
		if s.event == reception.Identify {
			step.SubjectLabel, step.SessionLabel, step.Storage = "sub-01", "ses-"+r.ID, storage
		}
		var err error
		r, err = e.p.StepReception(e.ctx, step, nil)
		e.ok("StepReception "+string(s.event), err)
	}
	if r.State != to {
		e.Fatalf("the reception is %s, want %s", r.State, to)
	}
	return r
}

// failReception moves a reception from its state to failed.
func (e *penv) failReception(r tenant.Reception) tenant.Reception {
	e.Helper()
	r, err := e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Fail,
		At: r.LastFile.Add(time.Hour), Failure: "storage unreachable"}, nil)
	e.ok("StepReception fail", err)
	return r
}

// labelsOf returns the labels the suite gives a reception's study and a
// series: the catalog's when it has them, and otherwise ones made from the
// UIDs, as a naming rule of UIDs would.
func (e *penv) labelsOf(studyUID, seriesUID string) label.Path {
	e.Helper()
	l, err := e.p.CatalogLabels(e.ctx, studyUID, []string{seriesUID})
	e.ok("CatalogLabels", err)
	p := label.Path{Subject: "sub-01", Session: "ses-" + studyUID, Scan: "scan-" + seriesUID}
	if l.Session != nil {
		p.Subject, p.Session = l.Session.SubjectLabel, l.Session.SessionLabel
	}
	if v, ok := l.Scans[seriesUID]; ok {
		p.Scan = v
	}
	return p
}

// commit claims files of an identified reception (all of one series) and
// commits those it claimed into their scan, as the archive step does once
// they are on the storage; it returns the claim's answers.
func (e *penv) commit(r tenant.Reception, files ...tenant.ReceivedFile) []tenant.Duplicate {
	e.Helper()
	claims := make([]tenant.InstanceClaim, len(files))
	for i, f := range files {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
	}
	answers, err := e.p.ClaimInstances(e.ctx, r.ID, claims, r.StepTime, nil)
	e.ok("ClaimInstances", err)
	e.confirm(r, files, answers)
	return answers
}

// confirm commits into their scans the files whose claim answered claimed
// now or before, grouped by series, in one commit per series.
func (e *penv) confirm(r tenant.Reception, files []tenant.ReceivedFile, answers []tenant.Duplicate) {
	e.Helper()
	bySeries := map[string][]tenant.ReceivedFile{}
	var order []string
	for i, f := range files {
		if k := answers[i].Kind; k != tenant.ClaimedNow && k != tenant.ClaimedBefore {
			continue
		}
		if _, ok := bySeries[f.SeriesUID]; !ok {
			order = append(order, f.SeriesUID)
		}
		bySeries[f.SeriesUID] = append(bySeries[f.SeriesUID], f)
	}
	for _, series := range order {
		p := e.labelsOf(r.StudyUID, series)
		storage := r.Storage
		if l, err := e.p.CatalogLabels(e.ctx, r.StudyUID, nil); err == nil && l.Session != nil {
			storage = l.Session.Storage
		}
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: series, SubjectLabel: p.Subject,
			SessionLabel: p.Session, ScanLabel: p.Scan, Storage: storage, ManifestSHA256: sum(len(series)), At: r.StepTime}
		for _, f := range bySeries[series] {
			key, err := layout.InstanceKey(p, f.SOPInstanceUID)
			e.ok("InstanceKey", err)
			c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key,
				Length: f.Size, Size: f.Size, ArchivedSHA256: f.SHA256})
		}
		_, err := e.p.CommitScan(e.ctx, c)
		e.ok("CommitScan", err)
	}
}

// allPages calls list until it returns no cursor, and returns every item.
func allPages[T any](e *penv, limit int, list func(master.Page) ([]T, string, error)) []T {
	e.Helper()
	var out []T
	page := master.Page{Limit: limit}
	for range 100000 {
		items, next, err := list(page)
		e.ok("list", err)
		out = append(out, items...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
	e.Fatal("a list did not end")
	return nil
}
