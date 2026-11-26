package api_test

import (
	"context"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// holdWriter is the env's Config.Holds: the hold writer of the tenant
// named over its database as the registry answers it, its storages opened
// by their rows as the server opens them, beside any other writer.
func (e *env) holdWriter(ctx context.Context, name string) (*holds.Writer, error) {
	st, err := e.registry.Store(name)
	if err != nil {
		return nil, err
	}
	hs, ok := st.(holds.Store)
	if !ok {
		return nil, fault.New(fault.Internal, "the tenant's database keeps no holds")
	}
	row, err := e.master.TenantByName(ctx, name)
	if err != nil {
		return nil, err
	}
	opener := place.Opener{Values: settingvalue.Reader{Master: e.faulty}, Keys: e.keys, StaleUploadAge: -1}
	return holds.New(holds.Config{Tenant: name, TenantID: row.ID, Keys: e.keys, Store: hs, Now: e.now,
		Open: func(ctx context.Context, storage string) (blob.Store, error) {
			s, err := hs.Storage(ctx, storage)
			if err != nil {
				return nil, err
			}
			return opener.OpenBeside(ctx, place.TenantOf(row.ID, name, st), s)
		}})
}

// placeCall is a call of PlaceHold on lab1.
func placeCall(target *v1.HoldTarget, reason string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.retention.PlaceHold, &v1.PlaceHoldRequest{Tenant: "lab1", Target: target, Reason: reason})
	}
}

// releaseCall is a call of ReleaseHold on lab1.
func releaseCall(id, reason string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.retention.ReleaseHold, &v1.ReleaseHoldRequest{Tenant: "lab1", Id: id, Reason: reason})
	}
}

// holdRecordOf reads and checks the record of the hold id on lab1's
// storage.
func holdRecordOf(t *testing.T, c *correctionArchive, id string) record.Hold {
	t.Helper()
	key, err := layout.HoldKey(id)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.a.store.Read(ctx, key, 0, -1)
	if err != nil {
		t.Fatalf("the record of hold %s: %v", id, err)
	}
	defer func() { _ = rc.Close() }()
	row, err := c.master.TenantByName(ctx, "lab1")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.DecodeHold(rc, row.ID, c.env.keys)
	if err != nil {
		t.Fatalf("the record of hold %s does not verify: %v", id, err)
	}
	return rec
}

// testPlaceHold guards PlaceHold: admin on the tenant, so a reader and a
// tenant's writer are refused and a tenant's admin places on its own
// tenant; the target is checked before the reason; a subject the tenant
// does not hold is not found; a hold placed has its record on the primary
// (read back) and its row, and its entry names the target and the reason;
// the same target and reason answer the same hold.
func testPlaceHold(t *testing.T) {
	c := newCorrectionArchive(t)
	writer, _ := c.key(c.onTenant("lab1", master.RoleWriter))
	subject := &v1.HoldTarget{SubjectLabel: c.subject}
	var placed *v1.Hold
	c.run(t, []tcase{
		{name: "reader", as: c.lab1Reader, call: placeCall(subject, "litigation"), code: connect.CodePermissionDenied},
		{name: "writer", as: writer, call: placeCall(subject, "litigation"), code: connect.CodePermissionDenied},
		{name: "no target", as: c.lab1Admin, call: placeCall(nil, "litigation"), code: connect.CodeInvalidArgument, msg: "target"},
		{name: "a label with the whole tenant", as: c.lab1Admin, call: placeCall(&v1.HoldTarget{SubjectLabel: c.subject, WholeTenant: true}, "x"),
			code: connect.CodeInvalidArgument, msg: "target"},
		{name: "a scan with no session, and no reason", as: c.lab1Admin, call: placeCall(&v1.HoldTarget{SubjectLabel: c.subject, ScanLabel: "s"}, ""),
			code: connect.CodeInvalidArgument, msg: "target"},
		{name: "no reason", as: c.lab1Admin, call: placeCall(subject, ""), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "a subject the tenant does not hold", as: c.lab1Admin, call: placeCall(&v1.HoldTarget{SubjectLabel: "sub-none"}, "litigation"),
			code: connect.CodeNotFound, audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "placed by the tenant's admin", as: c.lab1Admin, call: placeCall(subject, "litigation"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"target": c.subject, "reason": "litigation"}},
			check: func(t *testing.T, o outcome) {
				placed = o.res.(*v1.PlaceHoldResponse).GetHold()
				if placed.GetState() != v1.HoldState_HOLD_STATE_ACTIVE || placed.GetTarget().GetSubjectLabel() != c.subject ||
					placed.GetReason() != "litigation" || len(placed.GetStorages()) != 1 || len(placed.GetStoragesPending()) != 0 {
					t.Errorf("hold %v", placed)
				}
			}},
		{name: "the same again", as: c.lab1Admin, call: placeCall(subject, "litigation"), check: func(t *testing.T, o outcome) {
			if h := o.res.(*v1.PlaceHoldResponse).GetHold(); h.GetId() != placed.GetId() {
				t.Errorf("the same hold placed again answers %s, want %s", h.GetId(), placed.GetId())
			}
		}},
	})
	rec := holdRecordOf(t, c, placed.GetId())
	if rec.State != record.HoldActive || rec.Subject != c.subject || rec.ReasonSHA256 != record.ReasonDigest("litigation") {
		t.Errorf("the hold's record on the primary is %+v", rec)
	}
	st, err := c.registry.Store("lab1")
	if err != nil {
		t.Fatal(err)
	}
	if h, err := st.(tenant.Holds).Hold(ctx, placed.GetId()); err != nil || h.State != tenant.HoldActive || h.Target.Subject != c.subject {
		t.Errorf("the hold's row is %+v (%v)", h, err)
	}
}

