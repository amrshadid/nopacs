package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newSwitch builds "switch".
func (a *app) newSwitch() *cobra.Command {
	var validate, skip, wait bool
	var confirm, reason string
	var drain time.Duration
	cmd := &cobra.Command{
		Use:   "switch <tenant> <primary> <replica> (--validate | --confirm TOKEN) [--skip-verification --reason TEXT] [--drain-timeout DURATION] [--wait]",
		Short: "Make a replica the primary, under the primary's name, in one step",
		Long: `Move a tenant off a storage: the replica takes the primary's name and place in
one step, and the old place becomes a replica of it. No catalog row changes.

It is two commands. "--validate" changes nothing: it says whether the switch
could start now, what stands in its way (a replica not in step, records owed to
it, no clean scrub of it since its backfill, no empty rebuild compare from it, a
correction prepared), which receivers would pause, and a confirm token. Then
"--confirm TOKEN" does the switch. The token is a digest of both storages'
places and the primary's switch generation; it holds for an hour and once, and
is refused when either place changed since, so the switch acts on what was
shown. Typing the token is the confirmation: there is no --yes.

During the switch every write to the primary pauses: receivers and routes that
name it refuse new data in a way a sender tries again, and reads go on. The
copy drains first; if it does not finish within --drain-timeout (1 second to 1
hour, ten minutes by default) the switch is refused and every write resumes on
the old place. Once accepted a switch runs to its end or refusal whatever
becomes of this command, so a call that times out stops nothing: --wait then
reads the storage's status until the switch is over and exits 0 only when the
primary's switch generation rose; a switch that did not happen (the server
refused it, or the status shows no rise) exits non-zero and says why. "storage
status" tells how it ended.

--skip-verification passes over the two checks (a clean scrub, an empty compare
from the replica alone) and needs --reason, which goes in the audit log.

It needs the admin role on the tenant. A refusal is shown with the server's
reason, such as STORAGE_SWITCHING when another switch runs.`,
		Example: `  nopacs switch lab1 main copy --validate
  nopacs switch lab1 main copy --confirm sw1_4Zq8fN2k
  nopacs switch lab1 main copy --confirm sw1_4Zq8fN2k --skip-verification --reason "scrub of the new volume ran elsewhere" --wait`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if validate == (confirm != "") {
				return fault.New(fault.Invalid, "give --validate to see what the switch would do, or --confirm TOKEN, the token that --validate showed, to do it")
			}
			if skip != (reason != "") {
				return fault.New(fault.Invalid, "--skip-verification and --reason go together: say why the checks are passed over")
			}
			if skip {
				if err := checkReason(reason); err != nil {
					return err
				}
			}
			req := &v1.SwitchStorageRequest{
				Tenant: args[0], Primary: args[1], Replica: args[2],
				ValidateOnly: validate, Confirm: confirm, SkipVerification: skip, SkipReason: reason,
			}
			if cmd.Flags().Changed("drain-timeout") {
				req.DrainTimeout = durationpb.New(drain)
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// The primary's generation before the call is how a wait tells a
			// switch that happened from one that was refused: a switch raises
			// it, a refusal leaves it.
			var before int64
			if wait && !validate {
				st, _, err := a.storageOf(ctx, c, cmd, args[0], args[1])
				if err != nil {
					return err
				}
				before = st.GetSwitchGeneration()
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SwitchStorageResponse, error) {
				return c.Replicas.SwitchStorage(ctx, req)
			})
			if err != nil {
				switch {
				case wait && !validate && client.NoAnswer(err):
					// Only the client's own deadline ended the call: an accepted
					// switch goes on in the server, so read how it ends.
					return a.waitSwitch(ctx, c, cmd, args[0], args[1], before, err)
				case wait && !validate:
					// The server answered, and an answer of any kind, even
					// "unavailable", is final: the switch did not start.
					return fault.New(fault.KindOf(err), "the switch did not happen: "+err.Error())
				}
				return a.switchTimedOut(err)
			}
			if err := messageOut(a, res, func(w io.Writer) error { return a.switchText(w, args, validate, res) }); err != nil {
				return err
			}
			if validate && !res.GetReady() {
				return fault.New(fault.Precondition, fmt.Sprintf("the switch is not ready: %d things stand in its way", len(res.GetBlockers())))
			}
			return nil
		}),
	}
	fl := cmd.Flags()
	fl.BoolVar(&validate, "validate", false, "say whether the switch could start now and what stands in its way, and show the confirm token; change nothing")
	fl.StringVar(&confirm, "confirm", "", "the confirm token that --validate showed, to do the switch")
	fl.BoolVar(&skip, "skip-verification", false, "switch although the replica has no clean scrub since its backfill or no empty compare from it alone; needs --reason")
	fl.StringVar(&reason, "reason", "", "why the checks are passed over, in your own words, kept in the audit entry (with --skip-verification)")
	fl.DurationVar(&drain, "drain-timeout", 0, "how long the copy may take to drain before the switch is refused, 1s to 1h; the server's ten minutes when not given")
	fl.BoolVar(&wait, "wait", false, "if the call times out, read the storage's status until the switch is over, and exit 0 only if it happened; a refusal the server sends ends the command at once")
	cmd.MarkFlagsMutuallyExclusive("validate", "confirm")
	cmd.MarkFlagsOneRequired("validate", "confirm")
	return cmd
}

