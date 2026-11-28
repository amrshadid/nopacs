package transfer

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// MaxRefusalsCounted is the most instances a preview reads one by one to
// count those the rule of what may leave would fail: above it the preview
// gives the totals alone.
const MaxRefusalsCounted = 100000

// PreviewSource is the catalog as a preview reads it.
type PreviewSource interface {
	ScanLister
	tenant.InstanceReads
}

// Preview is what a transfer would cover and whether the target's receiver
// would take all of it.
type Preview struct {
	// Sessions, Scans, Instances and Bytes are what it covers in the source.
	Sessions, Scans, Instances, Bytes int64
	// Refused counts the instances the send rule (tenant.SendRefusal) would
	// fail for a receiver that de-identifies, or not; RefusalsCounted says
	// the count was made.
	Refused         int64
	RefusalsCounted bool
}

// PreviewOf reads what a transfer of the subject or session t covers, and,
// while it covers at most MaxRefusalsCounted instances, how many of them the
// send rule would fail for a receiver that de-identifies when deid is set.
func PreviewOf(ctx context.Context, src PreviewSource, t tenant.JobTarget, deid bool) (Preview, error) {
	var p Preview
	var paths []label.Path
	sessions := map[string]bool{}
	err := EachScan(ctx, src, t, func(path label.Path, sc tenant.CatalogScan) error {
		sessions[path.Session] = true
		paths = append(paths, path)
		p.Scans++
		p.Instances += sc.Instances
		p.Bytes += sc.Bytes
		return nil
	})
	if err != nil {
		return Preview{}, err
	}
	p.Sessions = int64(len(sessions))
	if p.Instances > MaxRefusalsCounted {
		return p, nil
	}
	for _, path := range paths {
		page := master.Page{Limit: tenant.MaxTransferItemBatch}
		for {
			held, next, err := src.ListHeldInstances(ctx, path, page)
			if err != nil {
				return Preview{}, err
			}
			for _, in := range held {
				if _, refuse := tenant.SendRefusal(in.Provenance(), deid); refuse {
					p.Refused++
				}
			}
			if next == "" {
				break
			}
			page.Cursor = next
		}
	}
	p.RefusalsCounted = true
	return p, nil
}
