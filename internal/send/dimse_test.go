package send

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// dimsePeer is a DICOM peer on go-dicom's own SCP, which takes each data set
// as a stream: it records each one it received whole (its bytes, or only
// their SHA-256 when it hashes), counts those that arrived cut short, and
// answers the status a test chooses.
type dimsePeer struct {
	network.BaseHandler
	// answer gives the status of the n-th whole arrival (0 first) of an
	// instance; nil answers success.
	answer func(uid string, n int) uint16
	// delay is how long it takes over each instance.
	delay time.Duration
	// hash keeps only the SHA-256 of each data set.
	hash bool
	// classes and syntaxes are what it accepts; nil for the SOP classes of
	// the test files and every transfer syntax.
	classes, syntaxes []string

	mu      sync.Mutex
	data    map[string][]byte
	sums    map[string][32]byte
	syntax  map[string]string
	count   map[string]int
	cut     int
	arrived []string
}

// HandleCStoreStream reads the data set to its end and answers.
func (h *dimsePeer) HandleCStoreStream(_ context.Context, req *network.CStoreRequest, ts string, data io.Reader) (*network.CStoreResponse, error) {
	var buf bytes.Buffer
	hsh := sha256.New()
	var w io.Writer = &buf
	if h.hash {
		w = hsh
	}
	if _, err := io.Copy(w, data); err != nil {
		h.mu.Lock()
		h.cut++
		h.mu.Unlock()
		return nil, err
	}
	if h.delay > 0 {
		time.Sleep(h.delay)
	}
	uid := req.AffectedSOPInstance
	h.mu.Lock()
	n := h.count[uid]
	h.count[uid]++
	h.arrived = append(h.arrived, uid)
	if h.hash {
		h.sums[uid] = [32]byte(hsh.Sum(nil))
	} else {
		h.data[uid] = buf.Bytes()
	}
	h.syntax[uid] = ts
	h.mu.Unlock()
	status := uint16(0)
	if h.answer != nil {
		status = h.answer(uid, n)
	}
	return &network.CStoreResponse{Status: status}, nil
}

// arrivals returns how many data sets arrived whole.
func (h *dimsePeer) arrivals() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.arrived)
}

// startPeer starts h as the AE "PEER" on a free loopback port, taking every
// transfer syntax, and returns its address.
func startPeer(t testing.TB, h *dimsePeer) string {
	t.Helper()
	h.data, h.sums, h.syntax, h.count = map[string][]byte{}, map[string][32]byte{}, map[string]string{}, map[string]int{}
	srv, err := network.StartServer(context.Background(), network.SCPConfig{AETitle: "PEER", Port: 0, BindAddress: "127.0.0.1"}, h)
	if err != nil {
		t.Fatal(err)
	}
	if h.syntaxes == nil {
		h.syntaxes = network.AllTransferSyntaxes()
	}
	if h.classes == nil {
		h.classes = []string{network.VerificationSOPClassUID, "1.2.840.10008.5.1.4.1.1.2", "1.2.840.10008.5.1.4.1.1.4",
			"1.2.840.10008.5.1.4.1.1.7", "1.2.840.10008.5.1.4.1.1.3.1", "1.2.840.10008.5.1.4.1.1.66.4"}
	}
	srv.SetSupportedTransferSyntaxes(h.syntaxes)
	srv.SetSupportedAbstractSyntaxes(h.classes)
	t.Cleanup(srv.Stop)
	return srv.Addr()
}

// dataSetOfFile returns the data set of a Part 10 file as go-dicom's own
// reader finds it (network.StoreStreamRequestFromFile), with its syntax.
func dataSetOfFile(t *testing.T, b []byte) ([]byte, string) {
	t.Helper()
	req, err := network.StoreStreamRequestFromFile(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	ds, err := io.ReadAll(req.DataSet)
	if err != nil {
		t.Fatal(err)
	}
	return ds, req.TransferSyntaxUID
}

// TestDIMSESendsTheArchivedDataSets guards the DIMSE sender's main line: a
// session of two scans, CT in Explicit VR Little Endian, and MR, secondary
// capture, ultrasound and segmentation in RLE, JPEG and explicit, each instance's data set arrives byte for byte as
// stored, in the transfer syntax its file names, the job ends done with
// every count right, and the sender is up.
func TestDIMSESendsTheArchivedDataSets(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	addr := startPeer(t, peer)
	var ct, mr [][]byte
	for _, n := range []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"} {
		ct = append(ct, corpus(t, n))
	}
	for _, n := range []string{"MR_small_RLE.dcm", "SC_rgb_rle_2frame.dcm", "jpeg_3frame_nobot.dcm", "liver_1frame.dcm"} {
		mr = append(mr, corpus(t, n))
	}
	f.archive("ses-1", scanFiles{"ct", ct}, scanFiles{"mr", mr})
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone {
		t.Fatalf("job ended %s (%q), want done: %+v; failures %+v", j.State, j.Reason, j.Counts, f.failures(job.ID))
	}
	c := j.Counts
	if c.Instances != 11 || c.InstancesSent != 11 || c.InstancesRemaining() != 0 || c.Scans != 2 || c.ScansDone != 2 || !c.Listed {
		t.Errorf("counts %+v, want 11 instances sent over 2 scans", c)
	}
	var total int64
	for uid, file := range f.files {
		total += int64(len(file))
		want, ts := dataSetOfFile(t, file)
		peer.mu.Lock()
		got, gotTS := peer.data[uid], peer.syntax[uid]
		peer.mu.Unlock()
		if !bytes.Equal(got, want) {
			t.Errorf("instance %s: the peer holds %d bytes, not the %d of the stored data set", uid, len(got), len(want))
		}
		if gotTS != ts {
			t.Errorf("instance %s arrived in %s, stored in %s", uid, gotTS, ts)
		}
	}
	if c.BytesSent != total {
		t.Errorf("bytes sent %d, want the %d bytes of the archived files", c.BytesSent, total)
	}
	h, ok := s.Health("lab", "peer")
	if !ok || h.State != tenant.SenderUp || h.LastSuccess.IsZero() {
		t.Errorf("health %+v, want up after a connection that succeeded", h)
	}
}

