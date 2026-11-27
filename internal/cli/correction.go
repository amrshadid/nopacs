package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Every name of a correction's kind and state starts with one of these.
const (
	correctionKindPrefix  = "CORRECTION_KIND_"
	correctionStatePrefix = "CORRECTION_STATE_"
)

// newCorrection builds the correction noun.
func (a *app) newCorrection() *cobra.Command {
	return group("correction", "Delete, relabel, replace or edit archived data, and follow, undo or purge each change",
		`Change archived data on purpose, each change kept as a correction with the
reason a person gave: delete a subject, a session, a scan or one instance;
relabel a subject, a session or a scan; replace an archived instance with the
corrected file a sender sent again, held in quarantine; edit header values of
every instance of a subject, a session, a scan or one instance.

A delete hides its data from every read at once and keeps its bytes, unserved,
for the tenant's grace period (the setting delete.grace, 7d by default), during
which "correction undo" shows the data again as it was. Then the server purges
the bytes from every storage that holds them and records what each storage did
with each object. "correction purge" purges at once. What names deleted data
(a subject's label, a study's, a series' or an instance's UID) is kept after the
purge, and a file that matches it later is quarantined, not archived, until
"correction allow" lets that value arrive again.

Every command takes the tenant first. Deleting, relabeling, replacing, editing,
undoing, purging and allowing need the admin role on the tenant; listing and
showing need operator. Each reason is 1 to 1024 bytes, in your own words, never
a value read from a file.`,
		a.newCorrectionDelete(), a.newCorrectionRelabel(), a.newCorrectionReplace(), a.newCorrectionEdit(), a.newCorrectionList(),
		a.newCorrectionShow(), a.newCorrectionUndo(), a.newCorrectionPurge(), a.newCorrectionAllow())
}

// correctionTarget reads a target given as <subject>[/<session>[/<scan>]],
// with one instance named by instance.
func correctionTarget(arg, instance string) (*v1.CorrectionTarget, error) {
	ref, err := splitRef(arg, 1, 3, "a subject, session or scan: use <subject>[/<session>[/<scan>]]")
	if err != nil {
		return nil, err
	}
	for _, p := range ref {
		if err := label.Check(p); err != nil {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not a label: %s", output.Clean(p), err))
		}
	}
	t := &v1.CorrectionTarget{SubjectLabel: ref[0], InstanceId: instance}
	if len(ref) > 1 {
		t.SessionLabel = ref[1]
	}
	if len(ref) > 2 {
		t.ScanLabel = ref[2]
	}
	if instance != "" && t.ScanLabel == "" {
		return nil, fault.New(fault.Invalid, "--instance names an instance of a scan: give <subject>/<session>/<scan>")
	}
	return t, nil
}

// targetText writes a target as a person reads it.
func targetText(t *v1.CorrectionTarget) string {
	parts := []string{t.GetSubjectLabel()}
	for _, p := range []string{t.GetSessionLabel(), t.GetScanLabel()} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	s := strings.Join(parts, "/")
	if t.GetInstanceId() != "" {
		s += " instance " + t.GetInstanceId()
	}
	return output.Clean(s)
}

// enumWord writes an enum value as a person types it.
func enumWord(name, prefix string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, prefix)), "_", "-")
}

