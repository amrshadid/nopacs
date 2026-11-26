package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newRetention builds the retention noun.
func (a *app) newRetention() *cobra.Command {
	return group("retention", "Keep rules of how long data is kept and when it is due, list what is due, and dispose of it",
		`A retention rule says how long a tenant's sessions are kept at least (until
then a person's delete of one is refused, and no rule makes it due) and when they
are due, counted from the session's start: when it was first archived, by the
server's clock, never a DICOM date. A rule may match every session of the tenant,
or those archived through one receiver, one route of it, or one primary storage.
Its action is review (the due sessions are listed for a person) or delete (the
daily pass queues a job that deletes them, each as an ordinary correction that
can be undone during its grace period). A hold, which an admin places, stops any
of it for a reason, against every rule.

A rule does what it says; whether it meets a law or a consent form is the
institution's to judge, and the server makes no claim of it. A change that would
make data due sooner is held back for the tenant's notice (retention.shorten-
notice, 30 days unless set), so a mistyped rule cannot make a year of data due
tonight.

Every command takes the tenant first. Listing rules and due sessions needs the
reader role on the tenant; the rest, the admin role.`,
		a.newRetentionRule(), a.newRetentionDue(), a.newRetentionDispose(), a.newRetentionClock())
}

// dueRows is a page of due sessions as table rows.
func (a *app) dueRows(sessions []*v1.DueSession) [][]string {
	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		note := ""
		switch {
		case s.GetHeld():
			note = "held"
		case s.GetUndone():
			note = "undone by a person"
		}
		rows = append(rows, []string{output.Clean(s.GetSubjectLabel()) + "/" + output.Clean(s.GetSessionLabel()), output.Clean(s.GetRule()),
			output.RetentionAction(s.GetAction()), a.when(s.GetDueTime()), strconv.FormatInt(s.GetInstances(), 10), output.Human(s.GetBytes()), output.Dash(note)})
	}
	return rows
}

// dueHeader is the header of a table of due sessions.
var dueHeader = []string{"SESSION", "RULE", "ACTION", "DUE", "INSTANCES", "SIZE", "NOTE"}