// TestDIMSEClassifiesStatuses guards the reading of the peer's statuses
// (PS3.4 B.2.3): C000 and 0122 refuse the instance, listed with the status;
// B000 sends it with a warning, counted apart and listed with it; A700 is a
// temporary failure that keeps the unit's place, so after the wait the
// instance is sent and nothing before it twice; the job ends partial.
func TestDIMSEClassifiesStatuses(t *testing.T) {
	f := newFixture(t)
	files := generated(t, 6, 256)
	uids := make([]string, len(files))
	for i, b := range files {
		uids[i] = uidOf(t, b)
	}
	sortStrings(uids)
	want := map[string]uint16{uids[1]: 0xC000, uids[2]: 0x0122, uids[3]: 0xB000}
	peer := &dimsePeer{answer: func(uid string, n int) uint16 {
		if uid == uids[4] && n == 0 {
			return 0xA700
		}
		return want[uid]
	}}
	addr := startPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", files})
	f.addDIMSE("peer", addr)
	rec := f.record()
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	// The A700 gives the unit back to wait 10 seconds: the clock moves
	// past it once the attempt has failed.
	f.stepOnEach(s, "the job's end", time.Minute, 11*time.Second, func() int { _, n := rec.counts(); return n }, func() bool {
		j, err := f.p.Job(f.ctx, job.ID)
		return err == nil && j.State.Ended()
	})
	j, err := f.p.Job(f.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != tenant.JobPartial {
		t.Fatalf("job ended %s, want partial: %+v", j.State, j.Counts)
	}
	c := j.Counts
	if c.InstancesSent != 3 || c.InstancesWarned != 1 || c.InstancesRefused != 2 || c.InstancesRemaining() != 0 {
		t.Errorf("counts %+v, want 3 sent, 1 warned, 2 refused", c)
	}
	got := map[string]tenant.JobFailure{}
	for _, fl := range f.failures(job.ID) {
		got[fl.SOPInstanceUID] = fl
	}
	for uid, status := range want {
		fl, ok := got[uid]
		wantCode := tenant.FailurePeerRefused
		if status == 0xB000 {
			wantCode = tenant.FailurePeerWarning
		}
		if !ok || fl.Code != wantCode || fl.PeerStatus != int32(status) || fl.TransferSyntaxUID == "" || fl.SOPClassUID == "" {
			t.Errorf("instance answered %04X: failure %+v, want code %s with the status, class and syntax", status, fl, wantCode)
		}
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	for _, uid := range uids {
		n := peer.count[uid]
		if uid == uids[4] && n != 2 || uid != uids[4] && n != 1 {
			t.Errorf("instance %s arrived %d times; only the one answered A700 arrives twice", uid, n)
		}
	}
}

// sortStrings sorts in byte order, the catalog's order of SOP Instance
// UIDs.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestDIMSERefusesWhatThePeerDoesNotAccept guards the negotiation's
// outcome per instance (PS3.8 9.3.3.2): a peer that takes CT only in
// Explicit VR Little Endian and no MR has the CT instance stored in RLE
// refused as syntax_not_accepted and the MR one as class_not_accepted, never
// sent in another syntax, while the CT instance in its own syntax is sent.
func TestDIMSERefusesWhatThePeerDoesNotAccept(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{classes: []string{"1.2.840.10008.5.1.4.1.1.2", "1.2.840.10008.5.1.4.1.1.7"},
		syntaxes: []string{"1.2.840.10008.1.2.1"}}
	addr := startPeer(t, peer)
	f.archive("ses-1", scanFiles{"a", [][]byte{corpus(t, "ct2n-6293.dcm"), corpus(t, "MR_small_RLE.dcm"), corpus(t, "SC_rgb_rle_2frame.dcm")}})
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 2 {
		t.Fatalf("job %s %+v, want partial with 1 sent and 2 refused", j.State, j.Counts)
	}
	codes := map[string]tenant.JobFailureCode{}
	for _, fl := range f.failures(job.ID) {
		codes[fl.SOPClassUID] = fl.Code
	}
	if codes["1.2.840.10008.5.1.4.1.1.4"] != tenant.FailureClassNotAccepted || codes["1.2.840.10008.5.1.4.1.1.7"] != tenant.FailureSyntaxNotAccepted {
		t.Errorf("failures by class %v, want MR class_not_accepted and SC (RLE) syntax_not_accepted", codes)
	}
	if peer.arrivals() != 1 {
		t.Errorf("the peer received %d data sets, want only the CT one", peer.arrivals())
	}
}
