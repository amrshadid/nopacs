package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// receptionStepPrefix is what every name of a reception step starts with.
const receptionStepPrefix = "RECEPTION_STEP_"

// newReception builds the reception noun.
func (a *app) newReception() *cobra.Command {
	return group("reception", "List and show receptions, and retry or discard the ones that failed",
		`A reception is the files of one study arriving at one receiver, from its
first file until it is complete, then carried through its steps until every file
is archived and verified, ignored as a duplicate, or quarantined. The server
makes receptions; no command does. A reception that failed keeps its files in
the server's incoming area until they are retried or discarded. Every command
takes the tenant first. Listing and showing need the reader role on the tenant
(filtering by study needs operator); retrying and discarding need operator.`,
		a.newReceptionList(), a.newReceptionShow(), a.newReceptionRetry(), a.newReceptionDiscard())
}

// filesText words a count of files and their bytes.
func filesText(files, bytes int64) string {
	return fmt.Sprintf("%d files, %d bytes", files, bytes)
}

// receptionDetails lists one reception as labeled values.
func (a *app) receptionDetails(r *v1.Reception) []output.Pair {
	cn := r.GetCounts()
	pairs := []output.Pair{
		{Label: "ID", Value: r.GetId()},
		{Label: "Receiver", Value: r.GetReceiver()},
		{Label: "Study", Value: r.GetStudyInstanceUid()},
		{Label: "Arrival", Value: strconv.Itoa(int(r.GetArrival()))},
		{Label: "Step", Value: output.ReceptionStep(r.GetStep())},
	}
	if r.GetFailure() != "" {
		pairs = append(pairs, output.Pair{Label: "Failure", Value: r.GetFailure()})
	}
	pairs = append(pairs,
		output.Pair{Label: "Received", Value: filesText(cn.GetFilesReceived(), cn.GetBytesReceived())},
		output.Pair{Label: "Archived", Value: filesText(cn.GetFilesArchived(), cn.GetBytesArchived())},
		output.Pair{Label: "Duplicate", Value: filesText(cn.GetFilesDuplicate(), cn.GetBytesDuplicate())},
		output.Pair{Label: "Quarantined", Value: filesText(cn.GetFilesQuarantined(), cn.GetBytesQuarantined())},
		output.Pair{Label: "Released in", Value: strconv.FormatInt(cn.GetFilesReleased(), 10) + " files"},
		output.Pair{Label: "Subject", Value: output.Dash(r.GetSubjectLabel())},
		output.Pair{Label: "Session", Value: output.Dash(r.GetSessionLabel())},
		output.Pair{Label: "Storage", Value: output.Dash(r.GetStorage())},
		output.Pair{Label: "Associations", Value: strconv.FormatInt(r.GetAssociations(), 10)})
	titles := strings.Join(r.GetCallingAeTitles(), ", ")
	if r.GetCallingAeTitlesTruncated() {
		titles += ", and others"
	}
	pairs = append(pairs,
		output.Pair{Label: "Calling AE titles", Value: output.Dash(titles)},
		output.Pair{Label: "Started", Value: a.when(r.GetStartTime())},
		output.Pair{Label: "Last file", Value: a.whenOr(r.GetLastFileTime(), "-")},
		output.Pair{Label: "Step since", Value: a.whenOr(r.GetStepTime(), "-")},
		output.Pair{Label: "Ended", Value: a.whenOr(r.GetEndTime(), "-")})
	if r.GetRetryOf() != "" {
		pairs = append(pairs, output.Pair{Label: "Retry of", Value: r.GetRetryOf()})
	}
	if r.GetRetryReception() != "" {
		pairs = append(pairs, output.Pair{Label: "Retried as", Value: r.GetRetryReception()})
	}
	if r.GetDiscardTime() != nil {
		pairs = append(pairs,
			output.Pair{Label: "Discarded", Value: a.when(r.GetDiscardTime())},
			output.Pair{Label: "Discard reason", Value: r.GetDiscardReason()})
	}
	return pairs
}

