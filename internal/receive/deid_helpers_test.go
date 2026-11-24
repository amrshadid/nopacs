package receive

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testDeidKeys are the tenant's keys of de-identification in these tests:
// the naming key the env's hasher holds, and a UID and a date key of their
// own.
var testDeidKeys = engine.Keys{
	UID:  bytes.Repeat([]byte{'u'}, 32),
	Date: bytes.Repeat([]byte{'d'}, 32),
	Hash: bytes.Repeat([]byte{9}, naming.MinKeyLength),
}

// testProfile is the profile version the receiver de-identifies with.
var testProfile = tenant.ProfileRef{Name: "research", Version: 1}

// basicProfile is PS3.15 Annex E's basic profile with the changes the
// public test files need for the check after to pass them, as a site's
// profile would: a segmentation's content label removed (liver_1frame: it names its maker, as Manufacturer
// does), the scan's options removed (CT: SCOUT MODE holds the series
// description the basic profile removes), and the device's maker and model
// removed, since a vendor's name can hold the institution's (MR_small:
// TOSHIBA in TOSHIBA_MEC).
func basicProfile() *v1.ProfileDocument {
	remove := &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	kw := func(k string) *v1.ProfileSelector {
		return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}
	}
	return &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, Rules: []*v1.ProfileRule{
		{Name: "content-label", Selector: kw("ContentLabel"), Action: remove},
		{Name: "scan-options", Selector: kw("ScanOptions"), Action: remove},
		{Name: "maker", Selector: kw("Manufacturer"), Action: remove},
		{Name: "model", Selector: kw("ManufacturerModelName"), Action: remove},
	}}
}

// deidOptions make the env's receiver de-identify.
type deidOptions struct {
	// doc is the profile's document; nil is basicProfile.
	doc *v1.ProfileDocument
	// check is the check after; nil is a new recordingCheck.
	check *recordingCheck
	// pixelLimit, when set, is the pipeline's reading of deid.pixel-limit
	// (Config.PixelLimit).
	pixelLimit Limit
	// asReceived leaves the env's receiver archiving as received: the
	// profile is stored and the pipeline given the keys and the check, for
	// a test that adds a receiver that de-identifies of its own.
	asReceived bool
}

// setUpDeid stores the profile as testProfile, points the receiver at it,
// and gives the pipeline the keys and the check.
func (e *env) setUpDeid(o *deidOptions) {
	e.t.Helper()
	doc := o.doc
	if doc == nil {
		doc = basicProfile()
	}
	if o.check == nil {
		o.check = &recordingCheck{}
	}
	e.storeProfile(testProfile, doc)
	e.cfg.PixelLimit = o.pixelLimit
	if o.asReceived {
		e.cfg.DeidKeys = func(context.Context) (engine.Keys, error) { return testDeidKeys, nil }
		e.cfg.Check = func(int64) Checker { return o.check }
		e.check = o.check
		return
	}
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", Profile: testProfile},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		e.t.Fatal(err)
	}
	e.cfg.DeidKeys = func(context.Context) (engine.Keys, error) { return testDeidKeys, nil }
	e.cfg.Check = func(int64) Checker { return o.check }
	e.check = o.check
}

// storeProfile stores doc as the version ref names, unless the tenant
// holds it already (a second process of a crash test).
func (e *env) storeProfile(ref tenant.ProfileRef, doc *v1.ProfileDocument) {
	e.t.Helper()
	v, err := profile.New(ref.Name, ref.Version, doc, profile.Shipped())
	if err != nil {
		e.t.Fatalf("the test profile: %v", err)
	}
	profiles, ok := e.store.(tenant.Profiles)
	if !ok {
		e.t.Fatal("the tenant store keeps no profiles")
	}
	if _, err := profiles.ProfileVersion(e.ctx, ref.Name, ref.Version); err == nil {
		return
	}
	if _, _, err := profiles.CreateProfileVersion(e.ctx, tenant.NewProfileVersion{
		Name: ref.Name, Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum(),
	}, t0, e.audit(), nil); err != nil {
		e.t.Fatal(err)
	}
}

