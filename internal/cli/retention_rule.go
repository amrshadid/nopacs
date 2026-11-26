package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// retentionActionNames are what a person may type for a rule's action.
var retentionActionNames = []string{"review", "delete"}

// newRetentionRule builds "retention rule".
func (a *app) newRetentionRule() *cobra.Command {
	return group("rule", "Add, change, remove, list and show a tenant's retention rules",
		`A rule has a name, which is its id and never changes, a selector (every
session of the tenant, or those archived through one receiver, one route of it,
or one primary storage), a period to keep sessions at least, a period after
which they are due, and an action. Periods are whole years and days counted from
a session's start: 7y, 90d or 2y90d. Of the rules that match a session, the
latest keep and the earliest dispose apply, and the session is deleted by a
rule's action only when every rule whose dispose period matches it says delete.

Every change of a rule applies at once, and what it would make due sooner is
held back for the tenant's notice. All of these need the admin role on the
tenant, but list and show, which need the reader role.`,
		a.newRuleAdd(), a.newRuleSet(), a.newRuleRemove(), a.newRuleList(), a.newRuleShow())
}

// ruleDefFlags are the flags that describe a rule, for add and for set.
type ruleDefFlags struct {
	receiver, route, storage string
	keep, dispose, action    string
}

// add registers the flags on a command.
func (f *ruleDefFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.receiver, "receiver", "", "match the sessions archived through this receiver; empty for every session")
	fl.StringVar(&f.route, "route", "", "with --receiver: match only the scans archived by this route of it")
	fl.StringVar(&f.storage, "storage", "", "match the sessions archived first to this primary storage (not with --receiver)")
	fl.StringVar(&f.keep, "keep-at-least", "", `how long a matched session is kept at least, such as 7y or 90d; "none" for no minimum`)
	fl.StringVar(&f.dispose, "dispose-after", "", `how long after its start a matched session is due, such as 10y; not shorter than --keep-at-least; "none" to only keep`)
	fl.StringVar(&f.action, "action", "", "what is done with due sessions: review (list them for a person, the default) or delete (the daily pass deletes them)")
	_ = cmd.RegisterFlagCompletionFunc("action", cobra.FixedCompletions(retentionActionNames, cobra.ShellCompDirectiveNoFileComp))
}

// selector builds the rule's selector from the flags; nil for every session.
func (f *ruleDefFlags) selector() *v1.RetentionSelector {
	if f.receiver == "" && f.route == "" && f.storage == "" {
		return nil
	}
	return &v1.RetentionSelector{Receiver: f.receiver, Route: f.route, Storage: f.storage}
}

// periodFlag reads a --keep-at-least or --dispose-after value: nil for "none".
func periodFlag(flag, s string) (*v1.RetentionPeriod, error) {
	if strings.EqualFold(strings.TrimSpace(s), "none") {
		return nil, nil
	}
	p, err := output.ParseRetentionPeriod(s)
	if err != nil {
		return nil, fault.New(fault.Invalid, fmt.Sprintf("--%s %q is not a period: %s", flag, output.Clean(s), err))
	}
	return p, nil
}

// parseRetentionAction reads --action.
func parseRetentionAction(s string) (v1.RetentionAction, error) {
	switch strings.ToLower(s) {
	case "review":
		return v1.RetentionAction_RETENTION_ACTION_REVIEW, nil
	case "delete":
		return v1.RetentionAction_RETENTION_ACTION_DELETE, nil
	}
	return 0, fault.New(fault.Invalid, fmt.Sprintf("--action %q is not an action; use review or delete", output.Clean(s)))
}

