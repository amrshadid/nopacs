package servecmd

import (
	"context"
	"sync"

	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/trial"
)

// editJudge returns the judge of a tenant's edits (correct.ProfileJudge):
// each new revision de-identified in memory by the profile version that
// de-identified its instance, or, for one archived as received, by its
// receiver's profile now, as a file archived now would be, and the check
// after run over what that writes. Its engines are compiled once a version
// (a version never changes), under keys of the judge's own made here
// (trial.ThrowawayKeys), never the tenant's, with lookups that read no
// table and assign no code (correct.FixedLookups). The bounds are the
// reception's, as they apply when the tenant's engine is made: the header
// limit, the pixel limit, and the server's one bound on de-identification's
// memory.
func editJudge(ctx context.Context, values settingvalue.Reader, src settingvalue.Source, p tenant.Pipeline,
	memory check.Memory) (correct.ProfileJudge, error) {
	profiles, ok := p.(tenant.Deidentification)
	if !ok {
		return correct.ProfileJudge{}, fault.New(fault.Internal, "the tenant's database keeps no profiles")
	}
	keys, err := trial.ThrowawayKeys()
	if err != nil {
		return correct.ProfileJudge{}, err
	}
	headers, err := values.HeaderLimit(ctx)
	if err != nil {
		return correct.ProfileJudge{}, err
	}
	pixels, err := values.OnTenant(src).PixelLimit(ctx)
	if err != nil {
		return correct.ProfileJudge{}, err
	}
	var mu sync.Mutex
	engines := map[tenant.ProfileRef]*engine.Engine{}
	return correct.ProfileJudge{
		Profile: func(ctx context.Context, t tenant.EditTarget) (tenant.ProfileRef, error) {
			r, err := p.Receiver(ctx, t.Receiver)
			if fault.KindOf(err) == fault.NotFound {
				return tenant.ProfileRef{}, fault.New(fault.Precondition,
					"the receiver an instance archived as received came through was removed; no profile judges its edit")
			}
			return r.Profile, err
		},
		Engine: func(ctx context.Context, ref tenant.ProfileRef) (*engine.Engine, error) {
			mu.Lock()
			defer mu.Unlock()
			if en, ok := engines[ref]; ok {
				return en, nil
			}
			v, err := profiles.ProfileVersion(ctx, ref.Name, ref.Version)
			if fault.KindOf(err) == fault.NotFound {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			// Loaded through the profile language, which checks the expanded
			// text against its checksum, as a reception loads it.
			version, err := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum)
			if err != nil {
				return nil, nil
			}
			en, err := engine.New(version.Expanded(), keys, engine.Options{Lookups: correct.FixedLookups{}, Method: ref.String()})
			if err != nil {
				return nil, nil
			}
			engines[ref] = en
			return en, nil
		},
		Options: receive.TrialOptions{Keys: keys, HeaderLimit: headers, PixelLimit: pixels, Memory: memory,
			Check: func(n int64) receive.Checker { return check.New(check.Options{MaxFileBytes: n}) }},
	}, nil
}
