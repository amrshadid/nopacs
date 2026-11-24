package registry

import (
	"sync"
)

// nameLocks is one mutex per tenant name in use, made on first use and
// dropped when nobody holds or waits for it, so it does not grow with every
// name ever created.
type nameLocks struct {
	mu sync.Mutex
	m  map[string]*nameLock
}

// nameLock is one name's mutex and how many hold or wait for it.
type nameLock struct {
	sync.Mutex
	refs int
}

// lock locks name and returns the function that unlocks it.
func (n *nameLocks) lock(name string) func() {
	n.mu.Lock()
	if n.m == nil {
		n.m = map[string]*nameLock{}
	}
	l := n.m[name]
	if l == nil {
		l = &nameLock{}
		n.m[name] = l
	}
	l.refs++
	n.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		n.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(n.m, name)
		}
		n.mu.Unlock()
	}
}
