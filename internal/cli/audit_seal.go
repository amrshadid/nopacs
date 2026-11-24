package cli

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// logText names the log a scope reads, for a person.
func logText(tenant string) string {
	if tenant == "" {
		return "the installation's audit log"
	}
	return "the audit log of tenant " + output.Clean(tenant)
}

// problemKey identifies a problem, so one a later call of a verification
// in several calls finds again (the anchors and the values kept outside are
// checked by each call) is shown once.
type problemKey struct {
	kind   v1.AuditVerifyProblemKind
	seq    int64
	detail string
}

// newAuditVerify builds "audit verify".
func (a *app) newAuditVerify() *cobra.Command {
	var tenant, expectDigest string
	var from, to, expectSeq int64
	cmd := &cobra.Command{
		Use:   "verify [--tenant NAME] [--from SEQ] [--to SEQ] [--expect-seq N] [--expect-digest HEX]",
		Short: "Recompute an audit log's hash chain and check its signed checkpoints",
		Long: `Have the server recompute the hash chain of the installation's audit log, or
with --tenant of that tenant's, check each signed checkpoint against it with the
public keys derived from the installation key file, and check the anchors the
other logs keep of it. The server verifies at most a million entries or ten
thousand checkpoints a call, and stops early when its deadline is near; the
command calls again from where the last call stopped until the range is done. Entries are named by their seq, their place in the chain, from 1.

--expect-seq and --expect-digest are values kept outside the installation from
an earlier export or verification: the last sealed seq, and a checkpoint's
digest. A log taken back with its database is consistent and passes everything
else; only these show it.

A log with a problem is a result, not an error: each problem is listed with
its kind and seq, and the command exits 1 so a script sees it.`,
		Example: `  nopacs audit verify
  nopacs audit verify --tenant lab1 --expect-seq 120344
  nopacs audit verify --from 5000 --to 9000 --json`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			tenant, err := a.defaultTenant(cmd, tenant)
			if err != nil {
				return err
			}
			req := &v1.VerifyAuditRequest{Scope: optionalScope(tenant), FromSeq: from}
			if cmd.Flags().Changed("to") {
				req.ToSeq = &to
			}
			if cmd.Flags().Changed("expect-seq") {
				req.ExpectedMinSeq = &expectSeq
			}
			if expectDigest != "" {
				d, err := hex.DecodeString(expectDigest)
				if err != nil || len(d) != 32 {
					return fault.New(fault.Invalid, "--expect-digest is a checkpoint's digest: 64 hexadecimal digits")
				}
				req.ExpectedCheckpointDigest = d
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			total := &v1.VerifyAuditResponse{Intact: true}
			seen := map[problemKey]bool{}
			for {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.VerifyAuditResponse, error) {
					return c.AuditSeal.VerifyAudit(ctx, req)
				})
				if err != nil {
					return err
				}
				total.Intact = total.Intact && res.GetIntact()
				total.EntriesChecked += res.GetEntriesChecked()
				total.CheckpointsChecked += res.GetCheckpointsChecked()
				total.UnsealedEntries, total.LastCheckpoint = res.GetUnsealedEntries(), res.GetLastCheckpoint()
				for _, p := range res.GetProblems() {
					k := problemKey{p.GetKind(), p.GetSeq(), p.GetDetail()}
					if !seen[k] && len(total.Problems) < 1000 {
						seen[k] = true
						total.Problems = append(total.Problems, p)
					}
				}
				next := res.GetNextSeq()
				if next == 0 {
					break
				}
				if next <= req.GetFromSeq() {
					return fault.New(fault.Internal, "the server answered a next seq that does not go forward")
				}
				req.FromSeq = next
			}
			if err := messageOut(a, total, func(w io.Writer) error { return a.verifyTable(w, tenant, total) }); err != nil {
				return err
			}
			if !total.GetIntact() {
				return fault.New(fault.Internal, fmt.Sprintf("%s did not verify: %d problems", logText(tenant), len(total.GetProblems())))
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "verify this tenant's log (default: the installation's log)")
	cmd.Flags().Int64Var(&from, "from", 0, "the first seq to verify; moved back to the start of the checkpoint that covers it (default: the first)")
	cmd.Flags().Int64Var(&to, "to", 0, "the last seq to verify (default: the last sealed entry)")
	cmd.Flags().Int64Var(&expectSeq, "expect-seq", 0, "a seq the log is known to have reached, kept outside the installation")
	cmd.Flags().StringVar(&expectDigest, "expect-digest", "", "the digest of a checkpoint known to be in the log, in hexadecimal, kept outside the installation")
	return cmd
}

