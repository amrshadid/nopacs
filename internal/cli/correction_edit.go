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
	"github.com/amrshadid/nopacs/internal/fault"
)

// editChanges reads --set Tag=Value and --remove Tag as an edit's changes,
// each set first, in the order given.
func editChanges(set, remove []string) ([]*v1.EditChange, error) {
	var out []*v1.EditChange
	for _, s := range set {
		k, v, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("--set %q is not Tag=Value", output.Clean(s)))
		}
		out = append(out, &v1.EditChange{Attribute: strings.TrimSpace(k), Value: v})
	}
	for _, k := range remove {
		if strings.TrimSpace(k) == "" {
			return nil, fault.New(fault.Invalid, "--remove names an attribute")
		}
		out = append(out, &v1.EditChange{Attribute: strings.TrimSpace(k), Remove: true})
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Invalid, "name what to change with --set Tag=Value or --remove Tag")
	}
	return out, nil
}

// editAttributeText writes what one change does to one instance.
func editAttributeText(a *v1.EditPreviewAttribute, show bool) string {
	now := "absent"
	switch {
	case !a.GetKnown():
		now = "not known"
	case a.GetPresent() && show:
		now = strconv.Quote(output.Clean(a.GetOldValue()))
	case a.GetPresent():
		now = "present"
	}
	if a.GetRemove() {
		return now + " -> removed"
	}
	return now + " -> " + strconv.Quote(output.Clean(a.GetNewValue()))
}

// editPreviewDetails lists what an edit would change as labeled values:
// its target, its storages, its token, then each instance and attribute.
func editPreviewDetails(t *v1.CorrectionTarget, p *v1.EditPreview, show bool) []output.Pair {
	pairs := []output.Pair{
		{Label: "Target", Value: targetText(t)},
		{Label: "Instances", Value: strconv.Itoa(len(p.GetInstances()))},
		{Label: "Storages", Value: output.Dash(output.Clean(strings.Join(p.GetStorages(), ", ")))},
		{Label: "Confirm", Value: output.Clean(p.GetConfirm())},
	}
	for _, in := range p.GetInstances() {
		where := output.Clean(in.GetSubjectLabel()+"/"+in.GetSessionLabel()+"/"+in.GetScanLabel()) + " " + output.Clean(in.GetInstanceId())
		pairs = append(pairs, output.Pair{Label: where, Value: "revision " + strconv.Itoa(int(in.GetRevision()))})
		for _, a := range in.GetAttributes() {
			pairs = append(pairs, output.Pair{Label: "  " + output.Clean(a.GetAttribute()), Value: editAttributeText(a, show)})
		}
	}
	return pairs
}

// newCorrectionEdit builds "correction edit".
func (a *app) newCorrectionEdit() *cobra.Command {
	var reason, instance string
	var set, remove []string
	var preview, showValues bool
	cmd := &cobra.Command{
		Use:   "edit <tenant> <subject>[/<session>[/<scan>]] [--instance UID] (--set Tag=Value | --remove Tag)... --reason TEXT [--preview] [--show-values]",
		Short: "Set, change or remove header values of archived instances",
		Long: `Set, change or remove named attributes of every instance of a subject, a
session, a scan or, with --instance, one instance of a scan. Each instance gets
a later revision written by the DICOM library; the revision it supersedes stays,
unserved, until the correction is purged after the grace period, and "correction
undo" takes the edit back until then.

An attribute is named by its keyword (SeriesDescription) or its tag as
gggg,eeee; a value is printable ASCII, several values separated by a backslash.
Refused: the Study, Series and SOP Instance UIDs, the SOP Class UID, the
Specific Character Set, the Study Date, Pixel Data and what lays out its bytes,
private attributes, binary VRs, and a value that does not fit its VR. Each new
revision is judged by the tenant's profile and its check after before it is
written: an attribute the profile takes out is refused, and so is a value that
puts back one the profile removed. The check after cannot tell a real name typed
into an attribute the profile keeps: type no identifier.

The command first asks the server for a preview: every instance and attribute,
values hidden unless --show-values (recorded in the audit log). --preview shows
it and stops; without it the command shows it and edits with its token, which
holds only while the target is unchanged. A masked key is refused.`,
		Example: `  nopacs correction edit lab1 sub-3f9a/ses-81c2/scan-7 --set SeriesDescription="T1 axial" --reason "series named wrongly at the scanner" --preview
  nopacs correction edit lab1 sub-3f9a/ses-81c2 --remove StationName --reason "station named by the site"
  nopacs correction edit lab1 sub-3f9a --set 0020,0011=12 --reason "series number typed wrongly" --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			changes, err := editChanges(set, remove)
			if err != nil {
				return err
			}
			target, err := correctionTarget(args[1], instance)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			req := &v1.EditRequest{Tenant: args[0], Target: target, Changes: changes, Reason: reason, ValidateOnly: true, ShowValues: showValues}
			prev, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.EditResponse, error) { return c.Corrections.Edit(ctx, req) })
			if err != nil {
				return err
			}
			showPreview := func(w io.Writer) error {
				return output.Details(w, a.style(), editPreviewDetails(target, prev.GetPreview(), showValues))
			}
			if preview {
				return messageOut(a, prev, showPreview)
			}
			if !a.opts.json {
				if err := a.write(showPreview); err != nil {
					return err
				}
			}
			edit := &v1.EditRequest{Tenant: args[0], Target: target, Changes: changes, Reason: reason, Confirm: prev.GetPreview().GetConfirm()}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.EditResponse, error) { return c.Corrections.Edit(ctx, edit) })
			if err != nil {
				return err
			}
			cor := res.GetCorrection()
			say := fmt.Sprintf("edit %s of %s is %s", cor.GetId(), targetText(target), enumWord(cor.GetState().String(), correctionStatePrefix))
			if cor.GetState() == v1.CorrectionState_CORRECTION_STATE_COMMITTED {
				say += fmt.Sprintf("; undo with: nopacs correction undo %s %s --reason TEXT", output.Clean(args[0]), cor.GetId())
			}
			return a.correctionOut(res, cor, say)
		}),
	}
	cmd.Flags().StringArrayVar(&set, "set", nil, "Tag=Value to set or change, by keyword or gggg,eeee (repeatable)")
	cmd.Flags().StringArrayVar(&remove, "remove", nil, "Tag to remove, by keyword or gggg,eeee (repeatable)")
	cmd.Flags().StringVar(&instance, "instance", "", "edit only this instance of the scan, by its id")
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with the correction (required)")
	cmd.Flags().BoolVar(&preview, "preview", false, "show what the edit would change, and change nothing")
	cmd.Flags().BoolVar(&showValues, "show-values", false, "show each attribute's value now in the preview (recorded in the audit log)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
