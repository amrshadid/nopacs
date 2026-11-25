package backup

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// keptQuarantine returns the ids of the quarantined files held whose bytes
// the tenant's incoming area under root holds whole: a file at its
// recorded place of its recorded size and SHA-256 (its size alone, for a
// record that holds no checksum). A reconcile runs on the data directory
// in place, so those bytes were never lost and the file can still be
// released; one missing or different was released, rejected or removed
// after the recovery point, or changed since, and is marked incoming
// lost. An area not there holds none.
func keptQuarantine(ctx context.Context, p tenant.Pipeline, root, tenantName string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(root, tenantName)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	area, err := incoming.Open(root, tenantName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = area.Close() }()
	var out []string
	cursor := ""
	for range maxListPages {
		page, next, err := p.ListQuarantined(ctx, tenant.QuarantineFilter{State: tenant.QuarantineHeld}, master.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, q := range page {
			if !q.ContentKept || q.Incoming == "" {
				continue
			}
			same, err := sameBytes(area, q)
			if err != nil {
				return nil, err
			}
			if same {
				out = append(out, q.ID)
			}
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, fault.New(fault.Unavailable, "the listing of the quarantined files did not end")
}

// sameBytes reports whether the incoming area holds q's file as recorded.
// A place the area refuses, or one that is not a regular file, does not
// hold it; an area that cannot be read is the error.
func sameBytes(area *incoming.Area, q tenant.QuarantinedFile) (bool, error) {
	f, size, err := area.Open(q.Incoming)
	if fault.KindOf(err) == fault.Unavailable {
		return false, err
	}
	if err != nil {
		return false, nil
	}
	defer func() { _ = f.Close() }()
	if size != q.Size {
		return false, nil
	}
	if q.SHA256.IsZero() {
		return true, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fault.Wrap(fault.Unavailable, err, "a quarantined file's bytes in incoming cannot be read")
	}
	var sum tenant.SHA256
	copy(sum[:], h.Sum(nil))
	return sum == q.SHA256, nil
}
