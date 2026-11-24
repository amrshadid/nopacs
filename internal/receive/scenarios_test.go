package receive

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// byArrival returns the receptions in order of arrival number.
func byArrival(rs []tenant.Reception) []tenant.Reception {
	out := make([]tenant.Reception, len(rs))
	copy(out, rs)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].Arrival < out[i].Arrival || (out[j].Arrival == out[i].Arrival && out[j].Started.Before(out[i].Started)) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// TestTwoArrivalsMakeOneSession guards the quiet period: the same study in
// two parts with a gap longer than the quiet period is two receptions, two
// arrivals of one study, in one session, the second's scan beside the
// first's.
func TestTwoArrivalsMakeOneSession(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send(studyFiles[:2]...)
	first := e.settle(1)
	e.clock.Advance(time.Minute)
	e.send(studyFiles[2:]...)
	rs := byArrival(e.settle(2))
	if len(rs) != 2 || !done(rs[0]) || !done(rs[1]) || rs[0].Arrival != 1 || rs[1].Arrival != 2 {
		t.Fatalf("receptions %+v", rs)
	}
	if rs[0].SessionLabel != rs[1].SessionLabel || rs[0].SubjectLabel != rs[1].SubjectLabel || rs[0].ID != first[0].ID {
		t.Fatalf("the two arrivals are not one session: %+v", rs)
	}
	if rs[0].Counts.FilesArchived != 2 || rs[1].Counts.FilesArchived != 5 {
		t.Fatalf("counts %+v / %+v", rs[0].Counts, rs[1].Counts)
	}
	totals, _ := e.store.CatalogTotals(e.ctx)
	if totals.Sessions != 1 || totals.Scans != 2 || totals.Instances != 7 {
		t.Fatalf("totals %+v", totals)
	}
	checkArchive(t, e, rs[1], sentMap(t, studyFiles[2:]...))
}

// TestVerifySeesAManifestMissingAnotherReceptionsInstance guards the whole
// check of verify (scanListed): two arrivals of one series write one scan;
// when the scan's manifest on the storage is left without the instance the
// first arrival confirmed (as another process writing the scan could leave
// it), the second arrival's verify fails rather than let its incoming
// copies go, though every instance it confirmed itself is listed.
func TestVerifySeesAManifestMissingAnotherReceptionsInstance(t *testing.T) {
	const firstUID = "1.3.6.1.4.1.5962.1.1.0.0.0.1194734704.16302.0.3"
	var e *env
	var checks, failed atomic.Int64
	e = newEnv(t, envOptions{hook: func(point string) {
		switch point {
		case "verify.before-check":
			if checks.Add(1) < 2 {
				return
			}
			page, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "meta/"})
			if err != nil {
				return
			}
			for _, k := range page.Keys {
				if !strings.HasSuffix(k, "/manifest.json") {
					continue
				}
				rc, err := e.storage.Read(e.ctx, k, 0, -1)
				if err != nil {
					return
				}
				m, err := manifest.Decode(rc)
				_ = rc.Close()
				if err != nil {
					return
				}
				kept := m.Files[:0]
				for _, f := range m.Files {
					if f.SOPInstanceUID != firstUID {
						kept = append(kept, f)
					}
				}
				b, err := manifest.Encode(manifest.Manifest{Files: kept})
				if err == nil {
					_, _ = e.storage.Put(e.ctx, k, bytes.NewReader(b))
				}
			}
		case "step.failed:" + kindVerify:
			failed.Add(1)
		}
	}})
	e.send(studyFiles[0])
	e.settle(1)
	e.clock.Advance(time.Minute)
	e.send(studyFiles[1])
	e.waitFor("the second arrival's verify to fail", time.Second, func() bool { return failed.Load() > 0 })
	for _, r := range e.receptions() {
		if r.Arrival == 2 && done(r) {
			t.Fatalf("the second arrival was finished with a manifest that misses an instance of its scan: %+v", r)
		}
	}
}