// testReleaseHold guards ReleaseHold: admin on the tenant, so a reader and
// a tenant's writer are refused; an id missing or of no hold's form, or
// one the tenant does not have, is not found; a reason is required; a
// release rewrites the record on the primary released (read back) and the
// row; a hold released already is answered as it stands.
func testReleaseHold(t *testing.T) {
	c := newCorrectionArchive(t)
	writer, _ := c.key(c.onTenant("lab1", master.RoleWriter))
	h := mustCall[*v1.PlaceHoldResponse](t, "place", placeCall(&v1.HoldTarget{SubjectLabel: c.subject}, "litigation")(c.as(c.lab1Admin))).GetHold()
	c.run(t, []tcase{
		{name: "reader", as: c.lab1Reader, call: releaseCall(h.GetId(), "settled"), code: connect.CodePermissionDenied},
		{name: "writer", as: writer, call: releaseCall(h.GetId(), "settled"), code: connect.CodePermissionDenied},
		{name: "no id", as: c.lab1Admin, call: releaseCall("", "settled"), code: connect.CodeNotFound, msg: "no hold"},
		{name: "an id of no hold's form", as: c.lab1Admin, call: releaseCall("not-a-hold", "settled"), code: connect.CodeNotFound, msg: "no hold"},
		{name: "no reason", as: c.lab1Admin, call: releaseCall(h.GetId(), ""), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "no such hold", as: c.lab1Admin, call: releaseCall("01jh0000000000000000000000", "settled"), code: connect.CodeNotFound, msg: "no hold",
			audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "released", as: c.lab1Admin, call: releaseCall(h.GetId(), "settled"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: h.GetId(), details: map[string]string{"reason": "settled"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ReleaseHoldResponse).GetHold()
				if r.GetState() != v1.HoldState_HOLD_STATE_RELEASED || r.GetReleaseReason() != "settled" || r.GetReleaseTime() == nil {
					t.Errorf("hold %v", r)
				}
			}},
		{name: "released again", as: c.lab1Admin, call: releaseCall(h.GetId(), "again"), check: func(t *testing.T, o outcome) {
			if r := o.res.(*v1.ReleaseHoldResponse).GetHold(); r.GetReleaseReason() != "settled" {
				t.Errorf("a hold released again answers %v", r)
			}
		}},
	})
	if rec := holdRecordOf(t, c, h.GetId()); rec.State != record.HoldReleased {
		t.Errorf("the hold's record on the primary after the release is %+v", rec)
	}
	st, err := c.registry.Store("lab1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.(tenant.Holds).Hold(ctx, h.GetId()); err != nil || got.State != tenant.HoldReleased {
		t.Errorf("the hold's row after the release is %+v (%v)", got, err)
	}
}
