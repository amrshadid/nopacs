package api_test

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testParts stands for the reception pipeline in the tests: it records what
// the handlers hand it to release, retry and remove, releases and retries
// through the store as a pipeline does around its own steps, and answers a
// header as a test sets it.
type testParts struct {
	mu        sync.Mutex
	discarded []string
	rejected  []string
	released  []string
	retried   []string
	// header, when set, answers Header; otherwise no header can be read.
	header atomic.Pointer[func(f tenant.QuarantinedFile, limit int) (string, bool, error)]
	// heldFile, when set, answers HeldFile; otherwise no held file can be
	// read.
	heldFile atomic.Pointer[func(f tenant.QuarantinedFile) ([]byte, error)]
}

// HeldFile answers as heldFile says, or ErrNoHeldFile.
func (p *testParts) HeldFile(_ context.Context, _ string, f tenant.QuarantinedFile, _ int64) ([]byte, error) {
	if h := p.heldFile.Load(); h != nil {
		return (*h)(f)
	}
	return nil, api.ErrNoHeldFile
}

// Release records the record, as "<tenant>/<id>", and releases it through
// the store.
func (p *testParts) Release(ctx context.Context, tenantName string, store tenant.Pipeline, rel tenant.Release, audit master.AuditEntry, guard tenant.Guard) (tenant.QuarantinedFile, error) {
	p.mu.Lock()
	p.released = append(p.released, tenantName+"/"+rel.ID)
	p.mu.Unlock()
	return store.ReleaseFile(ctx, rel, audit, guard)
}

// Retry records the reception, as "<tenant>/<id>", and retries it through
// the store.
func (p *testParts) Retry(ctx context.Context, tenantName string, store tenant.Pipeline, id, newReceptionID, receiver string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Reception, tenant.Reception, error) {
	p.mu.Lock()
	p.retried = append(p.retried, tenantName+"/"+id)
	p.mu.Unlock()
	return store.RetryReception(ctx, id, newReceptionID, receiver, now, audit, guard)
}

// RemoveDiscarded records the reception, as "<tenant>/<id>".
func (p *testParts) RemoveDiscarded(_ context.Context, tenantName, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.discarded = append(p.discarded, tenantName+"/"+id)
	return nil
}

// RemoveRejected records the place, as "<tenant>/<incoming>".
func (p *testParts) RemoveRejected(_ context.Context, tenantName, incoming string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rejected = append(p.rejected, tenantName+"/"+incoming)
	return nil
}

// Header answers as the test set, or api.ErrNoHeader.
func (p *testParts) Header(_ context.Context, _ string, f tenant.QuarantinedFile, limit int) (string, bool, error) {
	if h := p.header.Load(); h != nil {
		return (*h)(f, limit)
	}
	return "", false, api.ErrNoHeader
}

// decisions returns what was handed over to release and retry so far.
func (p *testParts) decisions() (released, retried []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.released), slices.Clone(p.retried)
}

// removals returns what was handed over to remove so far.
func (p *testParts) removals() (discarded, rejected []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.discarded), slices.Clone(p.rejected)
}

// AddStorage keeps the sealed secret of an S3 storage it adds, so
// StorageSecret can hand it back.
func (s pgStore) AddStorage(ctx context.Context, st tenant.Storage, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Storage, error) {
	out, err := s.Pipeline.AddStorage(ctx, st, now, audit, guard)
	if err == nil && len(st.S3.SealedSecret) > 0 {
		s.f.secrets.Store(s.name+"/"+st.Name, slices.Clone(st.S3.SealedSecret))
	}
	return out, err
}

// SetStorage keeps a new sealed secret, as AddStorage does.
func (s pgStore) SetStorage(ctx context.Context, st tenant.Storage, fields []tenant.StorageField, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Storage, error) {
	out, err := s.Pipeline.SetStorage(ctx, st, fields, now, audit, guard)
	if err == nil && slices.Contains(fields, tenant.FieldS3SecretKey) {
		s.f.secrets.Store(s.name+"/"+st.Name, slices.Clone(st.S3.SealedSecret))
	}
	return out, err
}

