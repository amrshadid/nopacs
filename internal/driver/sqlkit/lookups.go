package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ListLookupTables is the lookup tables' list name, the first field of its
// cursors.
const ListLookupTables = "tenant-lookup-tables"

// lockCounter makes the table's counter row when it has none and locks it
// (SendingDialect.Lock), as AssignLookupCodes does before it reads any of
// the table's codes, and returns its last code assigned in order.
func (k Deidentification) lockCounter(ctx context.Context, tx SendingTx, table string) (int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO lookup_counters (table_name, last_code) VALUES (?, 0)
		ON CONFLICT (table_name) DO NOTHING`, table); err != nil {
		return 0, err
	}
	var n int64
	_, err := tx.QueryRow(ctx, `SELECT last_code FROM lookup_counters WHERE table_name = ?`+k.D.Lock, []any{table}, &n)
	return n, err
}

// ImportLookupCodes implements tenant.LookupImports. Each entry is two
// point reads of the codes' primary key and its unique index on code, and
// one insert when none conflicts, under the table's counter row.
func (k Deidentification) ImportLookupCodes(ctx context.Context, table string, entries []tenant.LookupEntry, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.LookupImport, error) {
	entries, err := tenant.ValidateLookupImport(table, entries)
	if err != nil {
		return tenant.LookupImport{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.LookupImport{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.LookupImport{}, err
	}
	var out tenant.LookupImport
	err = k.Host.Write(ctx, "the lookup codes cannot be imported", func(tx SendingTx) error {
		out = tenant.LookupImport{}
		if _, err := k.lockCounter(ctx, tx, table); err != nil {
			return err
		}
		conflicts := 0
		var add []tenant.LookupEntry
		for _, e := range entries {
			var code string
			found, err := tx.QueryRow(ctx, `SELECT code FROM lookup_codes WHERE table_name = ? AND lookup_key = ?`,
				[]any{table, e.Key}, &code)
			if err != nil {
				return err
			}
			if found {
				if code != e.Code {
					conflicts++
				} else {
					out.Unchanged++
				}
				continue
			}
			taken, err := exists(ctx, tx, `SELECT 1 FROM lookup_codes WHERE table_name = ? AND code = ?`, table, e.Code)
			if err != nil {
				return err
			}
			if taken {
				conflicts++
				continue
			}
			add = append(add, e)
		}
		// Every entry is judged before the refusal, so its count says
		// nothing of where in the request a conflict was.
		if conflicts > 0 {
			return tenant.ConflictsError(table, conflicts)
		}
		for _, e := range add {
			if _, err := tx.Exec(ctx, `INSERT INTO lookup_codes (table_name, lookup_key, code, assigned) VALUES (?, ?, ?, ?)`,
				table, e.Key, e.Code, k.D.Time(now)); err != nil {
				return err
			}
			out.Added++
		}
		if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM lookup_codes WHERE table_name = ?`, []any{table}, &out.Codes); err != nil {
			return err
		}
		a, err := tenant.ImportedDetails(audit, len(entries), out)
		if err != nil {
			return err
		}
		return tx.Audited(ctx, a, guard)
	})
	if err != nil {
		return tenant.LookupImport{}, err
	}
	return out, nil
}

// ListLookupTables implements tenant.LookupImports: a range of the
// counters' primary key, each table's count a range of the codes' primary
// key.
func (k Deidentification) ListLookupTables(ctx context.Context, page master.Page) ([]tenant.LookupTableCount, string, error) {
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	after := ""
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListLookupTables, k.Host.Binding(), 1)
		if err != nil {
			return nil, "", err
		}
		after = f[0]
	}
	var out []tenant.LookupTableCount
	err := k.Host.Read(ctx, "the lookup tables cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT c.table_name, (SELECT count(*) FROM lookup_codes AS l WHERE l.table_name = c.table_name)
			FROM lookup_counters AS c WHERE c.table_name > ? ORDER BY c.table_name LIMIT ?`,
			[]any{after, page.Limit + 1}, func(r Row) error {
				var t tenant.LookupTableCount
				err := r.Scan(&t.Name, &t.Codes)
				out = append(out, t)
				return err
			})
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListLookupTables, k.Host.Binding(), last.Name), nil
}
