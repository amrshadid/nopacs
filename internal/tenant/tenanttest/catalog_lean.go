package tenanttest

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The rows a driver's test writes into a tenant at the schema before the
// lean catalog, for CheckLeanCatalog to read through the port after the
// migration. Each is named for what it guards.
const (
	// LeanStudy is the one session's Study Instance UID.
	LeanStudy = "2.25.1.1"
	// LeanSeries is the one scan's Series Instance UID; the scan is
	// Sub-A/ses-1/scan-1 on the storage "store".
	LeanSeries = "2.25.2.1"
	// LeanLayoutUID is held with the layout's key and an archived checksum
	// equal to its received one, which the migration keeps neither of.
	LeanLayoutUID = "2.25.3.1"
	// LeanKeptUID is held with a key that is not the layout's (LeanKeptKey)
	// and another archived checksum, which the migration keeps.
	LeanKeptUID = "2.25.3.2"
	// LeanKeptKey is LeanKeptUID's key, kept as it was.
	LeanKeptKey = "k/2.25.3.2"
	// LeanClaimedUID is claimed by the reception "rc-open", identified and
	// not ended.
	LeanClaimedUID = "2.25.3.9"
	// LeanDone ended done and confirmed both held instances.
	LeanDone = "rc-done"
	// LeanQuarantined ended quarantined.
	LeanQuarantined = "rc-quar"
	// LeanFailed ended failed.
	LeanFailed = "rc-failed"
	// LeanOpen is archiving: identified, not ended.
	LeanOpen = "rc-open"
	// LeanReceivedUID is the file each of the four receptions received.
	LeanReceivedUID = "2.25.3.5"
)

// LeanSum is the checksum the rows written for CheckLeanCatalog give a
// UID: the SHA-256 of its text.
func LeanSum(uid string) tenant.SHA256 { return sha256.Sum256([]byte(uid)) }

// LeanArchivedSum is LeanKeptUID's archived checksum: the SHA-256 of its
// text with "x" after.
func LeanArchivedSum() tenant.SHA256 { return sha256.Sum256([]byte(LeanKeptUID + "x")) }

// CheckLeanCatalog checks, through the port, a tenant migrated to the lean
// catalog from the rows the constants above describe: each held instance
// keeps its key and its archived checksum as they were, whether the
// migration kept them or makes them again; a held and a claimed UID are
// both still answered with their study, series, checksum and reception; the
// scans a reception confirmed are counted; and the received files of the
// receptions that ended done or quarantined are gone, while those of a
// failed one and of one in progress stay.
func CheckLeanCatalog(t *testing.T, p tenant.Pipeline) {
	t.Helper()
	ctx := context.Background()
	path := label.Path{Subject: "Sub-A", Session: "ses-1", Scan: "scan-1"}
	held, _, err := p.ListHeldInstances(ctx, path, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	layoutKey, err := layout.InstanceKey(path, LeanLayoutUID)
	if err != nil {
		t.Fatal(err)
	}
	want := []tenant.ScanInstance{
		{SOPInstanceUID: LeanLayoutUID, ReceptionID: LeanDone, Storage: "store", Key: layoutKey, Length: 10, Size: 10,
			SHA256: LeanSum(LeanLayoutUID), ArchivedSHA256: LeanSum(LeanLayoutUID)},
		{SOPInstanceUID: LeanKeptUID, ReceptionID: LeanDone, Storage: "store", Key: LeanKeptKey, Length: 20, Size: 20,
			SHA256: LeanSum(LeanKeptUID), ArchivedSHA256: LeanArchivedSum()},
	}
	if len(held) != len(want) {
		t.Fatalf("held instances after the migration: %+v", held)
	}
	for i := range want {
		got := held[i]
		got.Held = want[i].Held
		if got != want[i] {
			t.Errorf("held instance %d after the migration: %+v, want %+v", i, held[i], want[i])
		}
	}
	one, err := p.HeldInstance(ctx, path, LeanLayoutUID)
	if err != nil || one.Key != layoutKey || one.Storage != "store" {
		t.Errorf("an instance by its UID after the migration: %+v, %v", one, err)
	}
	ds, err := p.CheckDuplicates(ctx, []tenant.DuplicateQuery{
		{SOPInstanceUID: LeanLayoutUID, StudyUID: LeanStudy, SeriesUID: LeanSeries, SHA256: LeanSum(LeanLayoutUID)},
		{SOPInstanceUID: LeanClaimedUID, StudyUID: LeanStudy, SeriesUID: LeanSeries, SHA256: LeanSum(LeanClaimedUID)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := ds[0]; d.Kind != tenant.SameFile || d.HeldStudyUID != LeanStudy || d.HeldSeriesUID != LeanSeries || d.HeldBy != LeanDone {
		t.Errorf("a held UID after the migration: %+v", d)
	}
	if d := ds[1]; d.Kind != tenant.Claimed || d.HeldStudyUID != LeanStudy || d.HeldSeriesUID != LeanSeries || d.HeldBy != LeanOpen ||
		d.HeldSHA256 != LeanSum(LeanClaimedUID) {
		t.Errorf("a claimed UID after the migration: %+v", d)
	}
	scans, more, err := p.ReceptionScans(ctx, LeanDone)
	if err != nil || more || len(scans) != 1 || scans[0] != (tenant.ReceptionScan{Label: "scan-1", SeriesUID: LeanSeries, Instances: 2, Bytes: 30}) {
		t.Errorf("the scans a reception confirmed after the migration: %+v, %v, %v", scans, more, err)
	}
	for id, keeps := range map[string]bool{LeanDone: false, LeanQuarantined: false, LeanFailed: true, LeanOpen: true} {
		files, _, err := p.ReceivedFiles(ctx, id, master.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if keeps != (len(files) == 1) {
			t.Errorf("reception %s keeps %d received files after the migration, want them kept: %v", id, len(files), keeps)
		}
	}
}