// ruleDetails lists a rule as labeled values.
func (a *app) ruleDetails(r *v1.RetentionRule) []output.Pair {
	pairs := []output.Pair{
		{Label: "Name", Value: output.Clean(r.GetName())},
		{Label: "Matches", Value: output.Clean(output.RetentionSelector(r.GetSelector()))},
		{Label: "Keep at least", Value: output.RetentionPeriod(r.GetKeepAtLeast())},
		{Label: "Due after", Value: output.RetentionPeriod(r.GetDisposeAfter())},
		{Label: "Action", Value: output.RetentionAction(r.GetAction())},
		{Label: "Sessions due now", Value: strconv.FormatInt(r.GetSessionsDue(), 10)},
		{Label: "Created", Value: a.when(r.GetCreateTime())},
		{Label: "Changed", Value: a.when(r.GetUpdateTime())},
	}
	if r.GetNoticeUntil() != nil {
		pairs = append(pairs, output.Pair{Label: "Change held back until", Value: a.when(r.GetNoticeUntil())})
	}
	return pairs
}

// newRuleAdd builds "retention rule add".
func (a *app) newRuleAdd() *cobra.Command {
	var f ruleDefFlags
	cmd := &cobra.Command{
		Use:   "add <tenant> <name> [--receiver R [--route RT] | --storage S] [--keep-at-least P] [--dispose-after P] [--action review|delete]",
		Short: "Add a retention rule to a tenant",
		Long: `Add a rule. At least one of --keep-at-least and --dispose-after is needed; a
period is whole years and days (7y, 90d, 2y90d), at most 100 years, and
--dispose-after is not shorter than --keep-at-least. Without --action the rule
only lists what is due (review). A rule that keeps applies at once; what it
would make due is held back for the tenant's notice.

A name is 1 to 63 lowercase letters, digits and hyphens, starting with a letter.
A receiver, route or storage the tenant does not have is refused, and a replica
is never a selector's storage, since nothing is archived to it first.`,
		Example: `  nopacs retention rule add lab1 seven-years --keep-at-least 7y --dispose-after 10y --action review
  nopacs retention rule add lab1 scanner-days --receiver scanner1 --route research --keep-at-least 180d`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			rule := &v1.RetentionRule{Name: args[1], Selector: f.selector()}
			var err error
			if cmd.Flags().Changed("keep-at-least") {
				if rule.KeepAtLeast, err = periodFlag("keep-at-least", f.keep); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("dispose-after") {
				if rule.DisposeAfter, err = periodFlag("dispose-after", f.dispose); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("action") {
				if rule.Action, err = parseRetentionAction(f.action); err != nil {
					return err
				}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.AddRetentionRuleResponse, error) {
				return c.Retention.AddRetentionRule(ctx, &v1.AddRetentionRuleRequest{Tenant: args[0], Rule: rule})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return output.Details(w, a.style(), a.ruleDetails(res.GetRule())) })
		}),
	}
	f.add(cmd)
	return cmd
}

// newRuleSet builds "retention rule set".
func (a *app) newRuleSet() *cobra.Command {
	var f ruleDefFlags
	cmd := &cobra.Command{
		Use:   "set <tenant> <name> [--receiver R [--route RT] | --storage S] [--keep-at-least P] [--dispose-after P] [--action review|delete]",
		Short: "Change some fields of a retention rule",
		Long: `Change the fields of a rule that the flags name, and no other. A period given as
"none" removes it. The selector is changed as a whole: give any of --receiver,
--route and --storage and the selector becomes exactly what is given, and
--receiver "" returns the rule to every session. The change applies at once, and
what it would make due sooner is held back for the tenant's notice.`,
		Example: `  nopacs retention rule set lab1 seven-years --dispose-after 12y
  nopacs retention rule set lab1 seven-years --action delete
  nopacs retention rule set lab1 scanner-days --receiver "" --keep-at-least none --dispose-after 20y`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			rule := &v1.RetentionRule{Name: args[1]}
			var paths []string
			var err error
			if f.given(cmd, "receiver", "route", "storage") {
				rule.Selector = f.selector()
				paths = append(paths, "selector")
			}
			if cmd.Flags().Changed("keep-at-least") {
				if rule.KeepAtLeast, err = periodFlag("keep-at-least", f.keep); err != nil {
					return err
				}
				paths = append(paths, "keep_at_least")
			}
			if cmd.Flags().Changed("dispose-after") {
				if rule.DisposeAfter, err = periodFlag("dispose-after", f.dispose); err != nil {
					return err
				}
				paths = append(paths, "dispose_after")
			}
			if cmd.Flags().Changed("action") {
				if rule.Action, err = parseRetentionAction(f.action); err != nil {
					return err
				}
				paths = append(paths, "action")
			}
			if len(paths) == 0 {
				return fault.New(fault.Invalid, "nothing to change: give at least one flag, such as --dispose-after")
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetRetentionRuleResponse, error) {
				return c.Retention.SetRetentionRule(ctx, &v1.SetRetentionRuleRequest{Tenant: args[0], Rule: rule, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return output.Details(w, a.style(), a.ruleDetails(res.GetRule())) })
		}),
	}
	f.add(cmd)
	return cmd
}

