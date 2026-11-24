package servecmd

import (
	"context"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/settingvalue"
)

// readLimitsEvery is how long the bounds of open reads are kept once read,
// so a change of read.per-caller or read.per-server applies to the next read
// within it, and a burst of reads asks the master once a second, not once a
// read.
const readLimitsEvery = time.Second

// readLimitsTimeout bounds one read of the settings, so a stalled master
// cannot hold up the reads that ask.
const readLimitsTimeout = 5 * time.Second

// readLimits returns the bounds of open reads (checkedread.Slots.Limits)
// read from the installation's settings and kept for readLimitsEvery. When
// the settings cannot be read, the last bounds read stay in force, or the
// defaults before any was read: a failure of the master must neither lift
// the bound nor refuse every read.
func readLimits(values settingvalue.Reader, now func() time.Time) func() (int, int) {
	var (
		mu        sync.Mutex
		read      time.Time
		perCaller = checkedread.MaxOpenReads
		perServer = checkedread.MaxServerReads
	)
	return func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		if !read.IsZero() && now().Sub(read) < readLimitsEvery {
			return perCaller, perServer
		}
		ctx, cancel := context.WithTimeout(context.Background(), readLimitsTimeout)
		defer cancel()
		if c, s, err := values.ReadLimits(ctx); err == nil {
			perCaller, perServer = c, s
		}
		read = now()
		return perCaller, perServer
	}
}
