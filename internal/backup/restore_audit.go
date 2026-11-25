package backup

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
)

// What a restore, and a reconcile of a site's own copy, write in the
// master's log.

// restoreMethod is the method of the audit entries a restore writes.
const restoreMethod = "nopacs backup restore"

// The local users of the audit entries a restore and a reconcile of a
// site's own copy write, and the start of their rebuild jobs' ids.
const (
	restoreUser   = "restore"
	reconcileUser = "reconcile"
)

// suspendKeys suspends every key the master holds, with an audit entry of
// method by user (a restore's, or a reconcile's of a site's own copy).
func suspendKeys(ctx context.Context, m master.Store, method, user string, now time.Time) (int64, error) {
	ks, ok := m.(master.KeySuspension)
	if !ok {
		return 0, fault.New(fault.Internal, "backup: this database type cannot suspend keys")
	}
	return ks.SuspendKeys(ctx, now, master.AuditEntry{ID: server.NewID(), Time: now, LocalUser: user, Method: method,
		Object: "keys", Outcome: master.OutcomeDone,
		Reason: "every key a restore brings back is suspended, since a key revoked after its recovery point would work again; the local operator reinstates each key to be used again"})
}

// appendRestoreAudit writes the restore's own entry, with what it lost and
// the tenants it left fenced, in the master's log.
func appendRestoreAudit(ctx context.Context, m master.Store, rep Report, now time.Time) error {
	e := master.AuditEntry{ID: server.NewID(), Time: now, LocalUser: restoreUser, Method: restoreMethod, Object: rep.Installation,
		Outcome: master.OutcomeDone, Reason: "the installation restored from a backup made at " + rep.BackupTime.Format(time.RFC3339)}
	pairs := []string{"backup_time", rep.BackupTime.Format(time.RFC3339Nano), "keys_suspended", strconv.FormatInt(rep.KeysSuspended, 10)}
	if rep.ReconcileOnly {
		e.LocalUser, e.Method = reconcileUser, reconcileMethod
		e.Reason = "the installation's databases, restored by the site's own point-in-time recovery, reconciled with its storages"
		pairs = []string{"keys_suspended", strconv.FormatInt(rep.KeysSuspended, 10)}
	}
	for _, t := range rep.Tenants {
		if t.Fenced != "" {
			pairs = append(pairs, "not_reconciled."+t.Tenant, "fenced")
		}
		var files, receptions int64
		for _, r := range t.Loss.Receptions {
			files += r.Files
			receptions += r.Receptions
		}
		pairs = append(pairs, "lost_receptions."+t.Tenant, strconv.FormatInt(receptions, 10),
			"lost_files."+t.Tenant, strconv.FormatInt(files, 10),
			"lost_quarantine_files."+t.Tenant, strconv.FormatInt(t.Loss.QuarantineFiles, 10))
	}
	return m.AppendAudit(ctx, e.WithDetails(pairs...))
}
