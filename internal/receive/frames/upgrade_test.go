package frames

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

var (
	upT0       = time.Date(2026, 11, 18, 9, 0, 0, 123456000, time.UTC)
	profileRef = tenant.ProfileRef{Name: "research", Version: 2}
	profileSum = strings.Repeat("5a", 32)
)

// upCatalog is a catalog of scans with their sessions, instances and the
// profile versions, answering as the tenant port does, two to a page.
type upCatalog struct {
	mu       sync.Mutex
	scans    []tenant.CatalogScan
	sessions map[string]tenant.CatalogSession
	route    tenant.ReceptionRoute
	held     map[string][]tenant.ScanInstance
	versions map[string][]tenant.ProfileVersion
	// onHeld, when set, is called with the scan's label at each read of its
	// instances; an error it returns fails the read.
	onHeld func(scan string) error
	// heldReads counts the reads of instances.
	heldReads int
}

func (c *upCatalog) ListScans(_ context.Context, _ tenant.ScanFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	start := 0
	for i, s := range c.scans {
		if page.Cursor != "" && s.Label == page.Cursor {
			start = i + 1
		}
	}
	end := min(start+2, len(c.scans))
	out := tenant.CatalogPage[tenant.CatalogScan]{Items: c.scans[start:end]}
	if end < len(c.scans) {
		out.Next = c.scans[end-1].Label
	}
	return out, nil
}

func (c *upCatalog) CatalogScan(_ context.Context, p label.Path) (tenant.CatalogScan, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.scans {
		if s.Label == p.Scan {
			return s, nil
		}
	}
	return tenant.CatalogScan{}, fault.New(fault.NotFound, "no such scan")
}

func (c *upCatalog) CatalogSession(_ context.Context, subject, session string) (tenant.CatalogSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sessions[subject+"/"+session]; ok {
		return s, nil
	}
	return tenant.CatalogSession{}, fault.New(fault.NotFound, "no such session")
}

func (c *upCatalog) CatalogLabels(context.Context, string, []string) (tenant.CatalogLabels, error) {
	return tenant.CatalogLabels{Session: &tenant.SessionLabels{Route: c.route}}, nil
}

func (c *upCatalog) ListHeldInstances(_ context.Context, p label.Path, page master.Page) ([]tenant.ScanInstance, string, error) {
	c.mu.Lock()
	c.heldReads++
	hook := c.onHeld
	rows := c.held[p.Scan]
	c.mu.Unlock()
	if hook != nil {
		if err := hook(p.Scan); err != nil {
			return nil, "", err
		}
	}
	start := 0
	for i, r := range rows {
		if page.Cursor != "" && r.SOPInstanceUID == page.Cursor {
			start = i + 1
		}
	}
	end := min(start+2, len(rows))
	next := ""
	if end < len(rows) {
		next = rows[end-1].SOPInstanceUID
	}
	return rows[start:end], next, nil
}

func (c *upCatalog) ListProfileVersions(_ context.Context, name string, _ master.Page) ([]tenant.ProfileVersion, string, error) {
	vs, ok := c.versions[name]
	if !ok {
		return nil, "", fault.New(fault.NotFound, "no such profile")
	}
	return vs, "", nil
}

// upArchive is an archive of n scans of three instances each in one
// session: a version 2 sidecar on the storage for each, and the catalog's
// scans and rows. The second instance of every scan was de-identified under
// profileRef, with a received checksum of its own.
type upArchive struct {
	*archive
	cat *upCatalog
}

// scanLabel names the i-th scan.
func scanLabel(i int) string { return fmt.Sprintf("scan-%02d", i) }

func newUpArchive(t *testing.T, n int) *upArchive {
	t.Helper()
	u := &upArchive{archive: newArchive(t), cat: &upCatalog{
		sessions: map[string]tenant.CatalogSession{"SUB/SES": {SubjectLabel: "SUB", Label: "SES", StudyUID: "1.2.3", StudyDate: "20240131", Receiver: "scanner-a"}},
		route:    tenant.ReceptionRoute{Route: "trial", Profile: profileRef},
		held:     map[string][]tenant.ScanInstance{},
		versions: map[string][]tenant.ProfileVersion{"research": {{Name: "research", Version: 2, Checksum: profileSum}, {Name: "research", Version: 1, Checksum: strings.Repeat("11", 32)}}},
	}}
	for i := 1; i <= n; i++ {
		u.addScan(i)
	}
	return u
}

