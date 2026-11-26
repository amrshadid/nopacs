package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newHold builds the hold noun.
func (a *app) newHold() *cobra.Command {
	return group("hold", "Place, release, list and show the holds that stop data from going",
		`A hold is a mark, with a reason, on a subject, a session, a scan or the whole
tenant. It has no expiry: it lasts until an admin releases it. It covers what it
names now and what arrives under it later, and follows a relabel. While it holds,
a delete of what it covers is refused (reason HELD), a purge of it waits, and a
retention disposal skips it, against every rule. A delete committed before the
hold stays committed and may still be undone.

A hold's record is written beside the data on each primary before the hold
counts, and a replica that does not answer is owed it: until it has the record no
purge removes anything from it and no switch to it swaps. A server that does not
serve placing or releasing yet says so ("not served"); listing and showing need
the operator role, placing and releasing the admin role. The reason of a hold is
a person's words about a subject: it is answered only to an operator or more.`,
		a.newHoldList(), a.newHoldShow(), a.newHoldPlace(), a.newHoldRelease())
}

// holdDetails lists one hold as labeled values.
func (a *app) holdDetails(h *v1.Hold) []output.Pair {
	pairs := []output.Pair{
		{Label: "ID", Value: output.Clean(h.GetId())},
		{Label: "Covers", Value: output.Clean(output.HoldTarget(h.GetTarget()))},
		{Label: "State", Value: output.HoldState(h.GetState())},
		{Label: "Reason", Value: output.Dash(output.Clean(h.GetReason()))},
		{Label: "Placed by", Value: output.Dash(output.Clean(h.GetPlacedByKeyId()))},
		{Label: "Placed", Value: a.when(h.GetPlaceTime())},
	}
	if h.GetState() == v1.HoldState_HOLD_STATE_RELEASED {
		pairs = append(pairs,
			output.Pair{Label: "Released by", Value: output.Dash(output.Clean(h.GetReleasedByKeyId()))},
			output.Pair{Label: "Released", Value: a.whenOr(h.GetReleaseTime(), "-")},
			output.Pair{Label: "Release reason", Value: output.Dash(output.Clean(h.GetReleaseReason()))})
	}
	pairs = append(pairs,
		output.Pair{Label: "Record on", Value: output.Dash(output.Clean(strings.Join(h.GetStorages(), ", ")))},
		output.Pair{Label: "Record owed to", Value: output.Dash(output.Clean(strings.Join(h.GetStoragesPending(), ", ")))})
	if h.GetFoundOnStorage() {
		pairs = append(pairs, output.Pair{Label: "Found on a storage", Value: "yes: its record was found with no row, and kept"})
	}
	return pairs
}

