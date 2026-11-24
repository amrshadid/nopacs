package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// jobStatePrefix is what every name of a job state starts with.
const jobStatePrefix = "JOB_STATE_"

// newJob builds the job noun.
func (a *app) newJob() *cobra.Command {
	return group("job", "Create, list, show, watch, cancel and retry a tenant's jobs",
		`A job is a piece of work a person or a rule asks for, kept until it ends, so it
survives a restart of the server midway. A send job sends the archived instances
of a subject, a session or a scan to one of the tenant's senders; a receiver that
lists a sender in its forward-to makes one for each reception it completes. A
deid job judges archived instances against a de-identification profile version
and changes nothing ("deid check", "deid dry-run"). An import job hands the
files of a directory under an import root to an import receiver ("job import";
its files and units are listed by "job files" and "job units"). A scrub job
reads every stored object again and compares it with the catalog ("job scrub";
what it found is listed by "job findings" and "job finding"). A rebuild job makes
the catalog again from the storages, to compare it with the one in use or to
replace it ("job rebuild"). A
job ends done (every instance sent), partial (some refused, the others sent),
failed (some not sent because the attempts ran out) or canceled.
Every command takes the tenant first. Creating, canceling and retrying, and
listing an import's files and units, need the operator role on the tenant; the
others need the reader role.`,
		a.newJobCreate(), a.newJobImport(), a.newJobScrub(), a.newJobRebuild(), a.newJobGet(), a.newJobList(), a.newJobWatch(), a.newJobCancel(), a.newJobRetry(), a.newJobFailures(),
		a.newJobFiles(), a.newJobUnits(), a.newJobFindings(), a.newJobFinding())
}

// jobDetails lists one job as labeled values.
func (a *app) jobDetails(j *v1.Job) []output.Pair {
	cn := j.GetCounts()
	pairs := []output.Pair{
		{Label: "ID", Value: j.GetId()},
		{Label: "State", Value: output.JobState(j.GetState())},
	}
	if d := j.GetDeid(); d != nil {
		pairs = append(pairs,
			output.Pair{Label: "Judges", Value: deidTarget(d)},
			output.Pair{Label: "Mode", Value: deidMode(d.GetMode())},
			output.Pair{Label: "Version", Value: deidVersion(d)})
	}
	if s := j.GetSend(); s != nil {
		pairs = append(pairs,
			output.Pair{Label: "Sends", Value: output.JobTarget(s)},
			output.Pair{Label: "Sender", Value: s.GetSender()})
		if s.GetReception() != "" {
			pairs = append(pairs,
				output.Pair{Label: "Forwarded from", Value: "receiver " + s.GetReceiver() + ", reception " + s.GetReception()})
		}
	}
	if ij := j.GetImportJob(); ij != nil {
		pairs = append(pairs, importJobDetails(ij)...)
	}
	if sj := j.GetScrub(); sj != nil {
		pairs = append(pairs, scrubJobDetails(sj)...)
	}
	if rj := j.GetRebuild(); rj != nil {
		pairs = append(pairs, rebuildJobDetails(rj)...)
	}
	sentLabel, refusedLabel := countLabels(j)
	if j.GetReason() != "" {
		pairs = append(pairs, output.Pair{Label: "Reason", Value: j.GetReason()})
	}
	pairs = append(pairs,
		output.Pair{Label: "Progress", Value: output.JobBar(cn, 22) + " " + output.JobProgressOf(j)},
		output.Pair{Label: "Instances", Value: fmt.Sprintf("%d, %s", cn.GetInstances(), output.Human(cn.GetBytes()))},
		output.Pair{Label: sentLabel, Value: fmt.Sprintf("%d, %s", cn.GetInstancesSent(), output.Human(cn.GetBytesSent()))},
		output.Pair{Label: "Warned", Value: fmt.Sprintf("%d, %s", cn.GetInstancesWarned(), output.Human(cn.GetBytesWarned()))},
		output.Pair{Label: refusedLabel, Value: fmt.Sprintf("%d, %s", cn.GetInstancesRefused(), output.Human(cn.GetBytesRefused()))},
		output.Pair{Label: "Remaining", Value: fmt.Sprintf("%d, %s", cn.GetInstancesRemaining(), output.Human(cn.GetBytesRemaining()))},
		output.Pair{Label: "Created by", Value: output.Dash(j.GetCreatedByKeyId())},
		output.Pair{Label: "Created", Value: a.when(j.GetCreateTime())},
		output.Pair{Label: "Started", Value: a.whenOr(j.GetStartTime(), "-")},
		output.Pair{Label: "State since", Value: a.whenOr(j.GetStateTime(), "-")},
		output.Pair{Label: "Ended", Value: a.whenOr(j.GetEndTime(), "-")})
	if j.GetRetryOf() != "" {
		pairs = append(pairs, output.Pair{Label: "Retry of", Value: j.GetRetryOf()})
	}
	if j.GetRetryJob() != "" {
		pairs = append(pairs, output.Pair{Label: "Retried as", Value: j.GetRetryJob()})
	}
	if j.GetFailuresNotListed() > 0 {
		pairs = append(pairs, output.Pair{Label: "Failures not listed", Value: strconv.FormatInt(j.GetFailuresNotListed(), 10)})
	}
	return pairs
}

