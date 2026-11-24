package send

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// benchScan is the scan BenchmarkSendScan sends each time: benchInstances
// small instances of benchPixels bytes of pixel data each, a normal-size
// scan that measures what a send costs per instance rather than the copy
// of large files.
const (
	benchInstances = 64
	benchPixels    = 16 << 10
)

// BenchmarkSendScan measures sending one archived scan of benchInstances
// small instances to a peer in the same process, by DIMSE (a go-dicom SCP
// that keeps what it is sent in memory) and by STOW-RS (an HTTP service on
// loopback that reads each part whole), with one association. Each
// iteration is a whole job: made, its scan listed, every instance read from
// the storage with its checksum checked, sent, answered and recorded, and
// the job ended done. It reports instances per second and MB of files per
// second.
//
// A job's end is seen by polling the database every 20 ms, as the tests'
// waitJob does, so a short job's time is rounded up by up to that much.
func BenchmarkSendScan(b *testing.B) {
	for _, kind := range []string{"dimse", "stow"} {
		b.Run(kind, func(b *testing.B) {
			f := newFixture(b)
			f.inst.plain = true
			files := generated(b, benchInstances, benchPixels)
			var bytes int64
			for _, fl := range files {
				bytes += int64(len(fl))
			}
			f.archive("ses-1", scanFiles{"mr", files})
			one := func(s *tenant.Sender) { s.MaxAssociations = 1 }
			if kind == "dimse" {
				f.addDIMSE("peer", startPeer(b, &dimsePeer{}), one)
			} else {
				url := startWebPeer(b, &webPeer{})
				f.addStow("peer", url, one, func(s *tenant.Sender) { s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil })
			}
			s := f.start(f.config())
			b.ResetTimer()
			start := time.Now()
			n := 0
			for b.Loop() {
				job := f.createJob("peer", "ses-1")
				s.Poke("lab")
				if j := f.waitJob(s, job.ID, time.Minute, ended); j.State != tenant.JobDone || j.Counts.InstancesSent != benchInstances {
					b.Fatalf("job %s %+v, want done with %d sent", j.State, j.Counts, benchInstances)
				}
				n++
			}
			secs := time.Since(start).Seconds()
			b.ReportMetric(float64(n*benchInstances)/secs, "instances/s")
			b.ReportMetric(float64(int64(n)*bytes)/secs/1e6, "MB/s")
		})
	}
}
