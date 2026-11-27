package correct

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// EditJudge judges each new revision an edit would write before it is
// written: the check after an edit. The server's runs the tenant's
// effective profile over the edited file (ProfileJudge).
type EditJudge interface {
	// JudgeEdit answers nil when the edited file f may be archived, and a
	// Precondition failure saying why when it may not, naming attributes
	// and causes, never a value. Any other error says nothing of the file.
	JudgeEdit(ctx context.Context, f EditedFile) error
}

// EditedFile is one instance's new revision, as an edit would write it.
type EditedFile struct {
	// Target is the instance as the catalog holds it.
	Target tenant.EditTarget
	// File is the new revision's bytes.
	File []byte
	// Set are the tags the edit gives a value; Removed those it removes.
	Set, Removed []uint32
}

// ProfileJudge judges an edited file by the tenant's effective profile, as
// a job that re-runs a profile over the archive judges an archived file
// (receive.TryFile): the file de-identified in memory and the check after
// run over what that writes. A file it would hold is refused, and so is
// one where the profile would change, remove or replace an attribute the
// edit sets: the edit would put back a value de-identification takes out.
// An instance whose effective profile is none (archived as received, by a
// receiver without one) is not judged.
type ProfileJudge struct {
	// Profile answers the version that judges an instance of no version of
	// its own (Target.Profile zero): the profile of the receiver it came
	// through, as one archived now would be; the zero ref for none. Nil
	// judges such an instance by none.
	Profile func(ctx context.Context, t tenant.EditTarget) (tenant.ProfileRef, error)
	// Engine compiles a version under keys the judge uses, nil when the
	// version cannot be run (which refuses the edit). Its lookups are
	// FixedLookups, never a tenant's tables: judging a file must not assign
	// a code to a value no archived file holds.
	Engine func(ctx context.Context, ref tenant.ProfileRef) (*engine.Engine, error)
	// Options are the trial's: keys, bounds and the check after
	// (receive.TrialOptions.Check, required). NoValues is set.
	Options receive.TrialOptions
}

// JudgeEdit implements EditJudge.
func (j ProfileJudge) JudgeEdit(ctx context.Context, f EditedFile) error {
	ref := f.Target.Profile
	if ref.IsZero() && j.Profile != nil {
		var err error
		if ref, err = j.Profile(ctx, f.Target); err != nil {
			return err
		}
	}
	if ref.IsZero() {
		return nil
	}
	if j.Engine == nil || j.Options.Check == nil {
		return fault.New(fault.Internal, "an edit's judge needs the profile's engine and the check after")
	}
	en, err := j.Engine(ctx, ref)
	if err != nil {
		return err
	}
	if en == nil {
		return fault.New(fault.Precondition, "the profile version "+ref.String()+" that judges the instance cannot be run")
	}
	o := j.Options
	o.NoValues = true
	t, err := receive.TryFile(ctx, en, f.File, engine.Reception{Receiver: f.Target.Receiver}, o)
	if err != nil {
		return err
	}
	if t.Hold != nil {
		return fault.New(fault.Precondition, "the check after holds the edited file ("+holdText(t.Hold)+"), as "+ref.String()+" would")
	}
	// Undecided is a lookup whose table the engine read no code from: the
	// profile ran part way, the check after not at all. A file never judged
	// is never archived.
	if t.Undecided || t.Record == nil {
		return fault.New(fault.Precondition, "the profile "+ref.String()+" could not judge the edited file: a lookup table it reads "+
			"decided nothing; an edit is never archived unjudged")
	}
	set, removed := map[string]uint32{}, map[string]uint32{}
	for _, s := range f.Set {
		set[fmt.Sprintf("%08X", s)] = s
	}
	for _, s := range f.Removed {
		removed[fmt.Sprintf("%08X", s)] = s
	}
	for _, e := range t.Record.Entries {
		if !e.Changed {
			continue
		}
		path := strings.ToUpper(e.Path)
		if s, ok := set[path]; ok {
			return fault.New(fault.Precondition, "the tenant's profile "+ref.String()+" takes "+tagText(s)+" out ("+e.Action+
				"): an edit does not put back what de-identification takes out")
		}
		// An attribute the profile writes (a marker it adds, a value it
		// replaces) is one de-identification put there: an edit does not
		// take it out.
		if s, ok := removed[path]; ok {
			return fault.New(fault.Precondition, "the tenant's profile "+ref.String()+" writes "+tagText(s)+" ("+e.Action+
				"): an edit does not remove what de-identification writes")
		}
	}
	return nil
}

// FixedCode is the code FixedLookups answers for every value. It has the
// form of a code (tenant.ValidLookupCode).
const FixedCode = "UNASSIGNED"

// FixedLookups answers an edit judge's lookups as if every value had one
// code, FixedCode, and assigns none. What a profile decides for an
// attribute (kept, replaced by its table's code, removed) does not depend
// on the code, and an archived value is a code already; a judge that read
// or wrote the tenant's tables would assign codes to values no archived
// file holds, from a check that writes nothing.
type FixedLookups struct{}

// Code reports every value as having FixedCode.
func (FixedLookups) Code(context.Context, string, string) (string, bool, error) {
	return FixedCode, true, nil
}

// Assign answers FixedCode for every key; nothing is assigned.
func (FixedLookups) Assign(_ context.Context, keys []engine.LookupKey) ([]string, error) {
	out := make([]string, len(keys))
	for i := range out {
		out[i] = FixedCode
	}
	return out, nil
}

// holdText writes a trial's hold as reason/detail=value pairs, in order;
// a hold's details name causes, paths and kinds, never a value.
func holdText(h *receive.TrialHold) string {
	parts := []string{string(h.Reason)}
	keys := make([]string, 0, len(h.Details))
	for k := range h.Details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+h.Details[k])
	}
	return strings.Join(parts, " ")
}