// newJobCreate builds "job create".
func (a *app) newJobCreate() *cobra.Command {
	var sender string
	var wait bool
	cmd := &cobra.Command{
		Use:   "create <tenant> --sender NAME <subject>[/<session>[/<scan>]] [--wait]",
		Short: "Send a subject, a session or a scan to a sender",
		Long: `Make a send job and queue its work. Name what to send by labels: a subject, a
session of it (<subject>/<session>) or a scan of that
(<subject>/<session>/<scan>). The job sends the instances the catalog holds for
the target now; what is archived into it later is sent by a new job, or by a
receiver's forward-to. The sender may be stopped: its jobs wait until it is
enabled. A sender the tenant does not have, or a target the catalog does not
hold, is refused.

The answer is the job as queued. "job watch" follows it to its end; with --wait
this command follows it itself, writing what "job watch" writes, and its exit
code is that of "job watch": 0 when the job ended done, 1 when it ended partial,
failed or canceled. Interrupting the wait stops only the watching, never the
job.`,
		Example: `  nopacs job create lab1 --sender pacs sub-0017
  nopacs job create lab1 --sender archive2 sub-0017/ses-2026-03-02 --wait
  nopacs job create lab1 --sender pacs sub-0017/ses-2026-03-02/scan-0004 --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			parts, err := splitRef(args[1], 1, 3, "<subject>, <subject>/<session> or <subject>/<session>/<scan>")
			if err != nil {
				return err
			}
			job := &v1.SendJob{Sender: sender, SubjectLabel: parts[0]}
			if len(parts) > 1 {
				job.SessionLabel = parts[1]
			}
			if len(parts) > 2 {
				job.ScanLabel = parts[2]
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// With --wait the feed's position is read before the job is
			// made, so none of its events can come before it.
			cursor := ""
			if wait {
				if cursor, err = a.feedCursor(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateJobResponse, error) {
				return c.Jobs.CreateJob(ctx, &v1.CreateJobRequest{Tenant: args[0], Kind: &v1.CreateJobRequest_Send{Send: job}})
			})
			if err != nil {
				return err
			}
			if wait {
				return a.followJob(ctx, c, cmd, args[0], res.GetJob().GetId(), cursor)
			}
			return messageOut(a, res, func(w io.Writer) error { return a.jobOut(w, res.GetJob()) })
		}),
	}
	cmd.Flags().BoolVar(&wait, "wait", false, `follow the job until it ends, as "job watch" does, with its exit codes`)
	cmd.Flags().StringVar(&sender, "sender", "", "the name of the tenant's sender to send to (required)")
	_ = cmd.MarkFlagRequired("sender")
	return cmd
}

// newJobGet builds "job show", which "job get" also names, as every other
// noun's command that shows one item is named show.
func (a *app) newJobGet() *cobra.Command {
	return &cobra.Command{
		Use:     "show <tenant> <id>",
		Aliases: []string{"get"},
		Short:   "Show one job with its progress now",
		Long: `Show one job: what it sends and to which sender, its state and why, how many
instances have been sent, sent with a warning or refused, how many remain, and
its times. For a job a receiver's forward made it also says which reception.
"job get" is the same command.`,
		Example: `  nopacs job show lab1 job_8Qm2`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetJobResponse, error) {
				return c.Jobs.GetJob(ctx, &v1.GetJobRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.jobOut(w, res.GetJob()) })
		}),
	}
}

// newJobList builds "job list".
func (a *app) newJobList() *cobra.Command {
	var sender, since, until string
	var states []string
	var inProgress bool
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant> [--state STATE]... [--in-progress] [--sender NAME] [--since TIME] [--until TIME]",
		Short: "List a tenant's jobs, newest first",
		Long: `List a tenant's jobs, newest first by the time they were made, optionally only
those in one or more states, still in progress, sent to one sender, or made in a
span of time. Every filter given must hold.

A STATE is one of: ` + strings.Join(enumNames(v1.JobState_name, jobStatePrefix), ", ") + `.
A TIME is a date (2026-11-17, midnight in your zone), a time with its offset
(2026-11-17T09:30:00+03:00) or an age (24h, 7d: that long before now). --since
is inclusive and --until is exclusive.`,
		Example: `  nopacs job list lab1
  nopacs job list lab1 --in-progress --sender pacs
  nopacs job list lab1 --state partial --state failed --since 7d --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			req := &v1.ListJobsRequest{Tenant: args[0], Sender: sender, InProgress: inProgress}
			for _, s := range states {
				st, err := parseEnum[v1.JobState]("state", s, v1.JobState_name, jobStatePrefix)
				if err != nil {
					return err
				}
				req.States = append(req.States, st)
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
			jobs, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Job, string, error) {
				page := &v1.ListJobsRequest{
					Tenant: req.GetTenant(), States: req.GetStates(), InProgress: req.GetInProgress(), Sender: req.GetSender(),
					StartTime: req.GetStartTime(), EndTime: req.GetEndTime(), PageSize: size, PageToken: token,
				}
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListJobsResponse, error) {
					return c.Jobs.ListJobs(ctx, page)
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetJobs(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "jobs", jobs, more, func() error {
				rows := make([][]string, 0, len(jobs))
				for _, j := range jobs {
					cn := j.GetCounts()
					by, what := jobKindText(j)
					rows = append(rows, []string{
						j.GetId(), by, what, output.JobState(j.GetState()),
						strconv.Itoa(output.JobPercent(cn)) + "%",
						strconv.FormatInt(cn.GetInstancesSent()+cn.GetInstancesWarned(), 10), strconv.FormatInt(cn.GetInstancesRefused(), 10), strconv.FormatInt(cn.GetInstancesRemaining(), 10),
						a.when(j.GetCreateTime()),
					})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"ID", "KIND", "ON", "STATE", "DONE", "OK", "NOT OK", "REMAINING", "CREATED"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&sender, "sender", "", "only the jobs that send to the sender of this name")
	cmd.Flags().StringArrayVar(&states, "state", nil, "only the jobs in this state; repeat to include more states")
	cmd.Flags().BoolVar(&inProgress, "in-progress", false, "only the jobs that have not ended (queued or running)")
	cmd.Flags().StringVar(&since, "since", "", "only jobs made at or after this time: a date, a time with its offset, or an age such as 24h")
	cmd.Flags().StringVar(&until, "until", "", "only jobs made before this time, written like --since")
	enumCompletion(cmd, "state", v1.JobState_name, jobStatePrefix)
	l.add(cmd)
	return cmd
}

// newJobCancel builds "job cancel".
func (a *app) newJobCancel() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <tenant> <id>",
		Short: "End a queued or running job as canceled",
		Long: `End a queued or running job as canceled. Units that have not started end at
once; one that is sending ends within one batch (at most 64 instances or one
second), and what the peer answered for that batch still counts, since the peer
holds it. A job already canceled is answered as it stands. A job that has ended
otherwise cannot be canceled (exit 5). What was not sent can be sent later with
"job retry".`,
		Example: `  nopacs job cancel lab1 job_8Qm2`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CancelJobResponse, error) {
				return c.Jobs.CancelJob(ctx, &v1.CancelJobRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				if _, err := fmt.Fprintf(w, "job %s is %s\n\n", output.Clean(res.GetJob().GetId()), output.JobState(res.GetJob().GetState())); err != nil {
					return err
				}
				return output.Details(w, a.style(), a.jobDetails(res.GetJob()))
			})
		}),
	}
}