// StorageSecret hands back the sealed secret it kept, as a store with the
// operation the handlers ask for would.
func (s pgStore) StorageSecret(_ context.Context, name string) ([]byte, error) {
	v, ok := s.f.secrets.Load(s.name + "/" + name)
	if !ok {
		return nil, fault.New(fault.NotFound, "no such storage")
	}
	return v.([]byte), nil
}

// placeFixture is the fixture with the installation's places set: a root
// "fast" in the test's directory, ports allotted to lab1, and a second
// tenant, lab2, with an admin of its own.
type placeFixture struct {
	*fixture
	// root is the root's directory; lab1's directories are under
	// root/lab1.
	root      string
	lab2Admin caller
}

// newPlaceFixture returns the place fixture.
func newPlaceFixture(t *testing.T) *placeFixture {
	t.Helper()
	f := &placeFixture{fixture: newFixture(t)}
	f.root = filepath.Join(f.dir, "roots", "fast")
	if err := os.MkdirAll(f.root, 0o700); err != nil {
		t.Fatal(err)
	}
	f.tenant("lab2")
	f.lab2Admin, _ = f.key(f.onTenant("lab2", master.RoleAdmin))
	f.setSetting(installation(), settings.StorageDirectoryRoots, "fast="+f.root)
	f.setSetting(tenantScope("lab1"), settings.ReceiverPorts, "11112,11120-11129")
	// The receivers of these tests archive as received, which a tenant made
	// now allows only when the installation's admin says so.
	f.setSetting(tenantScope("lab1"), settings.ReceiverAllowAsReceived, "true")
	return f
}

// setSetting sets a setting as the local operator, failing the test if it
// is refused.
func (e *env) setSetting(scope *v1.Scope, name, value string) {
	e.t.Helper()
	o := call(e.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: name, Scope: scope, Value: value}})
	if o.err != nil {
		e.t.Fatalf("setting %s to %q: %v", name, value, o.err)
	}
}

// pipelineOf returns a tenant's pipeline, as the handlers reach it.
func (e *env) pipelineOf(name string) tenant.Pipeline {
	e.t.Helper()
	st, err := e.registry.Store(name)
	if err != nil {
		e.t.Fatal(err)
	}
	p, ok := st.(tenant.Pipeline)
	if !ok {
		e.t.Fatalf("%s's store keeps no pipeline", name)
	}
	return p
}

// dirStorage is a directory storage as a request gives it.
func dirStorage(name, root, path string) *v1.Storage {
	return &v1.Storage{Name: name, Type: &v1.Storage_Directory{Directory: &v1.DirectoryStorage{Root: root, Path: path}}}
}

// s3Storage is an S3 storage as a request gives it.
func s3Storage(name, endpoint, secret string) *v1.Storage {
	return &v1.Storage{Name: name, Type: &v1.Storage_S3{S3: &v1.S3Storage{
		Endpoint: endpoint, Region: "us-east-1", Bucket: "archive", Prefix: "lab/", PathStyle: true,
		AccessKeyId: "AKIATEST", SecretAccessKey: secret,
	}}}
}

// addStorage is a call of AddStorage.
func addStorage(tenantName string, s *v1.Storage) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.storages.AddStorage, &v1.AddStorageRequest{Tenant: tenantName, Storage: s})
	}
}

// mask is a field mask of paths.
func mask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

// dimseReceiver is a DIMSE receiver as a request gives it: on port, archived
// to storage, admitting one caller by AE title and range.
func dimseReceiver(name, storage string, port int32) *v1.Receiver {
	return &v1.Receiver{
		Name: name, Storage: storage, Enabled: true,
		Type: &v1.Receiver_Dimse{Dimse: &v1.DimseReceiver{
			Port: port, AeTitle: "NOPACS",
			Callers: &v1.DimseReceiver_AllowedCallers{AllowedCallers: &v1.AllowedCallers{Rules: []*v1.CallerRule{{CallingAeTitle: "PRISMA1", AddressRange: "10.20.0.0/24"}}}},
		}},
	}
}

