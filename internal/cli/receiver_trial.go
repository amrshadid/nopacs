package cli

import (
	"context"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// newReceiverTryProfile builds "receiver try-profile".
func (a *app) newReceiverTryProfile() *cobra.Command {
	var profileRef string
	cmd := &cobra.Command{
		Use:   "try-profile <tenant> <receiver> --profile <name>[@<version>]",
		Short: "Try a profile version over what a receiver last received, before pointing it at the version",
		Long: `Run a profile version over the first archived file of each scan of the receiver's
newest sessions (the setting deid.change-trial-sessions says how many), as
"deid rerun --preview" runs it, which changes nothing. The answer says how many
files the version would change, leave and hold, which elements it changes with
its action and in how many files, and why files would be held; it holds no
value of any file. The result is recorded: "receiver set --profile" then points
the receiver at the version only when it was tried and none of its files would
be held, or with --force and a reason. It needs the admin role, never a masked key.`,
		Example: `  nopacs receiver try-profile lab1 scanner1 --profile research@5
  nopacs receiver try-profile lab1 scanner1 --profile research --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			ref, err := parseVersionArg(profileRef)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.TryReceiverProfileResponse, error) {
				return c.Receivers.TryReceiverProfile(ctx, &v1.TryReceiverProfileRequest{Tenant: args[0], Receiver: args[1], Profile: ref.Name, ProfileVersion: ref.Version})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.changeTrialOut(w, res) })
		}),
	}
	cmd.Flags().StringVar(&profileRef, "profile", "", "the profile version to try, <name>[@<version>]; required")
	_ = cmd.MarkFlagRequired("profile")
	return cmd
}

// changeTrialOut writes a trial's counts, its elements and its holds.
func (a *app) changeTrialOut(w io.Writer, r *v1.TryReceiverProfileResponse) error {
	if err := output.Details(w, a.style(), []output.Pair{
		{Label: "Profile", Value: r.GetProfile() + "@" + strconv.Itoa(int(r.GetProfileVersion()))},
		{Label: "Sessions", Value: strconv.FormatInt(r.GetSessions(), 10)}, {Label: "Files", Value: strconv.FormatInt(r.GetFiles(), 10)},
		{Label: "Would change", Value: strconv.FormatInt(r.GetChanged(), 10)}, {Label: "Would hold", Value: strconv.FormatInt(r.GetHeld(), 10)},
		{Label: "Unchanged", Value: strconv.FormatInt(r.GetUnchanged(), 10)}, {Label: "Not judged", Value: strconv.FormatInt(r.GetSkipped(), 10)}}); err != nil {
		return err
	}
	if len(r.GetElements()) > 0 {
		rows := make([][]string, 0, len(r.GetElements()))
		for _, e := range r.GetElements() {
			rows = append(rows, []string{e.GetTag(), e.GetAction(), strconv.FormatInt(e.GetFiles(), 10)})
		}
		if err := output.Table(w, a.style(), []string{"ELEMENT", "ACTION", "FILES"}, rows); err != nil {
			return err
		}
	}
	if len(r.GetHolds()) > 0 {
		rows := make([][]string, 0, len(r.GetHolds()))
		for _, h := range r.GetHolds() {
			rows = append(rows, []string{h.GetReason(), strconv.FormatInt(h.GetFiles(), 10)})
		}
		return output.Table(w, a.style(), []string{"HELD FOR", "FILES"}, rows)
	}
	return nil
}
