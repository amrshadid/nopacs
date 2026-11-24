package receive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestHoldsAtLandingHoldNoOriginal guards every record a receiver that
// de-identifies makes when a file lands, for each reason the receiving
// side holds a file: it is marked de-identified, holds the keyed checksum
// of the bytes, never their SHA-256, and the keyed remapping of each
// placing UID it names, never the original.
func TestHoldsAtLandingHoldNoOriginal(t *testing.T) {
	ct := corpus(t, studyFiles[0])
	study, series, inst := fileUIDs(t, ct)
	class := "1.2.840.10008.5.1.4.1.1.2"
	noSeries := rewrite(t, ct, "", func(ds *dataset.Dataset) { ds.Remove(tag.New(0x0020, 0x000E)) })
	for _, c := range []struct {
		name   string
		opts   envOptions
		body   []byte
		told   Arrival
		reason tenant.QuarantineReason
		// uids are the placing UIDs the record holds, remapped; empty
		// for one the record cannot read.
		study, series, inst string
		// keyed says the record keeps a checksum, keyed.
		keyed bool
	}{
		{name: "SOP class not accepted", opts: envOptions{sopClasses: []string{"1.2.3.4"}}, body: ct,
			reason: tenant.QuarantineSOPClassNotAccepted, study: study, series: series, inst: inst, keyed: true},
		{name: "UIDs told otherwise", body: ct, told: Arrival{SOPClassUID: class, SOPInstanceUID: inst, StudyUID: "1.2.3", SeriesUID: series},
			reason: tenant.QuarantineUIDMismatch, study: study, series: series, inst: inst, keyed: true},
		{name: "a placing UID missing", body: noSeries, reason: tenant.QuarantineBadUID, study: study, inst: inst, keyed: true},
		{name: "not DICOM", body: []byte("plain text that a sender called a DICOM file\n"), reason: tenant.QuarantineNotDICOM, keyed: true},
		{name: "a header past the bound", opts: envOptions{headerLimit: func(context.Context) (int64, error) { return 256, nil }}, body: ct,
			reason: tenant.QuarantineHeaderTooLarge, keyed: true},
		{name: "a file too large", opts: envOptions{maxFileBytes: 1024}, body: ct, told: Arrival{SOPInstanceUID: inst},
			reason: tenant.QuarantineTooLarge, inst: inst},
	} {
		t.Run(c.name, func(t *testing.T) {
			opts := c.opts
			opts.deid = &deidOptions{}
			e := newEnv(t, opts)
			a := c.told
			a.Receiver, a.Body = "dimse", bytes.NewReader(c.body)
			e.p.Accept(e.ctx, a)
			e.waitFor("the hold", time.Second, func() bool { return len(e.held()) == 1 })
			q := e.held()[0]
			if q.Reason != c.reason {
				t.Fatalf("held %s %v, want %s", q.Reason, q.Details, c.reason)
			}
			if !q.Deidentified {
				t.Error("the record is not marked de-identified")
			}
			want := map[string][2]string{"study": {q.StudyUID, c.study}, "series": {q.SeriesUID, c.series}, "instance": {q.SOPInstanceUID, c.inst}}
			for what, v := range want {
				got, orig := v[0], v[1]
				switch {
				case orig == "" && got != "":
					t.Errorf("the record holds a %s UID it should not", what)
				case orig != "" && got != remapped(orig):
					t.Errorf("the record holds the %s UID %s, want the remapping of the original", what, got)
				}
			}
			sum := tenant.SHA256(sha256.Sum256(c.body))
			switch {
			case c.keyed && q.SHA256 != keyedSum(testDeidKeys, sum):
				t.Errorf("the record's checksum is not the keyed checksum of the bytes")
			case q.SHA256 == sum:
				t.Error("the record holds the bytes' SHA-256")
			}
		})
	}
}

// TestAProfileThatKeepsAPlacingUIDHoldsTheFile guards the comparison after
// de-identification: a profile whose own rule keeps SOP Instance UID (the
// API refuses it for a receiver; stored here past it) leaves the file with
// a UID its row does not hold, and the file is held (deid_failed,
// uid_not_remapped), never archived.
func TestAProfileThatKeepsAPlacingUIDHoldsTheFile(t *testing.T) {
	doc := basicProfile()
	doc.Rules = append(doc.Rules, &v1.ProfileRule{Name: "keep-sop",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "SOPInstanceUID"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}})
	e := newEnv(t, envOptions{deid: &deidOptions{doc: doc}})
	e.send(studyFiles[0])
	r := e.settle(1)[0]
	qs := e.held()
	if r.State != "quarantined" || len(qs) != 1 || qs[0].Reason != tenant.QuarantineDeidFailed || qs[0].Details["cause"] != "uid_not_remapped" {
		t.Fatalf("reception %s, held %+v", r.State, qs)
	}
}

// TestTheSOPClassIsTheSameInRowsAndFile guards the SOP Class UID at a
// receiver that de-identifies: a vendor's private class is kept as
// received in a quarantine record and its details, in the received file's
// row, and in the archived file, which the basic profile leaves it in, so
// what an operator reads agrees with what is archived and with what a
// receiver's list of classes holds.
func TestTheSOPClassIsTheSameInRowsAndFile(t *testing.T) {
	const private = "1.2.3.4.5.6.7"
	body := rewrite(t, corpus(t, studyFiles[0]), "", func(ds *dataset.Dataset) {
		ds.Remove(tag.New(0x0008, 0x0016))
		if err := ds.Add(dataelem.NewDataElement(tag.New(0x0008, 0x0016), dataelem.VR("UI"), []byte(private))); err != nil {
			t.Fatal(err)
		}
	})
	held := newEnv(t, envOptions{deid: &deidOptions{}, sopClasses: []string{"1.2.840.10008.5.1.4.1.1.2"}})
	held.sendBytes(body, Received)
	held.waitFor("the hold", time.Second, func() bool { return len(held.held()) == 1 })
	q := held.held()[0]
	if q.SOPClassUID != private || q.Details["sop_class_uid"] != private {
		t.Errorf("the record holds the class %s and the detail %s, want %s", q.SOPClassUID, q.Details["sop_class_uid"], private)
	}

	e := newEnv(t, envOptions{deid: &deidOptions{}})
	e.sendBytes(body, Received)
	e.waitFor("the file to be recorded", time.Second, func() bool { return e.p.stats.Recorded.Load() == 1 })
	files, _, err := e.store.ReceivedFiles(e.ctx, e.receptions()[0].ID, master.Page{Limit: 10})
	if err != nil || len(files) != 1 {
		t.Fatalf("%d files: %v", len(files), err)
	}
	if files[0].SOPClassUID != private {
		t.Errorf("the received file's row holds the class %s, want %s", files[0].SOPClassUID, private)
	}
	r := e.settle(1)[0]
	_, _, in := fileUIDs(t, body)
	obj, ok := archivedObjects(t, e, r)[remapped(in)]
	if !ok {
		t.Fatalf("not archived: reception %s, held %+v", r.State, e.held())
	}
	h, err := header.Read(bytes.NewReader(obj), int64(len(obj)))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := h.UID(header.SOPClassUID); got != private {
		t.Errorf("the archived file holds the class %s, want %s as the rows do", got, private)
	}
}
