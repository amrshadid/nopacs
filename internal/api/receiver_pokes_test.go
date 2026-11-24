package api_test

import (
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Health answers as a server that runs no receivers does: down, disabled,
// or not started.
func (p *testParts) Health(_ string, r tenant.Receiver) api.Health {
	if !r.Enabled {
		return api.Health{State: api.HealthDown, Reason: api.DownDisabled, Since: r.Updated}
	}
	return api.Health{State: api.HealthDown, Reason: api.DownOther, Detail: "not started", Since: r.Updated}
}

// ReceiversChanged records the tenant.
func (p *testParts) ReceiversChanged(tenantName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.poked = append(p.poked, tenantName)
}

// receiversPoked returns how many times the handlers said the tenant's
// receivers changed.
func (p *testParts) receiversPoked(tenantName string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.poked {
		if s == tenantName {
			n++
		}
	}
	return n
}
