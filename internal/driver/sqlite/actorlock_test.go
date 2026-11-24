package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestGrantRemovalWhileChangeWaitsForTheWriter guards the in-transaction
// check of a change's key on SQLite against a grant removed while the change
// waits to write. A third connection holds the database's write lock; a
// removal of the key's grant, from one store, and a change by the key, from
// another store on the same file, both wait for it; then it is let go. SQLite
// lets one writer in at a time, and its write transaction (BEGIN IMMEDIATE)
// takes the lock before the check reads anything, so the check sees every
// commit made before it got the lock and none after. Whichever of the two
// gets the lock first, the outcome is consistent: either the change was made
// and committed before the removal (its audit entry was inserted first), or
// it was refused because the grant was already gone.
func TestGrantRemovalWhileChangeWaitsForTheWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "master.db")
	stores := make([]*Master, 2)
	for i := range stores {
		m, err := OpenMaster(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		if err := m.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		stores[i] = m
	}
	changer, remover := stores[0], stores[1]
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	holder.SetMaxOpenConns(1)

	t0 := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	clock := func() time.Time { return t0 }
	n := 0
	audit := func() master.AuditEntry {
		n++
		return master.AuditEntry{ID: fmt.Sprintf("a%04d", n), Time: t0, LocalUser: "501", Method: "Test", Outcome: master.OutcomeDone}
	}
	rowid := func(id string) int64 {
		var r int64
		if err := holder.QueryRowContext(ctx, `SELECT rowid FROM audit WHERE id = ?`, id).Scan(&r); err != nil {
			return -1
		}
		return r
	}
	made, refused := 0, 0
	for round := range 12 {
		k := master.Key{ID: fmt.Sprintf("k%04d", round), Hash: make([]byte, master.KeyHashLength), Holder: "h", Created: t0}
		if err := changer.CreateKey(ctx, k, audit(), master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
		if _, err := changer.SetGrant(ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleAdmin}, audit(), master.LocalOperator()); err != nil {
			t.Fatal(err)
		}
		conn, err := holder.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		removal, change := audit(), audit()
		removed, changed := make(chan error, 1), make(chan error, 1)
		go func() {
			_, err := remover.RemoveGrant(ctx, k.ID, master.Installation, removal, master.LocalOperator())
			removed <- err
		}()
		time.Sleep(50 * time.Millisecond)
		name := fmt.Sprintf("s%04d", round)
		go func() {
			changed <- changer.SetSetting(ctx, name, "v", change, master.ActingKey(k.ID, master.Installation, master.RoleAdmin, clock))
		}()
		time.Sleep(50 * time.Millisecond)
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		if err := <-removed; err != nil {
			t.Fatal(err)
		}
		err = <-changed
		switch {
		case err == nil:
			made++
			if rowid(change.ID) < 0 || rowid(change.ID) > rowid(removal.ID) {
				t.Errorf("round %d: the change was made after the grant's removal committed", round)
			}
		case fault.KindOf(err) == fault.Denied:
			refused++
			if rowid(change.ID) >= 0 {
				t.Errorf("round %d: the refused change wrote its entry", round)
			}
		default:
			t.Errorf("round %d: %v", round, err)
		}
	}
	t.Logf("%d made before the removal, %d refused after it", made, refused)
}
