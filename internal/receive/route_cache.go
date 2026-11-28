package receive

import (
	"strconv"
	"sync"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxCachedRuleSets bounds the compiled rule sets kept: a tenant's
// receivers each run one, and a version moved away from stays until the
// cache is full, when it is emptied and fills again.
const maxCachedRuleSets = 256

// ruleCache keeps rule set versions compiled, by receiver and version: a
// version is never changed once stored, so its compilation is reused for
// every reception it routes. A rule set of version 0 (one never stored,
// only a test makes) is compiled every time.
type ruleCache struct {
	mu sync.Mutex
	m  map[string]*tenant.CompiledRules
}

// get returns rs compiled.
func (c *ruleCache) get(rs tenant.RuleSet) *tenant.CompiledRules {
	if rs.Version == 0 {
		return rs.Compile()
	}
	key := rs.Receiver + "@" + strconv.Itoa(int(rs.Version))
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.m[key]; ok {
		return r
	}
	if c.m == nil || len(c.m) >= maxCachedRuleSets {
		c.m = map[string]*tenant.CompiledRules{}
	}
	r := rs.Compile()
	c.m[key] = r
	return r
}
