package testkit

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// HeapPeak samples the live heap every 2 ms until stop is called, and stop
// returns the highest it saw: the bound a test of memory compares with.
func HeapPeak() (stop func() uint64) {
	var peak atomic.Uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	return func() uint64 {
		close(done)
		wg.Wait()
		return peak.Load()
	}
}
