package checkedread

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A scan's metadata file is rewritten whole by each change of its scan, and
// the catalog keeps the SHA-256 of the one written with the scan's current
// manifest (tenant.CatalogScan.SidecarSHA256). A read of it is checked
// against that digest where the check costs no extra read: from the
// storage's own SHA-256 of the object when its Stat keeps one (an S3 server
// that checked the checksum at put), before the first byte, on the primary
// and on each replica alike; and, on a replica, from the bytes themselves
// when the read is of the whole file, at its end.
//
// A replica holds exactly the file the catalog names once in step (the
// replicator puts it against the digest), so another file there is the
// replica's damage: recorded, and the read goes to the next copy. The
// primary may hold a newer file for a moment (a reception into the scan
// puts its next file before it commits), so another file there turns the
// read to a replica in step that holds the catalog's, and with none the
// primary's file is read as before digests were kept, never failed for
// it. A read of a part of the file (a line found by its index) is checked
// only by the storage's own digest: its bytes are not the whole file's, and
// a scrub reads the whole file of every replica. A file whose digest the
// catalog does not keep is read as before.

// ErrOtherFile is a file the catalog keeps a digest of that is, as its
// storage tells it or as read whole, another file than the one the digest
// names: data loss, as a missing one is, on that storage.
var ErrOtherFile = errors.New("the file on the storage is not the one the catalog keeps the digest of")

// otherFile is ErrOtherFile with the size of the file the storage holds.
type otherFile struct{ size int64 }

// Error returns ErrOtherFile's text.
func (e *otherFile) Error() string { return ErrOtherFile.Error() }

// Unwrap returns ErrOtherFile.
func (e *otherFile) Unwrap() error { return ErrOtherFile }

// checkFile returns, for an object whose digest the catalog keeps, an error
// of kind DataLoss with ErrOtherFile in its chain when what its storage
// tells of it (info) shows another file: the storage's own SHA-256 of the
// whole object, where it keeps one, is not the digest.
func checkFile(obj Object, info blob.Info) error {
	if obj.SHA256.IsZero() || !info.HasSHA256 || tenant.SHA256(info.SHA256) == obj.SHA256 {
		return nil
	}
	return fault.Wrap(fault.DataLoss, &otherFile{size: info.Size}, "checkedread: the file is not the one the catalog keeps")
}

// checkWhole returns body, which reads obj from offset for length bytes,
// checked against the digest the catalog keeps of obj when the read is of
// the whole object: its last read returns ErrOtherFile instead of the end
// when the bytes read are another file. Any other read is returned as it
// is.
func checkWhole(body io.ReadCloser, obj Object, offset, length int64) io.ReadCloser {
	if obj.SHA256.IsZero() || offset != 0 || length >= 0 {
		return body
	}
	return &wholeCheck{ReadCloser: body, h: sha256.New(), want: obj.SHA256}
}

// wholeCheck is a body read whole and hashed as it is read.
type wholeCheck struct {
	io.ReadCloser
	h    hash.Hash
	want tenant.SHA256
	n    int64
}

// Read reads the body, and at its end compares the bytes read with the
// digest.
func (w *wholeCheck) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	w.h.Write(p[:n])
	w.n += int64(n)
	if errors.Is(err, io.EOF) && tenant.SHA256(w.h.Sum(nil)) != w.want {
		return n, fault.Wrap(fault.DataLoss, &otherFile{size: w.n}, "checkedread: the file read is not the one the catalog keeps")
	}
	return n, err
}
