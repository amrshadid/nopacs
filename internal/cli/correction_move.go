package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

// movePreviewDetails lists what a move or a merge would do as labeled
// values: what moves, where to, the counts, the storages, what refuses it
// and its token.
func (a *app) movePreviewDetails(what, to string, p *v1.MovePreview) []output.Pair {
	c := p.GetCounts()
	return []output.Pair{
		{Label: "Moves", Value: output.Clean(what)},
		{Label: "Under", Value: output.Clean(to)},
		{Label: "Sessions", Value: output.Dash(output.Clean(strings.Join(p.GetSessions(), ", ")))},
		{Label: "Scans", Value: strconv.FormatInt(c.GetScans(), 10)},
		{Label: "Instances", Value: strconv.FormatInt(c.GetInstances(), 10)},
		{Label: "Bytes", Value: strconv.FormatInt(c.GetBytes(), 10)},
		{Label: "Storages", Value: output.Dash(output.Clean(strings.Join(p.GetStorages(), ", ")))},
		{Label: "Holds", Value: output.Dash(output.Clean(strings.Join(p.GetHolds(), ", ")))},
		{Label: "Labels held", Value: output.Dash(output.Clean(strings.Join(p.GetClashes(), ", ")))},
		{Label: "Old objects purged", Value: a.whenOr(p.GetPurgeTime(), "-")},
		{Label: "Confirm", Value: output.Clean(p.GetConfirm())},
	}
}

// subjectLabel returns arg as a subject's label, or why it is not one.
func subjectLabel(arg, what string) (string, error) {
	if err := label.Check(arg); err != nil {
		return "", fault.New(fault.Invalid, fmt.Sprintf("%s %q is not a label: %s", what, output.Clean(arg), err))
	}
	return arg, nil
}

// moveRun is what a move or a merge command does with the server, its
// request made by ask (validate_only or with the token given).
type moveRun struct {
	tenant, what, to string
	preview          bool
	ask              func(ctx context.Context, c *client.Client, confirm string) (*v1.MovePreview, *v1.Correction, error)
}

// runMove previews, shows the preview, and makes the move with its token
// unless only the preview was asked.
func (a *app) runMove(ctx context.Context, cmd *cobra.Command, m moveRun) error {
	c, closeClient, err := a.open(cmd)
	if err != nil {
		return err
	}
	defer closeClient()
	prev, _, err := m.ask(ctx, c, "")
	if err != nil {
		return err
	}
	show := func(w io.Writer) error { return output.Details(w, a.style(), a.movePreviewDetails(m.what, m.to, prev)) }
	if m.preview {
		return messageOut(a, prev, show)
	}
	if !a.opts.json {
		if err := a.write(show); err != nil {
			return err
		}
	}
	_, cor, err := m.ask(ctx, c, prev.GetConfirm())
	if err != nil {
		return err
	}
	say := fmt.Sprintf("move %s of %s under %s is %s; it copies in the background (nopacs correction show %s %s)", cor.GetId(),
		output.Clean(m.what), output.Clean(m.to), enumWord(cor.GetState().String(), correctionStatePrefix), output.Clean(m.tenant), cor.GetId())
	return a.correctionOut(cor, cor, say)
}

