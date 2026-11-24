package receive

import (
	"context"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// holdDeleted holds, as was_deleted, every file of the reception that
// names data a delete removed (Corrections.DeletedMatches): its SOP
// Instance UID, its Series Instance UID, its study, or, after a subject was
// deleted, the subject label it would be archived under. A consent
// withdrawn must not be undone by the next time a scanner or a peer sends
// the study again. It returns the series with the files left, each in its
// order; a series with none left is dropped. The records' ids are made from
// the reception and the file, as every hold's, so a run again writes
// nothing new.
//
// A record keeps no content: the withdrawn data must not be kept again
// under another name, so each file's bytes are removed from the incoming
// area as soon as its record is committed, and the record says what came
// (its UIDs, size, checksum and the delete it matched) and that nothing of
// it was kept. A run again after a stop between the two finds the bytes
// still there and removes them; the archive step never sees such a file
// (its copy is gone, and its record holds it).
func (p *Pipeline) holdDeleted(ctx context.Context, w tenant.Work, r tenant.Reception, groups []series, subject string) ([]series, error) {
	var seriesUIDs, instances []string
	for _, g := range groups {
		seriesUIDs = append(seriesUIDs, g.uid)
		for _, f := range g.files {
			instances = append(instances, f.SOPInstanceUID)
		}
	}
	match := tenant.DeletedMatch{Series: map[string]string{}, Instances: map[string]string{}}
	for start := 0; start == 0 || start < max(len(seriesUIDs), len(instances)); start += tenant.MaxDeletedProbe {
		probe := tenant.DeletedProbe{Series: window(seriesUIDs, start), Instances: window(instances, start)}
		if start == 0 {
			probe.Subject, probe.StudyUID = subject, r.StudyUID
		}
		m, err := p.store.DeletedMatches(ctx, probe)
		if err != nil {
			return nil, err
		}
		if start == 0 {
			match.Subject, match.Study = m.Subject, m.Study
		}
		for k, v := range m.Series {
			match.Series[k] = v
		}
		for k, v := range m.Instances {
			match.Instances[k] = v
		}
	}
	var records []tenant.QuarantinedFile
	var gone []string
	var left []series
	for _, g := range groups {
		kept := series{uid: g.uid}
		for _, f := range g.files {
			if id := match.Of(g.uid, f.SOPInstanceUID); id != "" {
				q := p.holdRecord(r, f, tenant.QuarantineWasDeleted, map[string]string{"correction": id})
				q.Incoming, q.ContentKept = "", false
				records = append(records, q)
				gone = append(gone, f.Incoming)
				continue
			}
			kept.files = append(kept.files, f)
		}
		if len(kept.files) > 0 {
			left = append(left, kept)
		}
	}
	if len(records) == 0 {
		return groups, nil
	}
	p.at("identify.before-hold-deleted")
	if err := p.hold(ctx, w, records); err != nil {
		return nil, err
	}
	p.at("identify.after-hold-deleted")
	for _, path := range gone {
		if err := p.area.Remove(path); err != nil && fault.KindOf(err) != fault.NotFound {
			return nil, err
		}
	}
	return left, nil
}

// window returns the MaxDeletedProbe values of s from start, or none.
func window(s []string, start int) []string {
	if start >= len(s) {
		return nil
	}
	return s[start:min(start+tenant.MaxDeletedProbe, len(s))]
}
