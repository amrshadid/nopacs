package sqlkit

import (
	"context"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What only a deid job keeps: its tallies, added in the transaction of each
// progress write, and the catalog's UIDs its check looks up.

// addTallies adds a progress write's tallies to the job's, each key once:
// a key the job keeps is added to; a new key is kept while the job keeps
// fewer than tenant.MaxJobTallies, and added to tenant.TallyOther past it.
// Called inside recordInTx, after the write was found to move the cursor
// or settle, so a write sent again adds nothing twice.
func (k Sending) addTallies(ctx context.Context, tx SendingTx, jobID string, tallies map[string]int64) error {
	if len(tallies) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tallies))
	for key := range tallies {
		keys = append(keys, key)
	}
	slices.Sort(keys) // one order of row locks for every writer
	var kept int64
	if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM job_tallies WHERE job_id = ?`, []any{jobID}, &kept); err != nil {
		return err
	}
	var other int64
	for _, key := range keys {
		n := tallies[key]
		changed, err := tx.Exec(ctx, `UPDATE job_tallies SET count = count + ? WHERE job_id = ? AND tally = ?`, n, jobID, key)
		if err != nil {
			return err
		}
		if changed > 0 {
			continue
		}
		if kept >= tenant.MaxJobTallies || key == tenant.TallyOther {
			other += n
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO job_tallies (job_id, tally, count) VALUES (?, ?, ?)`, jobID, key, n); err != nil {
			return err
		}
		kept++
	}
	if other == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO job_tallies (job_id, tally, count) VALUES (?, ?, ?)
		ON CONFLICT (job_id, tally) DO UPDATE SET count = job_tallies.count + excluded.count`, jobID, tenant.TallyOther, other)
	return err
}

// JobTallies implements tenant.Jobs.
func (k Sending) JobTallies(ctx context.Context, id string) ([]tenant.JobTally, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return nil, err
	}
	var out []tenant.JobTally
	err := k.Host.Read(ctx, "the job's tallies cannot be read", func(tx SendingTx) error {
		out = nil
		if has, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, id); err != nil || !has {
			if err != nil {
				return err
			}
			return notFound("the tenant has no job with this id")
		}
		return tx.Query(ctx, jobTalliesSQL, []any{id, tenant.MaxJobTallies + 1},
			func(r Row) error {
				var t tenant.JobTally
				err := r.Scan(&t.Key, &t.Count)
				out = append(out, t)
				return err
			})
	})
	if err != nil {
		return nil, err
	}
	tenant.SortTallies(out)
	return out, nil
}

// jobTalliesSQL reads a job's tallies: a range of job_tallies' key.
const jobTalliesSQL = `SELECT tally, count FROM job_tallies WHERE job_id = ? LIMIT ?`

// The lookups of SeenUIDs, each by a unique index or a key: scans' Series
// Instance UID (its UNIQUE), their Study Instance UID (scans_by_study), and
// instances' SOP Instance UID (a unique index in SQLite, the key in
// Postgres).
const (
	seenSeriesSQL   = `SELECT series_uid FROM scans WHERE series_uid IN (`
	seenStudySQL    = `SELECT DISTINCT study_uid FROM scans WHERE study_uid IN (`
	seenInstanceSQL = `SELECT sop_instance_uid FROM instances WHERE sop_instance_uid IN (`
	// The same of what was archived as received: an instance that records
	// no profile version, or a scan or study that holds one.
	plainInstanceSQL = `SELECT sop_instance_uid FROM instances WHERE profile_version_id IS NULL AND sop_instance_uid IN (`
	plainSeriesSQL   = `SELECT s.series_uid FROM scans AS s WHERE EXISTS (SELECT 1 FROM instances AS i WHERE i.scan_id = s.id
		AND i.profile_version_id IS NULL) AND s.series_uid IN (`
	plainStudySQL = `SELECT DISTINCT s.study_uid FROM scans AS s WHERE EXISTS (SELECT 1 FROM instances AS i WHERE i.scan_id = s.id
		AND i.profile_version_id IS NULL) AND s.study_uid IN (`
	// The same of what was archived de-identified: an instance that
	// records a profile version, or a scan or study that holds one.
	deidInstanceSQL = `SELECT sop_instance_uid FROM instances WHERE profile_version_id IS NOT NULL AND sop_instance_uid IN (`
	deidSeriesSQL   = `SELECT s.series_uid FROM scans AS s WHERE EXISTS (SELECT 1 FROM instances AS i WHERE i.scan_id = s.id
		AND i.profile_version_id IS NOT NULL) AND s.series_uid IN (`
	deidStudySQL = `SELECT DISTINCT s.study_uid FROM scans AS s WHERE EXISTS (SELECT 1 FROM instances AS i WHERE i.scan_id = s.id
		AND i.profile_version_id IS NOT NULL) AND s.study_uid IN (`
)

// SeenUIDs implements tenant.JobWork.
func (k Sending) SeenUIDs(ctx context.Context, keyed, plain []string) (tenant.UIDsSeen, error) {
	if err := tenant.ValidateSeenUIDs(keyed, plain); err != nil {
		return tenant.UIDsSeen{}, err
	}
	var out tenant.UIDsSeen
	err := k.Host.Read(ctx, "the catalog's UIDs cannot be read", func(tx SendingTx) error {
		var err error
		if out.Keyed, err = lookUp(ctx, tx, keyed, seenSeriesSQL, seenStudySQL, seenInstanceSQL); err != nil {
			return err
		}
		if out.AsReceived, err = lookUp(ctx, tx, plain, plainSeriesSQL, plainStudySQL, plainInstanceSQL); err != nil {
			return err
		}
		out.Deidentified, err = lookUp(ctx, tx, plain, deidSeriesSQL, deidStudySQL, deidInstanceSQL)
		return err
	})
	if err != nil {
		return tenant.UIDsSeen{}, err
	}
	return out, nil
}

// lookUp returns the uids any of the statements finds, each once, in the
// order of uids.
func lookUp(ctx context.Context, tx SendingTx, uids []string, statements ...string) ([]string, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	uids = slices.Compact(slices.Sorted(slices.Values(uids)))
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(uids)), ", ") + ")"
	args := make([]any, len(uids))
	for i, u := range uids {
		args[i] = u
	}
	found := map[string]bool{}
	for _, q := range statements {
		// #nosec G202 -- q is a constant above and marks only placeholders; values are parameters.
		if err := tx.Query(ctx, q+marks, args, func(r Row) error {
			var u string
			err := r.Scan(&u)
			found[u] = true
			return err
		}); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, u := range uids {
		if found[u] {
			out = append(out, u)
		}
	}
	return out, nil
}