// sum returns a checksum made of b.
func sum(b byte) tenant.SHA256 {
	var s tenant.SHA256
	for i := range s {
		s[i] = b
	}
	return s
}

// addScan writes the i-th scan's version 2 sidecar and lists the scan.
func (u *upArchive) addScan(i int) {
	u.t.Helper()
	path := label.Path{Subject: "SUB", Session: "SES", Scan: scanLabel(i)}
	created := upT0.Add(time.Duration(i) * time.Hour)
	u.cat.scans = append(u.cat.scans, tenant.CatalogScan{SubjectLabel: "SUB", SessionLabel: "SES", Label: path.Scan, SeriesUID: fmt.Sprintf("1.2.3.%d", i),
		Storage: "main", Receiver: receiverOf(i), Created: created, Updated: created.Add(time.Minute),
		Values: tenant.ScanValues{Modality: "CT", SeriesNumber: seriesNumber(i), HasSeriesNumber: true}})
	var b bytes.Buffer
	w := sidecar.NewWriter(&b)
	var rows []tenant.ScanInstance
	for j := 1; j <= 3; j++ {
		uid := fmt.Sprintf("1.2.3.%d.%d", i, j)
		key, err := layout.InstanceKey(path, uid)
		if err != nil {
			u.t.Fatal(err)
		}
		row := tenant.ScanInstance{SOPInstanceUID: uid, ReceptionID: fmt.Sprintf("rcp-%d", i), Storage: "main", Key: key, Size: 1000,
			SHA256: sum(byte(j)), Held: created.Add(time.Duration(j) * time.Second)}
		line := sidecar.Instance{SOPInstanceUID: uid, Key: key, Size: 1000, SHA256: sum(byte(j)).String(), TransferSyntax: "1.2.840.10008.1.2.1", DatasetOmitted: true}
		if j == 2 {
			row.ArchivedSHA256, row.Profile = sum(0xAA), profileRef
			line.SHA256, line.Profile = row.ArchivedSHA256.String(), profileRef.String()
		}
		rows = append(rows, row)
		if err := w.Add(line); err != nil {
			u.t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		u.t.Fatal(err)
	}
	u.put(path, b.Bytes())
	u.cat.held[path.Scan] = rows
}

// receiverOf is the receiver the catalog holds for the i-th scan: none for
// the fourth, as for a scan archived before the catalog kept its receiver.
func receiverOf(i int) string {
	if i == 4 {
		return ""
	}
	return "scanner-b"
}

// seriesNumber is the series number the catalog holds for the i-th scan: i,
// which the tests keep small.
func seriesNumber(i int) int32 {
	if i < 0 || i > math.MaxInt32 {
		panic("a scan's index is a series number")
	}
	return int32(i)
}

// put writes the scan's sidecar.
func (u *upArchive) put(path label.Path, b []byte) {
	u.t.Helper()
	key, _ := layout.MetadataKey(path)
	if _, err := u.store.Store.Put(u.ctx, key, bytes.NewReader(b)); err != nil {
		u.t.Fatal(err)
	}
}

// sidecarBytes returns the scan's sidecar.
func (u *upArchive) sidecarBytes(i int) []byte {
	u.t.Helper()
	b, _, _ := u.sidecarOf(label.Path{Subject: "SUB", Session: "SES", Scan: scanLabel(i)})
	return b
}

// config returns the pass's configuration over the archive.
func (u *upArchive) config() UpgradeConfig {
	return UpgradeConfig{
		Catalog:     u.cat,
		OpenStorage: func(context.Context, string) (blob.Store, error) { return u.store, nil },
		Lock:        u.locks.lock,
		Clock:       u.clock,
	}
}

// wantV3 checks that the i-th scan's sidecar is what the pass writes from
// the catalog: the header, and each line with the catalog's reception,
// time, received checksum and profile checksum, nothing the catalog does not
// know.
func (u *upArchive) wantV3(i int) {
	u.t.Helper()
	path := label.Path{Subject: "SUB", Session: "SES", Scan: scanLabel(i)}
	b, version, _ := u.sidecarOf(path)
	rd := sidecar.NewReader(bytes.NewReader(b))
	var lines []sidecar.Instance
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			u.t.Fatalf("scan %d: %v", i, err)
		}
		lines = append(lines, in)
	}
	created := upT0.Add(time.Duration(i) * time.Hour)
	number := seriesNumber(i)
	wantHead := sidecar.ScanHeader{StudyUID: "1.2.3", SeriesUID: fmt.Sprintf("1.2.3.%d", i), SessionReceiver: "scanner-a", Receiver: receiverOf(i),
		Route:  &sidecar.Route{Name: "trial", Profile: profileRef.String()},
		Values: sidecar.Values{StudyDate: "20240131", Modality: "CT", SeriesNumber: &number}, Created: created, Updated: created.Add(time.Minute)}
	if version != sidecar.Version || !reflect.DeepEqual(rd.Scan(), &wantHead) || len(lines) != 3 {
		u.t.Fatalf("scan %d: version %d header %+v (want %+v), %d lines", i, version, rd.Scan(), wantHead, len(lines))
	}
	for j, in := range lines {
		want := sidecar.Instance{SOPInstanceUID: in.SOPInstanceUID, Key: in.Key, Size: 1000, SHA256: sum(byte(j + 1)).String(), TransferSyntax: "1.2.840.10008.1.2.1",
			DatasetOmitted: true, Reception: fmt.Sprintf("rcp-%d", i), HeldAt: created.Add(time.Duration(j+1) * time.Second), ReceivedSyntax: "1.2.840.10008.1.2.1"}
		if j == 1 {
			want.SHA256, want.ReceivedSHA256, want.Profile, want.ProfileSHA256 = sum(0xAA).String(), sum(2).String(), profileRef.String(), profileSum
			want.ReceivedSyntax = ""
		}
		if !reflect.DeepEqual(in, want) {
			u.t.Errorf("scan %d line %d is\n%+v\nwant\n%+v", i, j, in, want)
		}
	}
}

