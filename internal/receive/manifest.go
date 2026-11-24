package receive

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// eachScanInstance calls fn for each instance the catalog holds in the scan
// of the series, whichever reception confirmed it, a page at a time
// (tenant.ScanContents).
func (p *Pipeline) eachScanInstance(ctx context.Context, series string, fn func(tenant.ScanInstance) error) error {
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		ins, next, err := p.store.ScanInstances(ctx, series, page)
		if err != nil {
			return err
		}
		for _, in := range ins {
			if err := fn(in); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// manifestFile is a catalog instance as a manifest lists it: the checksum
// is that of what was written and read back.
func manifestFile(in tenant.ScanInstance) manifest.File {
	return manifest.File{Key: in.Key, Offset: in.Offset, Length: in.Length, Size: in.Size, SHA256: in.ArchivedSHA256.String(), SOPInstanceUID: in.SOPInstanceUID}
}

// scanManifest returns the manifest of the scan of the series as it is to
// be written now: every instance the catalog holds in the scan, whichever
// reception confirmed it, with this reception's files added. It is made
// from the catalog, never from what is on the storage, so a manifest a
// crash or another process left short is made whole by the next one
// written, and an entry a failed reception left behind is not carried on:
// the catalog lists only what was confirmed. A UID this reception writes is
// claimed by it, so the catalog holds none under it; its file is listed
// once.
func (p *Pipeline) scanManifest(ctx context.Context, series string, files []manifest.File) (manifest.Manifest, error) {
	ours := make(map[string]bool, len(files))
	for _, f := range files {
		ours[f.SOPInstanceUID] = true
	}
	var held []manifest.File
	if err := p.eachScanInstance(ctx, series, func(in tenant.ScanInstance) error {
		if !ours[in.SOPInstanceUID] {
			held = append(held, manifestFile(in))
		}
		return nil
	}); err != nil {
		return manifest.Manifest{}, err
	}
	m, _, err := manifest.Merge(manifest.Manifest{Files: held}, files)
	return m, err
}

// scanListed checks that every instance the catalog holds in the scan of
// the series, confirmed by this reception or by another, is in the scan's
// manifest with its key, place, size and checksum: an instance missing
// from a manifest is unreachable to a reader of the storage alone, whoever
// archived it.
// It returns the catalog's instances of the scan by UID, which the
// verification reads again (manifestHeld, and each file confirmed).
func (p *Pipeline) scanListed(ctx context.Context, series string, listed map[string]manifest.File) (map[string]tenant.ScanInstance, error) {
	held := make(map[string]tenant.ScanInstance, len(listed))
	err := p.eachScanInstance(ctx, series, func(in tenant.ScanInstance) error {
		if e, ok := listed[in.SOPInstanceUID]; !ok || e != manifestFile(in) {
			return notWhole("the scan's manifest does not list an instance the catalog holds in the scan")
		}
		held[in.SOPInstanceUID] = in
		return nil
	})
	return held, err
}
