package testkit

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// commitRuns counts the runs of BenchCommitScan in this process.
var commitRuns atomic.Uint64

// BenchCommitScan measures the catalog commit of one scan of one instance
// (a new session and scan of one subject, the confirmation of the instance
// and, when withSearch is set, the row a search table holds of the scan:
// every attribute filled), one transaction each, b.N times, into the tenant
// p as it is. The reception that leads to the commit is made with the timer
// stopped; only CommitScan is timed. The same benchmark with and without
// the row is the cost of keeping a search table. seed makes the UIDs and
// labels, so two runs into one tenant use different seeds.
func BenchCommitScan(b *testing.B, p tenant.Pipeline, withSearch bool, seed uint64) {
	b.Helper()
	ctx := context.Background()
	if err := ensureReceiver(ctx, p); err != nil {
		b.Fatal(err)
	}
	// A benchmark function runs again with a larger b.N into the same
	// tenant, so each run draws UIDs of its own.
	r := rand.New(rand.NewPCG(seed, commitRuns.Add(1))) // #nosec G404 -- a seeded generator, so a run makes the same UIDs.
	key := hashKey(seed)
	caller := tenant.Caller{CallingAETitle: "SCANNER1", CalledAETitle: "ARCHIVE", Address: "192.0.2.7:51234"}
	at := genEpoch
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		studyUID, seriesUID, sop, rid := uid25(r), uid25(r), uid25(r), hexOf(r, 16)
		subjectLabel := "sub-bench"
		sessionLabel := "ses-" + naming.HashText(key, []byte(studyUID))
		scanLabel := "scan-" + naming.HashText(key, []byte(seriesUID))
		rc, _, err := p.OpenReception(ctx, GenReceiver, studyUID, rid, at)
		if err != nil {
			b.Fatal(err)
		}
		f := tenant.ReceivedFile{ReceptionID: rid, SOPInstanceUID: sop, SeriesUID: seriesUID, StudyUID: studyUID,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: 524288,
			SHA256: tenant.SHA256(sha256.Sum256([]byte(sop))), Incoming: GenReceiver + "/" + rid[:2] + "/" + rid, Caller: caller, Association: rid}
		if _, err := p.AddReceivedFiles(ctx, []tenant.ReceivedFile{f}, at, nil); err != nil {
			b.Fatal(err)
		}
		for _, ev := range []reception.Event{reception.Quiet, reception.Identify} {
			st := tenant.ReceptionStep{ID: rid, From: rc.State, Event: ev, At: at}
			if ev == reception.Identify {
				st.SubjectLabel, st.SessionLabel, st.Storage = subjectLabel, sessionLabel, GenStorage
			}
			if rc, err = p.StepReception(ctx, st, nil); err != nil {
				b.Fatal(err)
			}
		}
		claims := []tenant.InstanceClaim{{SOPInstanceUID: sop, StudyUID: studyUID, SeriesUID: seriesUID, SHA256: f.SHA256}}
		if _, err := p.ClaimInstances(ctx, rid, claims, at, nil); err != nil {
			b.Fatal(err)
		}
		path := label.Path{Subject: subjectLabel, Session: sessionLabel, Scan: scanLabel}
		instanceKey, err := layout.InstanceKey(path, sop)
		if err != nil {
			b.Fatal(err)
		}
		c := tenant.ScanCommit{ReceptionID: rid, StudyUID: studyUID, SeriesUID: seriesUID, SubjectLabel: subjectLabel, SessionLabel: sessionLabel,
			ScanLabel: scanLabel, Storage: GenStorage, At: at, ManifestSHA256: tenant.SHA256(sha256.Sum256([]byte(seriesUID))),
			Values:    tenant.ScanValuesOf("20190101", "MR", "T1 MPRAGE", fmt.Sprint(i+1)),
			Instances: []tenant.ConfirmedInstance{{SOPInstanceUID: sop, Key: instanceKey, Length: f.Size, Size: f.Size, ArchivedSHA256: f.SHA256}}}
		if withSearch {
			c.Search = SearchRowOf(r, sop, "20190101", i+1)
		} else {
			c.Search = search.Row{}
		}
		at = at.Add(time.Second)
		b.StartTimer()
		if _, err := p.CommitScan(ctx, c); err != nil {
			b.Fatal(err)
		}
	}
}