// switchTimedOut adds to an error of the switch call what a person needs to
// know when the deadline, not the server, ended the call: the switch itself
// goes on.
func (a *app) switchTimedOut(err error) error {
	if client.NoAnswer(err) {
		return fault.New(fault.Unavailable, err.Error()+"; a switch that was accepted goes on in the server: read how it ended with \"nopacs storage status\", or pass --wait")
	}
	return err
}

// storageOf reads a tenant's status and returns the entry of one storage with
// the tenant's replicas, or a not-found error when the answer lacks the tenant
// or the storage: a status that does not name what was asked for says nothing
// about it.
func (a *app) storageOf(ctx context.Context, c *client.Client, cmd *cobra.Command, tenant, storage string) (*v1.StorageStatus, []*v1.ReplicaStatus, error) {
	res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetStatusResponse, error) {
		return c.Monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: optionalScope(tenant)})
	})
	if err != nil {
		return nil, nil, err
	}
	for _, t := range res.GetTenants() {
		if t.GetTenant() != tenant {
			continue
		}
		for _, s := range t.GetStorages() {
			if s.GetStorage() == storage {
				return s, t.GetReplicas(), nil
			}
		}
	}
	return nil, nil, fault.New(fault.NotFound, fmt.Sprintf("the server's status holds no storage %s of tenant %s", output.Clean(storage), output.Clean(tenant)))
}

// waitSwitch reads a tenant's status until the primary's switch is over, then
// decides how it ended by the primary's switch generation: a rise is a switch
// that happened, and the storage's status is shown; no rise is a switch that
// did not, an error that carries why the call got no answer.
func (a *app) waitSwitch(ctx context.Context, c *client.Client, cmd *cobra.Command, tenant, primary string, before int64, cause error) error {
	for {
		s, replicas, err := a.storageOf(ctx, c, cmd, tenant, primary)
		if err != nil {
			return err
		}
		if !s.GetSwitching() {
			if s.GetSwitchGeneration() <= before {
				return fault.New(fault.Precondition, "the switch did not happen: "+cause.Error()+"; the primary's switch generation is still "+strconv.FormatInt(before, 10))
			}
			return messageOut(a, s, func(w io.Writer) error {
				if _, err := fmt.Fprintln(w, "the switch is over; the storage as it stands:"); err != nil {
					return err
				}
				return a.storageStatusDetails(w, s, replicas)
			})
		}
		select {
		case <-ctx.Done():
			return fault.Wrap(fault.Unavailable, ctx.Err(), "stopped waiting; the switch goes on in the server")
		case <-time.After(a.watchPause()):
		}
	}
}

// switchText writes the answer of a validation or of a switch.
func (a *app) switchText(w io.Writer, args []string, validate bool, res *v1.SwitchStorageResponse) error {
	tenant, primary, replica := output.Clean(args[0]), output.Clean(args[1]), output.Clean(args[2])
	if validate {
		ready := "no"
		if res.GetReady() {
			ready = "yes"
		}
		pairs := []output.Pair{
			{Label: "Switch", Value: replica + " becomes the primary " + primary},
			{Label: "Ready", Value: ready},
			{Label: "Scans not in step", Value: strconv.FormatInt(res.GetScansPending(), 10)},
			{Label: "Receivers paused", Value: output.Dash(output.Clean(strings.Join(res.GetReceivers(), ", ")))},
			{Label: "Generation", Value: strconv.FormatInt(res.GetSwitchGeneration(), 10)},
		}
		for i, b := range res.GetBlockers() {
			label := ""
			if i == 0 {
				label = "Blocked by"
			}
			pairs = append(pairs, output.Pair{Label: label, Value: output.Clean(b)})
		}
		pairs = append(pairs, output.Pair{Label: "Confirm token", Value: output.Clean(res.GetConfirm())})
		if err := output.Details(w, a.style(), pairs); err != nil {
			return err
		}
		if res.GetReady() {
			_, err := fmt.Fprintf(w, "\nto switch: nopacs switch %s %s %s --confirm %s\n", tenant, primary, replica, output.Clean(res.GetConfirm()))
			return err
		}
		return nil
	}
	return output.Details(w, a.style(), []output.Pair{
		{Label: "Switched", Value: replica + " is now the primary " + primary + ", on the place the replica held; the old place is now " + replica + ", a replica of it"},
		{Label: "Switch time", Value: a.whenOr(res.GetSwitchTime(), "-")},
		{Label: "Scans drained", Value: strconv.FormatInt(res.GetScansPending(), 10)},
		{Label: "Writes paused for", Value: pauseText(res.GetPause())},
		{Label: "Associations refused", Value: strconv.FormatInt(res.GetAssociationsRefused(), 10) + " (each is tried again by its sender)"},
		{Label: "Generation", Value: strconv.FormatInt(res.GetSwitchGeneration(), 10)},
	})
}

// pauseText words how long writes were paused.
func pauseText(d *durationpb.Duration) string {
	if d == nil {
		return "-"
	}
	return d.AsDuration().Round(time.Millisecond).String()
}