// correctionDetails lists one correction as labeled values.
func (a *app) correctionDetails(c *v1.Correction) []output.Pair {
	n := c.GetCounts()
	pairs := []output.Pair{
		{Label: "ID", Value: c.GetId()},
		{Label: "Kind", Value: enumWord(c.GetKind().String(), correctionKindPrefix)},
		{Label: "State", Value: enumWord(c.GetState().String(), correctionStatePrefix)},
		{Label: "Target", Value: targetText(c.GetTarget())},
		{Label: "Reason", Value: output.Clean(c.GetReason())},
		{Label: "Made by", Value: output.Dash(c.GetCreatedByKeyId())},
		{Label: "Made", Value: a.when(c.GetCreateTime())},
		{Label: "Committed", Value: a.whenOr(c.GetCommitTime(), "-")},
		{Label: "Purge after", Value: a.whenOr(c.GetPurgeTime(), "-")},
		{Label: "Ended", Value: a.whenOr(c.GetEndTime(), "-")},
	}
	if c.GetStateReason() != "" {
		pairs = append(pairs, output.Pair{Label: "Why", Value: output.Clean(c.GetStateReason())})
	}
	switch c.GetKind() {
	case v1.CorrectionKind_CORRECTION_KIND_RELABEL:
		pairs = append(pairs, output.Pair{Label: "New label", Value: output.Clean(c.GetToLabel())},
			output.Pair{Label: "Objects copied", Value: strconv.FormatInt(n.GetObjectsCopied(), 10)})
		if c.GetState() == v1.CorrectionState_CORRECTION_STATE_ABANDONED {
			pairs = append(pairs, output.Pair{Label: "Copies removed", Value: yesNo(c.GetSwept())})
		}
	case v1.CorrectionKind_CORRECTION_KIND_REPLACE:
		pairs = append(pairs, output.Pair{Label: "Quarantined file", Value: output.Clean(c.GetQuarantinedFile())},
			output.Pair{Label: "Old revision", Value: strconv.Itoa(int(c.GetOldRevision())) + " " + c.GetOldSha256()},
			output.Pair{Label: "New revision", Value: strconv.Itoa(int(c.GetNewRevision())) + " " + c.GetNewSha256()})
	}
	if c.GetUndoReason() != "" {
		pairs = append(pairs, output.Pair{Label: "Undo reason", Value: output.Clean(c.GetUndoReason())})
	}
	pairs = append(pairs,
		output.Pair{Label: "Instances", Value: strconv.FormatInt(n.GetInstances(), 10)},
		output.Pair{Label: "Bytes", Value: output.Bytes(n.GetBytes())},
		output.Pair{Label: "Objects owned", Value: strconv.FormatInt(n.GetObjectsOwned(), 10)},
		output.Pair{Label: "Objects removed", Value: strconv.FormatInt(n.GetObjectsRemoved(), 10) + " (" + output.Bytes(n.GetBytesRemoved()) + ")"},
		output.Pair{Label: "Objects kept", Value: strconv.FormatInt(n.GetObjectsKept(), 10)})
	for _, d := range c.GetDispositions() {
		label := "  " + output.Clean(d.GetStorage())
		if d.GetReplicaOf() != "" {
			label += " (replica of " + output.Clean(d.GetReplicaOf()) + ")"
		}
		pairs = append(pairs, output.Pair{Label: label, Value: purgeClaim(d)})
	}
	if len(c.GetCopiesAbandoned()) > 0 {
		pairs = append(pairs, output.Pair{Label: "Copies abandoned", Value: output.Clean(strings.Join(c.GetCopiesAbandoned(), ", ")) +
			": a replica was detached before the purge reached it, so its objects are recorded as unknown and may still hold the bytes"})
	}
	if c.GetKind() == v1.CorrectionKind_CORRECTION_KIND_DELETE {
		pairs = append(pairs, output.Pair{Label: "Arrivals allowed", Value: yesNo(c.GetArrivalsAllowed())})
		if c.GetOrigin() == v1.CorrectionOrigin_CORRECTION_ORIGIN_RETENTION {
			pairs = append(pairs, output.Pair{Label: "Retention rule", Value: output.Clean(c.GetRetentionRule()) + ", due " + a.whenOr(c.GetRetentionDueTime(), "-")})
		}
		if c.GetOverrideRetention() {
			pairs = append(pairs, output.Pair{Label: "Retention overridden", Value: "yes: " + output.Clean(c.GetOverrideReason())})
		}
		for _, h := range c.GetHolds() {
			pairs = append(pairs, output.Pair{Label: "  holds " + enumWord(h.GetKind().String(), "HELD_VALUE_KIND_"), Value: output.Clean(h.GetValue())})
		}
	}
	if c.GetUnresolved() {
		pairs = append(pairs, output.Pair{Label: "Unresolved", Value: "yes: undo or purge it"})
	}
	return pairs
}

// purgeClaim says what one storage did with a correction's purged objects.
func purgeClaim(d *v1.StorageDisposition) string { return output.PurgeClaim(d) }

// correctionOut writes one correction: its JSON document with --json, or
// the line say writes and its details.
func (a *app) correctionOut(msg proto.Message, c *v1.Correction, say string) error {
	return messageOut(a, msg, func(w io.Writer) error {
		if say != "" {
			if _, err := fmt.Fprintln(w, say); err != nil {
				return err
			}
		}
		return output.Details(w, a.style(), a.correctionDetails(c))
	})
}

