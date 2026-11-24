package tenanttest

import (
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// sendingEnv returns a sending test's helpers on a store a driver's own
// test opened, its ids made from seq on.
func sendingEnv(t *testing.T, p tenant.Pipeline, seq int) *senv {
	return &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: p, ctx: context.Background(), seq: seq}, p: p}}}
}

// StartedJobUnit sets up, through the port, a job "j1" of a session of one
// scan of two instances to a sender "orthanc", lists it, claims its scan's
// unit and starts it, and returns the unit and the scan's stream. It is
// for a driver's own test of what the port cannot reach, such as a row the
// store never writes itself.
func StartedJobUnit(t *testing.T, p tenant.Pipeline) (tenant.Work, []tenant.ScanInstance) {
	t.Helper()
	e := sendingEnv(t, p, 0)
	_, target := e.setup("orthanc", 1, 2)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	w := e.claim("orthanc", "w", at(3700))
	_, err := p.StartJobUnit(e.ctx, w.Fence(), at(3700))
	e.ok("StartJobUnit", err)
	return w, e.instances(w, "")
}

// RefuseFirst returns the progress of a unit's write that refuses the
// first instance of its stream, as the peer answered.
func RefuseFirst(w tenant.Work, ins []tenant.ScanInstance) tenant.JobProgress {
	return progress(w, "", ins[:1], 0, func(int, tenant.ScanInstance) tenant.JobFailureCode { return tenant.FailurePeerRefused },
		at(3710))
}
