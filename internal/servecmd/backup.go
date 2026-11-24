package servecmd

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// Backup returns the "backup" command, whose commands run on this machine:
// create and restore with the server stopped, and verify, which writes
// nothing.
func Backup(opts Options) *cobra.Command {
	opts = opts.withDefaults()
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the installation, check a backup, or restore one, on this machine",
		Long: `Back up what the archive cannot make again from its storages (the master
database and each tenant's, without the catalog), encrypted with the
installation key file; check such a backup whole without writing anything; or
restore it into an empty data directory.

All run on this machine; create and restore with the server stopped. The
catalog is never in a backup: a restore rebuilds it from the storages.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(backupCreate(opts), backupVerify(opts), backupRestore(opts))
	return cmd
}

// backupVerify returns "backup verify".
func backupVerify(opts Options) *cobra.Command {
	var keyFile string
	cmd := &cobra.Command{
		Use:   "verify FILE",
		Short: "Check a backup whole, writing nothing",
		Long: `Read the backup FILE whole and check it as a restore does before it writes
anything: every chunk authenticated with the installation key file, the
layout and every count of every line, each section's SHA-256 against the
footer, the end of the file, and each section's schema version against the
versions this nopacs restores. Nothing is written anywhere; the server may run.

It prints what the backup holds and what a restore of it could not bring
back. It does not read the storages: what a restore would find there (the
correction records, the owner records) is checked by the restore.

The key file is --key-file, or the key of the data directory --data-dir (or
NOPACS_DATA_DIR) names.`,
		Example: `  nopacs-server backup verify /backups/nopacs-2026-11-22.backup --key-file /safe/nopacs.key`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keyFile == "" {
				dir, err := dataDir(cmd, opts.Getenv)
				if err != nil {
					return fault.New(fault.Invalid, "give the key file the backup was made with (--key-file), or the data directory that holds it")
				}
				keyFile = filepath.Join(dir, KeyFileName)
			}
			return runBackupVerify(opts, args[0], keyFile)
		},
	}
	cmd.Flags().StringVar(&keyFile, "key-file", "", "the installation key file the backup was made with; default the data directory's")
	return cmd
}

// runBackupVerify checks the backup in file with the key file at keyFile
// and prints what it found.
func runBackupVerify(opts Options, file, keyFile string) error {
	keys, err := secret.LoadKeyFile(keyFile)
	if err != nil {
		return err
	}
	f, err := os.Open(file) // #nosec G304 -- the file a person named on the command line.
	if err != nil {
		return fault.Wrap(fault.Invalid, err, "the backup "+file+" cannot be opened")
	}
	defer func() { _ = f.Close() }()
	v, err := backup.Verify(f, keys)
	if err != nil {
		return err
	}
	if err := v.CheckVersions(); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The backup %s of installation %s, made at %s, is whole: every chunk opened with the key file,\n"+
		"every line in its layout and every count right, each section's SHA-256 the footer's, nothing after the footer,\n"+
		"and every schema version one this nopacs restores. Nothing was written.\n",
		file, output.Clean(v.Header.Installation), v.Header.Time.UTC().Format(time.RFC3339))
	for i, s := range v.Sums {
		name := "master"
		if s.Tenant != "" {
			name = "tenant " + output.Clean(s.Tenant)
		}
		p := v.Ends[i].Point
		fmt.Fprintf(&b, "  %-24s as of %s: %d tables, %d rows (schema version %d), SHA-256 %s\n", name,
			p.SnapshotTime.UTC().Format(time.RFC3339), s.Tables, s.Rows, p.SchemaVersion, s.SHA256)
	}
	for _, l := range v.Lost {
		var files int64
		for _, r := range l.Receptions {
			files += r.Files
		}
		fmt.Fprintf(&b, "  a restore of tenant %s could not bring back %d files of unfinished receptions, nor the bytes of %d held\n"+
			"    quarantined files, which were in incoming\n", output.Clean(l.Tenant), files, l.QuarantineFiles)
	}
	b.WriteString("The storages were not read: a restore checks their owner records and correction records.\n")
	_, err = io.WriteString(opts.Stdout, b.String())
	return err
}

// backupCreate returns "backup create".
func backupCreate(opts Options) *cobra.Command {
	var out, masterEnv string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Write an encrypted backup of the installation to a file",
		Long: `Write an encrypted backup of the installation in the data directory to --out:
the master database and every tenant's, each read as of one moment, without
the catalog. It opens only with the installation key file.

The server must be stopped: the command takes the data directory's lock. The
file is written beside --out and renamed over it only once it is whole, so a
failed backup leaves nothing. A tenant whose database does not answer fails
the backup: a backup is of the whole installation or nothing.

The data directory is --data-dir, or NOPACS_DATA_DIR.`,
		Example: `  nopacs-server backup create --data-dir /var/lib/nopacs --out /backups/nopacs-2026-11-22.backup`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir(cmd, opts.Getenv)
			if err != nil {
				return err
			}
			if out == "" {
				return fault.New(fault.Invalid, "--out is required: the file to write the backup to")
			}
			return runBackupCreate(cmd.Context(), opts, dir, masterEnv, out)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "the file to write the backup to")
	cmd.Flags().StringVar(&masterEnv, "master-env", "", "the name of an environment variable holding the master database's address, when it is not SQLite in the data directory")
	return cmd
}

// runBackupCreate writes the backup of the installation in dir to out.
func runBackupCreate(ctx context.Context, opts Options, dir, masterEnv, out string) (err error) {
	if err := checkMade(dir, masterEnv); err != nil {
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
	address, _, err := masterAddress(dir, masterEnv, opts.Getenv)
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
	tenants, closeAll, err := backup.OpenTenants(ctx, m, keys, dir)
	if err != nil {
		return err
	}
	defer closeAll()
	sm, ok := m.(snapshot.Database)
	if !ok {
		return fault.New(fault.Internal, "this master database type cannot be backed up")
	}
	stage, err := os.CreateTemp(filepath.Dir(out), "."+filepath.Base(out)+".*.partial")
	if err != nil {
		return fault.Wrap(fault.Invalid, err, "the backup cannot be written beside "+out)
	}
	defer func() {
		if err != nil {
			_ = stage.Close()
			_ = os.Remove(stage.Name())
		}
	}()
	if err := stage.Chmod(0o600); err != nil {
		return fault.Wrap(fault.Internal, err, "the backup's file cannot be made private")
	}
	sum, err := backup.Create(ctx, backup.CreateConfig{Installation: id, Keys: keys, Master: sm, Tenants: tenants, Out: stage})
	if err != nil {
		return err
	}
	if err := stage.Sync(); err != nil {
		return fault.Wrap(fault.Internal, err, "the backup cannot be written to disk")
	}
	if err := stage.Close(); err != nil {
		return fault.Wrap(fault.Internal, err, "the backup cannot be written to disk")
	}
	if err := os.Rename(stage.Name(), out); err != nil {
		return fault.Wrap(fault.Internal, err, "the backup cannot be renamed to "+out)
	}
	return printSummary(opts, out, sum)
}

// printSummary says what the backup holds and how to check it.
func printSummary(opts Options, out string, sum backup.Summary) error {
	w := opts.Stdout
	_, err := fmt.Fprintf(w, "Wrote a backup of installation %s to %s: %d bytes, SHA-256 %s, key version %d.\n",
		sum.Installation, out, sum.Size, hex.EncodeToString(sum.SHA256), sum.KeyVersion)
	for _, s := range sum.Sections {
		name := "master"
		if s.Tenant != "" {
			name = "tenant " + s.Tenant
		}
		if _, err := fmt.Fprintf(w, "  %-24s as of %s: %d tables, %d rows, SHA-256 %s\n", name,
			s.Point.SnapshotTime.Format("2006-01-02T15:04:05.000000Z07:00"), s.Tables, s.Rows, s.SHA256); err != nil {
			return err
		}
	}
	if err == nil {
		_, err = fmt.Fprintln(w, "It opens only with the installation key file; keep a copy of that file apart from the backup.")
	}
	return err
}

// backupRestore returns "backup restore".
func backupRestore(opts Options) *cobra.Command {
	var keyFile, masterEnv, scratch string
	var takeOver bool
	cmd := &cobra.Command{
		Use:   "restore FILE",
		Short: "Restore a backup into an empty data directory",
		Long: `Restore the backup FILE into the data directory, which must be absent, empty,
or hold only the key file. The server must not run: the restore takes the
data directory's lock, so neither a server nor a second restore runs on it.

The whole file is read and checked first (every chunk authenticated, every
section's SHA-256 against the footer, every schema version one this nopacs
knows); nothing is written if any of it fails. Then each database is loaded
and brought to this version's schema, and every key is suspended: a key
revoked after the backup would otherwise work again. To give a holder a
working key, the local operator rotates it on this machine's socket (nopacs key
rotate ID); the successor holds the same grants and is not suspended.

