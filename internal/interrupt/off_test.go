//go:build !interrupt

package interrupt

import "testing"

// TestOffIgnoresEnv guards the shipped build: with the variable naming a
// known step, At returns at that step and the process goes on.
func TestOffIgnoresEnv(t *testing.T) {
	t.Setenv(Env, "correction:replace:meta")
	At("correction", "replace", "meta")
}
