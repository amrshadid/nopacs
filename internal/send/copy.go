package send

import (
	"bytes"
	"context"
	"io"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// Copier makes the copy of an archived instance that a job naming a
// destination profile sends: the file de-identified by that profile
// version and checked after (package forward).
type Copier interface {
	// Copy returns the copy of file, an archived DICOM file, made by the
	// profile version ref; or no bytes and the fixed cause it is refused
	// for, never a value; or an error that says nothing of the file, after
	// which the instance is tried again later.
	Copy(ctx context.Context, ref tenant.ProfileRef, file []byte) ([]byte, string, error)
}

// The fixed details of an instance whose copy is refused before any
// copier runs.
const (
	// detailNoCopier: the server was given no way to make copies.
	detailNoCopier = "no_copier"
	// detailCopyStow: a stow sender sends no copy, since a peer's answer
	// names each instance by the UIDs it was sent with, which a copy
	// changes.
	detailCopyStow = "copy_by_dimse_only"
	// detailCopyTooLarge: the archived file is larger than a copy is made
	// of in memory.
	detailCopyTooLarge = "copy_too_large"
)

// MaxCopyBytes is the largest archived file a copy is made of: the copy is
// made in memory, the file and what the engine writes held at once, as a
// trial holds them.
const MaxCopyBytes = 1 << 30

// copies reports whether the unit's job sends copies.
func (u *unitRun) copies() bool { return !u.profile.IsZero() }

// copyOf reads the instance's checked stream whole and returns a stream of
// its copy, with the copy's length; or the result the instance is refused
// with; or the outcome that ends the unit (a storage that failed, a copier
// that did not answer). st is closed either way.
func (s *dimseSession) copyOf(st *stream, in tenant.ScanInstance) (*stream, int64, *result, *outcome) {
	u := s.u
	defer func() { _ = st.Close() }()
	if u.copier == nil {
		r := refused(tenant.FailureWouldHold, 0, detailNoCopier)
		return nil, 0, &r, nil
	}
	if in.Length > MaxCopyBytes {
		r := refused(tenant.FailureTooLarge, 0, detailCopyTooLarge)
		return nil, 0, &r, nil
	}
	file := make([]byte, in.Length)
	if _, err := io.ReadFull(st, file); err != nil {
		_ = st.Close()
		res, out := s.readEnded(st, err)
		return nil, 0, &res, out
	}
	b, cause, err := u.copier.Copy(u.ctx, u.profile, file)
	switch {
	case err != nil:
		out := u.readFailure(err)
		return nil, 0, nil, &out
	case cause != "":
		r := refused(tenant.FailureWouldHold, 0, cause)
		return nil, 0, &r, nil
	}
	return bytesStream(b), int64(len(b)), nil, nil
}

// bytesStream returns a stream of bytes held in memory, which a copy is:
// read as an instance's stream is, never damaged nor failed.
func bytesStream(b []byte) *stream {
	pr, pw := io.Pipe()
	s := &stream{pr: pr, body: io.NopCloser(nil), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		_, err := io.Copy(pw, bytes.NewReader(b))
		_ = pw.CloseWithError(err) // A nil error is the end of the copy.
	}()
	return s
}
