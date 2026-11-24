package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// streamEnded reports whether a stream of the event feed ended in a way the
// command follows on from: the server may end any stream at any time and
// says so as unavailable ("the stream ended; resume from its last
// cursors"), which it does at the latest when the call's deadline passes;
// a connection that dropped is unavailable too. Either way the feed is read
// again from the last cursor, and a server that cannot be reached any more
// is told by the next call, which is not a stream.
func streamEnded(err error) bool {
	return err == nil || fault.KindOf(err) == fault.Unavailable
}

// feedCursor returns the position of a tenant's event feed now, from its
// status: a stream from it carries every event from this moment on.
func (a *app) feedCursor(ctx context.Context, c *client.Client, cmd *cobra.Command, tenant string) (string, error) {
	st, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetStatusResponse, error) {
		return c.Monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: optionalScope(tenant)})
	})
	if err != nil {
		return "", err
	}
	if len(st.GetTenants()) > 0 {
		return st.GetTenants()[0].GetEventCursor(), nil
	}
	return "", nil
}

// pause waits the follow pause, or until ctx ends.
func (a *app) pause(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(a.watchPause()):
	}
}

// followJob follows a job of the tenant from the feed position cursor
// until it ends, as "job watch" describes: the job as it stands, a line for
// each event about it, then the job as it finally stands; the error says
// when it did not end done.
func (a *app) followJob(ctx context.Context, c *client.Client, cmd *cobra.Command, tenant, id, cursor string) error {
	fetch := func() (*v1.Job, error) {
		res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetJobResponse, error) {
			return c.Jobs.GetJob(ctx, &v1.GetJobRequest{Tenant: tenant, Id: id})
		})
		if err != nil {
			return nil, err
		}
		return res.GetJob(), nil
	}
	job, err := fetch()
	if err != nil {
		return err
	}
	if err := a.watchLine(job, a.when(timestamppb.New(a.env.Now()))); err != nil {
		return err
	}
	last := job
	for !output.JobEnded(last.GetState()) {
		var ended bool
		var writeErr error
		werr := c.Watch(ctx, &v1.WatchRequest{
			Scope: optionalScope(tenant), Cursor: cursor,
			Types: []v1.EventType{v1.EventType_EVENT_TYPE_JOB_STEP, v1.EventType_EVENT_TYPE_JOB_PROGRESS},
		}, func(m *v1.WatchResponse) bool {
			if k := m.GetKeepAlive(); k != nil {
				// The feed's position past events the filter left out.
				if k.GetCursor() != "" {
					cursor = k.GetCursor()
				}
				return true
			}
			e := m.GetEvent()
			cursor = e.GetCursor()
			j, ok := jobOfEvent(e, id, last)
			if !ok {
				return true
			}
			last = j
			if err := a.watchLine(j, a.when(e.GetTime())); err != nil {
				writeErr = err
				return false
			}
			ended = output.JobEnded(j.GetState())
			return !ended
		})
		if writeErr != nil {
			return writeErr
		}
		if ctx.Err() != nil {
			return fault.New(fault.Internal, "stopped watching; the job goes on")
		}
		if !streamEnded(werr) {
			return werr
		}
		if !ended {
			// The server ended the stream, or it dropped: read the job
			// again, so an end the stream did not carry is not waited
			// for, then follow on from the last cursor.
			if last, err = fetch(); err != nil {
				return err
			}
			if !output.JobEnded(last.GetState()) {
				a.pause(ctx)
			}
		}
	}
	// The job as it finally stands: its times and reason, which the
	// events do not carry.
	final, err := fetch()
	if err != nil {
		return err
	}
	if a.opts.json {
		if err := a.write(func(w io.Writer) error { return output.JSONMessage(w, final) }); err != nil {
			return err
		}
	} else if err := a.write(func(w io.Writer) error {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		return a.jobOut(w, final)
	}); err != nil {
		return err
	}
	if final.GetState() != v1.JobState_JOB_STATE_DONE {
		return fault.New(fault.Internal, fmt.Sprintf("job %s ended %s", output.Clean(id), output.JobState(final.GetState())))
	}
	return nil
}