// newJobRetry builds "job retry".
func (a *app) newJobRetry() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <tenant> <id>",
		Short: "Send what an ended job did not send, as a new job",
		Long: `Make a new job that sends the instances an ended job did not send (refused,
never sent, or left when it was canceled), to the same sender. The ended job
stays as it is and names the new one. A job already retried is answered as it
stands, so a repeated command starts nothing more. A job that has not ended, or
sent everything, cannot be retried (exit 5). It may send again at most one batch
of each unit that was still sending when the job ended.`,
		Example: `  nopacs job retry lab1 job_8Qm2`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RetryJobResponse, error) {
				return c.Jobs.RetryJob(ctx, &v1.RetryJobRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				if _, err := fmt.Fprintf(w, "job %s retried as %s\n\n", output.Clean(res.GetJob().GetId()), output.Clean(res.GetRetry().GetId())); err != nil {
					return err
				}
				return output.Details(w, a.style(), a.jobDetails(res.GetRetry()))
			})
		}),
	}
}

// newJobFailures builds "job failures".
func (a *app) newJobFailures() *cobra.Command {
	var warnings bool
	var l listFlags
	cmd := &cobra.Command{
		Use:   "failures <tenant> <id> [--warnings]",
		Short: "List the instances a job did not send, with the peer's reason",
		Long: `List the instances a job did not send: those the peer refused or that were
refused before they reached it (a class or transfer syntax the peer does not
take, an instance whose stored bytes were not what was archived), each with the
peer's status and a reason. --warnings adds the instances the peer kept with a
warning, since it changed what it keeps. How many are kept is the setting
job.failures-kept; past it each is still counted in "job show", and not listed.
An instance is named by its catalog id (its SOP Instance UID) and its scan's
labels; no other value from its file is shown.

The peer's status is a DICOM status in hexadecimal (a DIMSE peer's C-STORE
status, PS3.7 Annex C, or a stow peer's Failure or Warning Reason), or, for a
stow request the peer refused whole, its HTTP status in decimal after "HTTP",
such as "HTTP 415".`,
		Example: `  nopacs job failures lab1 job_8Qm2
  nopacs job failures lab1 job_8Qm2 --warnings --all --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			failures, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.JobFailure, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListJobFailuresResponse, error) {
					return c.Jobs.ListJobFailures(ctx, &v1.ListJobFailuresRequest{Tenant: args[0], Id: args[1], IncludeWarnings: warnings, PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetFailures(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "failures", failures, more, func() error {
				rows := make([][]string, 0, len(failures))
				for _, f := range failures {
					status := peerStatus(f)
					rows = append(rows, []string{
						f.GetSubjectLabel() + "/" + f.GetSessionLabel() + "/" + f.GetScanLabel(), f.GetInstanceId(),
						output.JobFailureCode(f.GetCode()), status, output.Dash(f.GetDetail()), a.when(f.GetTime()),
					})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"SCAN", "INSTANCE", "WHY", "PEER STATUS", "DETAIL", "TIME"}, rows)
				})
			})
		}),
	}
	cmd.Flags().BoolVar(&warnings, "warnings", false, "also list the instances the peer kept with a warning")
	l.add(cmd)
	return cmd
}

// peerStatus writes a failure's peer status as a person reads it: an HTTP
// status in decimal after "HTTP" (RFC 9110 15), a DICOM status in
// hexadecimal (PS3.7 Annex C), "-" for none.
func peerStatus(f *v1.JobFailure) string {
	switch {
	case f.GetPeerStatus() == 0:
		return "-"
	case f.GetPeerStatusHttp():
		return "HTTP " + strconv.Itoa(int(f.GetPeerStatus()))
	}
	return fmt.Sprintf("%04X", f.GetPeerStatus())
}

// watchPause is how long "job watch" waits before it follows the feed again
// after a stream the server ended without the job ending: a second, so a
// server that ends streams at once is not asked in a tight loop.
func (a *app) watchPause() time.Duration {
	if a.env.WatchPause > 0 {
		return a.env.WatchPause
	}
	return time.Second
}

// newJobWatch builds "job watch".
func (a *app) newJobWatch() *cobra.Command {
	return &cobra.Command{
		Use:   "watch <tenant> <id>",
		Short: "Follow a job's events until it ends",
		Long: `Follow a job until it ends: the command shows the job as it stands, then a
line for each change of state and each report of progress the tenant's event feed
carries for it (at most one every 10 seconds while it runs), and ends with the job
as it finally stands. It reads the feed from the position the job was read at, so
no change between the two is missed. The server ends a stream at the latest when
its deadline passes (30 seconds), and the connection may drop: the command then
reads the job again and follows on from where the stream was, for as long as
the job runs.

With --json each line is replaced by one JSON document of the job as the event
reports it (state and counts), the last being the job as read at the end.

Exit codes: 0 when the job ended done, 1 when it ended partial, failed or
canceled (so a script sees that something was not sent), and the codes of any
call for a job or tenant that is not there. Interrupting the command stops only
the watching, never the job.`,
		Example: `  nopacs job create lab1 --sender pacs sub-0017
  nopacs job watch lab1 job_8Qm2`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// The cursor first, then the job: an event that happened between
			// them is read again, never missed.
			cursor, err := a.feedCursor(ctx, c, cmd, args[0])
			if err != nil {
				return err
			}
			return a.followJob(ctx, c, cmd, args[0], args[1], cursor)
		}),
	}
}

// jobOfEvent returns the job an event of the feed reports, with the state and
// counts it carries, when the event is about the job with this id; ok is false
// for another job's event. prev is the job as last seen, whose kind the event
// does not repeat.
func jobOfEvent(e *v1.Event, id string, prev *v1.Job) (*v1.Job, bool) {
	var j *v1.Job
	switch p := e.GetPayload().(type) {
	case *v1.Event_JobStep:
		if p.JobStep.GetJob() != id {
			return nil, false
		}
		j = &v1.Job{Id: id, State: p.JobStep.GetState(), Counts: p.JobStep.GetCounts(), Reason: p.JobStep.GetReason()}
	case *v1.Event_JobProgress:
		if p.JobProgress.GetJob() != id {
			return nil, false
		}
		j = &v1.Job{Id: id, State: prev.GetState(), Counts: p.JobProgress.GetCounts()}
	default:
		return nil, false
	}
	j.Kind = prev.GetKind()
	return j, true
}

// watchLine writes one update of a watched job: a line, or with --json one
// document.
func (a *app) watchLine(j *v1.Job, when string) error {
	if a.opts.json {
		return a.write(func(w io.Writer) error { return output.JSONMessage(w, j) })
	}
	return a.write(func(w io.Writer) error {
		line := fmt.Sprintf("%s  %s  %s %s", when, padState(output.JobState(j.GetState())), output.JobBar(j.GetCounts(), 22), output.JobProgressOf(j))
		if j.GetReason() != "" {
			line += " (" + output.Clean(j.GetReason()) + ")"
		}
		_, err := fmt.Fprintln(w, line)
		return err
	})
}

// padState pads a state's word to the longest one, so the bars line up.
func padState(s string) string { return fmt.Sprintf("%-8s", s) }
