package holds

import "sync"

// recordLocks holds, for each hold being written, the lock its writers
// share: the writer of a place or a release and the background worker of
// the same tenant are separate Writers, and without one lock the worker
// could put a record from a hold it read before a release began between
// the release's put and its mark, leaving a storage with a record older
// than the one the database says it has. One server process serves a
// tenant at a time (its writes are fenced), so a lock in the process is
// enough.
var recordLocks = &lockSet{held: map[string]*holdLock{}}

// lockSet is the locks of the holds being written, each made on its first
// user and dropped after its last.
type lockSet struct {
	mu   sync.Mutex
	held map[string]*holdLock
}

// holdLock is one hold's lock and how many hold or wait for it.
type holdLock struct {
	mu    sync.Mutex
	users int
}

// lock takes the lock of the hold of that id in the tenant whose id is
// tenantID, and returns its release.
func (s *lockSet) lock(tenantID, id string) func() {
	key := tenantID + "/" + id
	s.mu.Lock()
	l, ok := s.held[key]
	if !ok {
		l = &holdLock{}
		s.held[key] = l
	}
	l.users++
	s.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.mu.Lock()
		if l.users--; l.users == 0 {
			delete(s.held, key)
		}
		s.mu.Unlock()
	}
}
