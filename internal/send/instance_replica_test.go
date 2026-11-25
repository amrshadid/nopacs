package send

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// replicaStorages answers every read from a replica named "copy" with the
// bytes it holds, and records what the sender asked of it.
type replicaStorages struct {
	body     []byte
	openErr  error
	asked    []checkedread.Object
	mismatch []checkedread.Opened
}

// Open answers the replica's bytes, or openErr.
func (s *replicaStorages) Open(_ context.Context, obj checkedread.Object, _, _ int64) (checkedread.Opened, error) {
	s.asked = append(s.asked, obj)
	if s.openErr != nil {
		return checkedread.Opened{}, s.openErr
	}
	return checkedread.Opened{Body: io.NopCloser(bytes.NewReader(s.body)), Storage: "copy", Replica: true}, nil
}

// Mismatch records the read found wrong.
func (s *replicaStorages) Mismatch(_ context.Context, o checkedread.Opened, _ error) error {
	s.mismatch = append(s.mismatch, o)
	return nil
}

// Close closes nothing.
func (*replicaStorages) Close() {}

// TestStreamReadsThroughTheOneOpener guards a sender's read of an instance:
// it is opened through the storages' opener with the scan's labels (so a
// read that turns to a replica can find the scan), a replica's bytes that
// are not the catalog's fail the stream as a mismatch and are reported to
// Mismatch once, and an object no storage holds is errMissing.
func TestStreamReadsThroughTheOneOpener(t *testing.T) {
	good := bytes.Repeat([]byte("instance bytes "), 4000)
	in := tenant.ScanInstance{SOPInstanceUID: "1.2.3.4", Storage: "main", Key: "data/a/b/c/1.2.3.4.dcm", Length: int64(len(good)),
		Size: int64(len(good)), ArchivedSHA256: tenant.SHA256(sha256.Sum256(good))}
	path := label.Path{Subject: "sub-01", Session: "ses-01", Scan: "t1w"}
	bufs := [2][]byte{make([]byte, 4096), make([]byte, 4096)}
	log := slog.New(slog.DiscardHandler)

	bad := bytes.Clone(good)
	bad[len(bad)-1] ^= 0x01
	st := &replicaStorages{body: bad}
	s, err := openStream(context.Background(), st, path, in, bufs, log)
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := io.ReadAll(s)
	_ = s.Close()
	if !errors.Is(rerr, checkedread.ErrChecksum) || !s.damaged() {
		t.Errorf("a replica's wrong bytes: %v, damaged %v; want a checksum mismatch", rerr, s.damaged())
	}
	if len(st.asked) != 1 || st.asked[0].Path != path || st.asked[0].Key != in.Key || st.asked[0].Storage != "main" {
		t.Errorf("the opener was asked %+v, want the instance with its scan's labels", st.asked)
	}
	if len(st.mismatch) != 1 || !st.mismatch[0].Replica {
		t.Errorf("the mismatch was reported %+v, want once for the replica", st.mismatch)
	}

	gone := &replicaStorages{openErr: fault.New(fault.NotFound, "no such object")}
	if _, err := openStream(context.Background(), gone, path, in, bufs, log); !errors.Is(err, errMissing) {
		t.Errorf("an object no storage holds: %v, want errMissing", err)
	}
}
