package holds

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestADeadReplicaHoldsBackNoOtherStorage guards the records owed past a
// storage that does not answer: a thousand holds whose records the replica
// is owed sort ahead of one whose record the primary is owed; with the
// replica down, the first pass still writes the primary's record, and is
// the replica's error.
func TestADeadReplicaHoldsBackNoOtherStorage(t *testing.T) {
	r := newRig(t)
	target := tenant.HoldTarget{Subject: r.subjects[0]}
	early := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	found := func(at time.Time, storage string) tenant.HoldFound {
		id, err := tenant.NewCorrectionID(at)
		if err != nil {
			t.Fatal(err)
		}
		return tenant.HoldFound{ID: id, Target: target, Reason: "kept by the record", Placed: early, Seq: 1, SealVersion: r.keys.SealVersion(),
			Storage: storage, Now: time.Now()}
	}
	for i := range tenant.MaxHoldList {
		// Found on the primary: the replica is owed it.
		if _, _, err := r.db.AdoptHold(r.ctx, found(early.Add(time.Duration(i)*time.Millisecond), testkit.GenStorage), r.audit()); err != nil {
			t.Fatal(err)
		}
	}
	last := found(early.Add(time.Hour), replica)
	if _, _, err := r.db.AdoptHold(r.ctx, last, r.audit()); err != nil {
		t.Fatal(err)
	}
	r.stores[replica].fail.Store(true)
	n, err := r.writer().WriteOwed(r.ctx)
	if n != 1 || fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("the pass with the replica down wrote %d, %v, want the primary's one record and the replica's error", n, err)
	}
	if rec := r.record(testkit.GenStorage, last.ID); !rec.Active() || rec.Seq != 1 {
		t.Fatalf("the primary's record is %+v", rec)
	}
	if h, err := r.db.Hold(r.ctx, last.ID); err != nil || len(h.StoragesPending) != 0 {
		t.Fatalf("the hold owed on the primary is %+v, %v, want nothing owed", h, err)
	}
}

// versionedKeys is a keyring over fixed keys by version, sealing under
// seal: an installation key file as versions are added to it and retired.
type versionedKeys struct {
	seal uint32
	keys map[uint32][]byte
}

// SealVersion implements record.Keyring.
func (k versionedKeys) SealVersion() uint32 { return k.seal }

// mac is HMAC-SHA-256 under the version's key with use appended, so each
// kind of record has a key of its own.
func (k versionedKeys) mac(use byte, version uint32, message []byte) ([]byte, error) {
	key, ok := k.keys[version]
	if !ok {
		return nil, fmt.Errorf("no key version %d", version)
	}
	m := hmac.New(sha256.New, append(bytes.Clone(key), use))
	m.Write(message)
	return m.Sum(nil), nil
}

// RecordMAC implements record.Keyring.
func (k versionedKeys) RecordMAC(version uint32, message []byte) ([]byte, error) {
	return k.mac('c', version, message)
}

// HoldMAC implements record.Keyring.
func (k versionedKeys) HoldMAC(version uint32, message []byte) ([]byte, error) {
	return k.mac('h', version, message)
}

// readWith reads and checks the hold's record on the storage named under
// keys.
func (r *rig) readWith(keys record.Keyring, storage, id string) (record.Hold, error) {
	key, err := layout.HoldKey(id)
	if err != nil {
		return record.Hold{}, err
	}
	rc, err := r.stores[storage].Read(r.ctx, key, 0, -1)
	if err != nil {
		return record.Hold{}, err
	}
	defer func() { _ = rc.Close() }()
	return record.DecodeHold(rc, rigTenantID, keys)
}

// TestRecordsAreResealedUnderANewKeyVersion guards a key version added to
// the installation's key file: the first pass of a writer sealing under
// the new version writes every record of an active hold again under it, so
// once the old version is retired the records still verify, and a
// database lost then still finds the hold; a second pass writes nothing.
func TestRecordsAreResealedUnderANewKeyVersion(t *testing.T) {
	r := newRig(t)
	k1, k2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	writerWith := func(keys record.Keyring, st Store) *Writer {
		cfg := r.config(st)
		cfg.Keys = keys
		w, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	h := r.place(writerWith(versionedKeys{seal: 1, keys: map[uint32][]byte{1: k1}}, r.db), r.subjects[0])
	both := versionedKeys{seal: 2, keys: map[uint32][]byte{1: k1, 2: k2}}
	only2 := versionedKeys{seal: 2, keys: map[uint32][]byte{2: k2}}
	if n, err := writerWith(both, r.db).WriteOwed(r.ctx); err != nil || n != 2 {
		t.Fatalf("the first pass under the new version wrote %d, %v, want the primary's and the replica's", n, err)
	}
	for _, storage := range []string{testkit.GenStorage, replica} {
		rec, err := r.readWith(only2, storage, h.ID)
		if err != nil || rec.KeyVersion != 2 || !rec.Active() || rec.Seq != 1 {
			t.Fatalf("the record on %s under the new version alone is %+v, %v", storage, rec, err)
		}
	}
	if n, err := writerWith(both, r.db).WriteOwed(r.ctx); err != nil || n != 0 {
		t.Fatalf("a second pass wrote %d, %v", n, err)
	}
	fresh := openTenant(t, 1)
	a, err := writerWith(only2, fresh).AdoptFound(context.Background())
	if err != nil || !slices.Equal(a.Adopted, []string{h.ID}) || len(a.Refused) != 0 {
		t.Fatalf("after the old version was retired and the database lost, AdoptFound = %+v, %v", a, err)
	}
}
