package sqlite

import (
	"context"
	"strconv"
)

// setBetweenReads makes ReadEvents and StatusSnapshot of t call f inside
// their read transaction, between their reads (nil for none). It is the only
// way a test sets tenantSeams.betweenReads.
func (t *Tenant) setBetweenReads(f func()) { t.seams.betweenReads = f }

// setBulkWrites makes the write connection of t trade durability for speed
// while a test builds a large tenant through the port, or gives the
// connection back its own settings: on, synchronous=OFF (no commit waits
// for the disk) and a page cache of cacheMiB, so the inserts into indexes on
// random UIDs read pages from memory rather than the file; off,
// synchronous=FULL and fullfsync, as openPools sets them, and SQLite's
// default cache. The pool holds one connection forever, so a PRAGMA run on
// it holds for every later write. Only a test calls it; a server always
// writes with openPools' settings.
func (t *Tenant) setBulkWrites(ctx context.Context, on bool, cacheMiB int) error {
	stmts := []string{"PRAGMA synchronous = FULL", "PRAGMA fullfsync = 1", "PRAGMA cache_size = -2000"}
	if on {
		stmts = []string{"PRAGMA synchronous = OFF", "PRAGMA cache_size = -" + strconv.Itoa(cacheMiB*1024)}
	}
	for _, s := range stmts {
		if _, err := t.p.write.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