// waitCorrection asks for a correction until cond holds or ctx ends.
func waitCorrection(ctx context.Context, c *client.Client, cmd *cobra.Command, tenant, id string, cond func(*v1.Correction) bool) (*v1.GetCorrectionResponse, error) {
	for {
		res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetCorrectionResponse, error) {
			return c.Corrections.GetCorrection(ctx, &v1.GetCorrectionRequest{Tenant: tenant, Id: id})
		})
		if err != nil || cond(res.GetCorrection()) {
			return res, err
		}
		select {
		case <-ctx.Done():
			return nil, fault.Wrap(fault.Unavailable, ctx.Err(), "stopped waiting; the correction goes on in the server")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// newCorrectionRelabel builds "correction relabel".
func (a *app) newCorrectionRelabel() *cobra.Command {
	var reason, to string
	var wait bool
	cmd := &cobra.Command{
		Use:   "relabel <tenant> <subject>[/<session>[/<scan>]] --to LABEL --reason TEXT [--wait]",
		Short: "Give a subject, a session or a scan a new label",
		Long: `Give a subject, one session of it, or one scan a new label. Every stored
object of it is copied to its key under the new label and checked, in the
background; readers see the old label until the whole target switches at once.
The old objects stay, unserved, until the correction is purged after the grace
period. A lookup by the old label then answers not found, naming the new one.

The command answers once the relabel is recorded; --wait waits until it is
switched or abandoned. "correction undo" stops a relabel still copying.`,
		Example: `  nopacs correction relabel lab1 sub-3f9a --to sub-0042 --reason "wrong subject id at the scanner"
  nopacs correction relabel lab1 sub-3f9a/ses-81c2 --to ses-baseline --reason "session named by mistake" --wait`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			if err := label.Check(to); err != nil {
				return fault.New(fault.Invalid, fmt.Sprintf("--to %q is not a label: %s", output.Clean(to), err))
			}
			target, err := correctionTarget(args[1], "")
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RelabelResponse, error) {
				return c.Corrections.Relabel(ctx, &v1.RelabelRequest{Tenant: args[0], Target: target, ToLabel: to, Reason: reason})
			})
			if err != nil {
				return err
			}
			if wait {
				got, err := waitCorrection(ctx, c, cmd, args[0], res.GetCorrection().GetId(), func(c *v1.Correction) bool {
					return c.GetState() != v1.CorrectionState_CORRECTION_STATE_PREPARED
				})
				if err != nil {
					return err
				}
				res.Correction = got.GetCorrection()
			}
			say := fmt.Sprintf("relabel %s of %s to %s is %s", res.GetCorrection().GetId(), targetText(target), output.Clean(to),
				enumWord(res.GetCorrection().GetState().String(), correctionStatePrefix))
			return a.correctionOut(res, res.GetCorrection(), say)
		}),
	}
	cmd.Flags().StringVar(&to, "to", "", "the new label (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with the correction (required)")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the relabel is switched or abandoned")
	_ = cmd.MarkFlagRequired("to")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// newCorrectionReplace builds "correction replace".
