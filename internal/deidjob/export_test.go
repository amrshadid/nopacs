package deidjob

import (
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/receive"
)

// withJudged sets the seam called after each instance is judged.
func withJudged(c Config, f func(sopInstanceUID string)) Config { c.seams.judged = f; return c }

// withTried sets the seam given each dry run's trial.
func withTried(c Config, f func(t receive.Trial)) Config { c.seams.tried = f; return c }

// withMaxFileBytes sets the largest file a unit reads.
func withMaxFileBytes(c Config, n int64) Config { c.seams.maxFileBytes = n; return c }

// withKeys sets the source of a unit's own keys.
func withKeys(c Config, f func() (engine.Keys, error)) Config { c.seams.keys = f; return c }
