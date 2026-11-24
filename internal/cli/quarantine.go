package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"maps"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Every name of a quarantine reason and of a quarantine state starts with one
// of these.
const (
	quarantineReasonPrefix = "QUARANTINE_REASON_"
	quarantineStatePrefix  = "QUARANTINE_STATE_"
)

// newQuarantine builds the quarantine noun.
func (a *app) newQuarantine() *cobra.Command {
	return group("quarantine", "List and show quarantined files, and release or reject them",
		`A file the server received and could not archive as it is (not DICOM, a label
it cannot make, a different file under a SOP Instance UID the tenant holds, a
storage that refused it, and the other reasons) is held, with a record of why,
when, from whom and in which reception. It is never archived as it is and never
discarded on its own: it stays until a person releases it, so it runs through
the pipeline again, or rejects it, so its bytes are removed and its record
stays. Every command takes the tenant first. Listing and showing need the
reader role on the tenant (filtering by a UID, and the header, need operator);
releasing and rejecting need operator.`,
		a.newQuarantineList(), a.newQuarantineShow(), a.newQuarantineRelease(), a.newQuarantineReject())
}

// quarantineDetails lists one quarantined file's record as labeled values.
func (a *app) quarantineDetails(f *v1.QuarantinedFile) []output.Pair {
	keep := "no"
	if f.GetContentKept() {
		keep = "yes"
	}
	pairs := []output.Pair{
		{Label: "ID", Value: f.GetId()},
		{Label: "State", Value: output.QuarantineState(f.GetState())},
		{Label: "Reason", Value: output.QuarantineReason(f.GetReason())},
	}
	for _, k := range slices.Sorted(maps.Keys(f.GetDetails())) {
		pairs = append(pairs, output.Pair{Label: "  " + k, Value: f.GetDetails()[k]})
	}
	cl := f.GetCaller()
	pairs = append(pairs,
		output.Pair{Label: "Receiver", Value: f.GetReceiver()},
		output.Pair{Label: "Reception", Value: output.Dash(f.GetReception())},
		output.Pair{Label: "Calling AE title", Value: output.Dash(cl.GetCallingAeTitle())},
		output.Pair{Label: "Called AE title", Value: output.Dash(cl.GetCalledAeTitle())},
		output.Pair{Label: "Address", Value: output.Dash(cl.GetAddress())},
		output.Pair{Label: "Size", Value: strconv.FormatInt(f.GetSizeBytes(), 10) + " bytes"},
		output.Pair{Label: "SHA-256", Value: output.Dash(f.GetSha256())})
	if f.GetKeptSha256() != "" {
		pairs = append(pairs, output.Pair{Label: "Kept SHA-256", Value: f.GetKeptSha256()})
	}
	pairs = append(pairs,
		output.Pair{Label: "SOP class", Value: output.Dash(f.GetSopClassUid())},
		output.Pair{Label: "SOP instance", Value: output.Dash(f.GetSopInstanceUid())},
		output.Pair{Label: "Study", Value: output.Dash(f.GetStudyInstanceUid())},
		output.Pair{Label: "Series", Value: output.Dash(f.GetSeriesInstanceUid())},
		output.Pair{Label: "Transfer syntax", Value: output.Dash(f.GetTransferSyntaxUid())},
		output.Pair{Label: "Bytes held", Value: keep},
		output.Pair{Label: "Quarantined", Value: a.when(f.GetQuarantineTime())},
		output.Pair{Label: "Decided", Value: a.whenOr(f.GetDecideTime(), "-")})
	if f.GetState() == v1.QuarantineState_QUARANTINE_STATE_RELEASED {
		pairs = append(pairs,
			output.Pair{Label: "Released at", Value: f.GetReleaseReceiver()},
			output.Pair{Label: "Released into", Value: output.Dash(f.GetReleaseReception())})
		if f.GetReleaseStorage() != "" {
			pairs = append(pairs, output.Pair{Label: "Release storage", Value: f.GetReleaseStorage()})
		}
		if pv := f.GetReleaseProfile(); pv != nil {
			pairs = append(pairs, output.Pair{Label: "Release profile", Value: pv.GetName() + "@" + strconv.Itoa(int(pv.GetVersion()))})
		}
		if f.GetReleaseReason() != "" {
			pairs = append(pairs, output.Pair{Label: "Release reason", Value: f.GetReleaseReason()})
		}
	}
	if f.GetRejectReason() != "" {
		pairs = append(pairs, output.Pair{Label: "Reject reason", Value: f.GetRejectReason()})
	}
	if f.GetReleasedFrom() != "" {
		pairs = append(pairs, output.Pair{Label: "Released from", Value: f.GetReleasedFrom()})
	}
	return pairs
}

