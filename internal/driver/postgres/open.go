package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/amrshadid/nopacs/internal/fault"
)

// minConnections is the fewest connections the pool may have. Migrate holds
// the advisory lock on one connection while the runner works on another, so
// a pool of one would wait on itself.
const minConnections = 2

// openPool parses address, checks it is a postgres:// URL, opens a pool of
// connections to it and checks that the database answers. It returns the
// redactor that scrubs this address out of every later error.
//
// No error from here repeats the address: pgx's parse errors quote the URL
// (Go's own url.Error holds it whole, password included, and pgx's redaction
// does not reach inside it), so a parse failure is reported with no cause at
// all.
func openPool(ctx context.Context, address, name string, opts poolOptions) (*pgxpool.Pool, *redactor, error) {
	lower := strings.ToLower(address)
	if !strings.HasPrefix(lower, "postgres://") && !strings.HasPrefix(lower, "postgresql://") {
		return nil, nil, fault.New(fault.Invalid, "a Postgres database's address is a postgres:// URL")
	}
	cfg, err := pgxpool.ParseConfig(address)
	if err != nil {
		return nil, nil, fault.New(fault.Invalid, "the Postgres address is not a valid postgres:// URL")
	}
	if err := checkTransport(address, cfg.ConnConfig); err != nil {
		return nil, nil, err
	}
	red := newRedactor(address, cfg.ConnConfig)

	// A default for this kind of store, used only when the address does not
	// give the parameter itself (pool_max_conns, pool_max_conn_idle_time).
	given := poolParams(address)
	if opts.maxConns > 0 && !given["pool_max_conns"] {
		cfg.MaxConns = int32(opts.maxConns) //nolint:gosec // G115: a small constant of this package.
	}
	if opts.idle > 0 && !given["pool_max_conn_idle_time"] {
		cfg.MaxConnIdleTime = opts.idle
	}
	if cfg.MaxConns < minConnections {
		cfg.MaxConns = minConnections
	}
	// Set only when the URL did not, so an operator's choice stands. The
	// application name shows in pg_stat_activity, which is how an operator
	// finds the connections of this program; UTC makes the times a psql
	// session shows for these tables unsurprising.
	if _, ok := cfg.ConnConfig.RuntimeParams["application_name"]; !ok {
		cfg.ConnConfig.RuntimeParams["application_name"] = "nopacs"
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["timezone"]; !ok {
		cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, nil, classify(red, err, "the "+name+" cannot be opened")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, classify(red, err, "the "+name+" cannot be opened")
	}
	return pool, red, nil
}

// poolOptions are the defaults of one kind of store's pool, for the
// parameters its address does not give.
type poolOptions struct {
	// maxConns is the pool's size; 0 leaves pgx's own default.
	maxConns int
	// idle is how long an idle connection is kept; 0 leaves pgx's default.
	idle time.Duration
}

// poolParams returns which parameters the address's query gives.
func poolParams(address string) map[string]bool {
	out := map[string]bool{}
	if u, err := url.Parse(address); err == nil {
		for k := range u.Query() {
			out[k] = true
		}
	}
	return out
}

// redactor removes an address and what is in it from error text.
type redactor struct {
	secrets []string
}

// newRedactor collects what in the address must never appear in an error:
// the address itself, the password, the user, the hosts and the database
// name, each also in its URL-escaped form.
func newRedactor(address string, cfg *pgx.ConnConfig) *redactor {
	var all []string
	add := func(s string) {
		if s == "" {
			return
		}
		all = append(all, s, url.QueryEscape(s), url.PathEscape(s))
		// A socket directory is printed with the system's separators; on
		// Windows pgx joins it with backslashes.
		if strings.Contains(s, "/") {
			all = append(all, strings.ReplaceAll(s, "/", `\`))
		}
	}
	add(address)
	if cfg != nil {
		add(cfg.Password)
		add(cfg.User)
		add(cfg.Host)
		add(cfg.Database)
		for _, fb := range cfg.Fallbacks {
			add(fb.Host)
		}
	}
	// Longest first, so the whole address goes before the pieces of it.
	sort.Slice(all, func(i, j int) bool { return len(all[i]) > len(all[j]) })
	return &redactor{secrets: all}
}

// ipLiteral matches an IPv4 address or a bracketed IPv6 address.
var ipLiteral = regexp.MustCompile(`\b[0-9]{1,3}(?:\.[0-9]{1,3}){3}\b|\[[0-9A-Fa-f:.]+\]`)

// text returns s with every secret replaced.
func (r *redactor) text(s string) string {
	if r == nil {
		return s
	}
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, "***")
	}
	// A name pgx resolved is printed as the addresses it resolved to, which
	// name the same host.
	return ipLiteral.ReplaceAllString(s, "***")
}

// scrubbed is an error whose text has had the address taken out. It keeps the
// original in its chain, for errors.Is and errors.As (a canceled context, a
// PgError's code), but never prints it.
type scrubbed struct {
	text  string
	cause error
}

func (e *scrubbed) Error() string { return e.text }
func (e *scrubbed) Unwrap() error { return e.cause }

// scrub returns err with its text scrubbed, or nil for nil.
func (r *redactor) scrub(err error) error {
	if err == nil {
		return nil
	}
	return &scrubbed{text: r.text(err.Error()), cause: err}
}

// SQLSTATE classes and codes (appendix A of the Postgres manual) that mean
// "the database cannot be used now", so a request may be tried again later.
const (
	sqlstateUndefinedTable  = "42P01"
	sqlstateUniqueViolation = "23505"
	sqlstateForeignKey      = "23503"
	sqlstateLockNotAvail    = "55P03"
	sqlstateReadOnly        = "25006"
)

// unavailableClasses are the two-character SQLSTATE classes of failures that
// are about the server, not the request: connection exceptions (08), invalid
// authorization (28), invalid catalog name (3D), insufficient resources (53),
// transaction rollback, serialization failures and deadlocks (40), operator
// intervention, shutdown and cancel (57) and system errors (58).
//
// Read-only SQL transaction (25006) is there too: a standby, or a database set
// read-only, cannot take the write, and the same call may pass on the primary.
var unavailableClasses = map[string]bool{"08": true, "28": true, "3D": true, "53": true, "40": true, "57": true, "58": true}

// classify turns an error from pgx into a fault with a kind, from the error
// code and never from the text of its message. An error that already has a
// kind keeps it. message is what the caller is shown; the driver's own text,
// scrubbed of the address, stays in the chain for the log.
func classify(red *redactor, err error, message string) error {
	if err == nil {
		return nil
	}
	var fe *fault.Error
	if errors.As(err, &fe) {
		return err
	}
	cause := red.scrub(err)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fault.Wrap(fault.Unavailable, cause, message)
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch {
		case pe.Code == sqlstateUndefinedTable:
			return fault.Wrap(fault.Unavailable, cause, "the master database is not ready; has it been migrated?")
		case pe.Code == sqlstateLockNotAvail, pe.Code == sqlstateReadOnly, len(pe.Code) >= 2 && unavailableClasses[pe.Code[:2]]:
			return fault.Wrap(fault.Unavailable, cause, message)
		}
		return fault.Wrap(fault.Internal, cause, message)
	}
	var ne net.Error
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) || errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		pgconn.Timeout(err) || pgconn.SafeToRetry(err) {
		return fault.Wrap(fault.Unavailable, cause, message)
	}
	return fault.Wrap(fault.Internal, cause, message)
}

// constraintViolation reports whether err is Postgres refusing a row for
// breaking the unique or primary key constraint named name, found by
// SQLSTATE 23505 and the constraint's name, never by the message.
func constraintViolation(err error, name string) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == sqlstateUniqueViolation && pe.ConstraintName == name
}

// isNoTable reports whether err is Postgres saying a table does not exist
// (SQLSTATE 42P01): the schema has not been migrated yet.
func isNoTable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == sqlstateUndefinedTable
}

// isNoRow reports whether err is pgx's "no rows".
func isNoRow(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// isUniqueViolation reports whether err is any unique or primary key
// constraint failing.
func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == sqlstateUniqueViolation
}

// isForeignKeyViolation reports whether err is a foreign key constraint
// failing.
func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == sqlstateForeignKey
}

// checkTransport refuses, as Invalid, an address that would send the
// password and the archive's keys over a network with no encryption nobody
// asked for. The driver is closed by default: a host that is not loopback and
// not a Unix socket path needs an sslmode the person wrote down, one of
// verify-full, verify-ca, require or disable. With none, or with prefer or
// allow, which pgx falls back from to plain text, it is refused. Loopback and
// socket addresses keep pgx's default. The message names no host, user or
// password.
func checkTransport(address string, cfg *pgx.ConnConfig) error {
	hosts := []string{cfg.Host}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, fb.Host)
	}
	remote := false
	for _, h := range hosts {
		if !isLocalHost(h) {
			remote = true
		}
	}
	if !remote {
		return nil
	}
	mode := ""
	if u, err := url.Parse(address); err == nil {
		mode = u.Query().Get("sslmode")
	}
	if mode == "" {
		mode = os.Getenv("PGSSLMODE")
	}
	switch strings.ToLower(mode) {
	case "verify-full", "verify-ca", "require", "disable":
		return nil
	}
	return fault.New(fault.Invalid, "a Postgres address to another machine must say how the connection is protected: add sslmode=verify-full (with sslrootcert), or write sslmode=disable if the network is trusted")
}

// isLocalHost reports whether host is a Unix socket path, "localhost" or a
// loopback address.
func isLocalHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