// TestUpgradeRewritesFromTheCatalog guards the rewrite: every scan's
// version 2 sidecar becomes version 3 with the header and each line's values
// from the catalog (the reception, the time held, the received checksum of
// a de-identified instance, its profile version's checksum, the syntax of
// one archived as received), each replaced once and under the scan's lock,
// and a pass run again reads only the first bytes of each and writes
// nothing.
func TestUpgradeRewritesFromTheCatalog(t *testing.T) {
	u := newUpArchive(t, 5)
	rep, err := Upgrade(u.ctx, u.config())
	if err != nil || rep.ScansDone != 5 || rep.InstancesDone != 15 || rep.ScansChecked != 5 || rep.ScansLeft != 0 || rep.ScansWaiting != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	for i := 1; i <= 5; i++ {
		u.wantV3(i)
	}
	if u.store.unlocked != 0 {
		t.Errorf("%d sidecars were written without the scan's lock", u.store.unlocked)
	}
	for key, n := range u.store.puts {
		if strings.HasPrefix(key, layout.MetaRoot) && n != 1 {
			t.Errorf("%s was put %d times", key, n)
		}
	}
	puts := map[string]int{}
	for k, v := range u.store.puts {
		puts[k] = v
	}
	before := u.sidecarBytes(3)
	u.store.reads = map[string][]int64{}
	rep, err = Upgrade(u.ctx, u.config())
	if err != nil || rep.ScansDone != 0 || rep.ScansChecked != 5 {
		t.Fatalf("second pass: %+v %v", rep, err)
	}
	if !reflect.DeepEqual(u.store.puts, puts) || !bytes.Equal(before, u.sidecarBytes(3)) {
		t.Error("a second pass wrote")
	}
	for key, reads := range u.store.reads {
		if len(reads) != 1 || reads[0] != int64(sidecar.HeadLength) {
			t.Errorf("%s was read %v, want one read of the first %d bytes", key, reads, sidecar.HeadLength)
		}
	}
	if len(u.store.reads) != 5 {
		t.Errorf("%d objects were read, want the 5 sidecars' first bytes", len(u.store.reads))
	}
	if u.cat.heldReads != 5*2 {
		t.Errorf("the catalog's rows were read %d times, want two pages a scan once", u.cat.heldReads)
	}
}