// TestStudySentTwiceIsIgnoredTheSecondTime guards duplicates: the same study
// sent again is a second reception whose files are all counted duplicate,
// none written again, and the incoming area empties.
func TestStudySentTwiceIsIgnoredTheSecondTime(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send(studyFiles...)
	e.settle(1)
	e.clock.Advance(time.Minute)
	e.send(studyFiles...)
	rs := byArrival(e.settle(2))
	second := rs[1]
	if !done(second) || second.Counts.FilesReceived != 7 || second.Counts.FilesDuplicate != 7 || second.Counts.FilesArchived != 0 {
		t.Fatalf("second reception %+v", second)
	}
	if got := e.p.Stats().EarlyDuplicates.Load(); got != 7 {
		t.Errorf("early duplicates %d, want 7", got)
	}
	totals, _ := e.store.CatalogTotals(e.ctx)
	if totals.Instances != 7 {
		t.Fatalf("totals %+v", totals)
	}
	if n := e.incomingFiles(); n != 0 {
		t.Fatalf("%d files left in the incoming area", n)
	}
}

// TestChangedInstanceIsHeld guards a different file under a known UID: the
// first is kept, the second held as a different file with both checksums,
// its bytes kept in the incoming area.
func TestChangedInstanceIsHeld(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send("MR_small.dcm")
	e.settle(1)
	e.clock.Advance(time.Minute)
	e.send("MR_small_RLE.dcm")
	rs := byArrival(e.settle(2))
	if !done(rs[1]) || rs[1].Counts.FilesQuarantined != 1 || rs[1].Counts.FilesArchived != 0 {
		// A reception none of whose files go on ends quarantined.
		if rs[1].State != reception.Quarantined {
			t.Fatalf("second reception %+v", rs[1])
		}
	}
	held := e.held()
	if len(held) != 1 || held[0].Reason != tenant.QuarantineDifferentFile || held[0].KeptSHA256.IsZero() || !held[0].ContentKept {
		t.Fatalf("held %+v", held)
	}
	if n := e.incomingFiles(); n != 1 {
		t.Fatalf("the incoming area holds %d files, want the held one", n)
	}
	checkArchive(t, e, rs[0], sentMap(t, "MR_small.dcm"))
}

// TestNotDICOMIsHeldAndTheRestArchived guards a file that is not DICOM in
// the middle of a study: it is held with its offset, kept, and the study is
// archived without it.
func TestNotDICOMIsHeldAndTheRestArchived(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send(studyFiles[:3]...)
	e.sendBytes([]byte("this is a text file, not a DICOM file\n"), Received)
	e.send(studyFiles[3:]...)
	rs := e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != 7 {
		t.Fatalf("reception %+v", rs[0])
	}
	held := e.held()
	if len(held) != 1 || held[0].Reason != tenant.QuarantineNotDICOM || held[0].Details["offset"] == "" || !held[0].ContentKept {
		t.Fatalf("held %+v", held)
	}
	if n := e.incomingFiles(); n != 1 {
		t.Fatalf("the incoming area holds %d files, want the held one", n)
	}
}

// TestFileTooLargeIsRefusedAndRecorded guards the size bound: a file over
// the receiver's largest file fails, nothing of it is kept, and a record
// without content says it came.
func TestFileTooLargeIsRefusedAndRecorded(t *testing.T) {
	e := newEnv(t, envOptions{maxFileBytes: 4096})
	out := e.sendBytes(corpus(t, "MR_small.dcm"), Failed)
	if out.Reason != reasonTooLarge {
		t.Fatalf("reason %q", out.Reason)
	}
	held := e.held()
	if len(held) != 1 || held[0].Reason != tenant.QuarantineTooLarge || held[0].ContentKept || held[0].Details["limit"] != "4096" {
		t.Fatalf("held %+v", held)
	}
	if n := e.incomingFiles(); n != 0 {
		t.Fatalf("a refused file left %d files", n)
	}
	e.send(studyFiles[0])
	e.settle(1)
}

