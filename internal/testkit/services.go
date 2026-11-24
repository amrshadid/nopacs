package testkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The variables that name the services a test may use, and the ones that
// make a missing service a failure rather than a skip. CI sets the REQUIRE
// variables where the service is always there, so a service that failed to
// start cannot make a test pass by not running. docs/develop/testing.md lists them.
const (
	// PostgresEnv is the address of a Postgres database tests may make
	// schemas in (postgres://user:password@host:port/db).
	PostgresEnv = "NOPACS_TEST_POSTGRES"
	// RequirePostgresEnv, set to anything, fails a test that needs
	// Postgres when PostgresEnv is not set.
	RequirePostgresEnv = "NOPACS_REQUIRE_POSTGRES"
	// S3Env is a scratch S3 server's address,
	// http://KEY:SECRET@host:port/bucket, with region=NAME and pathstyle=0
	// in the query when needed.
	S3Env = "NOPACS_TEST_S3"
	// RequireS3Env, set to anything, fails a test that needs S3 when S3Env
	// is not set.
	RequireS3Env = "NOPACS_REQUIRE_S3"
)

// PostgresSchema makes a schema no other test uses in the database
// PostgresEnv names, dropped when tb ends, and returns an address whose
// search_path is that schema, with a pool of at most three connections. A
// tenant's schema belongs to one tenant, so each test that makes a tenant
// on Postgres needs one of its own; the small pool keeps tests running side
// by side under the server's connection limit (100 by default). It returns
// false when PostgresEnv is not set, and fails the test instead when
// RequirePostgresEnv is.
func PostgresSchema(tb testing.TB) (string, bool) {
	tb.Helper()
	base, ok := service(tb, PostgresEnv, RequirePostgresEnv)
	if !ok {
		return "", false
	}
	u, err := url.Parse(base)
	if err != nil {
		tb.Fatalf("%s is not a URL", PostgresEnv)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	schema := "tk_" + hex.EncodeToString(b)
	exec(tb, u, "CREATE SCHEMA "+schema)
	tb.Cleanup(func() { exec(tb, u, "DROP SCHEMA "+schema+" CASCADE") })
	q := u.Query()
	q.Set("search_path", schema)
	q.Set("pool_max_conns", "3")
	u.RawQuery = q.Encode()
	return u.String(), true
}

// PostgresScratch makes an empty schema no other test uses, on no
// connection's search path, in the database PostgresEnv names, dropped with
// what it holds when tb ends, and returns its name: the scratch schema a
// rebuild builds its shadow catalog in. It returns false when PostgresEnv
// is not set, and fails the test instead when RequirePostgresEnv is.
func PostgresScratch(tb testing.TB) (string, bool) {
	tb.Helper()
	base, ok := service(tb, PostgresEnv, RequirePostgresEnv)
	if !ok {
		return "", false
	}
	u, err := url.Parse(base)
	if err != nil {
		tb.Fatalf("%s is not a URL", PostgresEnv)
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		tb.Fatal(err)
	}
	schema := "tkscratch_" + hex.EncodeToString(b)
	exec(tb, u, "CREATE SCHEMA "+schema)
	tb.Cleanup(func() { exec(tb, u, "DROP SCHEMA "+schema+" CASCADE") })
	return schema, true
}

// S3Address returns the scratch S3 server's address S3Env names. It returns
// false when S3Env is not set, and fails the test instead when RequireS3Env
// is.
func S3Address(tb testing.TB) (string, bool) {
	tb.Helper()
	return service(tb, S3Env, RequireS3Env)
}

// service reads the variable env, failing tb when it is empty and require
// is set.
func service(tb testing.TB, env, require string) (string, bool) {
	tb.Helper()
	if v := os.Getenv(env); v != "" {
		return v, true
	}
	if os.Getenv(require) != "" {
		tb.Fatalf("%s is not set, and %s is: the test must run here, not be skipped", env, require)
	}
	return "", false
}

// exec runs one statement on a connection of its own to the database at u,
// without the pool's parameters (pgx would send them to the server as
// settings, which refuses them). pgx's error names the host, user and
// database, never the password.
func exec(tb testing.TB, u *url.URL, statement string) {
	tb.Helper()
	c := *u
	q := c.Query()
	for k := range q {
		if strings.HasPrefix(k, "pool_") || k == "search_path" {
			q.Del(k)
		}
	}
	c.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, c.String())
	if err != nil {
		tb.Fatalf("cannot connect to the database %s names: %v", PostgresEnv, err)
	}
	defer func() { _ = conn.Close(context.Background()) }() // A failed close of a test's connection hides nothing.
	if _, err := conn.Exec(ctx, statement); err != nil {
		tb.Fatalf("%s: %v", statement, err)
	}
}

// PostgresExec runs statements, in order, on a connection of its own to the
// database at address (as PostgresSchema returned it), in its schema: for a
// test that must change what a driver made, such as bringing a schema back
// to the form an older version left. The pool's parameters are dropped, as
// exec drops them; the search path is kept.
func PostgresExec(tb testing.TB, address string, statements ...string) {
	tb.Helper()
	u, err := url.Parse(address)
	if err != nil {
		tb.Fatal("the address is not a URL")
	}
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(k, "pool_") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		tb.Fatalf("cannot connect to the database %s names: %v", PostgresEnv, err)
	}
	defer func() { _ = conn.Close(context.Background()) }() // A failed close of a test's connection hides nothing.
	for _, s := range statements {
		if _, err := conn.Exec(ctx, s); err != nil {
			tb.Fatalf("%s: %v", s, err)
		}
	}
}
