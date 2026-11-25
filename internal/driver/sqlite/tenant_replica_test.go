package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestSwapRefusesChangesSinceBegin guards that a swap checks again, in its
// own transaction, what the begin of its switch checked: a correction of a
// scan on the primary prepared, or a rebuild queued, since the switch began
// refuses the swap, the places and the switch left as they were. The port
// refuses both while a switch runs, so the test makes each in a window it
// opens in the database: the switch marked refused while the change is
// made, then accepted again, as a change that passed the begin would leave
// it.
func TestSwapRefusesChangesSinceBegin(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, ts *Tenant)
		want   string
	}{
		{"a correction prepared", func(t *testing.T, ts *Tenant) {
			mustExecTenant(t, ts, `INSERT INTO corrections (id, kind, level, state, records_state, origin, subject_label, session_label,
				scan_label, instance_uid, to_label, quarantine_id, reason, key_id, confirm_token, created, committed_at, purge_after, ended_at,
				state_reason, old_revision, new_revision, old_sha256, new_sha256, received_sha256, new_size, instance_key, instance_size,
				restore_line, arrivals_allowed, undo_reason, unresolved, subjects, sessions, scans, instances, bytes, objects_copied,
				objects_superseded) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5FAV', 'relabel', 'scan', 'prepared', '', 'person', 'sub-01', 'ses-1',
				'scan-1', '', 'scan-2', '', 'wrong label', '', NULL, 0, NULL, NULL, NULL, '', 0, 0, NULL, NULL, NULL, 0, '', 0, NULL, 0, '',
				0, 0, 0, 1, 0, 0, 0, 0)`,
				`INSERT INTO correction_scans (correction_id, series_uid, study_uid, storage, subject_label, session_label, scan_label,
				new_subject, new_session, new_scan, instances, bytes) VALUES ('01ARZ3NDEKTSV4RRFFQ69G5FAV', '2.25.2.1', '2.25.1.1', 'store',
				'sub-01', 'ses-1', 'scan-1', 'sub-01', 'ses-1', 'scan-2', 0, 0)`)
		}, tenant.BlockerCorrectionPrepared},
		{"a rebuild queued", func(t *testing.T, ts *Tenant) {
			_, err := ts.CreateJob(context.Background(), tenant.NewJob{ID: "rb-1", Kind: tenant.JobRebuild,
				Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}, Created: swapT0}, swapAudit("a-rb"), nil)
			if err != nil {
				t.Fatal(err)
			}
		}, tenant.BlockerRebuild},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			ts := openTenantTest(t, "T0000000000001").(*Tenant)
			for _, s := range []tenant.Storage{
				{Name: "store", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "store"}},
				{Name: "copy", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast", Path: "copy"}, ReplicaOf: "store"},
			} {
				if _, err := ts.AddStorage(ctx, s, swapT0, swapAudit("a-"+s.Name), nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := ts.BeginSwitch(ctx, tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1",
				Now: swapT0}, swapAudit("a-begin"), nil); err != nil {
				t.Fatal(err)
			}
			mustExecTenant(t, ts, `UPDATE storage_switches SET state = 'refused', refusal = 'server restarted', ended_at = 0`)
			c.change(t, ts)
			mustExecTenant(t, ts, `UPDATE storage_switches SET state = 'accepted', refusal = '', ended_at = NULL`)
			_, err := ts.SwapStorages(ctx, tenant.SwitchSwap{ID: "sw-1", Now: swapT0.Add(time.Second)}, swapAudit("a-swap"))
			if fault.KindOf(err) != fault.Precondition || fault.Message(err) != c.want {
				t.Fatalf("SwapStorages after %s since the begin = %v; want the precondition failure %q", c.name, err, c.want)
			}
			if s, err := ts.Storage(ctx, "store"); err != nil || s.Directory.Path != "store" || s.SwitchGeneration != 0 {
				t.Errorf("a refused swap changed the primary: %+v, %v", s, err)
			}
			if sw, err := ts.Switch(ctx, "sw-1"); err != nil || sw.State != tenant.SwitchAccepted {
				t.Errorf("a switch whose swap was refused reads %+v, %v", sw, err)
			}
		})
	}
}

// swapT0 is the time TestSwapRefusesChangesSinceBegin works at.
var swapT0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// swapAudit returns an audit entry of id.
func swapAudit(id string) master.AuditEntry {
	return master.AuditEntry{ID: id, Time: swapT0, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000", Method: "Test",
		TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// mustExecTenant runs each statement on the tenant's write connection,
// failing the test on an error.
func mustExecTenant(t *testing.T, ts *Tenant, statements ...string) {
	t.Helper()
	for _, s := range statements {
		if _, err := ts.p.write.ExecContext(context.Background(), s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	}
}
