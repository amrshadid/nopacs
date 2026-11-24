package rebuild

import "time"

// withRenew returns c with its lease renewed every d.
func withRenew(c Config, d time.Duration) Config {
	c.seams.renew = d
	return c
}

// withAfterScan returns c calling fn after each scan's last write.
func withAfterScan(c Config, fn func(key string)) Config {
	c.seams.afterScan = fn
	return c
}

// withBatch returns c writing at most n instances of a scan at a time.
func withBatch(c Config, n int) Config {
	c.seams.batch = n
	return c
}

// withBeforeDrop returns c calling fn before a job's shadow is dropped.
func withBeforeDrop(c Config, fn func(jobID string)) Config {
	c.seams.beforeDrop = fn
	return c
}
