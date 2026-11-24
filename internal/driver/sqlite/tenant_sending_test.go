package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
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
	path := filepath.Join(t.TempDir(), "tenant.db")
	s, err := OpenTenant(ctx, path, "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	w, ins := tenanttest.StartedJobUnit(t, s)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `INSERT INTO job_failures SELECT job_id, s.subject_label, s.session_label, s.label, ?,
		js.scan_id, 0, 'peer_refused', 0, '', '', '', 0 FROM job_scans AS js JOIN scans AS s ON s.id = js.scan_id
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
