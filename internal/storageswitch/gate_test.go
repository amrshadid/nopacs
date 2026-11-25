package storageswitch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestGateWaitsThenRefuses guards the gate's wait: a writer at a paused
// gate waits while the pause lasts, is admitted the moment it is resumed,
// and is refused with ErrStorageSwitching, counted, once its wait runs out.
func TestGateWaitsThenRefuses(t *testing.T) {
	g := NewGate(GateConfig{Wait: 200 * time.Millisecond})
	g.pause("main")
	start := time.Now()
	_, err := g.Enter(context.Background(), "main")
	if !errors.Is(err, ErrStorageSwitching) || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("a writer at a paused gate answered %v after %v; want a refusal after its wait", err, time.Since(start))
	}
	if n := g.refusedCount("main"); n != 1 {
		t.Fatalf("the gate counted %d refusals, want 1", n)
	}
	entered := make(chan error, 1)
	go func() {
		release, err := g.Enter(context.Background(), "main")
		if err == nil {
			release()
		}
		entered <- err
	}()
	time.Sleep(50 * time.Millisecond)
	g.resume("main")
	if err := <-entered; err != nil {
		t.Fatalf("a writer waiting at a gate resumed was refused: %v", err)
	}
}

// TestPauseWaitsForWritersInFlight guards the order a switch relies on: a
// writer admitted before the pause holds the gate until it releases, so
// the pause's wait for idle ends only then, and a release called twice
// counts once.
func TestPauseWaitsForWritersInFlight(t *testing.T) {
	g := NewGate(GateConfig{Wait: -1})
	release, err := g.Enter(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	other, err := g.Enter(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	g.pause("main")
	idle := g.idleOf("main")
	release()
	release()
	select {
	case <-idle:
		t.Fatal("the gate was idle with a writer still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	other()
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("the gate was not idle once every writer released")
	}
}
