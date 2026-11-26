package holds

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxListPages bounds the pages of one listing of a storage's hold
// records, so a storage that never ends a listing cannot hold a reader for
// ever.
const maxListPages = 1 << 20

// foundMarker comes before the reason's SHA-256 in the reason of a hold
// adopted from its record (FoundReason): the record keeps only the digest,
// and its rewrites keep the same digest (digestOf).
const foundMarker = "; its reason's SHA-256 is "

// Reader is what reading a storage's hold records needs: blob.Store's Read
// and List. A rebuild holds its storages as no more than this.
type Reader interface {
	// Read returns length bytes of the object at key from offset, -1 for
	// the rest of it.
	Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	// List returns a page of keys.
	List(ctx context.Context, opts blob.ListOptions) (blob.Page, error)
}

// Found is what one storage's hold records say.
type Found struct {
	// Storage is the storage's name.
	Storage string
	// Records are the records that verify for the tenant, in the order of
	// their keys.
	Records []record.Hold
	// Refused are the keys under meta/.holds/ that make no hold: a record
	// whose MAC is missing or does not verify for the tenant, one not in
	// the record's form or named by another id's key, or a key that is not
	// a record's.
	Refused []string
}

// ReadRecords reads every hold record of the storage named storage, each
// checked for the tenant whose id is tenantID under keys. A record that
// cannot be listed or read is an error; one refused is counted in
// Found.Refused, never adopted.
func ReadRecords(ctx context.Context, st Reader, storage, tenantID string, keys record.Keyring) (Found, error) {
	out := Found{Storage: storage}
	token := ""
	for range maxListPages {
		page, err := st.List(ctx, blob.ListOptions{Prefix: layout.HoldsPrefix, Token: token})
		if err != nil {
			return out, err
		}
		for _, key := range page.Keys {
			id, err := layout.ParseHoldKey(key)
			if err != nil {
				out.Refused = append(out.Refused, key)
				continue
			}
			h, err := readRecord(ctx, st, key, tenantID, keys)
			switch {
			case errors.Is(err, record.ErrUnauthenticated), errors.Is(err, record.ErrFormat), err == nil && h.ID != id:
				out.Refused = append(out.Refused, key)
			case err != nil:
				return out, err
			default:
				out.Records = append(out.Records, h)
			}
		}
		if token = page.Next; token == "" {
			return out, nil
		}
	}
	return out, fault.New(fault.Unavailable, "the listing of the hold records of "+storage+" did not end")
}

// readRecord reads and checks one hold record.
func readRecord(ctx context.Context, st Reader, key, tenantID string, keys record.Keyring) (record.Hold, error) {
	rc, err := st.Read(ctx, key, 0, -1)
	if err != nil {
		return record.Hold{}, err
	}
	defer func() { _ = rc.Close() }()
	return record.DecodeHold(rc, tenantID, keys)
}

// Adopter is the part of a tenant's database adoption writes:
// tenant.Holds' AdoptHold.
type Adopter interface {
	// AdoptHold makes an active hold of a record found with no row.
	AdoptHold(ctx context.Context, f tenant.HoldFound, audit master.AuditEntry) (tenant.Hold, bool, error)
}

// Adoption is what Adopt did.
type Adoption struct {
	// Adopted are the ids of the holds made from their records.
	Adopted []string
	// Refused are the records that made no hold, each as
	// "<storage>: <key>".
	Refused []string
}

// Adopt makes an active hold of each hold whose latest record, over every
// storage read, is active and that has no row: of two records of a hold the
// one of the higher count is the later, and of two of the same count an
// active one is taken, since keeping data longer is the lesser error. A
// hold that has a row is left as its row says. audit gives the entry of
// each hold adopted, by its id and the storage its record was found on.
func Adopt(ctx context.Context, db Adopter, found []Found, now time.Time, audit func(id, storage string) master.AuditEntry) (Adoption, error) {
	var out Adoption
	type latest struct {
		rec     record.Hold
		storage string
	}
	best := map[string]latest{}
	var order []string
	for _, f := range found {
		for _, k := range f.Refused {
			out.Refused = append(out.Refused, f.Storage+": "+k)
		}
		for _, r := range f.Records {
			cur, ok := best[r.ID]
			if !ok {
				order = append(order, r.ID)
			}
			if !ok || r.Seq > cur.rec.Seq || (r.Seq == cur.rec.Seq && r.Active() && !cur.rec.Active()) {
				best[r.ID] = latest{rec: r, storage: f.Storage}
			}
		}
	}
	for _, id := range order {
		b := best[id]
		if !b.rec.Active() {
			continue
		}
		_, adopted, err := db.AdoptHold(ctx, foundOf(b.rec, b.storage, now), audit(id, b.storage))
		if err != nil {
			return out, err
		}
		if adopted {
			out.Adopted = append(out.Adopted, id)
		}
	}
	return out, nil
}

// foundOf returns what a record found on the storage says, as AdoptHold
// takes it.
func foundOf(r record.Hold, storage string, now time.Time) tenant.HoldFound {
	return tenant.HoldFound{ID: r.ID, Target: tenant.HoldTarget{Subject: r.Subject, Session: r.Session, Scan: r.Scan, WholeTenant: r.Level == record.LevelTenant},
		Reason: FoundReason(storage, r.ReasonSHA256), PlacedBy: r.PlacedBy, Placed: r.Placed, Seq: r.Seq, SealVersion: r.KeyVersion,
		Storage: storage, Now: now}
}

// AdoptMethod is the method of the audit entry of a hold adopted from its
// record.
const AdoptMethod = "hold.adopt"

// AdoptEntry returns the audit entry of the hold of that id adopted from
// its record found on the storage named, in the tenant named, by the local
// user named (the server's own work, or a restore's), at now.
func AdoptEntry(tenantName, localUser, id, storage string, now time.Time) master.AuditEntry {
	return master.AuditEntry{ID: server.NewID(), Time: now, LocalUser: localUser, Method: AdoptMethod, TenantName: tenantName, Object: id,
		Outcome: master.OutcomeDone, Reason: "a hold record found active on the storage " + storage + " with no row in the tenant's database"}
}

// FoundReason returns the reason of a hold adopted from a record found on
// the storage named: the record keeps only the reason's digest, never the
// person's words, which were in the database lost.
func FoundReason(storage, digest string) string {
	return "found on the storage " + storage + " as a hold record with no row in the tenant's database" + foundMarker + digest
}

// digestOf returns the reason's SHA-256 a hold's record carries: that of
// its reason, or, for a hold adopted from a record, the digest its record
// carried, so its rewrites name the person's reason still.
func digestOf(h tenant.Hold) string {
	if h.FoundOnStorage {
		if i := strings.LastIndex(h.Reason, foundMarker); i >= 0 {
			if d := h.Reason[i+len(foundMarker):]; len(d) == 64 && strings.Trim(d, "0123456789abcdef") == "" {
				return d
			}
		}
	}
	return record.ReasonDigest(h.Reason)
}

// recordOf returns the record of the hold as it stands.
func recordOf(h tenant.Hold) record.Hold {
	r := record.Hold{Format: record.Format, Kind: record.HoldKind, ID: h.ID, Level: h.Target.Level(), Subject: h.Target.Subject,
		Session: h.Target.Session, Scan: h.Target.Scan, State: record.HoldActive, ReasonSHA256: digestOf(h), PlacedBy: h.PlacedBy,
		Placed: h.Placed, Seq: h.Seq}
	if h.State == tenant.HoldReleased {
		r.State, r.Released = record.HoldReleased, h.Released
	}
	return r
}
