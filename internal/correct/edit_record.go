package correct

import (
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// editRecords returns what an edit's record on one storage names of its
// instances: those in the scans given, in byte order of SOP Instance UID,
// each with its scan's Series Instance UID, its revisions and checksums
// (the new one once the edit is published).
func editRecords(edits []tenant.EditedInstance, scans []tenant.CorrectionScan) []record.Replacement {
	in := make(map[string]bool, len(scans))
	for _, s := range scans {
		in[s.SeriesUID] = true
	}
	var out []record.Replacement
	for _, e := range edits {
		if !in[e.SeriesUID] {
			continue
		}
		r := record.Replacement{SOPInstanceUID: e.UID, SeriesUID: e.SeriesUID, OldRevision: e.OldRevision, NewRevision: e.NewRevision,
			OldSHA256: sha256Text(e.OldSHA256)}
		if !e.NewSHA256.IsZero() {
			r.NewSHA256 = sha256Text(e.NewSHA256)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b record.Replacement) int { return strings.Compare(a.SOPInstanceUID, b.SOPInstanceUID) })
	return out
}