// newQuarantineList builds "quarantine list".
func (a *app) newQuarantineList() *cobra.Command {
	var state, reason, receiver, reception, since, until, study, sop string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant> [--state STATE] [--reason REASON] [--receiver NAME] [--reception ID] [--since TIME] [--until TIME]",
		Short: "List a tenant's quarantined files, newest first",
		Long: `List a tenant's quarantined files, newest first by the time they were
quarantined, optionally only those in one state, for one reason, at one
receiver, in one reception, or quarantined in a span of time. A review asks for
--state held. Every filter given must hold.

A STATE is one of: ` + strings.Join(enumNames(v1.QuarantineState_name, quarantineStatePrefix), ", ") + `.
A REASON is one of: ` + strings.Join(enumNames(v1.QuarantineReason_name, quarantineReasonPrefix), ", ") + `.
A TIME is a date (2026-11-17, midnight in your zone), a time with its offset
(2026-11-17T09:30:00+03:00) or an age (24h, 7d: that long before now). --since
is inclusive and --until is exclusive.

--study-uid and --sop-uid look a file up by an identifier already in hand, which
is a search for a person's data: they need the operator role and are recorded in
the audit log as present, not as their value.`,
		Example: `  nopacs quarantine list lab1 --state held
  nopacs quarantine list lab1 --reason bad-label --since 7d --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			req := &v1.ListQuarantinedFilesRequest{Tenant: args[0], Receiver: receiver, Reception: reception, StudyInstanceUid: study, SopInstanceUid: sop}
			var err error
			if state != "" {
				if req.State, err = parseEnum[v1.QuarantineState]("state", state, v1.QuarantineState_name, quarantineStatePrefix); err != nil {
					return err
				}
			}
			if reason != "" {
				if req.Reason, err = parseEnum[v1.QuarantineReason]("reason", reason, v1.QuarantineReason_name, quarantineReasonPrefix); err != nil {
					return err
				}
			}
			if req.StartTime, req.EndTime, err = a.timeRange(since, until); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			files, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.QuarantinedFile, string, error) {
				page := &v1.ListQuarantinedFilesRequest{
					Tenant: req.GetTenant(), State: req.GetState(), Reason: req.GetReason(), Receiver: req.GetReceiver(), Reception: req.GetReception(),
					StartTime: req.GetStartTime(), EndTime: req.GetEndTime(), StudyInstanceUid: req.GetStudyInstanceUid(), SopInstanceUid: req.GetSopInstanceUid(),
					PageSize: size, PageToken: token,
				}
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListQuarantinedFilesResponse, error) {
					return c.Quarantine.ListQuarantinedFiles(ctx, page)
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetFiles(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "files", files, more, func() error {
				rows := make([][]string, 0, len(files))
				for _, f := range files {
					rows = append(rows, []string{
						f.GetId(), output.QuarantineState(f.GetState()), output.QuarantineReason(f.GetReason()), f.GetReceiver(),
						output.Dash(f.GetReception()), strconv.FormatInt(f.GetSizeBytes(), 10), a.when(f.GetQuarantineTime()),
					})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"ID", "STATE", "REASON", "RECEIVER", "RECEPTION", "BYTES", "QUARANTINED"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&state, "state", "", "only the files in this state")
	cmd.Flags().StringVar(&reason, "reason", "", "only the files quarantined for this reason")
	cmd.Flags().StringVar(&receiver, "receiver", "", "only the files that arrived at the receiver of this name")
	cmd.Flags().StringVar(&reception, "reception", "", "only the files of the reception with this id")
	cmd.Flags().StringVar(&since, "since", "", "only files quarantined at or after this time: a date, a time with its offset, or an age such as 24h")
	cmd.Flags().StringVar(&until, "until", "", "only files quarantined before this time, written like --since")
	cmd.Flags().StringVar(&study, "study-uid", "", "only the files of this Study Instance UID, as received (needs the operator role)")
	cmd.Flags().StringVar(&sop, "sop-uid", "", "only the files of this SOP Instance UID, as received (needs the operator role)")
	enumCompletion(cmd, "state", v1.QuarantineState_name, quarantineStatePrefix)
	enumCompletion(cmd, "reason", v1.QuarantineReason_name, quarantineReasonPrefix)
	l.add(cmd)
	return cmd
}

// newQuarantineShow builds "quarantine show".
func (a *app) newQuarantineShow() *cobra.Command {
	var header bool
	cmd := &cobra.Command{
		Use:   "show <tenant> <id> [--header]",
		Short: "Show one quarantined file's record, and its DICOM header on request",
		Long: `Show the record of one quarantined file: why it was quarantined, with the
details of that reason, which receiver and reception it belonged to, who sent
it, its size and checksums, the identifiers read from it, whether the server
still holds its bytes, and what was decided about it.

--header also shows the file's data set in the DICOM JSON model (PS3.18 Annex F),
without pixel data or any value longer than 1 KiB. It holds what the sender sent,
identifying data included, so it needs the operator role and is shown only when
asked for. It is left out when the file's bytes are gone, its data set cannot be
read, or it is longer than 1 MiB; the output says which.`,
		Example: `  nopacs quarantine show lab1 q_41Ab
  nopacs quarantine show lab1 q_41Ab --header --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetQuarantinedFileResponse, error) {
				return c.Quarantine.GetQuarantinedFile(ctx, &v1.GetQuarantinedFileRequest{Tenant: args[0], Id: args[1], IncludeHeader: header})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				if err := output.Details(w, a.style(), a.quarantineDetails(res.GetFile())); err != nil {
					return err
				}
				if !header {
					return nil
				}
				var text string
				switch {
				case res.GetHeaderJson() != "":
					text = res.GetHeaderJson()
				case res.GetHeaderTooLarge():
					text = "(not shown: longer than 1 MiB)"
				default:
					text = "(not available: the file's bytes are gone or its data set could not be read)"
				}
				_, err := fmt.Fprintf(w, "\nHeader:\n%s\n", output.Clean(text))
				return err
			})
		}),
	}
	cmd.Flags().BoolVar(&header, "header", false, "also show the file's DICOM header as JSON, without bulk data (needs the operator role)")
	return cmd
}

