package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/version"
)

// AgentGuideVersion is the version of the agent guide's own shape: it
// changes only when one of its fields is removed or changes its meaning, so an
// agent that read one version knows what the next holds. The commands it
// lists are this build's, whatever the version.
const AgentGuideVersion = 1

// guide is the agent guide as --json writes it.
type guide struct {
	GuideVersion int            `json:"guideVersion"`
	Build        string         `json:"build"`
	JSON         string         `json:"json"`
	ExitCodes    []guideExit    `json:"exitCodes"`
	Values       string         `json:"values"`
	GlobalFlags  []guideFlag    `json:"globalFlags"`
	Commands     []guideCommand `json:"commands"`
}

// guideExit is one exit code and what it means.
type guideExit struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

// guideCommand is one command of the tree.
type guideCommand struct {
	Path    string      `json:"path"`
	Usage   string      `json:"usage"`
	Summary string      `json:"summary"`
	Flags   []guideFlag `json:"flags"`
}

// guideFlag is one flag of a command.
type guideFlag struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default,omitempty"`
	Usage   string `json:"usage"`
}

// guideExitCodes are the exit codes every command keeps, as the root's help
// states them.
var guideExitCodes = []guideExit{
	{0, "done"},
	{1, "failed"},
	{2, "wrong usage"},
	{3, "refused: not authenticated, or not allowed (a masked key asking for an original value among them)"},
	{4, "not found"},
	{5, "a state that forbids the change"},
	{6, "a position that can no longer be answered"},
	{7, "a bound of the server reached"},
}

// The agent guide's fixed sentences: how --json is shaped, and what a masked key
// and the preview commands answer.
const (
	guideJSON = `With --json every command writes one JSON object on standard output: ` +
		`"schemaVersion" first, then the fields of the API's answer (package nopacs.v1) under their protobuf JSON names (lowerCamelCase); ` +
		`a list writes its items under the plural name the API gives them and "more", true when the server held more than were fetched. ` +
		`An error is one line on standard error and the exit code below; nothing is written to standard output.`
	guideValues = `Previews and trials (profile try, quarantine show --header) show no value of a file unless --show-values asks, ` +
		`which the server records in its audit log. A masked key (key create --masked), the key an agent should hold, ` +
		`is never answered an original value: --show-values, profile try and explain --record, quarantine show --header, deid code, ` +
		`data archived as received and the other originals are refused with exit 3, whatever its role; it tries the files it sends. ` +
		`It never chooses where archived data goes either: send jobs, replicas, storages, senders and forwarding receivers are refused with exit 3.`
)

// newAgentGuide builds "nopacs agent-guide".
func (a *app) newAgentGuide() *cobra.Command {
	return &cobra.Command{
		Use:   "agent-guide",
		Short: "Print a short description of every command and its JSON, for an AI agent to read",
		Long: `Print what an AI agent needs to drive this program: every command with its
usage, summary and flags, the shape of --json output, the exit codes, and what
a masked key and the preview commands answer. It is made from this program's
own command tree each time, so it lists exactly the commands this build has.
It does not contact a server. With --json it is one document, its own version
in "guideVersion"; each command's --help says more.`,
		Example: `  nopacs agent-guide
  nopacs agent-guide --json`,
		Args: noArgs,
		RunE: a.run(func(_ context.Context, cmd *cobra.Command, _ []string) error {
			g := buildGuide(cmd.Root())
			if a.opts.json {
				return a.write(func(w io.Writer) error { return output.JSONValue(w, g) })
			}
			return a.write(func(w io.Writer) error { return writeGuide(w, g) })
		}),
	}
}

// buildGuide walks the tree under root, in the order the help lists it,
// leaving out hidden commands and the ones cobra adds (help, completion).
func buildGuide(root *cobra.Command) guide {
	g := guide{
		GuideVersion: AgentGuideVersion, Build: version.Get().String(),
		JSON: guideJSON, ExitCodes: guideExitCodes, Values: guideValues,
		GlobalFlags: flagsOf(root.PersistentFlags()), Commands: []guideCommand{},
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if !GuideLists(sub) {
				continue
			}
			g.Commands = append(g.Commands, guideCommand{
				Path: sub.CommandPath(), Usage: sub.UseLine(), Summary: sub.Short, Flags: flagsOf(sub.LocalNonPersistentFlags()),
			})
			walk(sub)
		}
	}
	walk(root)
	return g
}

// GuideLists reports whether the agent guide lists c: every command of the
// tree but a hidden one (a command that only says where it moved) and the
// ones cobra adds itself.
func GuideLists(c *cobra.Command) bool {
	return !c.Hidden && c.Name() != "help" && c.Name() != "completion"
}

// flagsOf lists a flag set in name order, without help.
func flagsOf(fs *pflag.FlagSet) []guideFlag {
	out := []guideFlag{}
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		def := f.DefValue
		if def == "false" || def == "[]" {
			def = ""
		}
		out = append(out, guideFlag{Name: "--" + f.Name, Type: f.Value.Type(), Default: def, Usage: f.Usage})
	})
	return out
}

// writeGuide writes the agent guide as plain text.
func writeGuide(w io.Writer, g guide) error {
	var b strings.Builder
	fmt.Fprintf(&b, "nopacs agent guide, version %d (%s)\n\n", g.GuideVersion, g.Build)
	fmt.Fprintf(&b, "JSON: %s\n\nValues: %s\n\nExit codes:\n", g.JSON, g.Values)
	for _, e := range g.ExitCodes {
		fmt.Fprintf(&b, "  %d  %s\n", e.Code, e.Meaning)
	}
	b.WriteString("\nGlobal flags:\n")
	writeGuideFlags(&b, g.GlobalFlags)
	b.WriteString("\nCommands:\n")
	for _, c := range g.Commands {
		fmt.Fprintf(&b, "\n%s\n  %s\n", c.Usage, c.Summary)
		writeGuideFlags(&b, c.Flags)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeGuideFlags writes one line per flag.
func writeGuideFlags(b *strings.Builder, flags []guideFlag) {
	for _, f := range flags {
		line := "  " + f.Name + " (" + f.Type
		if f.Default != "" {
			line += ", default " + f.Default
		}
		fmt.Fprintf(b, "%s): %s\n", line, f.Usage)
	}
}