func (a *app) newCorrectionReplace() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "replace <tenant> <quarantined-file-id> --reason TEXT",
		Short: "Put a corrected file held in quarantine in place of the archived instance",
		Long: `Put a held quarantined file in place of the archived instance with its SOP
Instance UID: a file quarantined as a different file (the corrected file a
sender sent again), or as repairing a damaged instance. The file is first
de-identified as its receiver's profile version says, as a release would, and is
never archived as received when that receiver de-identifies. The new bytes go to
the instance's next revision; the old revision stays, unserved, until the
correction is purged after the grace period. A replace is not undone: the way
back is another replace from the old file.`,
		Example: `  nopacs correction replace lab1 q_41Ab --reason "corrected file sent again by the site"`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ReplaceResponse, error) {
				return c.Corrections.Replace(ctx, &v1.ReplaceRequest{Tenant: args[0], QuarantinedFile: args[1], Reason: reason})
			})
			if err != nil {
				return err
			}
			say := fmt.Sprintf("replace %s is %s", res.GetCorrection().GetId(), enumWord(res.GetCorrection().GetState().String(), correctionStatePrefix))
			return a.correctionOut(res, res.GetCorrection(), say)
		}),
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words, kept with the correction (required)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// newCorrectionList builds "correction list".
func (a *app) newCorrectionList() *cobra.Command {
	var kind, state string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant> [--kind KIND] [--state STATE]",
		Short: "List a tenant's corrections, newest first",
		Long: `List a tenant's corrections, newest first, optionally only those of one kind
or in one state.

A KIND is one of: ` + strings.Join(enumNames(v1.CorrectionKind_name, correctionKindPrefix), ", ") + `.
A STATE is one of: ` + strings.Join(enumNames(v1.CorrectionState_name, correctionStatePrefix), ", ") + `.`,
		Example: `  nopacs correction list lab1
  nopacs correction list lab1 --kind delete --state committed --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			req := &v1.ListCorrectionsRequest{Tenant: args[0]}
			if kind != "" {
				k, err := parseEnum[v1.CorrectionKind]("kind", kind, v1.CorrectionKind_name, correctionKindPrefix)
				if err != nil {
					return err
				}
				req.Kinds = []v1.CorrectionKind{k}
			}
			if state != "" {
				s, err := parseEnum[v1.CorrectionState]("state", state, v1.CorrectionState_name, correctionStatePrefix)
				if err != nil {
					return err
				}
				req.States = []v1.CorrectionState{s}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			items, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.Correction, string, error) {
				page := &v1.ListCorrectionsRequest{Tenant: req.GetTenant(), Kinds: req.GetKinds(), States: req.GetStates(), PageSize: size, PageToken: token}
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListCorrectionsResponse, error) {
					return c.Corrections.ListCorrections(ctx, page)
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetCorrections(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "corrections", items, more, func() error {
				rows := make([][]string, 0, len(items))
				for _, it := range items {
					rows = append(rows, []string{it.GetId(), enumWord(it.GetKind().String(), correctionKindPrefix),
						enumWord(it.GetState().String(), correctionStatePrefix), targetText(it.GetTarget()),
						strconv.FormatInt(it.GetCounts().GetInstances(), 10), a.when(it.GetCreateTime()), a.whenOr(it.GetPurgeTime(), "-")})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"ID", "KIND", "STATE", "TARGET", "INSTANCES", "MADE", "PURGE AFTER"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringVar(&kind, "kind", "", "only the corrections of this kind")
	cmd.Flags().StringVar(&state, "state", "", "only the corrections in this state")
	enumCompletion(cmd, "kind", v1.CorrectionKind_name, correctionKindPrefix)
	enumCompletion(cmd, "state", v1.CorrectionState_name, correctionStatePrefix)
	l.add(cmd)
	return cmd
}

// newCorrectionShow builds "correction show".
func (a *app) newCorrectionShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <tenant> <id>",
		Short: "Show one correction, with how far it got",
		Long: `Show one correction: what it acts on, why, who made it, where it is, and how
far it got. For a purge it says, per storage, what the storage did with each
object: the bytes are gone from a storage only where every object is removed;
objects hidden behind older versions, retained by object lock or unknown are
said so. For a delete it lists the values it holds arrivals by.`,
		Example: `  nopacs correction show lab1 01jd2k9w6m3x4c5v6b7n8p9q0r`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetCorrectionResponse, error) {
				return c.Corrections.GetCorrection(ctx, &v1.GetCorrectionRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			return a.correctionOut(res, res.GetCorrection(), "")
		}),
	}
}

// newCorrectionUndo builds "correction undo".
func (a *app) newCorrectionUndo() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "undo <tenant> <id> --reason TEXT",
		Short: "Undo a delete or an edit in its grace period, or stop a relabel still copying",
		Long: `Show a committed delete's data again, as it was, during its grace period; put
every instance a committed edit changed back at the revision it superseded,
removing its new revisions; or stop a relabel that is still copying, removing its
copies. A correction already undone is answered as it stands. A delete or an
edit whose purge has started cannot be undone (exit 5).`,
		Example: `  nopacs correction undo lab1 01jd2k9w6m3x4c5v6b7n8p9q0r --reason "wrong subject was deleted"`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.UndoCorrectionResponse, error) {
				return c.Corrections.UndoCorrection(ctx, &v1.UndoCorrectionRequest{Tenant: args[0], Id: args[1], Reason: reason})
			})
			if err != nil {
				return err
			}
			say := fmt.Sprintf("correction %s is %s", res.GetCorrection().GetId(), enumWord(res.GetCorrection().GetState().String(), correctionStatePrefix))
			return a.correctionOut(res, res.GetCorrection(), say)
		}),
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why it is undone, kept with the correction (required)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// newCorrectionPurge builds "correction purge".
func (a *app) newCorrectionPurge() *cobra.Command {
	var yes, wait bool
	cmd := &cobra.Command{
		Use:   "purge <tenant> <id> [--wait] [--yes]",
		Short: "Purge a correction's hidden or superseded bytes now, before its grace period ends",
		Long: `Start the purge of a committed correction now: a delete's hidden data, or the
objects a relabel or a replace superseded. It also confirms a delete a rebuild
or a restore found without its row. The purge runs in the server; --wait waits
until it ends and shows what each storage did. Once a purge has started nothing
is undone.

This cannot be undone. Without --yes the command asks for the id to be typed
again on a terminal; with no terminal it sends nothing and exits 2.`,
		Example: `  nopacs correction purge lab1 01jd2k9w6m3x4c5v6b7n8p9q0r --wait
  nopacs correction purge lab1 01jd2k9w6m3x4c5v6b7n8p9q0r --yes --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := a.confirmTyped(yes, args[1], "purged", fmt.Sprintf("remove for good the bytes correction %q hid or superseded, from every storage", output.Clean(args[1]))); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.PurgeCorrectionResponse, error) {
				return c.Corrections.PurgeCorrection(ctx, &v1.PurgeCorrectionRequest{Tenant: args[0], Id: args[1]})
			})
			if err != nil {
				return err
			}
			if wait {
				got, err := waitCorrection(ctx, c, cmd, args[0], args[1], func(c *v1.Correction) bool {
					return c.GetState() == v1.CorrectionState_CORRECTION_STATE_PURGED
				})
				if err != nil {
					return err
				}
				res.Correction = got.GetCorrection()
			}
			say := fmt.Sprintf("correction %s is %s", res.GetCorrection().GetId(), enumWord(res.GetCorrection().GetState().String(), correctionStatePrefix))
			return a.correctionOut(res, res.GetCorrection(), say)
		}),
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until the purge ends")
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for the id to be typed again")
	return cmd
}

