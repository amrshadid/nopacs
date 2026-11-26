package pace

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// TestPacerHoldsTheRate guards the pacing every background reader and
// writer shares: N bytes at a rate wait N divided by the rate, less the
// second an idle storage may bank, whatever the pieces; a rate of zero
// waits nothing; and Reader passes every byte through and counts it.
func TestPacerHoldsTheRate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var waited time.Duration
	p := New(func() time.Time { return now }, func(_ context.Context, d time.Duration) error {
		if d > 0 {
			waited += d
			now = now.Add(d)
		}
		return nil
	})
	src := bytes.Repeat([]byte{7}, 300<<10)
	r := &Reader{Ctx: context.Background(), R: bytes.NewReader(src), P: p, Rate: 100 << 10}
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, src) || r.N != int64(len(src)) {
		t.Fatalf("read %d bytes, counted %d, %v", len(got), r.N, err)
	}
	if want := 3*time.Second - MaxBurst; waited < want-time.Millisecond || waited > 3*time.Second {
		t.Errorf("300 KiB at 100 KiB a second waited %v, want about %v", waited, want)
	}
	before := waited
	if err := p.Take(context.Background(), 1<<20, 0); err != nil || waited != before {
		t.Errorf("a rate of zero waited %v, %v", waited-before, err)
	}
}
