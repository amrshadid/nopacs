package cli

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// destructiveVerbs are the verbs that remove or stop something for good, or
// change archived data (an edit, a move, a re-run, a quarantined file
// released into the archive). Their tenant is always named, whatever a
// context holds: a default that was never looked at must not be the tenant
// something is deleted from or rewritten in, even where an undo exists. A
// command with a --yes or --confirm flag is held to the same rule, whatever
// it is called.
var destructiveVerbs = []string{
	"remove", "delete", "delete-batch", "purge", "cancel", "discard", "reject",
	"detach", "rebuild", "dispose", "switch",
	"edit", "move", "merge", "relabel", "replace", "undo", "allow", "rerun", "release",
}

// tenantAsName lists the commands whose first argument is a tenant although
// their usage names it <name>.
var tenantAsName = []string{"tenant show"}

// takesDefaultTenant says whether a command may have its tenant filled from
// the context: its first argument is the tenant and nothing makes it
// destructive.
func takesDefaultTenant(c *cobra.Command) bool {
	if c.RunE == nil || c.Args == nil || c.HasSubCommands() {
		return false
	}
	path := what(c)
	fields := strings.Fields(c.Use)
	first := ""
	if len(fields) > 1 {
		first = fields[1]
	}
	if first != "<tenant>" && !slices.Contains(tenantAsName, path) {
		return false
	}
	if slices.Contains(destructiveVerbs, c.Name()) {
		return false
	}
	return c.Flags().Lookup("yes") == nil && c.Flags().Lookup("confirm") == nil
}

// fillTenantArg wraps every command that takes the tenant as its first
// argument so that the context's default tenant stands in for it. It does so
// only where that cannot be misread: the arguments given must not be a valid
// set as they are, and must become one with the tenant put in front. A command
// whose later arguments are optional (catalog sessions <tenant> [<subject>])
// therefore keeps requiring the tenant when one argument is given, since that
// argument could be either. The tenant used is said on standard error.
func (a *app) fillTenantArg(root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if !takesDefaultTenant(c) {
			return
		}
		args, run := c.Args, c.RunE
		// withTenant returns the arguments with the default tenant in front, or
		// false when the arguments are complete as given, there is no default
		// tenant, or even with it they would not be.
		withTenant := func(cmd *cobra.Command, given []string) ([]string, bool) {
			if args(cmd, given) == nil {
				return nil, false
			}
			sel, err := a.selectContext(cmd)
			if err != nil || sel == nil || sel.Tenant == "" {
				return nil, false
			}
			filled := append([]string{sel.Tenant}, given...)
			if args(cmd, filled) != nil {
				return nil, false
			}
			return filled, true
		}
		c.Args = func(cmd *cobra.Command, given []string) error {
			if _, ok := withTenant(cmd, given); ok {
				return nil
			}
			return args(cmd, given)
		}
		c.RunE = func(cmd *cobra.Command, given []string) error {
			if filled, ok := withTenant(cmd, given); ok {
				a.line("using tenant " + filled[0] + " from context " + a.picked.sel.name)
				given = filled
			}
			return run(cmd, given)
		}
	}
	walk(root)
}
