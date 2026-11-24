package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// eventTypePrefix is what every name of an event type starts with.
const eventTypePrefix = "EVENT_TYPE_"

// newWatch builds "watch".
func (a *app) newWatch() *cobra.Command {
	var types []string
	var cursor string
	var count int
	cmd := &cobra.Command{
		Use:   "watch <tenant> [--type TYPE]... [--cursor CURSOR] [--count N]",
		Short: "Follow a tenant's events as they happen",
		Long: `Follow a tenant's event feed: a line for each event as it happens, such as a
reception reaching a step, a file quarantined, a receiver's or a sender's health
changing, a job reaching a state or reporting its progress. It starts from now,
or from --cursor, the position an earlier event or "tenant status --json" gave,
so nothing between the two is missed. The server ends a stream at the latest
when its deadline passes, and the connection may drop: the command follows on
from the last event it showed, for as long as it runs. It ends when interrupted
(exit 0), or after --count events.

A TYPE is one of: ` + strings.Join(enumNames(v1.EventType_name, eventTypePrefix), ", ") + `.
Repeat --type to follow more than one; without it every type is followed.

With --json each event is one JSON document, with its cursor. An event names
the archive's own identifiers and labels, never a name or an identifier from a
file. It needs the reader role on the tenant.`,
		Example: `  nopacs watch lab1
  nopacs watch lab1 --type job-step --type sender-health
  nopacs watch lab1 --cursor "$(nopacs tenant status lab1 --json | jq -r '.tenants[0].eventCursor')" --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if count < 0 {
				return fault.New(fault.Invalid, "--count is zero (no bound) or a number of events")
			}
			req := &v1.WatchRequest{Scope: optionalScope(args[0])}
			for _, t := range types {
				et, err := parseEnum[v1.EventType]("type", t, v1.EventType_name, eventTypePrefix)
				if err != nil {
					return err
				}
				req.Types = append(req.Types, et)
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			if cursor == "" {
				if cursor, err = a.feedCursor(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			shown := 0
			for {
				req.Cursor = cursor
				var writeErr error
				werr := c.Watch(ctx, req, func(m *v1.WatchResponse) bool {
					if k := m.GetKeepAlive(); k != nil {
						if k.GetCursor() != "" {
							cursor = k.GetCursor()
						}
						return true
					}
					e := m.GetEvent()
					cursor = e.GetCursor()
					if writeErr = a.eventLine(e); writeErr != nil {
						return false
					}
					shown++
					return count == 0 || shown < count
				})
				switch {
				case writeErr != nil:
					return writeErr
				case ctx.Err() != nil, count > 0 && shown >= count:
					return nil
				case !streamEnded(werr):
					return werr
				}
				a.pause(ctx)
			}
		}),
	}
	cmd.Flags().StringArrayVar(&types, "type", nil, "follow only events of this type; repeat to follow more types")
	cmd.Flags().StringVar(&cursor, "cursor", "", "start after this position of the feed, as an event or a status gave it, instead of now")
	cmd.Flags().IntVar(&count, "count", 0, "end after this many events; zero follows until interrupted")
	enumCompletion(cmd, "type", v1.EventType_name, eventTypePrefix)
	return cmd
}

// eventLine writes one event of the feed: a line, or with --json one
// document.
func (a *app) eventLine(e *v1.Event) error {
	if a.opts.json {
		return a.write(func(w io.Writer) error { return output.JSONMessage(w, e) })
	}
	return a.write(func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%s  %-19s %s\n", a.when(e.GetTime()), strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(e.GetType().String(), eventTypePrefix)), "_", "-"),
			output.Clean(eventSummary(e)))
		return err
	})
}

// eventSummary says in words what an event reports.
func eventSummary(e *v1.Event) string {
	word := func(name, prefix string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, prefix)), "_", " ")
	}
	switch p := e.GetPayload().(type) {
	case *v1.Event_AssociationFailed:
		f := p.AssociationFailed
		return fmt.Sprintf("receiver %s refused %d association(s): %s", f.GetReceiver(), f.GetCount(), word(f.GetFailure().String(), "ASSOCIATION_FAILURE_"))
	case *v1.Event_ReceptionStep:
		r := p.ReceptionStep
		return fmt.Sprintf("reception %s of receiver %s (%s/%s) is %s", r.GetReception(), r.GetReceiver(), r.GetSubjectLabel(), r.GetSessionLabel(),
			output.ReceptionStep(r.GetStep()))
	case *v1.Event_FileQuarantined:
		q := p.FileQuarantined
		return fmt.Sprintf("file %s of reception %s held: %s", q.GetFile(), q.GetReception(), output.QuarantineReason(q.GetReason()))
	case *v1.Event_QuarantineDecided:
		q := p.QuarantineDecided
		return fmt.Sprintf("held file %s is %s", q.GetFile(), output.QuarantineState(q.GetState()))
	case *v1.Event_ReceiverHealth:
		return "receiver " + p.ReceiverHealth.GetReceiver() + " is " + output.ReceiverHealth(p.ReceiverHealth.GetHealth())
	case *v1.Event_StorageSpace:
		s := p.StorageSpace
		return fmt.Sprintf("storage %s is %s: %s used, %s free", s.GetStorage(), word(s.GetState().String(), "STORAGE_SPACE_STATE_"),
			output.Human(s.GetUsedBytes()), output.Human(s.GetFreeBytes()))
	case *v1.Event_FramesAdded:
		f := p.FramesAdded
		return fmt.Sprintf("frames of %s/%s/%s: %d instances", f.GetSubjectLabel(), f.GetSessionLabel(), f.GetScanLabel(), f.GetInstances())
	case *v1.Event_JobStep:
		j := p.JobStep
		line := "job " + j.GetJob() + " is " + output.JobState(j.GetState()) + ": " + output.JobProgress(j.GetCounts())
		if j.GetReason() != "" {
			line += " (" + j.GetReason() + ")"
		}
		return line
	case *v1.Event_JobProgress:
		return "job " + p.JobProgress.GetJob() + ": " + output.JobProgress(p.JobProgress.GetCounts())
	case *v1.Event_SenderHealth:
		return "sender " + p.SenderHealth.GetSender() + " is " + output.SenderHealth(p.SenderHealth.GetHealth())
	}
	return "an event this command does not describe; see --json"
}