// verifyTable writes what a verification found, for a person.
func (a *app) verifyTable(w io.Writer, tenant string, r *v1.VerifyAuditResponse) error {
	intact := "yes"
	if !r.GetIntact() {
		intact = "NO"
	}
	pairs := []output.Pair{
		{Label: "Log", Value: logText(tenant)},
		{Label: "Intact", Value: intact},
		{Label: "Checked", Value: fmt.Sprintf("%d entries, %d checkpoints", r.GetEntriesChecked(), r.GetCheckpointsChecked())},
		{Label: "Unsealed", Value: fmt.Sprintf("%d (sealed at the next pass; no verification covers them)", r.GetUnsealedEntries())},
	}
	if lc := r.GetLastCheckpoint(); lc != nil {
		pairs = append(pairs,
			output.Pair{Label: "Last checkpoint", Value: fmt.Sprintf("seq %d to %d, %s", lc.GetFromSeq(), lc.GetToSeq(), a.when(lc.GetTime()))},
			output.Pair{Label: "Its digest", Value: hex.EncodeToString(lc.GetDigest())})
	} else {
		pairs = append(pairs, output.Pair{Label: "Last checkpoint", Value: "none: the log was never sealed"})
	}
	if err := output.Details(w, a.style(), pairs); err != nil {
		return err
	}
	if len(r.GetProblems()) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(r.GetProblems()))
	for _, p := range r.GetProblems() {
		at := "-"
		if p.GetSeq() > 0 {
			at = strconv.FormatInt(p.GetSeq(), 10)
		}
		rows = append(rows, []string{output.AuditProblem(p.GetKind()), at, a.whenOr(p.GetTime(), "-"), p.GetDetail()})
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	return output.Table(w, a.style(), []string{"PROBLEM", "SEQ", "TIME", "DETAIL"}, rows)
}

