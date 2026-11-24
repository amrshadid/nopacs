package postgres

import (
	"net"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/amrshadid/nopacs/internal/fault"
)

// Location says where the database at address is, for an operator to read
// and for the registry to record: host:port/database, several hosts joined
// by commas, and for a Unix socket the directory and the database as
// unix:/directory/database. It holds no user and no password.
//
// It applies the rules OpenTenant and OpenMaster apply to an address that do
// not need a server: the scheme, the parse, and the rule that an address to
// another machine writes its sslmode down. An address that fails them is
// Invalid, with a fixed message that does not repeat it: pgx's parse errors
// quote the address, password included.
func Location(address string) (string, error) {
	lower := strings.ToLower(address)
	if !strings.HasPrefix(lower, "postgres://") && !strings.HasPrefix(lower, "postgresql://") {
		return "", fault.New(fault.Invalid, "a Postgres database's address is a postgres:// URL")
	}
	cfg, err := pgx.ParseConfig(address)
	if err != nil {
		return "", fault.New(fault.Invalid, "the Postgres address is not a valid postgres:// URL")
	}
	if err := checkTransport(address, cfg); err != nil {
		return "", err
	}
	hosts := []pgconn.Config{cfg.Config}
	for _, fb := range cfg.Fallbacks {
		hosts = append(hosts, pgconn.Config{Host: fb.Host, Port: fb.Port, Database: cfg.Database})
	}
	// pgx adds a fallback for the same host without TLS when the mode is
	// prefer; a place is written once.
	var parts []string
	seen := map[string]bool{}
	for _, h := range hosts {
		w := where(h.Host, h.Port, h.Database)
		if !seen[w] {
			seen[w] = true
			parts = append(parts, w)
		}
	}
	return strings.Join(parts, ","), nil
}

// where writes one host's location.
func where(host string, port uint16, database string) string {
	if strings.HasPrefix(host, "/") {
		return "unix:" + strings.TrimRight(host, "/") + "/" + database
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))) + "/" + database
}