// newRetentionDue builds "retention due".
func (a *app) newRetentionDue() *cobra.Command {
	var rule string
	var held bool
	var l listFlags
	cmd := &cobra.Command{
		Use:   "due <tenant> [--rule NAME] [--held]",
		Short: "List the sessions that are due now, oldest due first",
		Long: `List the sessions due now under the tenant's rules, oldest due first, with the
rule that makes each due, what is done with it, when it fell due and how much it
holds. It changes nothing. Held sessions are left out unless --held asks for
them, and then marked; a session a person brought back with an undo is marked
too, since the daily pass never deletes it again until a person disposes of it
or the rule changes.`,
		Example: `  nopacs retention due lab1
  nopacs retention due lab1 --rule seven-years --held --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			sessions, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.DueSession, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListDueSessionsResponse, error) {
					return c.Retention.ListDueSessions(ctx, &v1.ListDueSessionsRequest{Tenant: args[0], Rule: rule, IncludeHeld: held, PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetSessions(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "sessions", sessions, more, func() error {
				return a.write(func(w io.Writer) error { return output.Table(w, a.style(), dueHeader, a.dueRows(sessions)) })
			})
		}),
	}
	cmd.Flags().StringVar(&rule, "rule", "", "only the sessions whose rule has this name")
	cmd.Flags().BoolVar(&held, "held", false, "list the held sessions too, marked")
	l.add(cmd)
	return cmd
}

// newRetentionDispose builds "retention dispose".
func (a *app) newRetentionDispose() *cobra.Command {
	var rule, reason, confirm string
	var preview, wait bool
	var max int
	cmd := &cobra.Command{
		Use:   "dispose <tenant> --rule NAME --reason TEXT (--preview | --confirm TOKEN) [--max-sessions N] [--wait]",
		Short: "Delete the sessions due under a rule, exactly as a preview listed them",
		Long: `Delete the due sessions of a rule. It is two commands. "--preview" changes
nothing: it lists the sessions that would be deleted, the oldest due first and
none of them held, what that deletes in all, and a confirm token. Then
"--confirm TOKEN" queues a job that deletes exactly the sessions listed, each as
an ordinary delete that carries your key and your reason, can be undone during
its grace period and is purged on every copy afterwards. The token holds for an
hour and once, and is refused when a preview made now would list other sessions
(data fell due since, a hold was placed, the rule changed): it acts on what was
shown. Typing the token is the confirmation. A command with a token already
used answers the job it made, so it may be sent again.

--reason is kept with every delete. --max-sessions bounds the preview (the
server's own bound applies too). --wait follows the job as "job watch" does.`,
		Example: `  nopacs retention dispose lab1 --rule seven-years --reason "retention period over" --preview
  nopacs retention dispose lab1 --rule seven-years --reason "retention period over" --confirm dp1_8Hk2mQ4x --wait`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if preview == (confirm != "") {
				return fault.New(fault.Invalid, "give --preview to see what would be deleted, or --confirm TOKEN, the token the preview showed, to delete it")
			}
			if err := checkReason(reason); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			cursor := ""
			if wait && !preview {
				if cursor, err = a.feedCursor(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DisposeResponse, error) {
				return c.Retention.Dispose(ctx, &v1.DisposeRequest{Tenant: args[0], Rule: rule, Reason: reason, ValidateOnly: preview, Confirm: confirm, MaxSessions: int32(max)}) // #nosec G115 -- the server bounds it to 0..1000
			})
			if err != nil {
				return err
			}
			if !preview && wait {
				return a.followJob(ctx, c, cmd, args[0], res.GetJob().GetId(), cursor)
			}
			return messageOut(a, res, func(w io.Writer) error { return a.disposeText(w, args[0], rule, reason, preview, res) })
		}),
	}
	fl := cmd.Flags()
	fl.StringVar(&rule, "rule", "", "the name of the rule whose due sessions to delete (required)")
	fl.StringVar(&reason, "reason", "", "why, in your own words, kept with every delete (required)")
	fl.BoolVar(&preview, "preview", false, "list what would be deleted and show the confirm token; delete nothing")
	fl.StringVar(&confirm, "confirm", "", "the confirm token the preview showed, to queue the deletion")
	fl.IntVar(&max, "max-sessions", 0, "the most sessions to list and delete; the server's bound when 0")
	fl.BoolVar(&wait, "wait", false, `with --confirm: follow the job until it ends, as "job watch" does, with its exit codes`)
	_ = cmd.MarkFlagRequired("rule")
	_ = cmd.MarkFlagRequired("reason")
	cmd.MarkFlagsMutuallyExclusive("preview", "confirm")
	cmd.MarkFlagsOneRequired("preview", "confirm")
	return cmd
}

// disposeText writes a disposal preview, or the job a confirmed one queued.
func (a *app) disposeText(w io.Writer, tenant, rule, reason string, preview bool, res *v1.DisposeResponse) error {
	if !preview {
		return a.startedJob(w, tenant, res.GetJob())
	}
	p := res.GetPreview()
	n := p.GetCounts()
	if err := output.Details(w, a.style(), []output.Pair{
		{Label: "Rule", Value: output.Clean(p.GetRule())},
		{Label: "Sessions listed", Value: strconv.Itoa(len(p.GetSessions()))},
		{Label: "Sessions not listed", Value: strconv.FormatInt(p.GetSessionsNotListed(), 10)},
		{Label: "Sessions held (skipped)", Value: strconv.FormatInt(p.GetSessionsHeld(), 10)},
		{Label: "Instances", Value: strconv.FormatInt(n.GetInstances(), 10)},
		{Label: "Bytes", Value: output.Bytes(n.GetBytes())},
		{Label: "Confirm token", Value: output.Clean(p.GetConfirm())},
	}); err != nil {
		return err
	}
	if len(p.GetSessions()) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if err := output.Table(w, a.style(), dueHeader, a.dueRows(p.GetSessions())); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "\nto delete exactly these: nopacs retention dispose %s --rule %s --reason %q --confirm %s\n",
		output.Clean(tenant), output.Clean(rule), output.Clean(reason), output.Clean(p.GetConfirm()))
	return err
}