// newCorrectionMove builds "correction move".
func (a *app) newCorrectionMove() *cobra.Command {
	var reason, to string
	var preview, rewrite bool
	cmd := &cobra.Command{
		Use:   "move <tenant> <subject>/<session> --to <subject> --reason TEXT [--preview] [--rewrite-identity]",
		Short: "File a session under another subject",
		Long: `File a session, with its scans, under another subject the tenant holds: a
study filed under the wrong patient. Every object is copied to its key under the
other subject, read back and compared by SHA-256, and the session switches at
once; the old objects stay until the correction is purged after the grace
period, and "correction undo" files the session back until then. A lookup by
the old labels answers where the session is now.

Refused: a session label the other subject holds (relabel one first), a hold on
either subject, a correction on either not ended, a reception of the session in
progress, a subject's last session (use "correction merge"), and a masked key.

With --rewrite-identity, once the move is published, every moved instance is
given the other subject's archived Patient ID and Issuer of Patient ID by an
edit of the session (a correction of its own), each new revision judged by the
tenant's profile and its check after; an edit refused is abandoned with its
reason and the move stands.

The command first asks the server for a preview: what moves, the storages it
reaches, the holds and labels that refuse it. --preview shows it and stops;
without it the command shows it and moves with its token, which holds only
while what moves is unchanged.`,
		Example: `  nopacs correction move lab1 sub-3f9a/ses-81c2 --to sub-77c0 --reason "study filed under the wrong patient" --preview
  nopacs correction move lab1 sub-3f9a/ses-81c2 --to sub-77c0 --reason "study filed under the wrong patient" --rewrite-identity --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			ref, err := splitRef(args[1], 2, 2, "a session: use <subject>/<session>")
			if err != nil {
				return err
			}
			for _, p := range ref {
				if _, err := subjectLabel(p, "the label"); err != nil {
					return err
				}
			}
			dest, err := subjectLabel(to, "--to")
			if err != nil {
				return err
			}
			return a.runMove(ctx, cmd, moveRun{tenant: args[0], what: ref[0] + "/" + ref[1], to: dest, preview: preview,
				ask: func(ctx context.Context, c *client.Client, confirm string) (*v1.MovePreview, *v1.Correction, error) {
					req := &v1.MoveRequest{Tenant: args[0], SubjectLabel: ref[0], SessionLabel: ref[1], ToSubjectLabel: dest, Reason: reason,
						ValidateOnly: confirm == "", Confirm: confirm, RewriteIdentity: rewrite}
					res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.MoveResponse, error) { return c.Corrections.Move(ctx, req) })
					return res.GetPreview(), res.GetCorrection(), err
				}})
		}),
	}
	cmd.Flags().StringVar(&to, "to", "", "the label of the subject the session goes under (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with the correction (required)")
	cmd.Flags().BoolVar(&preview, "preview", false, "show what the move would do, and change nothing")
	cmd.Flags().BoolVar(&rewrite, "rewrite-identity", false, "give the moved instances the other subject's archived Patient ID and issuer")
	_ = cmd.MarkFlagRequired("to")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// newCorrectionMerge builds "correction merge".
func (a *app) newCorrectionMerge() *cobra.Command {
	var reason, into string
	var preview, rewrite bool
	cmd := &cobra.Command{
		Use:   "merge <tenant> <subject> --into <subject> --reason TEXT [--preview] [--rewrite-identity]",
		Short: "Merge a subject into another",
		Long: `Merge a whole subject into another the tenant holds: one patient archived
under two subjects. Every session moves as "correction move" moves one, in one
correction published at once, and the emptied subject is hidden; its label is
held for arrivals until the merge is purged, and a lookup by it answers where its
sessions are now. "correction undo" makes the subject again with every session
until the purge.

Refused as a move is: a session label the other subject holds, a hold on
either subject, a correction on either not ended, a reception of the subject in
progress, and a masked key. --rewrite-identity and --preview are a move's.`,
		Example: `  nopacs correction merge lab1 sub-3f9a --into sub-77c0 --reason "one patient under two ids" --preview
  nopacs correction merge lab1 sub-3f9a --into sub-77c0 --reason "one patient under two ids" --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			from, err := subjectLabel(args[1], "the subject")
			if err != nil {
				return err
			}
			dest, err := subjectLabel(into, "--into")
			if err != nil {
				return err
			}
			return a.runMove(ctx, cmd, moveRun{tenant: args[0], what: from, to: dest, preview: preview,
				ask: func(ctx context.Context, c *client.Client, confirm string) (*v1.MovePreview, *v1.Correction, error) {
					req := &v1.MergeRequest{Tenant: args[0], SubjectLabel: from, IntoSubjectLabel: dest, Reason: reason,
						ValidateOnly: confirm == "", Confirm: confirm, RewriteIdentity: rewrite}
					res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.MergeResponse, error) { return c.Corrections.Merge(ctx, req) })
					return res.GetPreview(), res.GetCorrection(), err
				}})
		}),
	}
	cmd.Flags().StringVar(&into, "into", "", "the label of the subject it merges into (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with the correction (required)")
	cmd.Flags().BoolVar(&preview, "preview", false, "show what the merge would do, and change nothing")
	cmd.Flags().BoolVar(&rewrite, "rewrite-identity", false, "give the moved instances the other subject's archived Patient ID and issuer")
	_ = cmd.MarkFlagRequired("into")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
