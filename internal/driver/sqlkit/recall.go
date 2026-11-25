package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The recalls of objects from archive-tier storages (tenant.Recalls), on
// the recalls table of each driver's tenant baseline schema: one row per
// object, so one recall is asked of the store however many wait. A row's
// key holds a SOP Instance UID as received: no error here holds one.

// recallColumns are a recall's columns as the store reads them.
const recallColumns = `storage, object_key, state, days, speed, requested_at, updated, due, online_until, reason`

// scanRecall reads one row of recallColumns.
func scanRecall(r Row) (tenant.Recall, error) {
	var out tenant.Recall
	var state, speed string
	var requested, updated, due, until DBTime
	err := r.Scan(&out.Storage, &out.Key, &state, &out.Days, &speed, &requested, &updated, &due, &until, &out.Reason)
	out.State, out.Speed = tenant.RecallState(state), tenant.RecallSpeed(speed)
	out.Requested, out.Updated, out.Due, out.OnlineUntil = requested.T, updated.T, due.T, until.T
	return out, err
}

// recallIn reads a recall in tx with the lock given, and whether there is
// one.
func recallIn(ctx context.Context, tx SendingTx, storage, key, lock string) (tenant.Recall, bool, error) {
	var out tenant.Recall
	found := false
	err := tx.Query(ctx, `SELECT `+recallColumns+` FROM recalls WHERE storage = ? AND object_key = ?`+lock, []any{storage, key},
		func(r Row) error {
			var err error
			out, err = scanRecall(r)
			found = true
			return err
		})
	return out, found, err
}

// RequestRecalls implements tenant.Recalls.
func (k Sending) RequestRecalls(ctx context.Context, a tenant.RecallAsk) (tenant.RecallAsked, error) {
	if err := a.Validate(); err != nil {
		return tenant.RecallAsked{}, err
	}
	var out tenant.RecallAsked
	err := k.Host.Write(ctx, "the recalls cannot be recorded", func(tx SendingTx) error {
		out = tenant.RecallAsked{}
		var tier string
		found, err := tx.QueryRow(ctx, `SELECT tier FROM storages WHERE name = ?`+k.D.Share, []any{a.Storage}, &tier)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no storage named " + a.Storage)
		}
		if !tenant.StorageTier(tier).Archive() {
			return tenant.Precondition("the storage " + a.Storage + " is not of the archive tier; it reads every object at once")
		}
		for _, key := range a.Keys {
			r, held, err := recallIn(ctx, tx, a.Storage, key, k.D.Lock)
			if err != nil {
				return err
			}
			switch {
			case held && r.Live():
				out.Joined++
				continue
			case held && r.Readable(a.Now):
				out.Online++
				continue
			}
			args := []any{a.Days, string(a.Speed), k.D.Time(a.Now), k.D.Time(a.Now), k.D.Time(a.Now), a.Storage, key}
			if held {
				_, err = tx.Exec(ctx, `UPDATE recalls SET state = 'requested', days = ?, speed = ?, requested_at = ?, updated = ?, due = ?,
					online_until = NULL, reason = '' WHERE storage = ? AND object_key = ?`, args...)
			} else {
				_, err = tx.Exec(ctx, `INSERT INTO recalls (days, speed, requested_at, updated, due, storage, object_key, state,
					online_until, reason) VALUES (?, ?, ?, ?, ?, ?, ?, 'requested', NULL, '')`, args...)
			}
			if err != nil {
				return err
			}
			out.Requested++
		}
		if a.Audit != nil {
			return tx.Audited(ctx, *a.Audit, a.Guard)
		}
		return nil
	})
	if err != nil {
		return tenant.RecallAsked{}, err
	}
	return out, nil
}

// Recall implements tenant.Recalls.
func (k Sending) Recall(ctx context.Context, storage, key string) (tenant.Recall, bool, error) {
	if err := tenant.ValidateRecallKey(storage, key); err != nil {
		return tenant.Recall{}, false, err
	}
	var out tenant.Recall
	var found bool
	err := k.Host.Read(ctx, "the recall cannot be read", func(tx SendingTx) error {
		var err error
		out, found, err = recallIn(ctx, tx, storage, key, "")
		return err
	})
	return out, found, err
}

// DueRecalls implements tenant.Recalls.
func (k Sending) DueRecalls(ctx context.Context, now time.Time, limit int) ([]tenant.Recall, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return nil, err
	}
	if limit < 1 || limit > tenant.MaxRecallBatch {
		return nil, fault.New(fault.Invalid, "a read of recalls due answers 1 to 1000")
	}
	var out []tenant.Recall
	err := k.Host.Read(ctx, "the recalls due cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT `+recallColumns+` FROM recalls WHERE state IN ('requested', 'in_progress') AND due <= ?
			ORDER BY due, storage, object_key LIMIT ?`, []any{k.D.Time(now), limit}, func(r Row) error {
			x, err := scanRecall(r)
			out = append(out, x)
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordRecall implements tenant.Recalls.
func (k Sending) RecordRecall(ctx context.Context, a tenant.RecallAnswer) (tenant.Recall, error) {
	if err := a.Validate(); err != nil {
		return tenant.Recall{}, err
	}
	var out tenant.Recall
	err := k.Host.Write(ctx, "the recall's answer cannot be recorded", func(tx SendingTx) error {
		r, found, err := recallIn(ctx, tx, a.Storage, a.Key, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no recall of this object")
		}
		next, err := tenant.NextRecall(r, a)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE recalls SET state = ?, updated = ?, due = ?, online_until = ?, reason = ?
			WHERE storage = ? AND object_key = ?`, string(next.State), k.D.Time(next.Updated), k.D.Time(next.Due),
			k.nullTime(next.OnlineUntil), next.Reason, a.Storage, a.Key); err != nil {
			return err
		}
		out = next
		return nil
	})
	if err != nil {
		return tenant.Recall{}, err
	}
	return out, nil
}

// RecallCounts implements tenant.Recalls.
func (k Sending) RecallCounts(ctx context.Context, now time.Time) (tenant.RecallCounts, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.RecallCounts{}, err
	}
	var out tenant.RecallCounts
	err := k.Host.Read(ctx, "the recalls cannot be counted", func(tx SendingTx) error {
		out = tenant.RecallCounts{}
		return tx.Query(ctx, `SELECT state, count(*) FROM recalls WHERE state <> 'online' OR online_until IS NULL OR online_until > ?
			GROUP BY state`, []any{k.D.Time(now)}, func(r Row) error {
			var state string
			var n int64
			if err := r.Scan(&state, &n); err != nil {
				return err
			}
			switch tenant.RecallState(state) {
			case tenant.RecallRequested:
				out.Requested = n
			case tenant.RecallInProgress:
				out.InProgress = n
			case tenant.RecallOnline:
				out.Online = n
			case tenant.RecallFailed:
				out.Failed = n
			}
			return nil
		})
	})
	return out, err
}