// newAuditCheckpoints builds "audit checkpoints".
func (a *app) newAuditCheckpoints() *cobra.Command {
	var tenant string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "checkpoints [--tenant NAME]",
		Short: "List an audit log's signed checkpoints, newest first",
		Long: `List the signed checkpoints of the installation's audit log, or with --tenant
of that tenant's, newest first: the range of seq each seals, when, the version of
the installation key that signed it, whether retention pruned its entries, and
its digest, which the next checkpoint names and which can be kept outside the
installation for audit verify --expect-digest.`,
		Example: `  nopacs audit checkpoints --limit 5
  nopacs audit checkpoints --tenant lab1 --all --json`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			if err := l.check(); err != nil {
				return err
			}
			tenant, err := a.defaultTenant(cmd, tenant)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			cps, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.AuditCheckpoint, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListAuditCheckpointsResponse, error) {
					return c.AuditSeal.ListAuditCheckpoints(ctx, &v1.ListAuditCheckpointsRequest{Scope: optionalScope(tenant), PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetCheckpoints(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "checkpoints", cps, more, func() error {
				rows := make([][]string, 0, len(cps))
				for _, cp := range cps {
					pruned := "no"
					if cp.GetPruned() {
						pruned = "yes"
					}
					rows = append(rows, []string{strconv.FormatInt(cp.GetFromSeq(), 10), strconv.FormatInt(cp.GetToSeq(), 10), strconv.FormatInt(cp.GetEntries(), 10),
						a.when(cp.GetTime()), strconv.Itoa(int(cp.GetKeyVersion())), pruned, hex.EncodeToString(cp.GetDigest())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"FROM", "TO", "ENTRIES", "TIME", "KEY", "PRUNED", "DIGEST"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "list this tenant's checkpoints (default: the installation's)")
	l.add(cmd)
	return cmd
}

// newAuditPublicKey builds "audit public-key".
func (a *app) newAuditPublicKey() *cobra.Command {
	var installation string
	var version int32
	cmd := &cobra.Command{
		Use:   "public-key [--version N [--installation ID]]",
		Short: "Write the public key that checks the audit logs' signatures, as PEM",
		Long: `Write the public key that checks the signatures of the installation's audit
checkpoints, as a PEM file openssl reads, so it is kept outside the installation.
Someone who can rewrite every database and holds the key file can sign a
rewritten log again; a public key copied elsewhere while the log was young is
what shows that. Keep one per version of the installation key.

Without --version it writes the key that signs now. --version N writes that
version's. --installation names the installation whose key it is: once a
tenant import exists, that of an installation a tenant was brought from; today
the server holds only its own installation's keys, and another id is not
found. --json lists every key with its installation and version. Any caller
the server authenticates may read the keys: they check a signature and cannot
make one.`,
		Example: `  nopacs audit public-key
  nopacs audit public-key --version 1
  nopacs audit public-key --json`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("installation") && !cmd.Flags().Changed("version") {
				return fault.New(fault.Invalid, "--installation names a key together with --version")
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetAuditPublicKeyResponse, error) {
				return c.AuditSeal.GetAuditPublicKey(ctx, &v1.GetAuditPublicKeyRequest{})
			})
			if err != nil {
				return err
			}
			// The server answers the keys of every installation it holds; an
			// installation none of them names is not found, with --json too,
			// rather than a list a script takes for an answer.
			if installation != "" && !slices.ContainsFunc(res.GetPublicKeys(), func(k *v1.AuditPublicKey) bool { return k.GetInstallationId() == installation }) {
				return fault.New(fault.NotFound, fmt.Sprintf("the server holds no audit public key of installation %s", output.Clean(installation)))
			}
			if a.opts.json {
				return messageOut(a, res, nil)
			}
			if installation == "" {
				installation = res.GetInstallationId()
			}
			var pick *v1.AuditPublicKey
			for _, k := range res.GetPublicKeys() {
				switch {
				case cmd.Flags().Changed("version"):
					if k.GetKeyVersion() == version && k.GetInstallationId() == installation {
						pick = k
					}
				case k.GetCurrent():
					pick = k
				}
			}
			if pick == nil {
				return fault.New(fault.NotFound, fmt.Sprintf("the server holds no audit public key of version %d of installation %s", version, output.Clean(installation)))
			}
			return a.write(func(w io.Writer) error {
				_, err := io.WriteString(w, pick.GetPublicKeyPem())
				return err
			})
		}),
	}
	cmd.Flags().Int32Var(&version, "version", 0, "the version of the installation key whose public key to write (default: the one that signs now)")
	cmd.Flags().StringVar(&installation, "installation", "", "the installation whose key it is, once a tenant import brings another's (default: this one)")
	return cmd
}

// exportSummary is what "audit export" says of a file it wrote.
type exportSummary struct {
	File           string `json:"file"`
	InstallationID string `json:"installationId"`
	Log            string `json:"log"`
	Tenant         string `json:"tenant"`
	FromSeq        int64  `json:"fromSeq"`
	ToSeq          int64  `json:"toSeq"`
	Entries        int64  `json:"entries"`
	Checkpoints    int64  `json:"checkpoints"`
	Pruned         int64  `json:"prunedCheckpoints"`
	LastDigest     string `json:"lastDigest"`
}

// lineOptions writes each message of an export: the contract's JSON names,
// fields at their default left out, which the format allows.
var lineOptions = protojson.MarshalOptions{}

// exportLine returns one message of an export as one line of JSON.
func exportLine(m *v1.ExportAuditResponse) ([]byte, error) {
	raw, err := lineOptions.Marshal(m)
	if err != nil {
		return nil, err
	}
	// protojson varies its white space from build to build; compacting
	// makes every line the same, and one line.
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// newAuditExport builds "audit export".
func (a *app) newAuditExport() *cobra.Command {
	var tenant, out string
	var from, to int64
	cmd := &cobra.Command{
		Use:   "export [--tenant NAME] [--from SEQ] [--to SEQ] [--out FILE]",
		Short: "Export an audit log's sealed entries and checkpoints as JSON lines",
		Long: `Export the sealed entries of the installation's audit log, or with --tenant of
that tenant's, with what checks them without the server: first a header (the
installation, the log, the public keys, the anchors and the chain value before
the first entry), then each entry with its seq, its encoded form and the chain
value after it, each signed checkpoint after the last entry it covers. One JSON
object a line, the messages of the API's ExportAudit as protobuf JSON. Entries
not sealed yet are not exported.

The file is checked with hashlib and openssl alone, for example by
scripts/prove/audit_verify.py, with the public key kept outside the
installation (audit public-key), never with the key in the file.

A range is of seq, never of time, and starts at the beginning of the checkpoint
that covers --from. With --out the file is written to a new temporary beside
FILE (.FILE.<random>.part, readable by its owner alone) and given the name
FILE when the export is whole; an existing FILE is never replaced, one that
appears while the export is written included, and the command says what it
exported. Without --out the lines go to standard output.

Export before setting audit.retention: what retention prunes is gone.`,
		Example: `  nopacs audit export --out audit.jsonl
  nopacs audit export --tenant lab1 --from 120001 --out lab1-audit.jsonl
  nopacs audit export --to 5000`,
		Args: exactArgs(0),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, _ []string) error {
			toStdout := out == "" || out == "-"
			if toStdout && a.opts.json {
				return fault.New(fault.Invalid, "--json describes the file --out wrote; the export itself is JSON lines on standard output")
			}
			tenant, err := a.defaultTenant(cmd, tenant)
			if err != nil {
				return err
			}
			req := &v1.ExportAuditRequest{Scope: optionalScope(tenant), FromSeq: from}
			if cmd.Flags().Changed("to") {
				req.ToSeq = &to
			}
			w := a.env.Stdout
			var file *os.File
			var part string
			if !toStdout {
				if err := absent(out); err != nil {
					return err
				}
				if file, err = createPart(out); err != nil {
					return err
				}
				part = file.Name()
				// Only what this command created is removed: the
				// temporary, whose name no other file had.
				defer func() {
					if file != nil {
						_ = file.Close()
					}
					if part != "" {
						_ = os.Remove(part)
					}
				}()
				w = file
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			sum, err := a.exportTo(ctx, c, req, w)
			if err != nil {
				return err
			}
			if toStdout {
				return nil
			}
			if err := file.Sync(); err != nil {
				return fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
			}
			if err := file.Close(); err != nil {
				return fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
			}
			file = nil
			if err := publish(part, out); err != nil {
				return err
			}
			part = ""
			sum.File, sum.Tenant = out, tenant
			if a.opts.json {
				return a.write(func(w io.Writer) error { return output.JSONValue(w, sum) })
			}
			last := "none: nothing was sealed in the range"
			if sum.LastDigest != "" {
				last = sum.LastDigest + " (keep it outside the installation, for audit verify --expect-digest)"
			}
			return a.write(func(w io.Writer) error {
				return output.Details(w, a.style(), []output.Pair{
					{Label: "File", Value: out},
					{Label: "Log", Value: logText(tenant) + " (" + sum.Log + "), installation " + sum.InstallationID},
					{Label: "Exported", Value: fmt.Sprintf("seq %d to %d: %d entries, %d checkpoints (%d pruned)", sum.FromSeq, sum.ToSeq, sum.Entries, sum.Checkpoints, sum.Pruned)},
					{Label: "Last digest", Value: last},
				})
			})
		}),
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "export this tenant's log (default: the installation's log)")
	cmd.Flags().Int64Var(&from, "from", 0, "the first seq to export; moved back to the start of the checkpoint that covers it (default: the first kept)")
	cmd.Flags().Int64Var(&to, "to", 0, "the last seq to export; the export ends with the checkpoint that covers it (default: the last sealed entry)")
	cmd.Flags().StringVar(&out, "out", "", "write the export to this file, never replacing one (default: standard output)")
	_ = cmd.MarkFlagFilename("out")
	return cmd
}

// absent returns a Precondition error when something is at path, a symbolic
// link included, and an Internal one when whether something is there cannot
// be told: an export never replaces a file, nor guesses that none is there.
func absent(path string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return fault.New(fault.Precondition, fmt.Sprintf("%s exists; an export never replaces a file, so name another or remove it", output.Clean(path)))
	case errors.Is(err, fs.ErrNotExist):
		return nil
	}
	return fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
}

// createPart creates the temporary an export is written to, beside out so
// it can be published by a link or a rename: a new file of a name no other
// file has (os.CreateTemp opens it with O_EXCL, so a symbolic link or a
// file put at a name it picks is never written through or truncated), read
// and written by its owner alone (audit entries name who did what).
func createPart(out string) (*os.File, error) {
	dir, base := filepath.Split(out)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".*.part")
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
	}
	return f, nil
}