// TestLabelsThatCannotBeMadeAreHeld guards the naming rule's failure: a rule
// that names an attribute the files lack holds every file with the label
// reason and its fault, and the reception ends quarantined.
func TestLabelsThatCannotBeMadeAreHeld(t *testing.T) {
	spec := naming.Spec{Subject: "{(0010,4000)}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"}
	e := newEnv(t, envOptions{naming: &spec})
	e.send(studyFiles...)
	rs := e.settle(1)
	if rs[0].State != reception.Quarantined || rs[0].Counts.FilesQuarantined != 7 {
		t.Fatalf("reception %+v", rs[0])
	}
	held := e.held()
	if len(held) != 7 {
		t.Fatalf("%d held", len(held))
	}
	for _, q := range held {
		if q.Reason != tenant.QuarantineBadLabel || q.Details["label"] != "subject" || q.Details["fault"] != tenant.FaultMissing || q.Details["attribute"] != "(0010,4000)" {
			t.Fatalf("held %+v", q)
		}
	}
	if n := e.incomingFiles(); n != 7 {
		t.Fatalf("the incoming area holds %d files, want the 7 held", n)
	}
}

// TestSOPClassNotAcceptedIsHeld guards the receiver's list of SOP classes.
func TestSOPClassNotAcceptedIsHeld(t *testing.T) {
	e := newEnv(t, envOptions{sopClasses: []string{"1.2.840.10008.5.1.4.1.1.2"}})
	e.send("MR_small.dcm")
	e.send(studyFiles[0])
	e.settle(1)
	held := e.held()
	if len(held) != 1 || held[0].Reason != tenant.QuarantineSOPClassNotAccepted || held[0].Details["sop_class_uid"] != "1.2.840.10008.5.1.4.1.1.4" {
		t.Fatalf("held %+v", held)
	}
}

// TestCompressedAndMultiFrameAreArchived guards files beyond plain slices:
// JPEG 2000, RLE and an enhanced multi-frame segmentation, each byte for
// byte with a sidecar that finds its pixel data.
func TestCompressedAndMultiFrameAreArchived(t *testing.T) {
	e := newEnv(t, envOptions{})
	names := []string{"MR_small_jp2klossless.dcm", "liver_1frame.dcm"}
	e.send(names...)
	rs := e.settle(2)
	for _, r := range rs {
		if !done(r) {
			t.Fatalf("reception %+v", r)
		}
		for _, n := range names {
			if studyOf(t, corpus(t, n)) == r.StudyUID {
				checkArchive(t, e, r, sentMap(t, n))
			}
		}
	}
}

// studyOf returns a file's Study Instance UID.
func studyOf(t *testing.T, b []byte) string {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := h.UID(header.StudyUID)
	return u
}

// bigHeader returns MR_small.dcm with a Patient Comments (0010,4000) of n
// spaces put first in its data set: a header of about n bytes before its
// pixel data, its UIDs and pixel data as they were.
func bigHeader(t *testing.T, n int) []byte {
	t.Helper()
	b := corpus(t, "MR_small.dcm")
	// The data set starts after the file meta information, whose length
	// its first element gives (PS3.10 7.1).
	ds := 144 + int(binary.LittleEndian.Uint32(b[140:144]))
	// UT, explicit VR little endian, with a four-byte length (PS3.5 7.1.2).
	el := []byte{0x10, 0x00, 0x00, 0x40, 'U', 'T', 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(el[8:], uint32(n)) // #nosec G115 -- a test's size.
	out := append(append([]byte{}, b[:ds]...), el...)
	out = append(out, bytes.Repeat([]byte{' '}, n)...)
	return append(out, b[ds:]...)
}

// TestAHeaderPastTheBoundIsKeptAndReleasedOnceItIsRaised guards the header
// bound as a setting read live: a file whose header is past the bound read
// (1 MiB) is held as header_too_large with its bytes kept and the bound in
// its details, nothing archived; once the bound read is raised (4 MiB), with
// no restart, the held file is released and archived, its bytes those sent.
func TestAHeaderPastTheBoundIsKeptAndReleasedOnceItIsRaised(t *testing.T) {
	var limit atomic.Int64
	limit.Store(1 << 20)
	e := newEnv(t, envOptions{headerLimit: func(context.Context) (int64, error) { return limit.Load(), nil }})
	file := bigHeader(t, 2<<20)
	e.sendBytes(file, Received)
	e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == 1 && e.p.batch.idle() })
	q := e.held()[0]
	if q.Reason != tenant.QuarantineHeaderTooLarge || !q.ContentKept || q.Details["limit"] != "1048576" || q.Incoming == "" {
		t.Fatalf("held %+v, want header_too_large with its bytes kept and the limit", q)
	}
	if ok, _ := e.p.area.Exists(q.Incoming); !ok {
		t.Fatal("the bytes of a header too large are not kept")
	}
	limit.Store(4 << 20)
	e.clock.Advance(2 * HeaderLimitInterval)
	out, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil)
	if err != nil {
		t.Fatalf("release %+v %v", out, err)
	}
	rs := e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != 1 {
		t.Fatalf("reception %+v", rs[0])
	}
	if n := e.incomingFiles(); n != 0 {
		t.Fatalf("%d incoming files", n)
	}
}

