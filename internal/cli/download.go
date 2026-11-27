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
	"github.com/amrshadid/nopacs/internal/fault"
)

// newDownload builds "download".
func (a *app) newDownload() *cobra.Command {
	var to, rate, layout, progress string
	var quiet bool
	var parallel int
	var set downloadSet
	cmd := &cobra.Command{
		Use:   "download <tenant> [<subject>[/<session>[/<scan>]]...] --to <dir> [--refs-file F | scan filters] [--parallel N] [--limit-rate R] [--selection F] [--layout bagit]",
		Short: "Download chosen subjects, sessions or scans, checked as they arrive",
		Long: `Download the DICOM files of a chosen set, each as it was archived, byte for byte.

The set is any mix of: references after the tenant (a subject, one session of
it, or one scan), a file of references (--refs-file, one per line, - for
standard input), the scans a filter selects (--modality, --description-prefix,
--receiver, --archived-since, --archived-until: the filters of catalog scans,
over the whole tenant), or, to resume, the references a --selection file from
an earlier run holds. A file that two references name is fetched once.

Layout: every file is written to
<dir>/<subject>/<session>/<scan>/<instance id>.dcm, under the labels the
archive holds, so two scans never share a directory. Directories are made
private to you (0700) and files too (0600), since they hold what the senders
sent. With --layout bagit the directory is a BagIt bag (RFC 8493): the files
under data/, bagit.txt, bag-info.txt, manifest-sha256.txt (the SHA-256 the
server announced and this command checked) and tagmanifest-sha256.txt, written
when every file of the set is whole. Any BagIt tool can validate it.

Checking: the server announces each file with its length and SHA-256 before its
bytes, and the file is checked against them as it arrives. A file is written as
<name>.part and renamed only when it is whole and correct. A file that does not
match is removed, named on standard error, and the command exits 1 after the
rest are done.

Parallel: --parallel streams (default 4, at most 16) run at once, each a run of
consecutive files of one scan. The server refuses a caller past its own bound
(the setting read.per-caller) instead of queueing it; this command then waits,
with growing pauses, holds fewer streams, and tries one more after a quiet
stretch, so it settles at what the server allows instead of failing.
--limit-rate bounds the bytes per second of all streams together, such as
10MiB.

Resuming: run the same command again after an interruption. Files already
complete (same length and SHA-256) are skipped without being fetched again; a
.part is continued with an HTTP range request guarded by the file's checksum
as its validator, so it can never join the bytes of two versions. With
--selection FILE, the files that are whole are recorded in FILE (replaced
atomically as the download goes), and a rerun skips each of them without
reading it again when it is still as recorded; a file that differs is read and
checked as before, so the record only saves time.

Progress is one overall line (files, bytes, rate, time left) on standard error
on a terminal; --quiet turns it off. --progress json writes one JSON document
per line to standard error instead, for a program that drives the command (see
the command line page of the documentation for its fields). The result is on
standard output. Needs the reader role on the tenant.`,
		Example: `  nopacs download lab1 sub-3f9a --to ./out
  nopacs download lab1 sub-3f9a/ses-81c2/scan-07d4 --to ./out --quiet
  nopacs download lab1 --refs-file scans.txt --to ./out --parallel 8 --selection ./out.selection.json
  nopacs download lab1 --modality MR --archived-since 30d --to ./mr --limit-rate 50MiB
  nopacs download lab1 sub-3f9a --to ./bag --layout bagit --progress json`,
		Args: rangeArgs(1, 100000),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(to) == "" {
				return fault.New(fault.Invalid, "--to must name the directory to download into")
			}
			if parallel < 1 || parallel > download.MaxParallel {
				return fault.New(fault.Invalid, fmt.Sprintf("--parallel %d is not from 1 to %d", parallel, download.MaxParallel))
			}
			var limit int64
			if rate != "" {
				var err error
				if limit, err = parseSize("limit-rate", strings.TrimSuffix(rate, "/s")); err != nil {
					return err
				}
				if limit < 1 {
					return fault.New(fault.Invalid, "--limit-rate must be at least 1 byte per second")
				}
			}
			lay, err := downloadLayout(layout)
			if err != nil {
				return err
			}
			rep, err := a.downloadReporter(progress, quiet)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			refs, err := a.downloadRefs(ctx, cmd, c, args[0], args[1:], &set)
			if err != nil {
				return err
			}
			res, err := download.Run(ctx, download.Options{
				Client: c, Tenant: args[0], Refs: refs, Dir: to,
				Parallel: parallel, LimitRate: limit, SelectionPath: set.selection, Layout: lay,
				OnProgress: rep.show, OnFailure: rep.failure, OnThrottle: rep.throttled,
			})
			rep.end()
			if err != nil {
				return err
			}
			return a.downloadResult(args[0], to, set.selection, lay, res)
		}),
	}
	cmd.Flags().StringVar(&to, "to", "", "the directory to download into (made when missing)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "show no progress on standard error (the same as --progress none)")
	cmd.Flags().IntVar(&parallel, "parallel", download.DefaultParallel, fmt.Sprintf("how many streams to hold open at once, from 1 to %d; lowered by itself when the server refuses one", download.MaxParallel))
	cmd.Flags().StringVar(&rate, "limit-rate", "", "the most bytes per second all streams receive together, such as 10MiB (default: no bound)")
	cmd.Flags().StringVar(&layout, "layout", "plain", "plain: <subject>/<session>/<scan>/<id>.dcm under --to; bagit: a BagIt bag (RFC 8493) in --to")
	cmd.Flags().StringVar(&progress, "progress", "auto", "auto (a line on a terminal), text, json (one JSON document per line on standard error) or none")
	_ = cmd.RegisterFlagCompletionFunc("layout", cobra.FixedCompletions([]string{"plain", "bagit"}, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("progress", cobra.FixedCompletions([]string{"auto", "text", "json", "none"}, cobra.ShellCompDirectiveNoFileComp))
	set.add(cmd)
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// downloadLayout turns the --layout value into the package's.
func downloadLayout(s string) (string, error) {
	switch s {
	case "", "plain":
		return "", nil
	case download.LayoutBagIt:
		return download.LayoutBagIt, nil
	}
	return "", fault.New(fault.Invalid, fmt.Sprintf("--layout %q is not one of: plain, bagit", output.Clean(s)))
}

// downloadReporter picks how progress is shown from --progress and --quiet.
func (a *app) downloadReporter(mode string, quiet bool) (downloadReporter, error) {
	switch mode {
	case "auto", "text", "json", "none":
	default:
		return nil, fault.New(fault.Invalid, fmt.Sprintf("--progress %q is not one of: auto, text, json, none", output.Clean(mode)))
	}
	switch {
	case quiet || mode == "none":
		return silentReporter{w: a.env.Stderr}, nil
	case mode == "json":
		return &jsonLines{w: a.env.Stderr}, nil
	case mode == "text" || output.IsTerminal(a.env.Stderr):
		return &progressLine{w: a.env.Stderr}, nil
	}
	return silentReporter{w: a.env.Stderr}, nil
}

// downloadJSON is the document of "download --json".
type downloadJSON struct {
	Tenant    string   `json:"tenant"`
	Directory string   `json:"directory"`
	Files     int64    `json:"files"`
	Bytes     int64    `json:"bytes"`
	Skipped   int64    `json:"skipped"`
	Resumed   int64    `json:"resumed"`
	Total     int64    `json:"total"`
	Failed    []string `json:"failed"`
	// Selection is the selection file written, when one was named.
	Selection string `json:"selection,omitempty"`
	// Layout is "bagit" when a bag was written.
	Layout string `json:"layout,omitempty"`
	// Seconds is how long the download took.
	Seconds float64 `json:"seconds"`
}

// downloadResult writes what the download did, and fails it when a file did
// not match what the server announced.
func (a *app) downloadResult(tenant, dir, selection, layout string, r *download.Result) error {
	failed := r.Failures
	if failed == nil {
		failed = []string{}
	}
	if err := a.write(func(w io.Writer) error {
		if a.opts.json {
			return output.JSONValue(w, downloadJSON{Tenant: tenant, Directory: dir, Files: r.Files, Bytes: r.Bytes, Skipped: r.Skipped, Resumed: r.Resumed, Total: r.Total, Failed: failed, Selection: selection, Layout: layout, Seconds: roundSeconds(r.Elapsed)})
		}
		_, err := fmt.Fprintf(w, "downloaded %d files (%s) to %s; %d already complete; %d failed", r.Files, output.Human(r.Bytes), output.Clean(dir), r.Skipped, len(failed))
		if r.Bag != "" {
			_, err = fmt.Fprint(w, "; BagIt bag written")
		}
		if err == nil {
			_, err = fmt.Fprintln(w)
		}
		return err
	}); err != nil {
		return err
	}
	if len(r.Failures) > 0 {
		return fault.New(fault.DataLoss, fmt.Sprintf("%d files did not match their announced SHA-256 or length and were removed: %s", len(r.Failures), output.Clean(strings.Join(r.Failures, "; "))))
	}
	return nil
}

// roundSeconds is a duration in seconds to the millisecond.
func roundSeconds(d time.Duration) float64 {
	return float64(d.Round(time.Millisecond)) / float64(time.Second)
}