// TestUpgradeResumesAfterAnInterruption guards the pass's restart: a pass
// cut off by its context in the middle of a scan leaves the scans before it
// rewritten and the rest as they were, a pass run again does the rest and
// does not rewrite the scans done, and a put cut off midway leaves the
// sidecar it was replacing whole.
func TestUpgradeResumesAfterAnInterruption(t *testing.T) {
	u := newUpArchive(t, 5)
	original := map[int][]byte{}
	for i := 1; i <= 5; i++ {
		original[i] = u.sidecarBytes(i)
	}

	ctx, cancel := context.WithCancel(u.ctx)
	u.cat.onHeld = func(scan string) error {
		if scan == scanLabel(3) {
			cancel()
		}
		return nil
	}
	rep, err := Upgrade(ctx, u.config())
	if err == nil || rep.ScansDone != 2 {
		t.Fatalf("cut off: %+v %v", rep, err)
	}
	u.wantV3(1)
	u.wantV3(2)
	for i := 3; i <= 5; i++ {
		if !bytes.Equal(u.sidecarBytes(i), original[i]) {
			t.Errorf("scan %d changed though the pass was cut off before it", i)
		}
	}

	// A put cut off midway on the fourth scan's sidecar.
	u.cat.onHeld = nil
	fourth, _ := layout.MetadataKey(label.Path{Subject: "SUB", Session: "SES", Scan: scanLabel(4)})
	failing := true
	u.store.failPut = func(key string) bool { return failing && key == fourth }
	rep, err = Upgrade(u.ctx, u.config())
	if err == nil || rep.ScansDone != 1 {
		t.Fatalf("put cut off: %+v %v", rep, err)
	}
	u.wantV3(3)
	if !bytes.Equal(u.sidecarBytes(4), original[4]) || !bytes.Equal(u.sidecarBytes(5), original[5]) {
		t.Error("a sidecar whose replacement was cut off is not as it was")
	}

	failing = false
	puts := map[string]int{}
	for k, v := range u.store.puts {
		puts[k] = v
	}
	rep, err = Upgrade(u.ctx, u.config())
	if err != nil || rep.ScansDone != 2 || rep.InstancesDone != 6 {
		t.Fatalf("resumed: %+v %v", rep, err)
	}
	for i := 1; i <= 5; i++ {
		u.wantV3(i)
	}
	for _, i := range []int{1, 2, 3} {
		key, _ := layout.MetadataKey(label.Path{Subject: "SUB", Session: "SES", Scan: scanLabel(i)})
		if u.store.puts[key] != puts[key] {
			t.Errorf("scan %d, done before, was rewritten again", i)
		}
	}
}

// TestUpgradeLeavesWhatTheCatalogDisagreesWith guards that a scan whose
// sidecar and rows are not the same instances with the same bytes is left as
// it is and counted, with the rest of the pass going on: a line with no row,
// a row with no line, another checksum, another profile version.
func TestUpgradeLeavesWhatTheCatalogDisagreesWith(t *testing.T) {
	u := newUpArchive(t, 6)
	u.cat.held[scanLabel(1)] = u.cat.held[scanLabel(1)][:2]
	extra := u.cat.held[scanLabel(2)][2]
	extra.SOPInstanceUID = "1.2.3.2.9"
	u.cat.held[scanLabel(2)] = append(append([]tenant.ScanInstance{}, u.cat.held[scanLabel(2)]...), extra)
	u.cat.held[scanLabel(3)] = append([]tenant.ScanInstance{}, u.cat.held[scanLabel(3)]...)
	u.cat.held[scanLabel(3)][0].SHA256, u.cat.held[scanLabel(3)][0].ArchivedSHA256 = sum(0xEE), tenant.SHA256{}
	u.cat.held[scanLabel(4)] = append([]tenant.ScanInstance{}, u.cat.held[scanLabel(4)]...)
	u.cat.held[scanLabel(4)][1].Profile = tenant.ProfileRef{Name: "research", Version: 1}
	original := map[int][]byte{}
	for i := 1; i <= 6; i++ {
		original[i] = u.sidecarBytes(i)
	}
	rep, err := Upgrade(u.ctx, u.config())
	if err != nil || rep.ScansDone != 2 || rep.ScansLeft != 4 {
		t.Fatalf("%+v %v", rep, err)
	}
	for _, i := range []int{1, 2, 3, 4} {
		if !bytes.Equal(u.sidecarBytes(i), original[i]) {
			t.Errorf("scan %d, which the catalog disagrees with, was changed", i)
		}
	}
	u.wantV3(5)
	u.wantV3(6)
}

