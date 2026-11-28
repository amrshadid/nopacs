// Package forward makes the copies a routing rule set's forward rules send
// with a destination profile: each archived file de-identified once more,
// in memory, by the profile version the rule names, and checked after by
// the same check a reception runs, before any byte of it is sent. A copy
// that cannot be made so, or that the check finds a removed value in, is
// refused with a fixed cause, never sent as archived.
package forward

import (
	"context"
	"sync"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The causes of a copy refused. Each is a fixed word a job's failure
// records as its detail; none carries a value of the file.
const (
	// CauseNoProfile: the tenant has no such profile version, or this
	// build cannot run it.
	CauseNoProfile = "copy_profile_unusable"
	// CauseUndecided: the profile has a lookup rule, whose codes a copy
	// neither reads nor assigns.
	CauseUndecided = "copy_lookup"
)

// Copies makes one tenant's copies. Its engines are compiled once a
// version, under the tenant's keys of de-identification, so a study sent
// twice to a destination is remapped the same both times. It is safe for
// use by several senders at once.
type Copies struct {
	// Profiles reads the tenant's profile versions.
	Profiles tenant.Profiles
	// Keys returns the tenant's keys of de-identification.
	Keys func(ctx context.Context) (engine.Keys, error)
	// Options returns the bounds a copy runs within, the reception's: the
	// header limit, the pixel limit, the bound on de-identification's
	// memory, and the check after. Its Keys are set by Copy.
	Options func(ctx context.Context) (receive.TrialOptions, error)

	mu      sync.Mutex
	engines map[tenant.ProfileRef]*engine.Engine
}

// Copy returns file, an archived DICOM file, de-identified by the profile
// version ref and checked after; or, when it cannot be made, "" bytes and
// the cause it is refused for (a quarantine reason, or one of the causes
// above), which a job records; or an error that says nothing of the file
// (the database or the bound on memory did not answer), after which the
// copy is tried again later.
func (c *Copies) Copy(ctx context.Context, ref tenant.ProfileRef, file []byte) ([]byte, string, error) {
	keys, err := c.Keys(ctx)
	if err != nil {
		return nil, "", err
	}
	en, cause, err := c.engine(ctx, ref, keys)
	if err != nil || cause != "" {
		return nil, cause, err
	}
	opts, err := c.Options(ctx)
	if err != nil {
		return nil, "", err
	}
	opts.Keys, opts.Receiver, opts.NoValues = keys, nil, false
	t, err := receive.TryFile(ctx, en, file, engine.Reception{}, opts)
	switch {
	case err != nil:
		return nil, "", err
	case t.Undecided:
		return nil, CauseUndecided, nil
	case t.Hold != nil:
		cause := string(t.Hold.Reason)
		if d := t.Hold.Details["cause"]; d != "" {
			cause += "/" + d
		}
		return nil, cause, nil
	case t.Written == nil:
		return nil, "", fault.New(fault.Internal, "a copy was neither written nor held")
	}
	return t.Written, "", nil
}

// engine returns the engine of a version, compiled once; a cause when the
// version cannot run.
func (c *Copies) engine(ctx context.Context, ref tenant.ProfileRef, keys engine.Keys) (*engine.Engine, string, error) {
	c.mu.Lock()
	en := c.engines[ref]
	c.mu.Unlock()
	if en != nil {
		return en, "", nil
	}
	v, err := c.Profiles.ProfileVersion(ctx, ref.Name, ref.Version)
	if fault.KindOf(err) == fault.NotFound {
		return nil, CauseNoProfile, nil
	}
	if err != nil {
		return nil, "", err
	}
	// Loaded through the profile language, which checks the expanded text
	// against its checksum, as a reception loads it.
	version, err := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum)
	if err != nil {
		return nil, CauseNoProfile, nil
	}
	en, err = engine.New(version.Expanded(), keys, engine.Options{Lookups: noLookups{}, Method: ref.String()})
	if err != nil {
		return nil, CauseNoProfile, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.engines == nil {
		c.engines = map[tenant.ProfileRef]*engine.Engine{}
	}
	c.engines[ref] = en
	return en, "", nil
}

// noLookups is the lookup tables of a copy: none read, none assigned, so a
// lookup rule leaves the copy undecided or held, and it is refused, never
// sent with a code a reception did not assign.
type noLookups struct{}

// Code reports no code for any value.
func (noLookups) Code(context.Context, string, string) (string, bool, error) { return "", false, nil }

// Assign assigns nothing: every code is empty, which the engine refuses as
// a bad value, so the copy is held.
func (noLookups) Assign(_ context.Context, keys []engine.LookupKey) ([]string, error) {
	return make([]string, len(keys)), nil
}
