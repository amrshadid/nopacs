package receive

import (
	"sync"
	"time"
)

// ThroughputWindow is how far back Throughput measures, the contract's
// 10 seconds (monitor.proto, Throughput).
const ThroughputWindow = 10 * time.Second

// Throughput is how fast files moved through a tenant's pipeline over the
// last ThroughputWindow, per second: files received into the incoming area
// and acknowledged, and files archived (committed to the catalog). It is
// held in memory only: after a restart it starts again from zero.
type Throughput struct {
	FilesReceivedPerSecond float64
	BytesReceivedPerSecond float64
	FilesArchivedPerSecond float64
	BytesArchivedPerSecond float64
}

// meter counts what passes, by the second, in a ring of one bucket per
// second of the window: memory stays fixed however much passes.
type meter struct {
	mu      sync.Mutex
	buckets [int(ThroughputWindow / time.Second)]meterBucket
}

// meterBucket is one second's counts, for the second sec (Unix time).
type meterBucket struct {
	sec                         int64
	filesIn, bytesIn            int64
	filesArchived, bytesArchive int64
}

// bucket returns the bucket of the second of now, emptied when it last held
// an older second.
func (m *meter) bucket(now time.Time) *meterBucket {
	sec := now.Unix()
	b := &m.buckets[sec%int64(len(m.buckets))]
	if b.sec != sec {
		*b = meterBucket{sec: sec}
	}
	return b
}

// received counts a file received at now.
func (m *meter) received(now time.Time, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.bucket(now)
	b.filesIn++
	b.bytesIn += bytes
}

// archived counts files archived at now.
func (m *meter) archived(now time.Time, files, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.bucket(now)
	b.filesArchived += files
	b.bytesArchive += bytes
}

// rates returns the throughput over the window that ends at now: the
// counts of the seconds within it, divided by its length.
func (m *meter) rates(now time.Time) Throughput {
	m.mu.Lock()
	defer m.mu.Unlock()
	end := now.Unix()
	start := end - int64(len(m.buckets)) + 1
	var t Throughput
	for _, b := range m.buckets {
		if b.sec < start || b.sec > end {
			continue
		}
		t.FilesReceivedPerSecond += float64(b.filesIn)
		t.BytesReceivedPerSecond += float64(b.bytesIn)
		t.FilesArchivedPerSecond += float64(b.filesArchived)
		t.BytesArchivedPerSecond += float64(b.bytesArchive)
	}
	w := ThroughputWindow.Seconds()
	t.FilesReceivedPerSecond /= w
	t.BytesReceivedPerSecond /= w
	t.FilesArchivedPerSecond /= w
	t.BytesArchivedPerSecond /= w
	return t
}

// Throughput returns how fast files moved through the pipeline over the
// last ThroughputWindow.
func (p *Pipeline) Throughput() Throughput { return p.meter.rates(p.cfg.Clock.Now()) }
