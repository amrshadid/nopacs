package servecmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
)

// importFlags are the flags of "tenant import".
type importFlags struct {
	transferKey, masterEnv, dbEnv, scratch string
	takeOver                               bool
}

// TenantImport returns the "tenant import" command, which runs on this
// machine with the server stopped.
func TenantImport(opts Options) *cobra.Command {
	opts = opts.withDefaults()
	var f importFlags
	cmd := &cobra.Command{
		Use:   "import FILE --transfer-key FILE",
		Short: "Load a tenant another installation exported, on this machine",
		Long: `Load the tenant another installation exported (nopacs tenant export there)
into the installation in the data directory, with its transfer key. The server
must be stopped: the command takes the data directory's lock.

The whole file is read and checked under the transfer key first; nothing is
written if any of it fails. Then the tenant's database is loaded, every sealed
value in it (the naming, UID and date secrets, storage and sender
credentials) opened with the transfer key and sealed again with this
installation's key file, so labels, remapped UIDs and date shifts stay the
same. The tenant keeps its name and its id.

Its storages stay where they are: this installation must list their roots
(storage.directory-roots, by the same root names) or their endpoints. Before
the tenant opens, each storage is taken over: its owner record is written
naming this installation, one generation past the export's, so the exporting
installation, started again against the same storages, fences the tenant
(read only) rather than writing beside this one. A storage whose owner record
names another installation than the exporting one, or a later generation, is
refused unless --take-over is given. Every correction record is written again
under this installation's key, the catalog is rebuilt from the storages, and
a new event history begins. Keys and grants stay with the exporting
installation: give this installation's keys grants on the tenant.

A tenant of the same name or id here refuses the import. An import stopped
half way leaves the tenant's database, named in the message, and no tenant:
remove it, then run the import again.

The transfer key is the file nopacs tenant export wrote, readable by its owner
alone. A tenant on Postgres is loaded into the empty schema whose address the
variable --db-env names, its catalog rebuilt in --scratch-schema.

The data directory is --data-dir, or NOPACS_DATA_DIR.`,
		Example: `  nopacs-server tenant import lab1.export --transfer-key lab1.key --data-dir /var/lib/nopacs`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := dataDir(cmd, opts.Getenv)
			if err != nil {
				return err
			}
			if f.transferKey == "" {
				return fault.New(fault.Invalid, "--transfer-key is required: the file nopacs tenant export wrote the transfer key to")
			}
			return runTenantImport(cmd.Context(), opts, dir, args[0], f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.transferKey, "transfer-key", "", "the file holding the export's transfer key")
	fl.StringVar(&f.masterEnv, "master-env", "", "the name of an environment variable holding the master database's address, when it is not SQLite in the data directory")
	fl.StringVar(&f.dbEnv, "db-env", "", "the name of an environment variable holding the address of the empty Postgres schema to load the tenant into; without it the tenant is SQLite in the data directory")
	fl.StringVar(&f.scratch, "scratch-schema", "", "the Postgres schema a tenant on Postgres has its catalog rebuilt in")
	fl.BoolVar(&f.takeOver, "take-over", false, "take a storage over even when its owner record names another installation than the exporting one, or a later generation")
	return cmd
}

// runTenantImport loads the export in file into the installation in dir.
func runTenantImport(ctx context.Context, opts Options, dir, file string, f importFlags) error {
	if err := checkMade(dir, f.masterEnv); err != nil {
		return err
	}
	lock, err := server.TakeLock(dir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	keys, err := secret.LoadKeyFile(filepath.Join(dir, KeyFileName))
	if err != nil {
		return err
	}
	key, err := secret.ReadTransferKeyFile(f.transferKey)
	if err != nil {
		return err
	}
	defer clear(key)
	address, _, err := masterAddress(dir, f.masterEnv, opts.Getenv)
	if err != nil {
		return err
	}
	m, err := driver.OpenMaster(ctx, address)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()
	if err := checkNotEmpty(ctx, m); err != nil {
		return err
	}
	if err := m.Migrate(ctx); err != nil {
		return err
	}
	if err := checkIdentity(ctx, m, dir); err != nil {
		return err
	}
	if err := checkKeyFile(ctx, m, dir, keys); err != nil {
		return err
	}
	id, err := server.ReadInstallationID(dir)
	if err != nil {
		return err
	}
	cfg := backup.ImportConfig{File: file, TransferKey: key, Keys: keys, Installation: id, Master: m, DataDir: dir, Scratch: f.scratch, TakeOver: f.takeOver}
	if f.dbEnv != "" {
		cfg.Driver, cfg.Address = master.PostgresDriver, opts.Getenv(f.dbEnv)
		if cfg.Address == "" {
			return fault.New(fault.Invalid, "--db-env names "+f.dbEnv+", which is empty or not set")
		}
	}
	rep, err := backup.ImportTenant(ctx, cfg)
	if err != nil {
		return err
	}
	return printImport(opts.Stdout, dir, rep)
}

// printImport says what the import did.
func printImport(w io.Writer, dir string, rep backup.ImportReport) error {
	var b strings.Builder
	c := output.Clean
	fmt.Fprintf(&b, "Imported tenant %s (id %s) from installation %s, exported at %s.\n", c(rep.Tenant), c(rep.TenantID), c(rep.From),
		rep.Exported.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "  its database as of %s: %d tables, %d rows (schema version %d), SHA-256 %s\n",
		rep.Section.Point.SnapshotTime.UTC().Format(time.RFC3339), rep.Section.Tables, rep.Section.Rows, rep.Section.Point.SchemaVersion, rep.Section.SHA256)
	for _, o := range rep.Result.Owners {
		fmt.Fprintf(&b, "  storage %s taken over at owner generation %d\n", c(o.Storage), o.Generation)
	}
	fmt.Fprintf(&b, "  correction records written again under this installation's key: %d\n", rep.Rewritten)
	fmt.Fprintf(&b, "  catalog rebuilt from the storages by job %s; a new event history begins\n", rep.Result.Rebuild.ID)
	if rep.Result.QuarantineMarked > 0 {
		fmt.Fprintf(&b, "  %d quarantined files' bytes stayed in the other installation's incoming area; their records say so\n", rep.Result.QuarantineMarked)
	}
	b.WriteString(backup.CredentialsText(rep.Result.Credentials))
	b.WriteString("Keys and grants stayed with the other installation: grant this installation's keys on the tenant.\n")
	fmt.Fprintf(&b, "\nStart the server with:\n  nopacs-server serve --data-dir %s\n", dir)
	_, err := io.WriteString(w, b.String())
	return err
}
