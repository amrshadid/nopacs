package receive

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// riskFile is a public ultrasound image in JPEG baseline: a modality
// commonly drawn on, Burned In Annotation absent, so it is at risk of
// burned-in text (PS3.15 E.3.1) and held unless a pixel rule decides.
const riskFile = "jpeg_3frame_nobot.dcm"

// pixelRiskOf archives riskFile through a receiver whose profile's one pixel
// rule is rule, and returns what the catalog, the metadata file and the
// stream of a job to an unmarked sender say of its accepted pixel risk:
// the catalog row's, the sidecar line's, and the refusal tenant.SendRefusal
// gives the instance as the job's stream answers it.
func pixelRiskOf(t *testing.T, rule *v1.PixelRule) (catalog, line bool, refusal tenant.JobFailureCode) {
	t.Helper()
	e := newEnv(t, envOptions{deid: &deidOptions{doc: pixelDoc(rule)}})
	e.send(riskFile)
	r := e.settle(1)[0]
	if !done(r) || r.Counts.FilesArchived != 1 {
		t.Fatalf("reception %s %+v, held %v", r.State, r.Counts, e.held())
	}
	scans, _, err := e.store.ReceptionScans(e.ctx, r.ID)
	if err != nil || len(scans) != 1 {
		t.Fatalf("the reception's scans: %v, %v", scans, err)
	}
	path := label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: scans[0].Label}
	held, _, err := e.store.ListHeldInstances(e.ctx, path, master.Page{Limit: 10})
	if err != nil || len(held) != 1 {
		t.Fatalf("the scan's instances: %v, %v", held, err)
	}
	if held[0].Profile.IsZero() {
		t.Fatal("the instance records no profile version")
	}
	catalog = held[0].PixelRiskAccepted

	objects := archivedObjects(t, e, r)
	rd := sidecar.NewReader(bytes.NewReader(objects["sidecar:"+scans[0].Label]))
	lines := 0
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		lines++
		line = in.PixelRiskAccepted
	}
	if lines != 1 {
		t.Fatalf("the metadata holds %d lines", lines)
	}

	// A job to a sender not marked, its stream read as the send worker
	// reads it, each instance judged by the rule the worker asks.
	plain, err := e.store.AddSender(e.ctx, tenant.Sender{Name: "plain", Type: tenant.SenderDIMSE, DIMSE: tenant.DIMSESender{
		Host: "pacs.lab.example", Port: 104, CalledAETitle: "PACS", CallingAETitle: "NOPACS"}}, t0, e.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := e.clock.Now()
	if _, err := e.store.CreateJob(e.ctx, tenant.NewJob{ID: "job00000001", Sender: plain.Name,
		Target: tenant.JobTarget{SubjectLabel: r.SubjectLabel, SessionLabel: r.SessionLabel}, Created: now}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	claim := func() tenant.Work {
		t.Helper()
		w, err := e.store.ClaimWork(e.ctx, tenant.SendKind(plain.Name), "test", now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.StartJobUnit(e.ctx, w.Fence(), now); err != nil {
			t.Fatal(err)
		}
		return w
	}
	lister := claim()
	for more := true; more; {
		if _, more, err = e.store.ListJobScans(e.ctx, lister.Fence(), now); err != nil {
			t.Fatal(err)
		}
	}
	stream, err := e.store.JobScanInstances(e.ctx, claim().Fence(), "", 10)
	if err != nil || len(stream) != 1 {
		t.Fatalf("the job's stream: %v, %v", stream, err)
	}
	refusal, _ = tenant.SendRefusal(stream[0].Provenance(), plain.AllowIdentified)
	return catalog, line, refusal
}

// TestAnAcceptedPixelRiskIsRecordedAndRefused guards the record of an
// accepted pixel risk from reception to send: an image at risk of burned-in
// text that a pixel rule accepts as it is is archived with the catalog row
// and the metadata line saying so, and a sender not marked refuses it as
// pixel_risk_accepted; the same image under a rule that fills its corners
// is recorded without it and let out.
func TestAnAcceptedPixelRiskIsRecordedAndRefused(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		accept := &v1.PixelRule{Name: "accept", Reason: "reviewed by the site", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}}
		catalog, line, refusal := pixelRiskOf(t, accept)
		if !catalog || !line || refusal != tenant.FailurePixelRiskAccepted {
			t.Errorf("catalog %v, metadata %v, refusal %q; want true, true, pixel_risk_accepted", catalog, line, refusal)
		}
	})
	t.Run("redacted", func(t *testing.T) {
		catalog, line, refusal := pixelRiskOf(t, fillRule())
		if catalog || line || refusal != "" {
			t.Errorf("catalog %v, metadata %v, refusal %q; want false, false, none", catalog, line, refusal)
		}
	})
}
