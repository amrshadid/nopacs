//go:build !clock

package clock

import (
	"testing"
	"time"
)

// TestOffIgnoresEnv guards the shipped build: with an offset in the
// environment, Now is still the system's time.
func TestOffIgnoresEnv(t *testing.T) {
	t.Setenv(EnvOffset, "9999h")
	if d := time.Until(Now()); d > time.Second || d < -time.Second {
		t.Fatalf("Now is %v from the system's time, want none", d)
	}
}

// TestSystemWaits guards System: its Now is Now and its After fires.
func TestSystemWaits(t *testing.T) {
	if d := time.Until(System{}.Now()); d > time.Second || d < -time.Second {
		t.Fatalf("System.Now is %v from the system's time, want none", d)
	}
	select {
	case <-System{}.After(time.Millisecond):
	case <-time.After(5 * time.Second):
		t.Fatal("After did not fire")
	}
}
