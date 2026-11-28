package cli

import (
	"context"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// describeEffective runs "profile describe --effective": the profile a
// receiver de-identifies with, stacked over the tenant's base profile, one
// row for each selector with the action that decides it and the layer it came
// from.
func (a *app) describeEffective(ctx context.Context, cmd *cobra.Command, args []string, document string) error {
	if len(args) != 2 || document != "" {
		return fault.New(fault.Invalid, "--effective takes a tenant and a receiver, and no --document")
	}
	c, closeClient, err := a.open(cmd)
	if err != nil {
		return err
	}
	defer closeClient()
	res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DescribeProfileResponse, error) {
		return c.Profiles.DescribeProfile(ctx, &v1.DescribeProfileRequest{Tenant: args[0], EffectiveReceiver: args[1]})
	})
	if err != nil {
		return a.withProblems(err)
	}
	return messageOut(a, res, func(w io.Writer) error {
		if err := output.Details(w, a.style(), []output.Pair{
			{Label: "Receiver", Value: output.Clean(args[1])},
			{Label: "Profile", Value: output.Clean(res.GetVersion().GetName()) + "@" + strconv.Itoa(int(res.GetVersion().GetVersion()))},
			{Label: "Elements", Value: strconv.Itoa(len(res.GetEffective()))},
		}); err != nil {
			return err
		}
		rows := make([][]string, 0, len(res.GetEffective()))
		for _, e := range res.GetEffective() {
			action := e.GetAction()
			if e.GetConditional() {
				action += " (under conditions)"
			}
			rows = append(rows, []string{output.Clean(e.GetWhat()), action, e.GetLayer(), output.Clean(e.GetLayerProfile()),
				strconv.Itoa(int(e.GetRule())), output.Dash(output.Clean(e.GetRuleName()))})
		}
		if _, err := io.WriteString(w, "\nThe strictest action for each element, and the layer it came from:\n"); err != nil {
			return err
		}
		return output.Table(w, a.style(), []string{"ELEMENT", "ACTION", "LAYER", "PROFILE", "RULE", "NAME"}, rows)
	})
}
