package sqlkit

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The senders and jobs of a tenant (tenant.Sending) on a SQL database, one
// implementation both drivers answer with. The statements are written once,
// with "?" for each parameter, and run through a driver's transaction
// (SendingTx), which writes them in its database's terms; what differs
// besides (how a time and a bool are kept, which row locks a database takes,
// how a change is run again, how its audit entry and events are written) is
// the driver's, through SendingTx, SendingHost and SendingDialect.
//
// Every change is written to be right under several writers at read
// committed (Postgres) as well as under SQLite's one writer: a condition a
// change depends on is in the statement that changes the row, or on a row
// locked before it is read (SendingDialect.Lock), in the order the Postgres
// driver's documentation lists. A change may run more than once, each time in
// a new transaction, so a function here only assigns its results.

// SendingTx is one transaction of a driver, as the senders' and jobs'
// statements need it.
type SendingTx interface {
	// Exec runs a statement and returns how many rows it changed.
	Exec(ctx context.Context, query string, args ...any) (int64, error)
	// Query runs a statement and calls scan with each row.
	Query(ctx context.Context, query string, args []any, scan func(Row) error) error
	// QueryRow runs a statement and scans its first row into dest; false
	// when it returned none.
	QueryRow(ctx context.Context, query string, args []any, dest ...any) (bool, error)
	// Enqueue queues a unit of work as tenant.Pipeline.EnqueueWork does,
	// in this transaction.
	Enqueue(ctx context.Context, w tenant.Work, now time.Time) (tenant.Work, bool, error)
	// CheckFence refuses a write whose lease no longer holds, locking the
	// unit's row first, as every fenced write of the pipeline does.
	CheckFence(ctx context.Context, f tenant.WorkFence) error
	// LockConfig serializes the tenant's changes of configuration.
	LockConfig(ctx context.Context) error
	// WriteEvents writes the events of the change, last but for its audit
	// entry.
	WriteEvents(ctx context.Context, events ...tenant.EventRecord) error
	// Audited writes the change's audit entry, then runs its Guard, last.
	Audited(ctx context.Context, audit master.AuditEntry, guard tenant.Guard) error
}

// SendingHost runs a driver's transactions for the senders and jobs.
type SendingHost interface {
	// Write runs fn in one write transaction and commits only when it
	// returns nil; it may run fn again in a new one.
	Write(ctx context.Context, message string, fn func(SendingTx) error) error
	// Read runs fn in one read transaction: every statement reads one
	// snapshot.
	Read(ctx context.Context, message string, fn func(SendingTx) error) error
	// Binding returns what a list's cursor is bound to: the tenant and
	// parts.
	Binding(parts ...string) string
}

// SendingDialect is what differs between the databases in the statements
// themselves.
type SendingDialect struct {
	// Time returns a time as the driver's columns keep it.
	Time func(time.Time) any
	// Bool returns a bool as the driver's columns keep it.
	Bool func(bool) any
	// Lock is the clause that locks a row read for a later update in the
	// transaction (" FOR NO KEY UPDATE"), or "" where one writer runs at a
	// time.
	Lock string
	// Share is the clause that keeps a row from being removed or its key
	// changed until the commit (" FOR KEY SHARE"), or "".
	Share string
	// ShareAll is the clause that keeps a row from any change until the
	// commit while letting others read it so too (" FOR SHARE"), or "": it
	// waits for, and holds off, a transaction that read the row with Lock.
	ShareAll string
	// LockDelete is the clause that locks a row read for its removal
	// (" FOR UPDATE"), or "".
	LockDelete string
	// SkipLocked is the clause that locks the rows read and skips those
	// another transaction holds (" FOR UPDATE SKIP LOCKED"), or "".
	SkipLocked string
	// NotEnded is the condition of the jobs that have not ended, written
	// as the partial indexes of those jobs are ("ended = 0", "NOT ended"),
	// so a planner that matches a condition by its text uses them.
	NotEnded string
	// ScratchRequired says a rebuild names the scratch schema its shadow is
	// built in (Postgres), rather than none (SQLite, whose shadow is a file).
	ScratchRequired bool
	// Overriding is the clause an INSERT takes to write a row's integer id
	// itself into a table whose ids the database makes (" OVERRIDING
	// SYSTEM VALUE" for an identity column always generated), or "": an
	// undone delete puts its rows back with the ids they had.
	Overriding string
}

// Sending is tenant.Sending on a SQL database: the driver gives its host and
// dialect, and calls these from its own methods.
type Sending struct {
	// Host runs the transactions.
	Host SendingHost
	// D writes the statements in the database's terms.
	D SendingDialect
	// Shadow reaches a rebuild's shadow catalog (rebuild.go); CreateJob
	// checks a rebuild's scratch schema through it.
	Shadow ShadowHost
}

// DBTime is a time read from a column of either driver: microseconds since
// 1970 (SQLite) or a timestamp (Postgres), or NULL, which reads as the zero
// time.
type DBTime struct{ T time.Time }

// Scan implements sql.Scanner.
func (t *DBTime) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		t.T = time.Time{}
	case int64:
		t.T = time.UnixMicro(x).UTC()
	case time.Time:
		t.T = x.UTC()
	default:
		return errors.New("a time column holds a value of another type")
	}
	return nil
}

var _ interface{ Scan(any) error } = (*DBTime)(nil)

// Rebind writes a statement's "?" parameters as Postgres's "$1", "$2" and
// so on, remembering each statement it rewrote; the statements here hold no
// "?" in a literal.
func Rebind(query string) string {
	if v, ok := rebound.Load(query); ok {
		return v.(string)
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	out := b.String()
	rebound.Store(query, out)
	return out
}

// rebound holds the statements Rebind rewrote, a fixed set.
var rebound sync.Map

// notFound returns NotFound with the message.
func notFound(message string) error { return fault.New(fault.NotFound, message) }
