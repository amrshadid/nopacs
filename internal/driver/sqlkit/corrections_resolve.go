package sqlkit

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What deleted data an arrival matches, and what became of an instance's
// revision.

// DeletedMatches implements tenant.Corrections.
func (k Sending) DeletedMatches(ctx context.Context, p tenant.DeletedProbe) (tenant.DeletedMatch, error) {
	if err := p.Validate(); err != nil {
		return tenant.DeletedMatch{}, err
	}
	out := tenant.DeletedMatch{Series: map[string]string{}, Instances: map[string]string{}}
	err := k.Host.Read(ctx, "the deleted data cannot be looked up", func(tx SendingTx) error {
		out = tenant.DeletedMatch{Series: map[string]string{}, Instances: map[string]string{}}
		look := func(kind string, values []string, put func(value, id string)) error {
			if len(values) == 0 {
				return nil
			}
			args := []any{kind}
			for _, v := range values {
				args = append(args, v)
			}
			// The smallest id of the deletes that name a value answers it,
			// so a lookup made again answers the same.
			return tx.Query(ctx, `SELECT value, min(correction_id) FROM deleted_targets WHERE kind = ? AND value IN (`+
				marks(len(values))+`) GROUP BY value`, args, func(r Row) error {
				var value, id string
				err := r.Scan(&value, &id)
				put(value, id)
				return err
			})
		}
		if p.Subject != "" {
			if err := look("subject", []string{p.Subject}, func(_, id string) { out.Subject = id }); err != nil {
				return err
			}
		}
		if p.StudyUID != "" {
			if err := look("study", []string{p.StudyUID}, func(_, id string) { out.Study = id }); err != nil {
				return err
			}
		}
		if err := look("series", p.Series, func(v, id string) { out.Series[v] = id }); err != nil {
			return err
		}
		return look("instance", p.Instances, func(v, id string) { out.Instances[v] = id })
	})
	return out, err
}

// ResolveInstances implements tenant.Corrections.
func (k Sending) ResolveInstances(ctx context.Context, refs []tenant.InstanceRef) ([]tenant.ResolvedInstance, error) {
	if err := tenant.ValidateResolve(refs); err != nil {
		return nil, err
	}
	var out []tenant.ResolvedInstance
	err := k.Host.Read(ctx, "the instances cannot be resolved", func(tx SendingTx) error {
		out = make([]tenant.ResolvedInstance, 0, len(refs))
		for _, ref := range refs {
			r, err := resolveOne(ctx, tx, ref)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// revisionRow is a revision no longer current.
type revisionRow struct {
	found      bool
	sum        tenant.SHA256
	correction string
	fate       string
	purged     bool
}

// revisionOf reads the revision rev of an instance from instance_revisions.
func revisionOf(ctx context.Context, tx SendingTx, uid string, rev int) (revisionRow, error) {
	var out revisionRow
	var sum []byte
	found, err := tx.QueryRow(ctx, `SELECT sha256, correction_id, fate, purged FROM instance_revisions WHERE sop_instance_uid = ?
		AND revision = ?`, []any{uid, rev}, &sum, &out.correction, &out.fate, &out.purged)
	out.found, out.sum = found, ToSHA256(sum)
	return out, err
}

// resolveOne answers one pair: the catalog's row, a delete's hidden row,
// or a revision no longer current, in that order.
func resolveOne(ctx context.Context, tx SendingTx, ref tenant.InstanceRef) (tenant.ResolvedInstance, error) {
	out := tenant.ResolvedInstance{Ref: ref, Disposition: tenant.ResolvedUnknown}
	var key string
	var sum, archived []byte
	var p label.Path
	held, err := tx.QueryRow(ctx, `SELECT i.storage_key, i.sha256, i.archived_sha256, s.subject_label, s.session_label, s.label
		FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE i.sop_instance_uid = ?`, []any{ref.UID}, &key, &sum, &archived,
		&p.Subject, &p.Session, &p.Scan)
	if err != nil {
		return out, err
	}
	hidden, correction := false, ""
	if !held {
		hidden, err = tx.QueryRow(ctx, `SELECT storage_key, sha256, archived_sha256, correction_id FROM hidden_instances
			WHERE sop_instance_uid = ? ORDER BY correction_id DESC LIMIT 1`, []any{ref.UID}, &key, &sum, &archived, &correction)
		if err != nil {
			return out, err
		}
	}
	current := ToSHA256(archived)
	if current.IsZero() {
		current = ToSHA256(sum)
	}
	if held || hidden {
		cur := tenant.RevisionOfKey(key)
		switch {
		case ref.Revision == cur && held:
			out.Disposition, out.SHA256, out.CurrentRevision, out.BytesKept, out.Path = tenant.ResolvedCurrent, current, cur, true, p
			return out, nil
		case ref.Revision == cur:
			out.Disposition, out.SHA256, out.Correction, out.BytesKept = tenant.ResolvedDeleted, current, correction, true
			return out, nil
		case ref.Revision > cur:
			return out, nil
		}
		if held {
			out.CurrentRevision, out.Path = cur, p
		}
	}
	rev, err := revisionOf(ctx, tx, ref.UID, ref.Revision)
	if err != nil || !rev.found {
		return out, err
	}
	out.SHA256, out.Correction, out.BytesKept = rev.sum, rev.correction, !rev.purged
	out.Disposition = tenant.ResolvedSuperseded
	if rev.fate == "deleted" {
		out.Disposition = tenant.ResolvedDeleted
	}
	return out, nil
}