// newReceptionList builds "reception list".
func (a *app) newReceptionList() *cobra.Command {
	var receiver, study, since, until string
	var steps []string
	var inProgress bool
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant> [--receiver NAME] [--step STEP]... [--in-progress] [--since TIME] [--until TIME]",
		Short: "List a tenant's receptions, newest first",
		Long: `List a tenant's receptions, newest first by the time of their first file,
optionally only those at one receiver, in one or more steps, still in progress,
or whose first file arrived in a span of time. Every filter given must hold.

A STEP is one of: ` + strings.Join(enumNames(v1.ReceptionStep_name, receptionStepPrefix), ", ") + `.
A TIME is a date (2026-11-17, midnight in your zone), a time with its offset
(2026-11-17T09:30:00+03:00) or an age (24h, 7d: that long before now). --since
is inclusive and --until is exclusive.

--study-uid lists the receptions of one Study Instance UID, as received. Looking
a study up by an identifier already in hand is a search for a person's data, so
it needs the operator role and is recorded in the audit log as present, not as
its value.`,
		Example: `  nopacs reception list lab1
  nopacs reception list lab1 --step failed
  nopacs reception list lab1 --in-progress --receiver scanner1 --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			req := &v1.ListReceptionsRequest{Tenant: args[0], Receiver: receiver, StudyInstanceUid: study, InProgress: inProgress}
			for _, s := range steps {
				st, err := parseEnum[v1.ReceptionStep]("step", s, v1.ReceptionStep_name, receptionStepPrefix)
				if err != nil {
					return err
				}
				req.Steps = append(req.Steps, st)
			}
			var err error
			if req.StartTime, req.EndTime, err = a.timeRange(since, until); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			receptions, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Reception, string, error) {
				page := &v1.ListReceptionsRequest{
					Tenant: req.GetTenant(), Receiver: req.GetReceiver(), StudyInstanceUid: req.GetStudyInstanceUid(), Steps: req.GetSteps(),
					InProgress: req.GetInProgress(), StartTime: req.GetStartTime(), EndTime: req.GetEndTime(), PageSize: size, PageToken: token,
				}
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListReceptionsResponse, error) {
					return c.Receptions.ListReceptions(ctx, page)
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetReceptions(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "receptions", receptions, more, func() error {
				rows := make([][]string, 0, len(receptions))
				for _, r := range receptions {
					cn := r.GetCounts()
					rows = append(rows, []string{
						r.GetId(), r.GetReceiver(), output.ReceptionStep(r.GetStep()),
						strconv.FormatInt(cn.GetFilesReceived(), 10), strconv.FormatInt(cn.GetFilesArchived(), 10), strconv.FormatInt(cn.GetFilesQuarantined(), 10),
						a.when(r.GetStartTime()), output.Dash(r.GetSessionLabel()),
					})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"ID", "RECEIVER", "STEP", "RECEIVED", "ARCHIVED", "QUARANTINED", "STARTED", "SESSION"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&receiver, "receiver", "", "only the receptions that arrived at the receiver of this name")
	cmd.Flags().StringVar(&study, "study-uid", "", "only the receptions of this Study Instance UID, as received (needs the operator role)")
	cmd.Flags().StringArrayVar(&steps, "step", nil, "only the receptions in this step; repeat to include more steps")
	cmd.Flags().BoolVar(&inProgress, "in-progress", false, "only the receptions that have not ended")
	cmd.Flags().StringVar(&since, "since", "", "only receptions whose first file arrived at or after this time: a date, a time with its offset, or an age such as 24h")
	cmd.Flags().StringVar(&until, "until", "", "only receptions whose first file arrived before this time, written like --since")
	enumCompletion(cmd, "step", v1.ReceptionStep_name, receptionStepPrefix)
	l.add(cmd)
	return cmd
}

// newReceptionShow builds "reception show".
func (a *app) newReceptionShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <tenant> <id>",
		Short: "Show one reception, the scans it archived and the other arrivals of its study",
		Long: `Show one reception: its step and why it failed when it did, what was received,
archived, ignored as a duplicate and quarantined, the subject and session it
belongs to, the callers that brought it, the scans it archived into, and every
other reception of the same study in the tenant, in the order of their arrival.`,
		Example: `  nopacs reception show lab1 rcp_8Qm2`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetReceptionResponse, error) {
				return c.Receptions.GetReception(ctx, &v1.GetReceptionRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				if err := output.Details(w, a.style(), a.receptionDetails(res.GetReception())); err != nil {
					return err
				}
				if len(res.GetScans()) > 0 {
					rows := make([][]string, 0, len(res.GetScans()))
					for _, s := range res.GetScans() {
						rows = append(rows, []string{s.GetLabel(), s.GetSeriesInstanceUid(), strconv.FormatInt(s.GetFiles(), 10), strconv.FormatInt(s.GetBytes(), 10)})
					}
					if _, err := fmt.Fprintln(w, "\nScans:"); err != nil {
						return err
					}
					if err := output.Table(w, a.style(), []string{"LABEL", "SERIES", "FILES", "BYTES"}, rows); err != nil {
						return err
					}
					if res.GetScansTruncated() {
						if _, err := fmt.Fprintln(w, "(only the first scans are listed)"); err != nil {
							return err
						}
					}
				}
				if len(res.GetArrivals()) > 0 {
					rows := make([][]string, 0, len(res.GetArrivals()))
					for _, r := range res.GetArrivals() {
						rows = append(rows, []string{strconv.Itoa(int(r.GetArrival())), r.GetId(), output.ReceptionStep(r.GetStep()), a.when(r.GetStartTime())})
					}
					if _, err := fmt.Fprintln(w, "\nArrivals of this study:"); err != nil {
						return err
					}
					return output.Table(w, a.style(), []string{"ARRIVAL", "ID", "STEP", "STARTED"}, rows)
				}
				return nil
			})
		}),
	}
}

// newReceptionRetry builds "reception retry".
func (a *app) newReceptionRetry() *cobra.Command {
	var receiver string
	cmd := &cobra.Command{
		Use:   "retry <tenant> <id> [--receiver NAME]",
		Short: "Run a failed reception's files again, once its cause is mended",
		Long: `Run a failed reception's files again, once what failed is mended (a storage
that answers again, say). The failed reception stays as it is and names the new
one: its files that were not archived start a new reception of the same study at
the same receiver, or at the one --receiver names, which is needed when the
failed reception's receiver has been removed. A reception already retried is
answered as it stands, so a repeated command starts nothing more. A reception
that has not failed, or whose files were discarded, cannot be retried (exit 5).`,
		Example: `  nopacs reception retry lab1 rcp_8Qm2
  nopacs reception retry lab1 rcp_8Qm2 --receiver scanner2`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RetryReceptionResponse, error) {
				return c.Receptions.RetryReception(ctx, &v1.RetryReceptionRequest{Tenant: args[0], Id: args[1], Receiver: receiver})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				if _, err := fmt.Fprintf(w, "reception %s retried as %s\n\n", output.Clean(res.GetReception().GetId()), output.Clean(res.GetRetry().GetId())); err != nil {
					return err
				}
				return output.Details(w, a.style(), a.receptionDetails(res.GetRetry()))
			})
		}),
	}
	cmd.Flags().StringVar(&receiver, "receiver", "", "the receiver whose storage and naming rule the new reception runs by (default: the failed reception's own)")
	return cmd
}

// newReceptionDiscard builds "reception discard".
func (a *app) newReceptionDiscard() *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "discard <tenant> <id> --reason TEXT [--yes]",
		Short: "Remove a failed reception's files that were not archived, for good",
		Long: `Remove, for good, the files of a failed reception that were not archived from
