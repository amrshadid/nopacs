package send

import (
	"crypto/sha256"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// memoryBound is the most the live heap may grow while one instance of
// tens of MiB is sent: the worker's two read buffers and meta prefix, the
// association's two fragments (or the request's pipe), and the peer's own
// in this process, with room for the database and the runtime; far below
// the instance's size.
const memoryBound = 16 << 20

// TestMemoryIsBoundedWhateverTheFileSize guards the worker's memory: an
// instance of 40 MiB is sent by C-STORE and by STOW-RS, to peers that hash
// what they receive, while the heap, collected often, grows by less than
// memoryBound, and the peer's SHA-256 is the stored data set's (or
// file's).
func TestMemoryIsBoundedWhateverTheFileSize(t *testing.T) {
	const size = 40 << 20
	for _, kind := range []string{"dimse", "stow"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.inst.plain = true
			files := generated(t, 1, size)
			f.archive("ses-1", scanFiles{"mr", files})
			uid := uidOf(t, files[0])
			var want [32]byte
			var got func() [32]byte
			if kind == "dimse" {
				peer := &dimsePeer{hash: true}
				addr := startPeer(t, peer)
				ds, _ := dataSetOfFile(t, files[0])
				want = sha256.Sum256(ds)
				got = func() [32]byte { peer.mu.Lock(); defer peer.mu.Unlock(); return peer.sums[uid] }
				f.addDIMSE("peer", addr)
			} else {
				want = sha256.Sum256(files[0])
				var sum [32]byte
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
					mr := multipart.NewReader(r.Body, params["boundary"])
					part, err := mr.NextPart()
					if err != nil {
						http.Error(w, "no part", http.StatusBadRequest)
						return
					}
					h := sha256.New()
					_, _ = io.Copy(h, part)
					sum = [32]byte(h.Sum(nil))
					w.Header().Set("Content-Type", "application/dicom+json")
					_, _ = w.Write(storeAnswer([]string{uid}, nil, nil))
				}))
				t.Cleanup(srv.Close)
				got = func() [32]byte { return sum }
				f.addStow("peer", srv.URL+"/dicom-web", func(s *tenant.Sender) {
					s.STOW.Authentication, s.STOW.SealedCredential = tenant.StowAuthNone, nil
				})
			}
			// Neither the file nor the garbage of sending it is to count:
			// only what the send holds live. The collector runs often, so
			// the heap's peak is near what is live.
			delete(f.files, uid)
			defer debug.SetGCPercent(debug.SetGCPercent(10))
			job := f.createJob("peer", "ses-1")
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			base := m.HeapAlloc
			stop := testkit.HeapPeak()
			s := f.start(f.config())
			j := f.waitJob(s, job.ID, 60*time.Second, ended)
			peak := stop()
			if j.State != tenant.JobDone {
				t.Fatalf("job %s %+v, want done", j.State, j.Counts)
			}
			if got() != want {
				t.Error("the peer's SHA-256 is not the stored bytes'")
			}
			grew := float64(peak) - float64(base)
			t.Logf("heap grew by %.1f MiB while sending %d MiB", grew/(1<<20), size>>20)
			if grew > memoryBound {
				t.Errorf("the heap grew by %.0f bytes, more than %d, while sending one instance of %d", grew, memoryBound, size)
			}
		})
	}
}
