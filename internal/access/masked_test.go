package access_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// maskedKey makes a masked key with grants directly in the master, as a key
// made with --masked is stored.
func (e *env) maskedKey(grants ...master.Grant) testKey {
	e.t.Helper()
	made, err := secret.MakeAPIKey()
	if err != nil {
		e.t.Fatal(err)
	}
	k := master.Key{ID: made.ID, Hash: made.Hash[:], Holder: "an agent", Created: t0.Add(-48 * time.Hour), Masked: true}
	if err := e.master.CreateKey(e.ctx, k, e.setupEntry(), master.LocalOperator()); err != nil {
		e.t.Fatal(err)
	}
	for _, g := range grants {
		g.KeyID = made.ID
		if _, err := e.master.SetGrant(e.ctx, g, e.setupEntry(), master.LocalOperator()); err != nil {
			e.t.Fatal(err)
		}
	}
	return testKey{id: made.ID, full: made.Key()}
}

// entryOf returns the one entry of the key among entries, failing unless
// there is exactly one.
func entryOf(t *testing.T, entries []master.AuditEntry, keyID string) master.AuditEntry {
	t.Helper()
	var out []master.AuditEntry
	for _, en := range entries {
		if en.KeyID == keyID {
			out = append(out, en)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d entries of key %s, want 1: %+v", len(out), keyID, entries)
	}
	return out[0]
}

// TestCheckRefusesOriginalsToAMaskedKey guards the check of a requirement
// that answers original values: a masked key whose grant suffices is
// refused as Denied with MessageMasked, recorded refused with the reason
// and the masked mark; the same requirement is allowed to a plain key with
// the same role and to the local operator; a masked key with no grant is
// told what any key with no grant is told; and a requirement that answers
// no original value is allowed to the masked key.
func TestCheckRefusesOriginalsToAMaskedKey(t *testing.T) {
	e := newEnv(t)
	masked := e.maskedKey(on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	plain := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	ungranted := e.maskedKey()
	need := access.OnTenant("lab", master.RoleOperator).AnswersOriginals()

	e.reset()
	_, err := e.access.Check(e.ctx, access.APIKey(masked.full), need, facts("Originals"))
	wantErr(t, "a masked key asking for originals", err, fault.Denied, access.MessageMasked)
	_, entries := e.recorded()
	en := entryOf(t, entries, masked.id)
	if en.Outcome != master.OutcomeRefused || !strings.Contains(en.Reason, "masked key") || en.Details[access.DetailMasked] != "true" {
		t.Errorf("the refusal's entry: outcome %q, reason %q, details %v", en.Outcome, en.Reason, en.Details)
	}

	if _, err := e.access.Check(e.ctx, access.APIKey(plain.full), need, facts("Originals")); err != nil {
		t.Errorf("a plain key with the role was refused: %v", err)
	}
	if _, err := e.access.Check(e.ctx, local, need, facts("Originals")); err != nil {
		t.Errorf("the local operator was refused: %v", err)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(ungranted.full), need, facts("Originals"))
	wantErr(t, "a masked key with no grant", err, fault.Denied, access.MessageDenied)

	d, err := e.access.Check(e.ctx, access.APIKey(masked.full), access.OnTenant("lab", master.RoleOperator), facts("Plain"))
	if err != nil {
		t.Fatalf("a masked key asking for no original value: %v", err)
	}
	if !d.Masked() || !d.Principal().Masked() {
		t.Error("the Decision of a masked key does not say so")
	}
}

// TestCheckRefusesADestinationToAMaskedKey guards the check of a
// requirement that chooses where archived data goes: a masked key whose
// grant suffices is refused as Denied with MessageMaskedDestination,
// recorded refused with the reason and the mark; a plain key with the same
// role and the local operator are allowed; a masked key with no grant is
// told what any key with no grant is told.
func TestCheckRefusesADestinationToAMaskedKey(t *testing.T) {
	e := newEnv(t)
	masked := e.maskedKey(on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	plain := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	ungranted := e.maskedKey()
	need := access.OnTenant("lab", master.RoleOperator).ChoosesDestination()

	e.reset()
	_, err := e.access.Check(e.ctx, access.APIKey(masked.full), need, facts("Send"))
	wantErr(t, "a masked key choosing a destination", err, fault.Denied, access.MessageMaskedDestination)
	_, entries := e.recorded()
	en := entryOf(t, entries, masked.id)
	if en.Outcome != master.OutcomeRefused || !strings.Contains(en.Reason, "where archived data goes") || en.Details[access.DetailMasked] != "true" {
		t.Errorf("the refusal's entry: outcome %q, reason %q, details %v", en.Outcome, en.Reason, en.Details)
	}
	if _, err := e.access.Check(e.ctx, access.APIKey(plain.full), need, facts("Send")); err != nil {
		t.Errorf("a plain key with the role was refused: %v", err)
	}
	if _, err := e.access.Check(e.ctx, local, need, facts("Send")); err != nil {
		t.Errorf("the local operator was refused: %v", err)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(ungranted.full), need, facts("Send"))
	wantErr(t, "a masked key with no grant", err, fault.Denied, access.MessageDenied)
}

// TestCheckRefusesAWriteOfValuesToAMaskedKey guards the check of a
// requirement that writes values into archived files: a masked key whose
// grant suffices is refused as Denied with MessageMaskedWrite, recorded
// refused with the reason and the mark; a plain key with the same role and
// the local operator are allowed; a masked key with no grant is told what
// any key with no grant is told.
func TestCheckRefusesAWriteOfValuesToAMaskedKey(t *testing.T) {
	e := newEnv(t)
	masked := e.maskedKey(on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	plain := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	ungranted := e.maskedKey()
	need := access.OnTenant("lab", master.RoleAdmin).WritesValues()

	e.reset()
	_, err := e.access.Check(e.ctx, access.APIKey(masked.full), need, facts("Edit"))
	wantErr(t, "a masked key writing values", err, fault.Denied, access.MessageMaskedWrite)
	_, entries := e.recorded()
	en := entryOf(t, entries, masked.id)
	if en.Outcome != master.OutcomeRefused || !strings.Contains(en.Reason, "write values into archived files") || en.Details[access.DetailMasked] != "true" {
		t.Errorf("the refusal's entry: outcome %q, reason %q, details %v", en.Outcome, en.Reason, en.Details)
	}
	if _, err := e.access.Check(e.ctx, access.APIKey(plain.full), need, facts("Edit")); err != nil {
		t.Errorf("a plain key with the role was refused: %v", err)
	}
	if _, err := e.access.Check(e.ctx, local, need, facts("Edit")); err != nil {
		t.Errorf("the local operator was refused: %v", err)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(ungranted.full), need, facts("Edit"))
	wantErr(t, "a masked key with no grant", err, fault.Denied, access.MessageDenied)
}

// TestRefuseOriginals guards the one check a handler makes of what only it
// can tell holds originals: nil for a plain key and the local operator,
// Denied with MessageMasked for a masked key, and refused for a Decision
// that did not come from Check.
func TestRefuseOriginals(t *testing.T) {
	e := newEnv(t)
	masked := e.maskedKey(on(master.TenantScope(e.lab.ID), master.RoleReader))
	plain := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleReader))
	need := access.OnTenant("lab", master.RoleReader)
	if err := e.decide(access.APIKey(plain.full), need, "Read").RefuseOriginals(); err != nil {
		t.Errorf("a plain key: %v", err)
	}
	if err := e.decide(local, need, "Read").RefuseOriginals(); err != nil {
		t.Errorf("the local operator: %v", err)
	}
	wantErr(t, "a masked key", e.decide(access.APIKey(masked.full), need, "Read").RefuseOriginals(), fault.Denied, access.MessageMasked)
	wantErr(t, "the zero Decision", access.Decision{}.RefuseOriginals(), fault.Denied, access.MessageMasked)
}

// TestEveryEntryOfAMaskedKeyIsMarked guards the mark on a masked key's
// entries: on the entry a handler makes even when its facts replace the
// check's details, on a refusal for a missing grant, on an unchecked
// request's entry, and never on a plain key's.
func TestEveryEntryOfAMaskedKeyIsMarked(t *testing.T) {
	e := newEnv(t)
	masked := e.maskedKey(on(master.TenantScope(e.lab.ID), master.RoleReader))
	plain := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleReader))
	need := access.OnTenant("lab", master.RoleReader)

	d := e.decide(access.APIKey(masked.full), need, "Read")
	if got := d.Entry(master.OutcomeDone, "", access.Facts{Details: map[string]string{"n": "1"}}).Details; got[access.DetailMasked] != "true" || got["n"] != "1" {
		t.Errorf("a handler's entry with its own details: %v", got)
	}
	if got := e.decide(access.APIKey(plain.full), need, "Read").Entry(master.OutcomeDone, "", access.Facts{}).Details; got[access.DetailMasked] != "" {
		t.Errorf("a plain key's entry is marked: %v", got)
	}

	e.reset()
	_, err := e.access.Check(e.ctx, access.APIKey(masked.full), access.OnTenant("clinic", master.RoleReader), facts("NoGrant"))
	wantErr(t, "no grant", err, fault.Denied, access.MessageDenied)
	_, entries := e.recorded()
	if en := entryOf(t, entries, masked.id); en.Details[access.DetailMasked] != "true" {
		t.Errorf("a refusal for no grant is not marked: %v", en.Details)
	}

	e.reset()
	cred, err := e.access.Authenticate(e.ctx, access.APIKey(masked.full), facts("Unchecked"))
	if err != nil {
		t.Fatal(err)
	}
	e.access.RecordUnchecked(cred, facts("Unchecked"), "no such path")
	_, entries = e.recorded()
	if en := entryOf(t, entries, masked.id); en.Details[access.DetailMasked] != "true" {
		t.Errorf("an unchecked request's entry is not marked: %v", en.Details)
	}
}

