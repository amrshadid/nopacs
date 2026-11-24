package send

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// withMetaElement returns file with an element (0002,0102) of n zero bytes
// added at the end of its file meta information, and the group length
// (0002,0000) grown by it (PS3.10 7.1): a valid file whose meta group is n
// bytes longer. The file's (0002,0000) is its first meta element.
func withMetaElement(t *testing.T, file []byte, n int) []byte {
	t.Helper()
	req, err := network.StoreStreamRequestFromFile(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	ds, err := readAllOf(req)
	if err != nil {
		t.Fatal(err)
	}
	end := len(file) - len(ds)
	if string(file[132:136]) != "\x02\x00\x00\x00" || string(file[136:138]) != "UL" {
		t.Fatal("the test file's first meta element is not (0002,0000)")
	}
	el := make([]byte, 12+n)
	copy(el, "\x02\x00\x02\x01OB\x00\x00")
	binary.LittleEndian.PutUint32(el[8:], uint32(n)) // #nosec G115 -- a test's small length.
	out := append(append(append([]byte{}, file[:end]...), el...), file[end:]...)
	gl := binary.LittleEndian.Uint32(out[140:144])
	binary.LittleEndian.PutUint32(out[140:144], gl+uint32(len(el))) // #nosec G115 -- a test's small length.
	return out
}

// readAllOf reads a request's data set.
func readAllOf(req network.StoreStreamRequest) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(req.DataSet)
	return b.Bytes(), err
}

// withBadClass returns file with one character of its (0002,0002) Media
// Storage SOP Class UID made a letter, so the UID is malformed (PS3.5 9.1)
// and no length changes.
func withBadClass(t *testing.T, file []byte) []byte {
	t.Helper()
	out := bytes.Clone(file)
	i := bytes.Index(out[:1024], []byte("\x02\x00\x02\x00UI"))
	if i < 0 {
		t.Fatal("no (0002,0002) in the test file's meta")
	}
	out[i+8] = 'X'
	return out
}

// TestMetaTheReadCannotPlace guards an instance whose file meta information
// is longer than the bytes read for it, or names a malformed SOP class: it
// is refused alone, with the phrase that says why, never sent as a data set
// whose start is wrong and never counted sent, and the scan's other
// instances are sent; the job ends partial without a failed attempt.
func TestMetaTheReadCannotPlace(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	addr := startPeer(t, peer)
	files := generated(t, 3, 256)
	long := withMetaElement(t, files[0], 100<<10)
	bad := withBadClass(t, files[1])
	f.archive("ses-1", scanFiles{"mr", [][]byte{long, bad, files[2]}})
	f.addDIMSE("peer", addr)
	rec := f.record()
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, time.Minute, ended)
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 2 {
		t.Fatalf("job %s %+v, want partial with 1 sent and 2 refused", j.State, j.Counts)
	}
	if _, failed := rec.counts(); failed != 0 {
		t.Errorf("%d attempts failed, want none", failed)
	}
	byUID := map[string]tenant.JobFailure{}
	for _, fl := range f.failures(job.ID) {
		byUID[fl.SOPInstanceUID] = fl
	}
	for _, b := range [][]byte{long, bad} {
		fl := byUID[uidOf(t, b)]
		if fl.Code != tenant.FailureMetaUnreadable || fl.Detail != detailNoMeta {
			t.Errorf("failure %+v, want refused as %s (%q), not as damaged or a syntax not accepted", fl, tenant.FailureMetaUnreadable, detailNoMeta)
		}
		peer.mu.Lock()
		n := peer.count[uidOf(t, b)]
		peer.mu.Unlock()
		if n != 0 {
			t.Errorf("an instance whose meta the read cannot place reached the peer")
		}
	}
}

// withDataSetClass returns file with the last digit of the data set's SOP
// Class UID (0008,0016) changed to d, so it names another class than the
// meta group's (0002,0002); no length changes.
func withDataSetClass(t *testing.T, file []byte, d byte) []byte {
	t.Helper()
	out := bytes.Clone(file)
	i := bytes.Index(out, []byte("\x08\x00\x16\x00UI"))
	if i < 0 {
		t.Fatal("no (0008,0016) in the test file")
	}
	n := int(binary.LittleEndian.Uint16(out[i+6:]))
	v := bytes.TrimRight(out[i+8:i+8+n], "\x00 ")
	v[len(v)-1] = d
	return out
}

// TestSOPClassIsTheDataSets guards the class a C-STORE names: the data
// set's own (0008,0016) as received, which the meta group must repeat. A
// file whose two name different classes is refused alone with the phrase
// that says so, and nothing of it reaches the peer; the others are sent.
func TestSOPClassIsTheDataSets(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	addr := startPeer(t, peer)
	files := generated(t, 2, 256)
	odd := withDataSetClass(t, files[0], '2')
	f.archive("ses-1", scanFiles{"mr", [][]byte{odd, files[1]}})
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 1 {
		t.Fatalf("job %s %+v, want partial with 1 sent and 1 refused", j.State, j.Counts)
	}
	if fl := f.failures(job.ID); len(fl) != 1 || fl[0].Code != tenant.FailureMetaUnreadable || fl[0].Detail != detailMetaDisagrees || fl[0].SOPInstanceUID != uidOf(t, odd) {
		t.Errorf("failures %+v, want the changed file refused as %s with %q", fl, tenant.FailureMetaUnreadable, detailMetaDisagrees)
	}
	if peer.arrivals() != 1 {
		t.Errorf("the peer received %d, want only the other instance", peer.arrivals())
	}
}