// TestPlacingUIDsReadWholeMustAgree guards a file whose first bytes and
// whole header place it differently: a corpus file with a second Study
// Instance UID appended after its pixel data, out of the order of tags.
// Handed in with the UIDs a source read from its first bytes (a STOW-RS
// part, checked against the study its request named), it is held as
// uid_mismatch with its bytes, never archived into the study its last
// value names; the same file unchanged, told the same, is archived.
func TestPlacingUIDsReadWholeMustAgree(t *testing.T) {
	e := newEnv(t, envOptions{})
	src := corpus(t, studyFiles[0])
	h, err := header.Read(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	told := Arrival{Receiver: "dimse", Caller: tenant.Caller{Address: "192.0.2.7:40000"}, TransferSyntaxUID: h.TransferSyntaxUID}
	told.SOPClassUID, _ = h.UID(header.SOPClassUID)
	told.SOPInstanceUID, _ = h.UID(header.SOPInstanceUID)
	told.StudyUID, _ = h.UID(header.StudyUID)
	told.SeriesUID, _ = h.UID(header.SeriesUID)

	// (0020,000D) UI "1.2.3.999\0", explicit VR little endian (PS3.5 7.1.2).
	value := "1.2.3.999\x00"
	elem := []byte{0x20, 0x00, 0x0D, 0x00, 'U', 'I', byte(len(value)), 0} // #nosec G115 -- a 10-byte value.
	moved := append(append(append([]byte{}, src...), elem...), value...)
	if hm, err := header.Read(bytes.NewReader(moved), int64(len(moved))); err != nil {
		t.Fatal(err)
	} else if v, _ := hm.UID(header.StudyUID); v != "1.2.3.999" {
		t.Fatalf("the whole header reads the study as %q", v)
	}

	a := told
	a.Body = bytes.NewReader(moved)
	if out := e.p.Accept(e.ctx, a); out.Status != Received {
		t.Fatalf("outcome %v (%s)", out.Status, out.Reason)
	}
	e.waitFor("the file held or archived", time.Second, func() bool { return len(e.held()) == 1 || len(e.receptions()) > 0 })
	if rs := e.receptions(); len(rs) > 0 {
		t.Fatalf("the file opened a reception of study %s", rs[0].StudyUID)
	}
	q := e.held()[0]
	if q.Reason != tenant.QuarantineUIDMismatch || q.Details["attribute"] != string(header.StudyUID) || !q.ContentKept || q.Size != int64(len(moved)) {
		t.Fatalf("held %+v", q)
	}

	a = told
	a.Body = bytes.NewReader(src)
	if out := e.p.Accept(e.ctx, a); out.Status != Received {
		t.Fatalf("outcome %v (%s)", out.Status, out.Reason)
	}
	rs := e.settle(1)
	if len(rs) != 1 || !done(rs[0]) || rs[0].StudyUID != told.StudyUID || len(e.held()) != 1 {
		t.Fatalf("receptions %+v, held %d", rs, len(e.held()))
	}
}
