package record

import (
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/uid"
)

// validEdits checks an edit: its level and scans as a delete's at that
// level, and each instance it names once, in byte order, in one of its
// scans, at a later revision (above every one the instance had, an undone
// edit's among them, so not always the next), with its old checksum and,
// once it is published, its new one.
func (c Correction) validEdits() error {
	if c.Relabel != nil || c.Replace != nil || c.Move != nil || len(c.Instances) > 0 {
		return bad("an edit has its edits and nothing of a relabel, a replace or a delete")
	}
	switch c.Level {
	case LevelSubject, LevelSession, LevelScan, LevelInstance:
	default:
		return bad("an edit's level is subject, session, scan or instance")
	}
	if err := c.within(c.Level); err != nil {
		return err
	}
	if len(c.Edits) == 0 || len(c.Edits) > MaxEdits {
		return bad("an edit names 1 to %d instances", MaxEdits)
	}
	series := make(map[string]bool, len(c.Scans))
	for _, s := range c.Scans {
		series[s.SeriesUID] = true
	}
	written := c.State.Published() || c.State == StateUndone
	for i, e := range c.Edits {
		if err := uid.Check(e.SOPInstanceUID); err != nil {
			return bad("an edit's instance: %v", err)
		}
		if i > 0 && e.SOPInstanceUID <= c.Edits[i-1].SOPInstanceUID {
			return bad("an edit's instances are in byte order, each once")
		}
		if !series[e.SeriesUID] {
			return bad("an edit's instance is in one of its scans")
		}
		if e.OldRevision < 1 || e.NewRevision <= e.OldRevision || e.NewRevision > layout.MaxRevision {
			return bad("an edit adds a later revision, within 1 to %d", layout.MaxRevision)
		}
		if !lowerHex64(e.OldSHA256) || (e.NewSHA256 != "" || written) && !lowerHex64(e.NewSHA256) {
			return bad("an edit's checksums are 64 lowercase hex digits, the new one from its publish on")
		}
	}
	return nil
}