// newRetentionClock builds "retention clock".
func (a *app) newRetentionClock() *cobra.Command {
	var observed, reason string
	cmd := &cobra.Command{
		Use:   "clock <tenant> [--confirm TIME --reason TEXT]",
		Short: "Show a tenant's retention state, or confirm the time after a clock jump",
		Long: `Without --confirm, show the tenant's retention state: its rules, the sessions
due, when the next falls due, when the last daily pass ran, the active holds, and
whether retention is paused.

Retention pauses by itself when the server's clock jumps forward by more than the
installation's retention.max-clock-step since the last daily pass, so a clock that
is wrong can never make data due. While it is paused nothing becomes due, no
dispose job runs, and no purge starts by its timer. An admin ends the pause by
reading the time from a clock they trust and typing it with --confirm (a time
with its offset, such as 2026-11-26T09:30:00+03:00) and --reason. The server
takes its own clock as right from then; it refuses a time that differs from its
own by more than five minutes. The time is yours to type: it is never filled in
from the server, or the check would confirm nothing.`,
		Example: `  nopacs retention clock lab1
  nopacs retention clock lab1 --confirm 2026-11-26T09:30:00+03:00 --reason "clock set from the site's time server"`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if (observed != "") != (reason != "") {
				return fault.New(fault.Invalid, "--confirm and --reason go together: type the time you read and say why")
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			if observed == "" {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetStatusResponse, error) {
					return c.Monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: optionalScope(args[0])})
				})
				if err != nil {
					return err
				}
				if len(res.GetTenants()) == 0 || res.GetTenants()[0].GetRetention() == nil {
					return fault.New(fault.NotFound, fmt.Sprintf("the server answered no retention state for tenant %s", output.Clean(args[0])))
				}
				rs := res.GetTenants()[0].GetRetention()
				return messageOut(a, rs, func(w io.Writer) error { return output.Details(w, a.style(), a.retentionStatusPairs(rs)) })
			}
			if err := checkReason(reason); err != nil {
				return err
			}
			t, err := time.Parse(time.RFC3339, observed)
			if err != nil {
				return fault.New(fault.Invalid, fmt.Sprintf("--confirm %q is not a time with its offset, such as 2026-11-26T09:30:00+03:00", output.Clean(observed)))
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ConfirmClockResponse, error) {
				return c.Retention.ConfirmClock(ctx, &v1.ConfirmClockRequest{Scope: optionalScope(args[0]), ObservedTime: timestamppb.New(t), Reason: reason})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				if _, err := fmt.Fprintf(w, "time confirmed for %d tenant(s); retention resumes\n", len(res.GetTenants())); err != nil {
					return err
				}
				return output.Details(w, a.style(), a.retentionStatusPairs(res.GetStatus()))
			})
		}),
	}
	cmd.Flags().StringVar(&observed, "confirm", "", "the time you read from a clock you trust, with its offset (RFC 3339), to end a pause for a clock jump")
	cmd.Flags().StringVar(&reason, "reason", "", "why the time is right, in your own words, kept in the audit entry (with --confirm)")
	return cmd
}

// retentionStatusPairs lists a tenant's retention state as labeled values.
func (a *app) retentionStatusPairs(rs *v1.RetentionStatus) []output.Pair {
	paused := "no"
	if rs.GetPaused() {
		paused = "yes: " + output.Clean(rs.GetPauseReason()) + " (nothing becomes due until an admin confirms the time)"
	}
	step := "-"
	if rs.GetClockStep() != nil {
		step = output.Span(rs.GetClockStep().AsDuration())
	}
	return []output.Pair{
		{Label: "Rules", Value: strconv.FormatInt(rs.GetRules(), 10)},
		{Label: "Sessions due", Value: fmt.Sprintf("%d (%d for review, %d held)", rs.GetSessionsDue(), rs.GetSessionsDueReview(), rs.GetSessionsDueHeld())},
		{Label: "Next due", Value: a.whenOr(rs.GetNextDueTime(), "-")},
		{Label: "Last daily pass", Value: a.whenOr(rs.GetLastPassTime(), "-")},
		{Label: "Paused", Value: paused},
		{Label: "Clock step seen", Value: step},
		{Label: "Active holds", Value: strconv.FormatInt(rs.GetActiveHolds(), 10)},
		{Label: "Hold records owed", Value: strconv.FormatInt(rs.GetHoldRecordsOwed(), 10)},
	}
}