the server's incoming area, with a reason that is kept on the record for whoever
reads it later. Nothing on a storage is touched: files it archived stay
archived. The reception stays failed, with the reason and the time. A reception
already discarded is answered as it stands, with its first reason. A reception
that has not failed, or was retried, cannot be discarded (exit 5).

This cannot be undone. Without --yes the command asks for the id to be typed
again on a terminal; with no terminal it sends nothing, says what it would do
and exits 2.`,
		Example: `  nopacs reception discard lab1 rcp_8Qm2 --reason "sender resent the study"
  nopacs reception discard lab1 rcp_8Qm2 --reason "test data" --yes`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			if err := a.confirmTyped(yes, args[1], "discarded", fmt.Sprintf("remove for good the files of reception %q that were not archived", output.Clean(args[1]))); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DiscardReceptionResponse, error) {
				return c.Receptions.DiscardReception(ctx, &v1.DiscardReceptionRequest{Tenant: args[0], Id: args[1], Reason: reason})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				r := res.GetReception()
				if _, err := fmt.Fprintf(w, "reception %s: files not archived were removed (%s)\n\n", output.Clean(r.GetId()), output.Clean(r.GetDiscardReason())); err != nil {
					return err
				}
				return output.Details(w, a.style(), a.receptionDetails(r))
			})
		}),
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why the files are removed, in words for whoever reads the record later (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for the id to be typed again")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