Each tenant is then reconciled with its storages: their owner records taken,
the correction records read, the catalog rebuilt from the storages, a new
event history begun. A correction record is not trusted alone: one with no
row in the backup becomes an unresolved correction an admin decides (a delete
holds its data, never purged nor undone by its record), and one that
disagrees with the backup's row leaves the row as it is and is reported.
What could not be brought back is printed, per tenant.

A storage whose owner record names another installation or tenant, or a
later generation than the backup's, was taken over after the backup; the
restore refuses it unless --take-over is given. Stop whatever uses it first.

The installation's id is written last. A restore stopped half way leaves a
data directory the server refuses to start on, with a marker saying a restore
was interrupted: remove the data directory's contents except the key file
(and empty the Postgres schemas it filled), then run the restore again.

The key file is --key-file, or the data directory's key. A tenant on Postgres
is restored into the database its address names, which must be empty, and its
catalog is rebuilt in --scratch-schema, which must exist.

The data directory is --data-dir, or NOPACS_DATA_DIR.`,
		Example: `  nopacs-server backup restore /backups/nopacs-2026-11-22.backup --data-dir /var/lib/nopacs --key-file /safe/nopacs.key`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := dataDir(cmd, opts.Getenv)
			if err != nil {
				return err
			}
			return runBackupRestore(cmd.Context(), opts, dir, args[0], keyFile, masterEnv, scratch, takeOver)
		},
	}
	cmd.Flags().StringVar(&keyFile, "key-file", "", "the installation key file the backup was made with; default the data directory's")
	cmd.Flags().StringVar(&masterEnv, "master-env", "", "the name of an environment variable holding the address of the empty database to restore the master into, when it is not SQLite in the data directory")
	cmd.Flags().StringVar(&scratch, "scratch-schema", "", "the Postgres schema the catalog of a tenant on Postgres is rebuilt in")
	cmd.Flags().BoolVar(&takeOver, "take-over", false, "take a storage over even when its owner record names another installation or tenant, or a later generation than the backup's")
	return cmd
}

// runBackupRestore restores file into dir.
func runBackupRestore(ctx context.Context, opts Options, dir, file, keyFile, masterEnv, scratch string, takeOver bool) error {
	if keyFile == "" {
		keyFile = filepath.Join(dir, KeyFileName)
	}
	abs, err := filepath.Abs(keyFile)
	if err != nil {
		return fault.Wrap(fault.Invalid, err, "the key file "+keyFile+" is not a usable path")
	}
	keys, err := secret.LoadKeyFile(abs)
	if err != nil {
		return err
	}
	address, _, err := masterAddress(dir, masterEnv, opts.Getenv)
	if err != nil {
		return err
	}
	rep, err := backup.Restore(ctx, backup.RestoreConfig{File: file, Keys: keys, KeyFile: abs, DataDir: dir, MasterAddress: address,
		Scratch: scratch, TakeOver: takeOver})
	if err != nil {
		if rep.Installation != "" {
			_ = rep.Print(opts.Stderr)
		}
		return err
	}
	if err := rep.Print(opts.Stdout); err != nil {
		return err
	}
	_, err = fmt.Fprintf(opts.Stdout, "\nStart the server with:\n  nopacs-server serve --data-dir %s\n", dir)
	return err
}
