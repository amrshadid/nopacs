package correct

import (
	"context"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// SetCrashAt makes the engine stop at each durable step for which at
// answers an error, as a crash there would, nothing after it run.
func (e *Engine) SetCrashAt(at func(step string) error) { e.seams.at = at }

// SetHeld makes the engine's purge ask held before each object.
func (e *Engine) SetHeld(held func(context.Context, tenant.Correction) bool) { e.seams.held = held }
