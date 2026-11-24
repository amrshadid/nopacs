package frames

import (
	"context"
	"errors"
	"io"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/readat"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// fill gives a scan the catalog lists without a modality the header values
// its files say (tenant.ScanValueFill), for a scan archived before the
// catalog kept them: the header of the file its sidecar's first line names
// is read, by bounded ranged reads, and its values handed to the catalog,
// which keeps any value the scan or its session holds already. A scan
// whose files hold no modality is read again by the next pass, which costs
// one header; a fact of a file (no sidecar, a file missing, not DICOM, a
// header past the bound) leaves the scan as it is.
func (r *runner) fill(ctx context.Context, sc tenant.CatalogScan, path label.Path) error {
	store, err := r.storage(ctx, sc.Storage)
	if err != nil {
		return err
	}
	key, err := metadataKey(path)
	if err != nil {
		return err
	}
	at := r.cfg.Clock.Now()
	in, ok, err := firstLine(ctx, store, key)
	if err != nil || !ok {
		return err
	}
	v, ok, err := r.valuesOf(ctx, store, in)
	if err != nil || !ok {
		return err
	}
	if v != (tenant.ScanValues{}) {
		err := r.cfg.Catalog.FillScanValues(ctx, sc.SeriesUID, v)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			// Removed since it was listed: nothing is left to fill.
		case err != nil:
			return err
		default:
			r.rep.ScansFilled++
		}
	}
	return r.pause(ctx, r.cfg.Clock.Now().Sub(at))
}

// firstLine returns the first line of the scan's sidecar, or reports that
// it has none: no sidecar, no line, or one not in its form, which the
// metadata service reports on its own.
func firstLine(ctx context.Context, store blob.Store, key string) (sidecar.Instance, bool, error) {
	rc, err := store.Read(ctx, key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return sidecar.Instance{}, false, nil
	}
	if err != nil {
		return sidecar.Instance{}, false, storageError(err, "the scan's metadata cannot be read")
	}
	defer func() { _ = rc.Close() }()
	in, _, err := sidecar.NewReader(rc).Next()
	switch {
	case err == nil:
		return in, true, nil
	case errors.Is(err, io.EOF), errors.Is(err, sidecar.ErrFormat), errors.Is(err, sidecar.ErrOrder), errors.Is(err, sidecar.ErrIncomplete):
		return sidecar.Instance{}, false, nil
	}
	return sidecar.Instance{}, false, storageError(err, "the scan's metadata cannot be read")
}

// valuesOf reads the header of the instance a line names from its file and
// returns the values the catalog keeps of its scan and session, as the
// reception that archives a file makes them (header.CatalogText). It
// reports false for a file that is not what the line says: missing,
// shorter, not DICOM, or a header past the bound.
func (r *runner) valuesOf(ctx context.Context, store blob.Store, in sidecar.Instance) (tenant.ScanValues, bool, error) {
	limit := int64(header.MaxHeader)
	if r.cfg.HeaderLimit != nil {
		limit = r.cfg.HeaderLimit(ctx)
	}
	h, err := header.ReadWithin(readat.New(ctx, store, in.Key, in.Size), in.Size, limit)
	if err == nil {
		return tenant.ScanValuesOf(h.CatalogText()), true, nil
	}
	var notDICOM *header.NotDICOMError
	var tooLarge *header.TooLargeError
	switch {
	case ctx.Err() != nil:
		return tenant.ScanValues{}, false, ctx.Err()
	case errors.As(err, &notDICOM), errors.As(err, &tooLarge), fault.KindOf(err) == fault.NotFound,
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return tenant.ScanValues{}, false, nil
	}
	return tenant.ScanValues{}, false, storageError(err, "an instance's file cannot be read")
}
