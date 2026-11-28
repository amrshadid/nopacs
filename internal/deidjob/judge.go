package deidjob

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The findings of a check, the words of its failures' details and its
// tallies (job.proto, JobTally).
const (
	findingRemoved      = "removed_element"
	findingKeptValue    = "kept_value"
	findingPrivate      = "private_element"
	findingIdentity     = "identity_not_removed"
	findingMethod       = "method_missing"
	findingReceivedUID  = "received_uid"
	findingEngine       = "engine_refused"
	findingUnreadable   = "unreadable"
	detailTooLarge      = "not judged: larger than the 64 MiB a deid job reads"
	detailUndecidable   = "undecidable: a rule's condition reads what the archive no longer says"
	detailDamaged       = "its bytes on the storage are not what was archived"
	detailMissing       = "its file is missing from the storage"
	detailDeleted       = "not judged: a correction deleted it after the job was made"
	detailAsReceived    = "archived as received: it records no profile version"
	detailUndecided     = "depends on a lookup table, which a dry run never reads"
	detailUnusable      = "deid_failed/profile_unusable"
	unassignedCode      = "UNASSIGNED"
	maxTalliesPerFile   = tenant.MaxJobTallyBatch
	identityRemovedYES  = "YES"
	tallyPassed         = "passed"
	tallyRefused        = "refused"
	tallyClass          = "class"
	tallyCheck          = "check"
	tallyHold           = "hold"
	tallyRule           = "rule"
	tallyUndecided      = "undecided|lookup_table"
	tallyAsReceived     = "check|as_received"
	tallyUIDsNotChecked = "check|uids_past_bound"
	tallyUndecidable    = "undecidable"
)

// The tags a check reads (PS3.6).
var (
	tagIdentityRemoved = tag.New(0x0012, 0x0062)
	tagDeidMethod      = tag.New(0x0012, 0x0063)
)

// judge judges a unit's instances: it holds the engines it compiled, each
// under the unit's own keys, and for a check the tenant's UID key.
type judge struct {
	mode    tenant.DeidMode
	profile tenant.ProfileRef
	p       tenant.Pipeline
	// keys are the unit's keys, made for it and dropped with it; uidKey
	// is the tenant's UID key, read for a check alone.
	keys    engine.Keys
	uidKey  []byte
	engines map[tenant.ProfileRef]*engine.Engine
	// rules are the expanded rules of each version compiled, which a check
	// reads the conditions of.
	rules map[tenant.ProfileRef][]*v1.ExpandedRule
	// receptions are the facts of each reception read, by its id.
	receptions map[string]receptionFacts
	// unusable are the versions this build cannot run.
	unusable map[tenant.ProfileRef]bool
	// headerLimit and pixelLimit are the settings a reception reads within.
	headerLimit, pixelLimit int64
	// tried is the test seam of the same name; nil for none.
	tried func(t receive.Trial)
	// subject is the subject label of the unit's scan, from the catalog,
	// which the engine identifies each file's subject by
	// (engine.Reception.Subject): an archived file no longer holds the
	// Patient ID a date-shifting version keyed its shift by. In a dry run
	// the keys are the unit's own, so the shift it gives is throwaway; a
	// check compares no shifted value, only which elements remain. The
	// label is never written into a file or a verdict.
	subject string
	// memory is Config.Memory.
	memory check.Memory
	// expanded are the expanded profiles of the versions compiled, which a
	// re-run's layer rule compares; rr is a re-run's own state, nil for any
	// other mode.
	expanded expandedOf
	rr       *rerunState
	// sums are the checksums of the versions compiled, and base the tenant's
	// base profile read for the unit: what a re-run's new lines record.
	sums map[tenant.ProfileRef]string
	base tenant.ProfileRef
}

// throwawayKeys makes three random keys of the unit's own, never a
// tenant's, so nothing keyed a dry run computes can be matched with what
// the archive holds.
func throwawayKeys() (engine.Keys, error) {
	var k engine.Keys
	for _, b := range []*[]byte{&k.UID, &k.Date, &k.Hash} {
		*b = make([]byte, engine.MinKeyLength)
		if _, err := rand.Read(*b); err != nil {
			return engine.Keys{}, fault.New(fault.Internal, "deidjob: no random source for a unit's keys")
		}
	}
	return k, nil
}

// dryLookups answers a dry run's lookups without reading a tenant's
// tables, as a trial run does: no value has a code, and a value a receiver
// would assign one is answered unassignedCode. A dry run that read the
// tables would tell which values have codes.
type dryLookups struct{}

// Code reports every value as having no code.
func (dryLookups) Code(context.Context, string, string) (string, bool, error) { return "", false, nil }

