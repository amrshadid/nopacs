package servecmd

import (
	"context"
	"errors"
	"testing"
	"time"
)

// orderRunner records, in steps, when it is started and stopped, and how
// long its stop was given.
type orderRunner struct {
	steps  *[]string
	budget time.Duration
}

// Start records the start.
func (o *orderRunner) Start(context.Context) error {
	*o.steps = append(*o.steps, "start")
	return nil
}

// Stop records the stop and its deadline.
func (o *orderRunner) Stop(ctx context.Context) error {
	*o.steps = append(*o.steps, "stop")
	if d, ok := ctx.Deadline(); ok {
		o.budget = time.Until(d)
	}
	return errors.New("still held")
}

// TestCorrectionsStopAfterTheDrainNotWithTheReception guards the order of
// the server's stop for the corrections' engines: started with the
// reception, they are not stopped with it (a request in flight still writes
// through their storages), but in the step after the drain, before the
// audit's last seal, within at most half of that step's time; a stop that
// fails still leaves the seal its turn.
func TestCorrectionsStopAfterTheDrainNotWithTheReception(t *testing.T) {
	var steps []string
	r := &orderRunner{steps: &steps}
	reception := runners{startOnly{r}}
	if err := reception.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reception.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0] != "start" {
		t.Fatalf("with the reception: %v, want the start alone", steps)
	}
	after := stopThen(r, func(context.Context) error {
		steps = append(steps, "seal")
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*afterDrainStopShare)
	defer cancel()
	err := after(ctx)
	if len(steps) != 3 || steps[1] != "stop" || steps[2] != "seal" {
		t.Fatalf("after the drain: %v, want the stop then the seal", steps)
	}
	if err == nil {
		t.Error("the stop's failure was not answered")
	}
	if r.budget <= 0 || r.budget > afterDrainStopShare {
		t.Errorf("the stop was given %v, want at most %v", r.budget, afterDrainStopShare)
	}
}
