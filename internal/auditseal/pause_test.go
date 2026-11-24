package auditseal

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestAPausedSealerSealsNothing guards Pause: a pass that comes due while
// the sealer is paused seals nothing and returns at once rather than
// waiting; once resumed (twice, which is harmless), the next pass seals
// every entry written before and during the pause.
func TestAPausedSealerSealsNothing(t *testing.T) {
	b := sqliteBackend(t)
	keys := keyFile(t, 1)
	s := b.sealer(t, signerOf(t, keys, 1), &clock{t: t0.Add(time.Hour)}, 0)
	write(t, b.master, 3, time.Minute)
	resume, err := s.Pause(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		s.Pass(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a pass waited on the pause instead of being skipped")
	}
	if got := seqsOf(t, b.master); len(got) != 0 {
		t.Fatalf("a pass while paused sealed %d entries", len(got))
	}
	write(t, b.master, 2, 2*time.Minute)
	resume()
	resume()
	s.Pass(context.Background())
	if got := seqsOf(t, b.master); len(got) != 5 {
		t.Fatalf("the pass after the pause sealed %d entries, want 5", len(got))
	}
}

// TestAPauseGivesUpBehindAStuckPass guards Pause's wait for a running pass:
// when its context ends first it returns unavailable with nothing paused,
// so a pass after it seals as usual.
func TestAPauseGivesUpBehindAStuckPass(t *testing.T) {
	b := sqliteBackend(t)
	keys := keyFile(t, 1)
	s := b.sealer(t, signerOf(t, keys, 1), &clock{t: t0.Add(time.Hour)}, 0)
	write(t, b.master, 2, time.Minute)
	s.passing <- struct{}{} // A pass that does not end.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	resume, err := s.Pause(ctx)
	<-s.passing
	if resume != nil || fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a pause behind a stuck pass answered %v", err)
	}
	s.Pass(context.Background())
	if got := seqsOf(t, b.master); len(got) != 2 {
		t.Fatalf("the pass after a pause that gave up sealed %d entries, want 2", len(got))
	}
}