// Assign answers unassignedCode for every key; nothing is assigned.
func (dryLookups) Assign(_ context.Context, keys []engine.LookupKey) ([]string, error) {
	out := make([]string, len(keys))
	for i := range out {
		out[i] = unassignedCode
	}
	return out, nil
}

// checkLookups answers a check's lookups as if every value had a code: a
// check judges what the version decides for each element, which does not
// depend on the code, and an archived value is already a code.
type checkLookups struct{}

// Code reports every value as having unassignedCode.
func (checkLookups) Code(context.Context, string, string) (string, bool, error) {
	return unassignedCode, true, nil
}

// Assign answers unassignedCode for every key.
func (checkLookups) Assign(ctx context.Context, keys []engine.LookupKey) ([]string, error) {
	return dryLookups{}.Assign(ctx, keys)
}

// engineFor returns the engine of a version, compiled once per unit under
// the unit's keys; nil for a version this build cannot run. An error is a
// database that does not answer.
func (j *judge) engineFor(ctx context.Context, ref tenant.ProfileRef) (*engine.Engine, error) {
	if en := j.engines[ref]; en != nil || j.unusable[ref] {
		return en, nil
	}
	store, ok := j.p.(tenant.Deidentification)
	if !ok {
		return nil, fault.New(fault.Internal, "deidjob: the tenant's database keeps no profiles")
	}
	v, err := store.ProfileVersion(ctx, ref.Name, ref.Version)
	if err != nil && fault.KindOf(err) != fault.NotFound {
		return nil, err
	}
	var en *engine.Engine
	if err == nil {
		var lookups engine.Lookups = dryLookups{}
		if j.mode == tenant.DeidCheck {
			lookups = checkLookups{}
		}
		// Loaded through the profile language, which checks the expanded
		// text against its checksum, as a reception loads it.
		if version, lerr := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum); lerr == nil {
			expanded := version.Expanded()
			// The layer rule of a re-run compares the versions themselves: a
			// newer version at least as strict as the older, each already
			// stacked over the base by the checks above and at job creation.
			if j.expanded != nil {
				j.expanded[ref] = expanded
			}
			// A file is judged by what a reception runs: the version
			// stacked over the tenant's base, so a file never cleaned to
			// the base is found. A base that cannot be stacked leaves the
			// version unusable.
			base, berr := receive.BaseOf(ctx, j.p)
			if berr != nil {
				return nil, berr
			}
			j.base = base
			if j.sums != nil {
				j.sums[ref] = v.Checksum
			}
			if !base.IsZero() {
				stacked, usable, serr := receive.StackedProfile(ctx, store, base, ref, expanded)
				if serr != nil {
					return nil, serr
				}
				if !usable {
					expanded = nil
				} else {
					expanded = stacked
				}
			}
			if expanded != nil {
				en, _ = engine.New(expanded, j.keys, engine.Options{Lookups: lookups, Method: ref.String()})
				j.rules[ref] = expanded.GetRules()
			}
		}
	}
	if en == nil {
		j.unusable[ref] = true
		return nil, nil
	}
	j.engines[ref] = en
	return en, nil
}

// judgeFile judges one archived file. An error is a cause that is not the
// file's (a canceled unit, a database that does not answer): the unit is
// tried again later.
func (j *judge) judgeFile(ctx context.Context, in tenant.ScanInstance, file []byte) (verdict, error) {
	v := verdict{tallies: map[string]int64{}}
	h, herr := header.ReadWithin(bytes.NewReader(file), int64(len(file)), j.headerLimit)
	if herr == nil {
		if c, ok := h.UID(header.SOPClassUID); ok && validUID(c) {
			v.sopClass = c
		}
		if validUID(h.TransferSyntaxUID) {
			v.syntax = h.TransferSyntaxUID
		}
	}
	facts, err := j.receptionOf(ctx, in.ReceptionID)
	if err != nil {
		return verdict{}, err
	}
	facts.rec.Subject = j.subject
	switch {
	case j.mode == tenant.DeidCheck:
		err = j.check(ctx, in, file, h, herr, facts, &v)
	case j.mode.Rerun():
		err = j.rerun(ctx, in, file, facts, &v)
	default:
		err = j.dryRun(ctx, in, file, facts, &v)
	}
	if err != nil {
		return verdict{}, err
	}
	class := v.sopClass
	if class == "" {
		class = "unknown"
	}
	outcome := tallyPassed
	if v.kind == resultRefused {
		outcome = tallyRefused
	}
	v.tallies[tallyKey(tallyClass, class, outcome)]++
	capTallies(v.tallies, maxTalliesPerFile)
	return v, nil
}