// newHoldList builds "hold list".
func (a *app) newHoldList() *cobra.Command {
	var states []string
	var subject string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant> [--state active|released]... [--subject LABEL]",
		Short: "List a tenant's holds, newest first",
		Long: `List a tenant's holds, newest first, each with what it covers, its state and its
reason, which only an operator or more is answered. Without --state every hold
is listed.`,
		Example: `  nopacs hold list lab1
  nopacs hold list lab1 --state active --subject sub-0017`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			var want []v1.HoldState
			for _, s := range states {
				switch strings.ToLower(s) {
				case "active":
					want = append(want, v1.HoldState_HOLD_STATE_ACTIVE)
				case "released":
					want = append(want, v1.HoldState_HOLD_STATE_RELEASED)
				default:
					return fault.New(fault.Invalid, fmt.Sprintf("--state %q is not a state; use active or released", output.Clean(s)))
				}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			holds, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Hold, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListHoldsResponse, error) {
					return c.Retention.ListHolds(ctx, &v1.ListHoldsRequest{Tenant: args[0], States: want, SubjectLabel: subject, PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetHolds(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "holds", holds, more, func() error {
				rows := make([][]string, 0, len(holds))
				for _, h := range holds {
					rows = append(rows, []string{output.Clean(h.GetId()), output.Clean(output.HoldTarget(h.GetTarget())), output.HoldState(h.GetState()),
						a.when(h.GetPlaceTime()), output.Dash(output.Clean(h.GetReason()))})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"ID", "COVERS", "STATE", "PLACED", "REASON"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringArrayVar(&states, "state", nil, "only holds in this state: active or released; repeat for both")
	cmd.Flags().StringVar(&subject, "subject", "", "only the holds on this subject's label")
	_ = cmd.RegisterFlagCompletionFunc("state", cobra.FixedCompletions([]string{"active", "released"}, cobra.ShellCompDirectiveNoFileComp))
	l.add(cmd)
	return cmd
}

// newHoldShow builds "hold show".
func (a *app) newHoldShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <tenant> <id>",
		Short: "Show one hold with its reason",
		Long: `Show one hold: what it covers, its state, its reason, who placed and released
it and when, which storages hold its record and which are still owed it.`,
		Example: `  nopacs hold show lab1 hold_3Tq9`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetHoldResponse, error) {
				return c.Retention.GetHold(ctx, &v1.GetHoldRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return output.Details(w, a.style(), a.holdDetails(res.GetHold())) })
		}),
	}
}

// newHoldPlace builds "hold place".
func (a *app) newHoldPlace() *cobra.Command {
	var whole bool
	var reason string
	cmd := &cobra.Command{
		Use:   "place <tenant> (<subject>[/<session>[/<scan>]] | --whole-tenant) --reason TEXT",
		Short: "Place a hold on a subject, a session, a scan or the whole tenant",
		Long: `Place a hold. Name what it covers by labels, or with --whole-tenant for all of
the tenant. A subject, session or scan under a committed delete that is not yet
purged can be held by the labels it had: its purge then waits. An active hold
with the same target and reason is answered instead, so a repeated command places
nothing more. The reason is kept with the hold and in the audit log.

A server that does not serve this yet answers so ("not served") and nothing is
held.`,
		Example: `  nopacs hold place lab1 sub-0017 --reason "litigation hold 2026-114"
  nopacs hold place lab1 --whole-tenant --reason "audit in progress"`,
		Args: cobra.RangeArgs(1, 2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			target := &v1.HoldTarget{WholeTenant: whole}
			switch {
			case whole && len(args) == 2:
				return fault.New(fault.Invalid, "give a target or --whole-tenant, not both")
			case !whole && len(args) != 2:
				return fault.New(fault.Invalid, "give what to hold, <subject>[/<session>[/<scan>]], or --whole-tenant")
			case !whole:
				parts, err := splitRef(args[1], 1, 3, "<subject>, <subject>/<session> or <subject>/<session>/<scan>")
				if err != nil {
					return err
				}
				target.SubjectLabel = parts[0]
				if len(parts) > 1 {
					target.SessionLabel = parts[1]
				}
				if len(parts) > 2 {
					target.ScanLabel = parts[2]
				}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.PlaceHoldResponse, error) {
				return c.Retention.PlaceHold(ctx, &v1.PlaceHoldRequest{Tenant: args[0], Target: target, Reason: reason})
			})
			if err != nil {
				return client.OrNotServed(what(cmd), err)
			}
			return messageOut(a, res, func(w io.Writer) error {
				if _, err := fmt.Fprintf(w, "hold %s placed on %s\n", output.Clean(res.GetHold().GetId()), output.Clean(output.HoldTarget(res.GetHold().GetTarget()))); err != nil {
					return err
				}
				return output.Details(w, a.style(), a.holdDetails(res.GetHold()))
			})
		}),
	}
	cmd.Flags().BoolVar(&whole, "whole-tenant", false, "hold everything the tenant holds, now and later")
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with the hold (required)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// newHoldRelease builds "hold release".
func (a *app) newHoldRelease() *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "release <tenant> <id> --reason TEXT [--yes]",
		Short: "Release a hold, so what it held may be deleted and purged again",
		Long: `Release a hold: its record on each primary is rewritten as released (a replica's
too, or owed), then its row; what it held may then be purged, deleted and
disposed of again, as other holds allow. A hold already released is answered as it
stands.

Releasing lets data go, so on a terminal it asks for the hold's id to be typed
again; with --yes it does not; with no terminal and no --yes it releases nothing
and exits 2. A server that does not serve this yet answers so ("not served").`,
		Example: `  nopacs hold release lab1 hold_3Tq9 --reason "litigation closed"
  nopacs hold release lab1 hold_3Tq9 --reason "litigation closed" --yes`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			if err := a.confirmTyped(yes, args[1], "released", fmt.Sprintf("release hold %s; what it holds may then be deleted and purged", output.Clean(args[1]))); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ReleaseHoldResponse, error) {
				return c.Retention.ReleaseHold(ctx, &v1.ReleaseHoldRequest{Tenant: args[0], Id: args[1], Reason: reason})
			})
			if err != nil {
				return client.OrNotServed(what(cmd), err)
			}
			return messageOut(a, res, func(w io.Writer) error {
				if _, err := fmt.Fprintf(w, "hold %s released\n", output.Clean(res.GetHold().GetId())); err != nil {
					return err
				}
				return output.Details(w, a.style(), a.holdDetails(res.GetHold()))
			})
		}),
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept in the audit entry (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for the hold's id to be typed again")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