// addReceiver is a call of AddReceiver.
func addReceiver(tenantName string, r *v1.Receiver) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.receivers.AddReceiver, &v1.AddReceiverRequest{Tenant: tenantName, Receiver: r})
	}
}

// mustCall fails the test unless o succeeded, and returns its answer.
func mustCall[T any](t *testing.T, what string, o outcome) T {
	t.Helper()
	if o.err != nil {
		t.Fatalf("%s: %v", what, o.err)
	}
	return o.res.(T)
}

// receiving sets lab1 up to receive: a directory storage "main" and a
// receiver "scanner1" on it.
func (f *placeFixture) receiving(t *testing.T) {
	t.Helper()
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	mustCall[*v1.AddReceiverResponse](t, "add scanner1", addReceiver("lab1", dimseReceiver("scanner1", "main", 11112))(f.as(local)))
}

// failedReception makes a reception of study at receiver in lab1 that
// failed, as the pipeline would after a storage stopped answering, and
// returns its id.
func (f *placeFixture) failedReception(t *testing.T, receiver, study string) string {
	t.Helper()
	p := f.pipelineOf("lab1")
	id := f.newID()
	if _, _, err := p.OpenReception(ctx, receiver, study, id, f.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.StepReception(ctx, tenant.ReceptionStep{ID: id, From: reception.Receiving, Event: reception.Fail, At: f.now(), Failure: "storage unreachable"}, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// heldFile holds a file in lab1's quarantine, as the pipeline would, and
// returns its id: a file whose label could not be made, its bytes kept,
// or, when tooLarge, a file over the receiver's bound, its bytes not kept.
func (f *placeFixture) heldFile(t *testing.T, receiver string, tooLarge bool) string {
	t.Helper()
	id := f.newID()
	q := tenant.QuarantinedFile{
		ID: id, Reason: tenant.QuarantineBadLabel, Receiver: receiver,
		Details:     map[string]string{"label": "subject", "attribute": "PatientID", "fault": "missing"},
		Caller:      tenant.Caller{CallingAETitle: "PRISMA1", CalledAETitle: "NOPACS", Address: "10.20.0.31:50288"},
		Size:        528650,
		SHA256:      sha256.Sum256([]byte(id)),
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", SOPInstanceUID: "2.25.9" + strings.TrimPrefix(id, "id-"),
		StudyUID: "2.25.1234567890", SeriesUID: "2.25.222", TransferSyntaxUID: "1.2.840.10008.1.2.1",
		Incoming: "q/" + id, ContentKept: true, Quarantined: f.now(),
	}
	if tooLarge {
		q.Reason, q.Details = tenant.QuarantineTooLarge, map[string]string{"limit": "1024"}
		q.Incoming, q.ContentKept, q.SHA256 = "", false, tenant.SHA256{}
	}
	if _, err := f.pipelineOf("lab1").HoldFiles(ctx, []tenant.QuarantinedFile{q}); err != nil {
		t.Fatal(err)
	}
	return id
}

// refusedNotInTenant fails unless the request was recorded refused in the
// installation's log, and nothing of it is in the tenant's log: a refused
// caller causes no write in a tenant's database.
func (e *env) refusedNotInTenant(t *testing.T, name, tenantName string, o outcome) {
	t.Helper()
	e.checkAudit(t, name, o.requestID, wantAudit{outcome: master.OutcomeRefused})
	if got := entriesOf(e.tenantLog(tenantName), o.requestID); len(got) != 0 {
		t.Errorf("%s: %d entries of a refused request in the tenant's log", name, len(got))
	}
}

// seconds is a duration of n seconds as a request gives it.
func seconds(n int64) *durationpb.Duration { return &durationpb.Duration{Seconds: n} }