// capTallies folds the keys of m past limit into tenant.TallyOther, the
// smallest counts first, so one file's verdict always fits in one progress
// write (tenant.MaxJobTallyBatch keys), whatever the profile's rules.
func capTallies(m map[string]int64, limit int) {
	if len(m) <= limit {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != tenant.TallyOther {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return cmp.Or(cmp.Compare(m[b], m[a]), strings.Compare(a, b)) })
	for _, k := range keys[limit-1:] {
		m[tenant.TallyOther] += m[k]
		delete(m, k)
	}
}

// dryRun runs the job's version, or the instance's own, over the file in
// memory as a trial run does (receive.TryFile), with no receiver's checks,
// its conditions reading the facts of the instance's reception. It builds
// no copy of a value (TrialOptions.NoValues): a job answers none.
func (j *judge) dryRun(ctx context.Context, in tenant.ScanInstance, file []byte, facts receptionFacts, v *verdict) error {
	ref := j.profile
	if ref.IsZero() {
		ref = in.Profile
	}
	if ref.IsZero() {
		v.kind, v.code, v.detail = resultNoted, tenant.FailureNotDeidentified, detailAsReceived
		v.tallies[tallyAsReceived]++
		return nil
	}
	en, err := j.engineFor(ctx, ref)
	if err != nil {
		return err
	}
	if en == nil {
		v.kind, v.code, v.detail = resultRefused, tenant.FailureWouldHold, detailUnusable
		v.tallies[tallyKey(tallyHold, "deid_failed", "profile_unusable")]++
		return nil
	}
	newCheck := func(maxFileBytes int64) receive.Checker { return check.New(check.Options{MaxFileBytes: maxFileBytes}) }
	t, err := receive.TryFile(ctx, en, file, facts.rec, receive.TrialOptions{Keys: j.keys, HeaderLimit: j.headerLimit,
		PixelLimit: j.pixelLimit, Check: newCheck, NoValues: true, Memory: j.memory})
	if err != nil {
		return fault.New(fault.KindOf(err), fault.Message(err))
	}
	if j.tried != nil {
		j.tried(t)
	}
	if t.Record != nil {
		for _, e := range t.Record.Entries {
			v.tallies[tallyKey(tallyRule, ruleOf(e), e.Action)]++
		}
	}
	switch {
	case t.Hold != nil:
		cause := holdCause(t.Hold)
		v.kind, v.code, v.detail = resultRefused, tenant.FailureWouldHold, string(t.Hold.Reason)+"/"+cause
		v.tallies[tallyKey(tallyHold, string(t.Hold.Reason), cause)]++
	case t.Undecided:
		v.kind, v.code, v.detail = resultNoted, tenant.FailureUndecided, detailUndecided
		v.tallies[tallyUndecided]++
	default:
		v.kind = resultPassed
	}
	return nil
}

// ruleOf names the rule that decided an element as a tally does: its name,
// its position from 1, or "default".
func ruleOf(e engine.Entry) string {
	switch {
	case e.Rule < 0:
		return "default"
	case e.RuleName != "":
		return e.RuleName
	}
	return "rule " + strconv.Itoa(e.Rule+1)
}

// holdCause is the fixed cause of a hold: the record's cause, or for a
// value found the kind of value, or "rule" for a rule's refusal.
func holdCause(h *receive.TrialHold) string {
	if c := h.Details["cause"]; c != "" {
		return c
	}
	if k := h.Details["kind"]; k != "" {
		return k
	}
	return "rule"
}

// textOf returns the text of a top-level element, its padding removed.
func textOf(ds *dataset.Dataset, t tag.Tag) (string, bool) {
	elem, ok := ds.Get(t)
	if !ok {
		return "", false
	}
	b, ok := elem.Bytes()
	if !ok {
		return "", true
	}
	return strings.TrimRight(string(b), "\x00 "), true
}

// tallyKey joins parts with "|" into a key a store keeps: every byte not
// printable ASCII, and "|" inside a part, written "?", and the key cut to
// tenant.MaxTallyKeyLength. The parts are the server's words and a
// profile's names, never a file's value; the cleaning keeps a profile's
// name in another script from making a key no store takes.
func tallyKey(parts ...string) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('|')
		}
		for j := range len(p) {
			c := p[j]
			if c < 0x20 || c > 0x7e || c == '|' {
				c = '?'
			}
			b.WriteByte(c)
		}
	}
	s := b.String()
	if len(s) > tenant.MaxTallyKeyLength {
		s = s[:tenant.MaxTallyKeyLength]
	}
	return s
}

// validUID reports whether u is a UID a failure of a job may record
// (PS3.5 9.1), by the store's own rule.
func validUID(u string) bool {
	return u != "" && tenant.ValidateSeenUIDs(nil, []string{u}) == nil
}