// publish gives the written temporary part the name out, never replacing
// what is there: a hard link fails when out exists, at the moment it is
// made, so a file that appeared while the export was written is refused,
// not replaced, and the temporary is removed after. Where a link cannot be
// made (Windows, or a file system without hard links) it renames after
// checking out is absent, which leaves a moment between the check and the
// rename in which a file made at out is replaced.
func publish(part, out string) error {
	appeared := func() error {
		_ = os.Remove(part)
		return fault.New(fault.Precondition, fmt.Sprintf("%s appeared while the export was written; the export is not kept", output.Clean(out)))
	}
	if runtime.GOOS != "windows" {
		err := os.Link(part, out)
		if err == nil {
			_ = os.Remove(part)
			return nil
		}
		if errors.Is(err, fs.ErrExist) {
			return appeared()
		}
	}
	if err := absent(out); err != nil {
		if fault.KindOf(err) == fault.Precondition {
			return appeared()
		}
		_ = os.Remove(part)
		return err
	}
	if err := os.Rename(part, out); err != nil {
		_ = os.Remove(part)
		return fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
	}
	return nil
}

// exportTo streams an export to w, a line a message, and returns what it
// carried. The stream has no deadline of its own, as a download has none: a
// long log takes as long as it takes, and an interrupt ends it. A stream
// that ends before its last message is an error, so a cut export is never
// taken for a whole one.
func (a *app) exportTo(ctx context.Context, c *client.Client, req *v1.ExportAuditRequest, w io.Writer) (exportSummary, error) {
	var sum exportSummary
	st, err := c.AuditSeal.ExportAudit(ctx, req)
	if err != nil {
		return sum, c.StreamError(ctx, "audit export", err)
	}
	defer func() { _ = st.Close() }()
	header := false
	for {
		m, err := st.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sum, c.StreamError(ctx, "audit export", err)
		}
		switch k := m.GetKind().(type) {
		case *v1.ExportAuditResponse_Header:
			header = true
			sum.InstallationID, sum.Log, sum.FromSeq = k.Header.GetInstallationId(), k.Header.GetLog(), k.Header.GetFromSeq()
		case *v1.ExportAuditResponse_Entry:
			sum.Entries++
		case *v1.ExportAuditResponse_Checkpoint:
			sum.Checkpoints++
			if k.Checkpoint.GetPruned() {
				sum.Pruned++
			}
			sum.ToSeq, sum.LastDigest = k.Checkpoint.GetToSeq(), hex.EncodeToString(k.Checkpoint.GetDigest())
		}
		line, err := exportLine(m)
		if err != nil {
			return sum, fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
		}
		if _, err := w.Write(line); err != nil {
			return sum, fault.Wrap(fault.Internal, err, "writing the export: "+err.Error())
		}
	}
	if !header {
		return sum, fault.New(fault.Internal, "audit export: the server ended the export without its header")
	}
	return sum, nil
}
