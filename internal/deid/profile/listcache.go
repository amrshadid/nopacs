package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// DefaultListCacheBytes is the default bound of the memory the compiled
// text lists of stored versions are kept in (SetListCacheBytes).
const DefaultListCacheBytes = 256 << 20

// automatonNodeBytes is what a node of an automaton holds: 19 bytes
// (textmatch.Matcher), and the room its slices were made with.
const automatonNodeBytes = 24

// listCache holds the compiled lists of stored versions by the SHA-256 of
// their canonical text, so the receptions, jobs and corrections of one
// version share one set of automata, built once by one caller while the
// others wait (one build per key). It is bounded by the bytes of the
// automata it holds, the text's and the raw ones its users add (Retain),
// the least recently used dropped first; a dropped set lives on only in
// the Programs still holding it, and is built again when next asked for.
// A document's lists (a validation, a trial) never enter it.
var listCache = struct {
	sync.Mutex
	bound   int64
	total   int64
	entries map[string]*listEntry
	// clock orders the entries by last use.
	clock uint64
}{bound: DefaultListCacheBytes, entries: map[string]*listEntry{}}

// listEntry is one version's lists in the cache, or their build under way.
type listEntry struct {
	done  chan struct{}
	lists *TextLists
	used  uint64
}

// SetListCacheBytes sets the bound of the memory the compiled text lists
// of stored versions are kept in (DefaultListCacheBytes when n is not
// positive), dropping the least recently used past it.
func SetListCacheBytes(n int64) {
	if n <= 0 {
		n = DefaultListCacheBytes
	}
	listCache.Lock()
	defer listCache.Unlock()
	listCache.bound = n
	evictLists("")
}

// sharedTextLists returns a stored version's lists compiled, from the
// cache, or compiled once and kept, recording any problem at top (lists
// with a problem are not kept).
func sharedTextLists(top at, lists []*v1.TextList) *TextLists {
	h := sha256.New()
	for _, l := range lists {
		c, err := Canonical(l)
		if err != nil {
			return compileTextLists(top, textListsOf(lists), true)
		}
		h.Write(c)
		h.Write([]byte{0})
	}
	key := hex.EncodeToString(h.Sum(nil))
	listCache.Lock()
	if e, ok := listCache.entries[key]; ok {
		listCache.clock++
		e.used = listCache.clock
		listCache.Unlock()
		<-e.done
		if e.lists != nil {
			return e.lists
		}
		// The build failed; this caller builds and records the problems.
		return compileTextLists(top, textListsOf(lists), true)
	}
	e := &listEntry{done: make(chan struct{})}
	listCache.entries[key] = e
	listCache.Unlock()

	before := len(top.c.list)
	l := compileTextLists(top, textListsOf(lists), true)

	listCache.Lock()
	defer listCache.Unlock()
	if l == nil || len(top.c.list) > before {
		delete(listCache.entries, key)
		close(e.done)
		return l
	}
	e.lists = l
	l.cacheKey = key
	listCache.clock++
	e.used = listCache.clock
	if l.m != nil {
		l.retained.Store(int64(l.m.Nodes()) * automatonNodeBytes)
	}
	listCache.total += l.retained.Load()
	close(e.done)
	evictLists(key)
	return l
}

// evictLists drops the least recently used entries, but keep, while the
// cache holds more than its bound. The caller holds the lock.
func evictLists(keep string) {
	for listCache.total > listCache.bound {
		var oldest string
		var at uint64
		for k, e := range listCache.entries {
			if k == keep || e.lists == nil {
				continue
			}
			if oldest == "" || e.used < at {
				oldest, at = k, e.used
			}
		}
		if oldest == "" {
			return
		}
		listCache.total -= listCache.entries[oldest].lists.retained.Load()
		delete(listCache.entries, oldest)
	}
}

// Retain counts n more bytes (fewer when negative) a user of the lists
// keeps built from them, such as the check's raw automata, against the
// cache's bound when the lists are in it.
func (l *TextLists) Retain(n int64) {
	if l == nil {
		return
	}
	l.retained.Add(n)
	if l.cacheKey == "" {
		return
	}
	listCache.Lock()
	defer listCache.Unlock()
	if e, ok := listCache.entries[l.cacheKey]; ok && e.lists == l {
		listCache.total += n
		evictLists(l.cacheKey)
	}
}

// Retained returns the bytes of the automata the lists hold: the text's,
// and those their users added (Retain).
func (l *TextLists) Retained() int64 {
	if l == nil {
		return 0
	}
	return l.retained.Load()
}

// ListCacheBytes returns the bytes the cache of stored versions' lists
// counts, its bound and how many versions it holds.
func ListCacheBytes() (total, bound int64, entries int) {
	listCache.Lock()
	defer listCache.Unlock()
	return listCache.total, listCache.bound, len(listCache.entries)
}