// newQuarantineRelease builds "quarantine release".
func (a *app) newQuarantineRelease() *cobra.Command {
	var receiver, reason, storage, profileArg string
	cmd := &cobra.Command{
		Use:   "release <tenant> <id> [--receiver NAME] [--storage NAME] [--profile NAME@VERSION] [--reason TEXT]",
		Short: "Run a held file through the pipeline again",
		Long: `Run a held file through the pipeline again, as if it arrived now at the
receiver it first came to, or at the one --receiver names, which is needed when
that receiver has been removed. It joins that receiver's reception of its study
that is still receiving, or starts one, and the answer names it. Use it after
the cause was mended, such as a naming rule that was fixed: a file whose cause
remains is quarantined again under a new record, and this one stays released.

The output says why the file had been quarantined. A file quarantined as a
different file under a SOP Instance UID the tenant holds can only be rejected,
and so can a file whose bytes were never kept (exit 5). A file already released
is answered as it stands, so a repeated command releases nothing more, and it
keeps the reason of its first release.

--reason says why the file is released, in words for whoever reads the record
later, as a rejection's reason does. It is optional, and when given it is 1 to
1024 bytes, not only spaces. "quarantine show" prints it.

--storage names the storage the study is archived to, in place of the storage
the receiver's routes choose; the routes still choose the profile and the
senders. A file held as unrouted is released so. A study whose session is
already on a storage stays there.

--profile names the profile version the reception the file joins is
de-identified with, in place of the one its route or receiver names: a file the
receiver's version could not process is released through a fixed version. It
needs admin on the tenant; the version must be one the tenant has and pass the
checks a receiver's version passes, and the receiver must de-identify (exit 5
otherwise). Try the version on the held file first ("profile try --record").

A file held at a receiver that de-identifies is released only through a
receiver that de-identifies (exit 5): it is never archived as received.`,
		Example: `  nopacs quarantine release lab1 q_41Ab
  nopacs quarantine release lab1 q_41Ab --receiver scanner2 --reason "naming rule fixed"
  nopacs quarantine release lab1 q_7Zr2 --storage lab-a --reason "study from lab A, sent from a new AE title"
  nopacs quarantine release lab1 q_9Kc4 --profile research@2 --reason "version 2 removes the scanner's private tags"`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("reason") {
				if err := checkReason(reason); err != nil {
					return err
				}
			}
			var version *v1.ProfileVersionRef
			if cmd.Flags().Changed("profile") {
				ref, err := profile.ParseRef(profileArg)
				if err != nil || ref.Version == 0 {
					return fault.New(fault.Invalid, fmt.Sprintf("--profile %q is not NAME@VERSION, such as research@3: a release names the exact version it runs", output.Clean(profileArg)))
				}
				version = &v1.ProfileVersionRef{Name: ref.Name, Version: ref.Version}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ReleaseQuarantinedFileResponse, error) {
				return c.Quarantine.ReleaseQuarantinedFile(ctx, &v1.ReleaseQuarantinedFileRequest{
					Tenant: args[0], Id: args[1], Mode: v1.ReleaseMode_RELEASE_MODE_AS_IS, Receiver: receiver, Reason: reason, Storage: storage,
					Profile: version,
				})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				f := res.GetFile()
				_, err := fmt.Fprintf(w, "file %s released (it was quarantined: %s) into reception %s at receiver %s\n",
					output.Clean(f.GetId()), output.QuarantineReason(f.GetReason()), output.Clean(output.Dash(f.GetReleaseReception())), output.Clean(output.Dash(f.GetReleaseReceiver())))
				return err
			})
		}),
	}
	cmd.Flags().StringVar(&receiver, "receiver", "", "the receiver whose storage and naming rule the file runs by (default: the receiver it first came to)")
	cmd.Flags().StringVar(&reason, "reason", "", "why the file is released, kept with its record (1 to 1024 bytes, not only spaces)")
	cmd.Flags().StringVar(&storage, "storage", "", "the storage the study is archived to, in place of what the receiver's routes choose")
	cmd.Flags().StringVar(&profileArg, "profile", "", "the profile version, NAME@VERSION, the file is de-identified with, in place of its route's or receiver's (admin); it applies to the whole reception the file joins, files still arriving included")
	return cmd
}

