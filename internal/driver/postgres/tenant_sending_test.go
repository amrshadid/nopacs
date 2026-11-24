package postgres

import (
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// TestJobFailureListedOnce guards that a progress write whose failure is
// already listed for its job fails, rather than dropping the row and
// counting the instance with no failure listed: an instance is answered
// once per job, so a second row is a bug, which the port's own writes
// cannot make and this test plants with SQL.
func TestJobFailureListedOnce(t *testing.T) {
	ctx := context.Background()
	address := newSchema(t)
	s := openTenantAt(t, address, "dup")
	w, ins := tenanttest.StartedJobUnit(t, s)
	conn := admin(t, address)
	if _, err := conn.Exec(ctx, `INSERT INTO job_failures SELECT job_id, s.subject_label, s.session_label, s.label, $1,
		js.scan_id, false, 'peer_refused', 0, '', '', '', now() FROM job_scans AS js JOIN scans AS s ON s.id = js.scan_id
		WHERE js.job_id = 'j1'`, ins[0].SOPInstanceUID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordJobProgress(ctx, tenanttest.RefuseFirst(w, ins)); err == nil {
		t.Error("a progress write listed a failure the job lists already")
	}
	if j, err := s.Job(ctx, "j1"); err != nil || j.Counts.InstancesRefused != 0 {
		t.Errorf("the refused write left the job %+v, %v; want nothing counted", j.Counts, err)
	}
}
