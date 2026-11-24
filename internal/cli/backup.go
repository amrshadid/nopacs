package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newBackup builds "backup": create over the API; verify and restore are
// commands of nopacs-server, which run on the machine that holds the data
// directory, and here only say so.
func (a *app) newBackup() *cobra.Command {
	return group("backup", "Back up the installation while the server runs",
		`Back up what the archive cannot make again from its storages, while the
server runs: the master database and each tenant's, without the catalog,
encrypted on the server with the installation key file. Checking a backup and
restoring one are commands of nopacs-server (backup verify, backup restore),
run on the machine that holds the data directory and the key file.`,
		a.newBackupCreate(), moved("verify", "nopacs-server backup verify"), moved("restore", "nopacs-server backup restore"))
}

// newBackupCreate builds "backup create".
func (a *app) newBackupCreate() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "create --out FILE",
		Short: "Write an encrypted backup of the running installation to a file",
		Long: `Ask the server for a backup of the whole installation and write it to --out:
the master database and every tenant's, each read as of one moment while the
server goes on receiving and serving, without the catalog, which a restore
rebuilds from the storages. The server encrypts it with the installation key
file before it leaves; this command never sees a key, and the file opens only
with that key file.

The file is written beside --out and given its name only once the server's
summary has come and the SHA-256 of what was written is the summary's, so a
backup cut short leaves nothing; a file already at --out is never replaced.
A tenant whose database does not answer fails the backup: it is of the whole
installation or nothing. One backup or export runs at a time on a server.

Check it with nopacs-server backup verify FILE on a machine that holds the key
file, and restore it with nopacs-server backup restore.

Needs the admin role on the installation.`,
		Example: `  nopacs backup create --out /backups/nopacs-2026-11-24.backup
  nopacs backup create --out nopacs.backup --json`,
		Args: noArgs,
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			if out == "" {
				return fault.New(fault.Invalid, "--out is required: the file to write the backup to")
			}
			if err := absent(out, "backup"); err != nil {
				return err
			}
			file, err := createPart(out, "backup")
			if err != nil {
				return err
			}
			part := file.Name()
			defer func() {
				if file != nil {
					_ = file.Close()
				}
				if part != "" {
					_ = os.Remove(part)
				}
			}()
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			sum, err := backupTo(ctx, c, file)
			if err != nil {
				return err
			}
			if err := file.Sync(); err != nil {
				return fault.Wrap(fault.Internal, err, "writing the backup: "+err.Error())
			}
			if err := file.Close(); err != nil {
				return fault.Wrap(fault.Internal, err, "writing the backup: "+err.Error())
			}
			file = nil
			if err := publish(part, out, "backup"); err != nil {
				return err
			}
			part = ""
			if a.opts.json {
				return a.write(func(w io.Writer) error { return output.JSONMessage(w, sum) })
			}
			return a.write(func(w io.Writer) error { return a.backupDetails(w, out, sum) })
		}),
	}
	cmd.Flags().StringVar(&out, "out", "", "the file to write the backup to, never replacing one")
	_ = cmd.MarkFlagFilename("out")
	return cmd
}

// backupTo streams a backup to w and returns its summary, once the size and
// SHA-256 of what was written are the summary's. The stream has no deadline
// of its own: a large installation takes as long as it takes. A stream that
// ends before its summary is an error, so a cut backup is never taken for a
// whole one.
func backupTo(ctx context.Context, c *client.Client, w io.Writer) (*v1.BackupSummary, error) {
	st, err := c.Backups.CreateBackup(ctx, &v1.CreateBackupRequest{})
	if err != nil {
		return nil, c.StreamError(ctx, "backup create", err)
	}
	defer func() { _ = st.Close() }()
	written := counted{h: sha256.New()}
	dst := io.MultiWriter(w, &written)
	var sum *v1.BackupSummary
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, c.StreamError(ctx, "backup create", err)
		}
		switch k := m.GetKind().(type) {
		case *v1.CreateBackupResponse_Data:
			if sum != nil {
				return nil, fault.New(fault.Internal, "the server sent bytes after the backup's summary")
			}
			if _, err := dst.Write(k.Data); err != nil {
				return nil, fault.Wrap(fault.Internal, err, "writing the backup: "+err.Error())
			}
		case *v1.CreateBackupResponse_Summary:
			sum = k.Summary
		}
	}
	if sum == nil {
		return nil, fault.New(fault.Unavailable, "the backup ended before its summary; nothing was kept: run it again")
	}
	if sum.GetSizeBytes() != written.n || !bytes.Equal(sum.GetSha256(), written.h.Sum(nil)) {
		return nil, fault.New(fault.Internal, fmt.Sprintf("the backup written (%d bytes) is not the one the server's summary describes (%d bytes); nothing was kept",
			written.n, sum.GetSizeBytes()))
	}
	return sum, nil
}

// counted counts and digests what is written to it.
type counted struct {
	n int64
	h hash.Hash
}

// Write implements io.Writer.
func (c *counted) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	c.h.Write(p)
	return len(p), nil
}

// backupDetails writes what a backup holds as labeled lines.
func (a *app) backupDetails(w io.Writer, out string, sum *v1.BackupSummary) error {
	pairs := []output.Pair{
		{Label: "Backup", Value: fmt.Sprintf("%s: %d bytes, SHA-256 %s", out, sum.GetSizeBytes(), hex.EncodeToString(sum.GetSha256()))},
		{Label: "Installation", Value: sum.GetInstallationId()},
		{Label: "Made", Value: a.when(sum.GetTime())},
		{Label: "Key version", Value: fmt.Sprint(sum.GetKeyVersion())},
	}
	for _, s := range sum.GetSections() {
		name := "Master"
		if s.GetTenant() != "" {
			name = "Tenant " + s.GetTenant()
		}
		pairs = append(pairs, output.Pair{Label: name, Value: fmt.Sprintf("as of %s: %d tables, %d rows", a.when(s.GetSnapshotTime()), s.GetTables(), s.GetRows())})
	}
	pairs = append(pairs, output.Pair{Label: "Opens with", Value: "the installation key file alone; check it with nopacs-server backup verify " + out})
	return output.Details(w, a.style(), pairs)
}