// newQuarantineReject builds "quarantine reject".
func (a *app) newQuarantineReject() *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "reject <tenant> <id> --reason TEXT [--yes]",
		Short: "Remove a held file's bytes for good, and keep its record",
		Long: `Remove a held file's bytes from the server, for good, and keep its record as
rejected, with the reason given, in words for whoever reads the record later.
Nothing on a storage is touched. A file already rejected is answered as it
stands, with its first reason, so a repeated command removes nothing more. A
file that was released cannot be rejected (exit 5).

This cannot be undone. Without --yes the command asks for the id to be typed
again on a terminal; with no terminal it sends nothing, says what it would do
and exits 2.`,
		Example: `  nopacs quarantine reject lab1 q_41Ab --reason "not a study of this project"
  nopacs quarantine reject lab1 q_41Ab --reason "duplicate upload" --yes`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			if err := a.confirmTyped(yes, args[1], "rejected", fmt.Sprintf("remove for good the bytes of quarantined file %q; its record stays", output.Clean(args[1]))); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RejectQuarantinedFileResponse, error) {
				return c.Quarantine.RejectQuarantinedFile(ctx, &v1.RejectQuarantinedFileRequest{Tenant: args[0], Id: args[1], Reason: reason})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				f := res.GetFile()
				_, err := fmt.Fprintf(w, "file %s rejected: %s; its bytes were removed and its record stays\n", output.Clean(f.GetId()), output.Clean(f.GetRejectReason()))
				return err
			})
		}),
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the file is rejected, in words for whoever reads the record later (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for the id to be typed again")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
