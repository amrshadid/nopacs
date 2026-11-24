package trial

import (
	"context"
	"math"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
)

// LocalTrial is a trial run in the process that asks for it, with no
// server and no tenant: the command line's "profile try --local". Its
// profile is a document the caller expanded (profile.Expand); its files
// are read one at a time, so their number is not bounded by memory.
type LocalTrial struct {
	// Expanded is the profile, expanded.
	Expanded *v1.ExpandedProfile
	// Checksum is the expanded form's checksum (profile.Checksum).
	Checksum string
	// Method is what the engine records in De-identification Method
	// (0012,0063): the version's name, or "unsaved" and the start of the
	// checksum for a document.
	Method string
	// Version is the stored version the document was exported from,
	// answered back; nil for a document.
	Version *v1.ProfileVersion
	// Reception is what the files' conditions read: a receiver's name and
	// AE titles, or nothing.
	Reception engine.Reception
	// Names are the files, in order, each answered under its name.
	Names []string
	// Read reads one file's bytes, at most MaxHeldBytes of them; a
	// file past the bound is refused by the caller.
	Read func(name string) ([]byte, error)
	// HeaderLimit and PixelLimit bound a file's header and an image's
	// pixel work, as receiver.header-limit and deid.pixel-limit do; zero
	// is their default.
	HeaderLimit, PixelLimit int64
}

// TryLocal runs a profile over files as TryProfile does for files sent
// with no receiver named, in this process: each file de-identified by the
// engine in memory, checked after by the separate read, and answered
// element by element with the verdict a receiver would reach on
// de-identification alone. It uses keys made for the call and thrown away,
// and reads no lookup table (every lookup answers UNASSIGNED), as every
// trial does; nothing is written anywhere and nothing is sent.
func TryLocal(ctx context.Context, t LocalTrial) (*v1.TryProfileResponse, error) {
	if t.Expanded == nil || t.Read == nil {
		return nil, fault.New(fault.Invalid, "a local trial needs a profile and its files")
	}
	limit := t.HeaderLimit
	if limit <= 0 {
		def, _ := settings.MustBuiltin().Lookup(settings.ReceiverHeaderLimit)
		var err error
		if limit, err = settings.ParseHeaderLimit(def.Default); err != nil {
			return nil, fault.Wrap(fault.Internal, err, "trial: the default header limit")
		}
	}
	unassigned := 0
	en, keys, err := NewEngine(t.Expanded, t.Method, &unassigned)
	if err != nil {
		return nil, err
	}
	out := &v1.TryProfileResponse{Version: t.Version, Checksum: t.Checksum}
	if err := engine.ForReception(t.Expanded); err != nil {
		out.ReceptionRefusal = ReceptionRefusal(err)
	}
	sum := NewSummary()
	// The answer is not sent as one message, so only each file's element
	// bound holds it.
	budget := math.MaxInt
	newCheck := func(maxFileBytes int64) receive.Checker { return check.New(check.Options{MaxFileBytes: maxFileBytes}) }
	for _, name := range t.Names {
		b, err := t.Read(name)
		if err != nil {
			return nil, err
		}
		unassigned = 0
		tr, err := receive.TryFile(ctx, en, b, t.Reception, receive.TrialOptions{Keys: keys, HeaderLimit: limit, PixelLimit: t.PixelLimit, Check: newCheck})
		if err != nil {
			return nil, SafeError(err)
		}
		r := Result(tr, &budget)
		r.Name, r.LookupsUnassigned = name, int32(unassigned) //nolint:gosec // G115: at most engine.MaxLookups per file.
		sum.Add(tr, r)
		out.Results = append(out.Results, r)
	}
	out.Summary = sum.Proto()
	return out, nil
}
