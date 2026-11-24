package receive

import (
	"context"
	"sync"
)

// archiveParallel is how many of one scan's files are put to, or read back
// from, the storage at once. Measured with BenchmarkLandToVerified: one at a
// time, a series of small slices took a storage round trip per file in a
// row; eight overlap the directory's syncs and S3's requests while one
// scan's work stays a small share of a worker pool, and a storage's own
// limits (open files, connections) are far above it.
const archiveParallel = 8

// parallel calls fn for every index below n, at most archiveParallel at
// once, and returns the first error any call returned, after every call it
// started has ended. Once one fails, no new call starts, and the context the
// running ones were given ends, so a failed scan does not keep writing.
func parallel(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	if n == 1 {
		return fn(ctx, 0)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	slots := make(chan struct{}, archiveParallel)
	for i := 0; i < n; i++ {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := fn(ctx, i); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}()
	}
	wg.Wait()
	if first == nil && ctx.Err() != nil {
		// The caller's context ended before every call started.
		return context.Cause(ctx)
	}
	return first
}