// TestUpgradeWaitsForFrameTables guards that a sidecar that still has no
// frame tables (version 1) is left to the pass that gives them, counted, and
// not rewritten without them.
func TestUpgradeWaitsForFrameTables(t *testing.T) {
	u := newUpArchive(t, 2)
	path := label.Path{Subject: "SUB", Session: "SES", Scan: scanLabel(1)}
	old := []byte(`{"version":1,"instances":[` + "\n" + `{"sop_instance_uid":"1.2.3.1.1","key":"` + u.cat.held[scanLabel(1)][0].Key + `","size":1000,"sha256":"` +
		sum(1).String() + `","pixel_data":null,"dataset":null,"dataset_omitted":true}` + "\n" + `]}` + "\n")
	u.put(path, old)
	rep, err := Upgrade(u.ctx, u.config())
	if err != nil || rep.ScansWaiting != 1 || rep.ScansDone != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if !bytes.Equal(u.sidecarBytes(1), old) {
		t.Error("a sidecar of version 1 was rewritten")
	}
}

// TestUpgradeStopsOnAFailureToRead guards that a catalog or a storage that
// does not answer stops the pass with its error and leaves the sidecar as
// it was, for the next pass: it is not taken for a disagreement.
func TestUpgradeStopsOnAFailureToRead(t *testing.T) {
	u := newUpArchive(t, 3)
	before := u.sidecarBytes(2)
	boom := errors.New("the database is gone")
	u.cat.onHeld = func(scan string) error {
		if scan == scanLabel(2) {
			return boom
		}
		return nil
	}
	rep, err := Upgrade(u.ctx, u.config())
	if err == nil || fault.KindOf(err) != fault.Unavailable || rep.ScansDone != 1 || rep.ScansLeft != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if !bytes.Equal(u.sidecarBytes(2), before) {
		t.Error("the sidecar changed")
	}
	u.cat.onHeld = nil
	u.store.failRead = func(key string) error {
		if strings.Contains(key, scanLabel(3)) && strings.HasSuffix(key, layout.MetadataName) {
			return errors.New("the storage is gone")
		}
		return nil
	}
	rep, err = Upgrade(u.ctx, u.config())
	if err == nil || fault.KindOf(err) != fault.Unavailable || rep.ScansDone != 1 {
		t.Fatalf("a storage that does not answer: %+v %v", rep, err)
	}
}

// TestUpgradeLeavesAProfileVersionTheCatalogLacks guards that a line whose
// profile version the catalog no longer lists is written without that
// version's checksum rather than refused, and that the profile's versions
// are read once for all the scans that name it.
func TestUpgradeLeavesAProfileVersionTheCatalogLacks(t *testing.T) {
	u := newUpArchive(t, 3)
	u.cat.versions = map[string][]tenant.ProfileVersion{}
	rep, err := Upgrade(u.ctx, u.config())
	if err != nil || rep.ScansDone != 3 {
		t.Fatalf("%+v %v", rep, err)
	}
	b, _, lines := u.sidecarOf(label.Path{Subject: "SUB", Session: "SES", Scan: scanLabel(2)})
	if lines[1].Profile != profileRef.String() || lines[1].ProfileSHA256 != "" || !bytes.Contains(b, []byte(`"profile":"research@2"`)) {
		t.Errorf("line %+v", lines[1])
	}
}
