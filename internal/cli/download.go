package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/cli/download"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newDownload builds "download".
func (a *app) newDownload() *cobra.Command {
	var to string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "download <tenant> <subject>[/<session>[/<scan>]] --to <dir> [--quiet]",
		Short: "Download a subject's, a session's or a scan's files, checked as they arrive",
		Long: `Download the DICOM files of a subject, one session of it, or one scan, each as
it was archived, byte for byte.

Layout: every file is written to
<dir>/<subject>/<session>/<scan>/<instance id>.dcm, under the labels the
archive holds, so two scans never share a directory. Directories are made
private to you (0700) and files too (0600), since they hold what the senders
sent.

Checking: the server announces each file with its length and SHA-256 before its
bytes, and the file is checked against them as it arrives. A file is written as
<name>.part and renamed only when it is whole and correct. A file that does not
match is removed, named on standard error, and the command exits 1 after the
rest are done.

Resuming: run the same command again after an interruption. Files already
complete (same length and SHA-256) are skipped without being fetched again; a
.part is continued with an HTTP range request guarded by the file's checksum
as its validator, so it can never join the bytes of two versions, and the
stream goes on after the last complete file.

Progress is shown on standard error on a terminal; --quiet turns it off. The
result is on standard output. Needs the reader role on the tenant.`,
		Example: `  nopacs download lab1 sub-3f9a --to ./out
  nopacs download lab1 sub-3f9a/ses-81c2/scan-07d4 --to ./out --quiet`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			ref, err := splitRef(args[1], 1, 3, "a subject, session or scan: use <subject>[/<session>[/<scan>]]")
			if err != nil {
				return err
			}
			for _, p := range ref {
				if err := label.Check(p); err != nil {
					return fault.New(fault.Invalid, fmt.Sprintf("%q is not a label: %s", output.Clean(p), err))
				}
			}
			if strings.TrimSpace(to) == "" {
				return fault.New(fault.Invalid, "--to must name the directory to download into")
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			line := &progressLine{w: a.env.Stderr, on: !quiet && output.IsTerminal(a.env.Stderr)}
			res, err := download.Run(ctx, download.Options{
				Client: c, Tenant: args[0], Ref: ref, Dir: to,
				OnProgress: line.show,
				OnFailure: func(msg string) {
					line.end()
					_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: file failed its check and was removed: %s\n", output.Clean(msg))
				},
			})
			line.end()
			if err != nil {
				return err
			}
			return a.downloadResult(args[0], to, res)
		}),
	}
	cmd.Flags().StringVar(&to, "to", "", "the directory to download into (made when missing)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "show no progress on standard error")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// progressLine draws the progress on one line of a terminal, at most four
// times a second.
type progressLine struct {
	w     io.Writer
	on    bool
	last  time.Time
	shown bool
}

// show draws the progress.
func (p *progressLine) show(pr download.Progress) {
	if !p.on || time.Since(p.last) < 250*time.Millisecond {
		return
	}
	p.last, p.shown = time.Now(), true
	_, _ = fmt.Fprintf(p.w, "\r%d files done, %s received", pr.Files+pr.Skipped, output.Human(pr.Bytes))
}

// end finishes the line, so the next one starts clean.
func (p *progressLine) end() {
	if p.shown {
		_, _ = fmt.Fprintln(p.w)
		p.shown = false
	}
}

// downloadJSON is the document of "download --json".
type downloadJSON struct {
	Tenant    string   `json:"tenant"`
	Directory string   `json:"directory"`
	Files     int64    `json:"files"`
	Bytes     int64    `json:"bytes"`
	Skipped   int64    `json:"skipped"`
	Resumed   int64    `json:"resumed"`
	Failed    []string `json:"failed"`
}

// downloadResult writes what the download did, and fails it when a file did
// not match what the server announced.
func (a *app) downloadResult(tenant, dir string, r *download.Result) error {
	failed := r.Failures
	if failed == nil {
		failed = []string{}
	}
	if err := a.write(func(w io.Writer) error {
		if a.opts.json {
			return output.JSONValue(w, downloadJSON{Tenant: tenant, Directory: dir, Files: r.Files, Bytes: r.Bytes, Skipped: r.Skipped, Resumed: r.Resumed, Failed: failed})
		}
		_, err := fmt.Fprintf(w, "downloaded %d files (%s) to %s; %d already complete; %d failed\n", r.Files, output.Human(r.Bytes), output.Clean(dir), r.Skipped, len(failed))
		return err
	}); err != nil {
		return err
	}
	if len(r.Failures) > 0 {
		return fault.New(fault.DataLoss, fmt.Sprintf("%d files did not match their announced SHA-256 or length and were removed: %s", len(r.Failures), output.Clean(strings.Join(r.Failures, "; "))))
	}
	return nil
}
