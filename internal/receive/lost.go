package receive

import (
	"context"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// dropLost holds, as incoming_lost, every received file of the groups whose
// incoming copy is gone (removed or lost from the disk outside the server),
// and returns the groups without them; a group left with no file is left
// out. A step that went on with such a file would fail at every attempt
// until its unit gave up and failed the whole reception; holding it says
// what was lost, with the UIDs, size and checksum the record keeps, and
// lets the rest of the study be archived. A file whose bytes were let go
// on purpose, because the catalog holds the same file under its UID (an
// early duplicate), is not lost and stays. It runs before any claim, so a
// lost file never leaves a claim behind. The record's id is the one
// holdRecord makes, so a run again writes nothing new.
func (p *Pipeline) dropLost(ctx context.Context, w tenant.Work, r tenant.Reception, groups []series) ([]series, error) {
	var records []tenant.QuarantinedFile
	out := groups[:0:0]
	for _, g := range groups {
		kept := g.files[:0:0]
		for _, f := range g.files {
			there, err := p.area.Exists(f.Incoming)
			if err != nil {
				return nil, err
			}
			if there || p.heldElsewhere(ctx, f) {
				kept = append(kept, f)
				continue
			}
			q := p.holdRecord(r, f, tenant.QuarantineIncomingLost, nil)
			q.Incoming, q.ContentKept = "", false
			records = append(records, q)
			p.log.Error("a received file's incoming copy is gone; it is held as lost", "reception", r.ID)
		}
		if len(kept) > 0 {
			out = append(out, series{uid: g.uid, files: kept})
		}
	}
	if err := p.hold(ctx, w, records); err != nil {
		return nil, err
	}
	return out, nil
}