// given says whether any of the named flags was set on the command line.
func (f *ruleDefFlags) given(cmd *cobra.Command, names ...string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// newRuleRemove builds "retention rule remove".
func (a *app) newRuleRemove() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <tenant> <name> [--yes]",
		Short: "Remove a retention rule",
		Long: `Remove a rule at once. What it kept is kept until the tenant's notice ends, so a
removal never makes data due in a moment; adding the rule again keeps it at once.
Corrections and jobs the rule made keep its name. A repeated command finds no
rule and exits 4.

On a terminal it asks for the rule's name to be typed again; with --yes it does
not; with no terminal and no --yes it removes nothing and exits 2.`,
		Example: `  nopacs retention rule remove lab1 seven-years
  nopacs retention rule remove lab1 seven-years --yes`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := a.confirmTyped(yes, args[1], "removed", fmt.Sprintf("remove retention rule %s of tenant %s; what it kept stays kept until the notice ends", output.Clean(args[1]), output.Clean(args[0]))); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.RemoveRetentionRuleResponse, error) {
				return c.Retention.RemoveRetentionRule(ctx, &v1.RemoveRetentionRuleRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "retention rule %s of tenant %s removed\n", output.Clean(res.GetRule().GetName()), output.Clean(args[0]))
				return err
			})
		}),
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for the rule's name to be typed again")
	return cmd
}

// newRuleList builds "retention rule list".
func (a *app) newRuleList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant>",
		Short: "List a tenant's retention rules",
		Long: `List a tenant's rules ordered by name, with what each matches, its two periods,
its action and how many sessions are due under it now.`,
		Example: `  nopacs retention rule list lab1
  nopacs retention rule list lab1 --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			rules, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.RetentionRule, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListRetentionRulesResponse, error) {
					return c.Retention.ListRetentionRules(ctx, &v1.ListRetentionRulesRequest{Tenant: args[0], PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetRules(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "rules", rules, more, func() error {
				rows := make([][]string, 0, len(rules))
				for _, r := range rules {
					rows = append(rows, []string{output.Clean(r.GetName()), output.Clean(output.RetentionSelector(r.GetSelector())), output.RetentionPeriod(r.GetKeepAtLeast()),
						output.RetentionPeriod(r.GetDisposeAfter()), output.RetentionAction(r.GetAction()), strconv.FormatInt(r.GetSessionsDue(), 10)})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"NAME", "MATCHES", "KEEP AT LEAST", "DUE AFTER", "ACTION", "DUE NOW"}, rows)
				})
			})
		}),
	}
	l.add(cmd)
	return cmd
}

// newRuleShow builds "retention rule show".
func (a *app) newRuleShow() *cobra.Command {
	return &cobra.Command{
		Use:   "show <tenant> <name>",
		Short: "Show one retention rule",
		Long: `Show one rule: what it matches, its periods and action, the sessions due under it
now, when it was added and last changed, and, while a change of it is held back
by the tenant's notice, until when.`,
		Example: `  nopacs retention rule show lab1 seven-years`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetRetentionRuleResponse, error) {
				return c.Retention.GetRetentionRule(ctx, &v1.GetRetentionRuleRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return output.Details(w, a.style(), a.ruleDetails(res.GetRule())) })
		}),
	}
}