// recordingCheck is the check after de-identification of package check,
// which keeps, by the de-identified file's SOP Instance UID, the needles it
// was given, and which a test can tell to find a value in a file.
type recordingCheck struct {
	mu      sync.Mutex
	real    *check.Checker
	needles map[string]engine.Needles
	calls   int
	// find, when set, is asked about each file by its SOP Instance UID
	// before the real check; a residual it answers is the verdict.
	find func(uid string) *check.Residual
	// during, when set, is called with the file being checked, after it is
	// read: a test that changes the file under the check.
	during func(src io.ReaderAt)
}

// CheckWritten implements Checker.
func (c *recordingCheck) CheckWritten(ctx context.Context, src io.ReaderAt, size int64, needles engine.Needles, written engine.Written, characterSet *string) (check.Report, error) {
	h, err := header.Read(src, size)
	if err != nil {
		return check.Report{}, err
	}
	u, _ := h.UID(header.SOPInstanceUID)
	c.mu.Lock()
	c.calls++
	if c.needles == nil {
		c.needles = map[string]engine.Needles{}
	}
	c.needles[u] = append(engine.Needles(nil), needles...)
	find, during := c.find, c.during
	if c.real == nil {
		c.real = check.New(check.Options{})
	}
	real := c.real
	c.mu.Unlock()
	if find != nil {
		if r := find(u); r != nil {
			return check.Report{Total: r.Total}, r
		}
	}
	rep, err := real.CheckWritten(ctx, src, size, needles, written, characterSet)
	if during != nil {
		during(src)
	}
	return rep, err
}

// setFind replaces what the check finds.
func (c *recordingCheck) setFind(f func(uid string) *check.Residual) {
	c.mu.Lock()
	c.find = f
	c.mu.Unlock()
}

// remapped returns the UID a de-identifying receiver of the tests holds
// for an original.
func remapped(u string) string { return engine.RemapUID(testDeidKeys.UID, u) }

// fileUIDs returns a file's study, series and instance UIDs.
func fileUIDs(t *testing.T, b []byte) (study, series, inst string) {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	study, _ = h.UID(header.StudyUID)
	series, _ = h.UID(header.SeriesUID)
	inst, _ = h.UID(header.SOPInstanceUID)
	return study, series, inst
}

// rewrite reads a file whole with the DICOM library, lets change change its
// data set, and writes it again in the transfer syntax ts: a synthetic
// variant of a public file.
func rewrite(t *testing.T, b []byte, ts string, change func(ds *dataset.Dataset)) []byte {
	t.Helper()
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(b)), filereader.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ds := df.GetDataset()
	if change != nil {
		change(ds)
	}
	if ts == "" {
		ts = df.FileMetaInfo.TransferSyntaxUID
	}
	path := filepath.Join(t.TempDir(), "rewritten.dcm")
	f, err := os.Create(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDataset(f, ds, ts, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// withoutPatientID returns a file without its Patient ID (0010,0020).
func withoutPatientID(t *testing.T, b []byte) []byte {
	t.Helper()
	return rewrite(t, b, "", func(ds *dataset.Dataset) { ds.Remove(tag.New(0x0010, 0x0020)) })
}

// deflatedCorpus returns a public MR image written again as Deflated
// Explicit VR Little Endian (PS3.5 A.5).
func deflatedCorpus(t *testing.T) []byte {
	t.Helper()
	return rewrite(t, corpus(t, "MR_small.dcm"), header.TransferSyntaxDeflated, nil)
}

// fileUIDsB is fileUIDs for a benchmark.
func fileUIDsB(b *testing.B, body []byte) (study, series, inst string) {
	b.Helper()
	h, err := header.Read(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		b.Fatal(err)
	}
	study, _ = h.UID(header.StudyUID)
	series, _ = h.UID(header.SeriesUID)
	inst, _ = h.UID(header.SOPInstanceUID)
	return study, series, inst
}