// newCorrectionAllow builds "correction allow".
func (a *app) newCorrectionAllow() *cobra.Command {
	var reason, subject, study, series, instance string
	cmd := &cobra.Command{
		Use:   "allow <tenant> <id> (--subject LABEL | --study UID | --series UID | --instance UID) --reason TEXT",
		Short: "Let one value a purged delete holds arrive again",
		Long: `Let one value a purged delete holds arrive again ("correction show" lists
them): a file whose subject label, or Study, Series or SOP Instance UID, is that
value is archived from then on instead of quarantined as deleted, as when a
consent withdrawn is given again. A file quarantined as deleted kept none of its
bytes, so the sender sends it again once its values are allowed. One value at a
time, so a withdrawal is never lifted wider than meant.`,
		Example: `  nopacs correction allow lab1 01jd2k9w6m3x4c5v6b7n8p9q0r --subject sub-3f9a --reason "consent given again"
  nopacs correction allow lab1 01jd2k9w6m3x4c5v6b7n8p9q0r --study 2.25.1234 --reason "consent given again"`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := checkReason(reason); err != nil {
				return err
			}
			var values []*v1.HeldValue
			for _, v := range []struct {
				s string
				k v1.HeldValueKind
			}{{subject, v1.HeldValueKind_HELD_VALUE_KIND_SUBJECT}, {study, v1.HeldValueKind_HELD_VALUE_KIND_STUDY},
				{series, v1.HeldValueKind_HELD_VALUE_KIND_SERIES}, {instance, v1.HeldValueKind_HELD_VALUE_KIND_INSTANCE}} {
				if v.s != "" {
					values = append(values, &v1.HeldValue{Kind: v.k, Value: v.s})
				}
			}
			if len(values) != 1 {
				return fault.New(fault.Invalid, "name exactly one value: --subject, --study, --series or --instance")
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.AllowArrivalsResponse, error) {
				return c.Corrections.AllowArrivals(ctx, &v1.AllowArrivalsRequest{Tenant: args[0], Id: args[1], Reason: reason, Value: values[0]})
			})
			if err != nil {
				return err
			}
			return a.correctionOut(res, res.GetCorrection(), "the value may arrive again")
		}),
	}
	cmd.Flags().StringVar(&subject, "subject", "", "the subject label to let arrive again")
	cmd.Flags().StringVar(&study, "study", "", "the Study Instance UID to let arrive again")
	cmd.Flags().StringVar(&series, "series", "", "the Series Instance UID to let arrive again")
	cmd.Flags().StringVar(&instance, "instance", "", "the SOP Instance UID to let arrive again")
	cmd.Flags().StringVar(&reason, "reason", "", "why, kept with the correction (required)")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}