// TestMaskedKeysAreMadeAndKept guards making masked keys: the
// installation's admin makes one, stored masked, its entry saying so; a
// masked admin may make only masked keys, a plain one refused as Denied
// with MessageMasked and recorded; and a rotation, by the admin or by the
// key itself, carries it to the successor (the master's contract suite
// guards that a change of rules or a use leaves it).
func TestMaskedKeysAreMadeAndKept(t *testing.T) {
	e := newEnv(t)
	made, err := e.access.CreateMaskedKey(e.ctx, e.admin("CreateKey"), "review agent", access.ExpireAfter(24*time.Hour), master.KeyRules{})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := e.master.Key(e.ctx, made.Key.ID)
	if err != nil || !stored.Masked || !made.Key.Masked {
		t.Fatalf("a masked key as made %v and stored %v: %v", made.Key.Masked, stored.Masked, err)
	}
	_, entries := e.recorded()
	var found bool
	for _, en := range entries {
		if en.Object == made.Key.ID && en.Details["key_masked"] == "true" {
			found = true
		}
	}
	if !found {
		t.Errorf("no entry of the masked key's making says so: %+v", entries)
	}

	admin := e.maskedKey(on(master.Installation, master.RoleAdmin))
	d := e.decide(access.APIKey(admin.full), access.OnInstallation(master.RoleAdmin), "CreateKey")
	e.reset()
	_, err = e.access.CreateKey(e.ctx, d, "widened", access.ExpireAfter(time.Hour), master.KeyRules{})
	wantErr(t, "a masked admin making a plain key", err, fault.Denied, access.MessageMasked)
	_, entries = e.recorded()
	if en := entryOf(t, entries, admin.id); en.Outcome != master.OutcomeRefused || en.Details[access.DetailMasked] != "true" {
		t.Errorf("the refusal's entry: %+v", en)
	}
	byMasked, err := e.access.CreateMaskedKey(e.ctx, d, "helper", access.ExpireAfter(time.Hour), master.KeyRules{})
	if err != nil || !byMasked.Key.Masked {
		t.Fatalf("a masked admin making a masked key: %v %v", byMasked.Key.Masked, err)
	}

	next, _, err := e.access.RotateKey(e.ctx, e.admin("RotateKey"), made.Key.ID, time.Hour, 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := e.master.Key(context.Background(), next.Key.ID); !next.Key.Masked || !k.Masked {
		t.Errorf("the successor answered masked=%v, stored masked=%v", next.Key.Masked, k.Masked)
	}
	self := e.decide(access.APIKey(next.Secret.Key()), access.Authenticated(), "RotateKey")
	third, _, err := e.access.RotateKey(e.ctx, self, next.Key.ID, time.Hour, 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Key.Masked {
		t.Error("a masked key rotating itself made a plain successor")
	}
}

// TestMaskedAdminRotatesOnlyMaskedKeys guards rotation by a masked admin of
// the installation: a plain key's successor would be plain, with its
// secret answered to the masked caller, a plain key in the hands of the
// service a masked key was made for. So the rotation of a plain key is
// refused as Denied with MessageMasked, recorded refused with the mark, and
// leaves the plain key unrotated; a masked key's rotation by the same caller
// goes through.
func TestMaskedAdminRotatesOnlyMaskedKeys(t *testing.T) {
	e := newEnv(t)
	plain, err := e.access.CreateKey(e.ctx, e.admin("CreateKey"), "a person", access.ExpireAfter(24*time.Hour), master.KeyRules{})
	if err != nil {
		t.Fatal(err)
	}
	admin := e.maskedKey(on(master.Installation, master.RoleAdmin))
	d := e.decide(access.APIKey(admin.full), access.Authenticated(), "RotateKey")
	e.reset()
	next, _, err := e.access.RotateKey(e.ctx, d, plain.Key.ID, time.Hour, 365*24*time.Hour)
	wantErr(t, "a masked admin rotating a plain key", err, fault.Denied, access.MessageMasked)
	if next.Secret.Key() != "" {
		t.Error("a refused rotation answered a secret")
	}
	_, entries := e.recorded()
	en := entryOf(t, entries, admin.id)
	if en.Outcome != master.OutcomeRefused || !strings.Contains(en.Reason, "masked key") || en.Details[access.DetailMasked] != "true" || en.Object != plain.Key.ID {
		t.Errorf("the refusal's entry: %+v", en)
	}
	if k, err := e.master.Key(e.ctx, plain.Key.ID); err != nil || k.Successor != "" {
		t.Errorf("the plain key after a refused rotation: successor %q, %v", k.Successor, err)
	}

	other := e.maskedKey()
	rotated, _, err := e.access.RotateKey(e.ctx, d, other.id, time.Hour, 365*24*time.Hour)
	if err != nil || !rotated.Key.Masked {
		t.Fatalf("a masked admin rotating a masked key: masked %v, %v", rotated.Key.Masked, err)
	}
}
