package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/amrshadid/nopacs/internal/cli"
	"github.com/amrshadid/nopacs/internal/servecmd"
)

// noEnv reads no environment variable, so a default that depends on the
// machine the page is made on never reaches the page.
func noEnv(string) string { return "" }

// renderCommands writes both programs' command trees, as the programs'
// own command constructors define them.
func renderCommands(string) (string, error) {
	var b strings.Builder
	b.WriteString("Every command of the two programs, with the flags each takes. `nopacs <command> --help` prints the same,\n" +
		"with examples. Each program also has the commands `help` and `completion`, which the command library adds:\n" +
		"`completion bash` (also `zsh`, `fish` and `powershell`) writes the shell's completion script.\n\n" +
		"- [nopacs](#nopacs), the command line: a client of the API that links no database or storage driver.\n" +
		"- [nopacs-server](#nopacs-server): the server, and the commands that touch the data directory while it is stopped.\n\n")
	client := cli.Command(cli.Env{Getenv: noEnv})
	server := servecmd.Command(servecmd.Env{Getenv: noEnv})
	for _, root := range []*cobra.Command{client, server} {
		fmt.Fprintf(&b, "## %s\n\n", root.Name())
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(root.Long))
		if flags := flagTable(root.PersistentFlags()); flags != "" {
			b.WriteString("Flags every command takes:\n\n" + flags + "\n")
		}
		writeCommands(&b, root, 3)
	}
	return b.String(), nil
}

// writeCommands writes the visible subcommands of parent, depth first, each
// under a heading of level.
func writeCommands(b *strings.Builder, parent *cobra.Command, level int) {
	subs := slices.Clone(parent.Commands())
	slices.SortFunc(subs, func(x, y *cobra.Command) int { return strings.Compare(x.Name(), y.Name()) })
	for _, c := range subs {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		fmt.Fprintf(b, "%s `%s`\n\n", strings.Repeat("#", min(level, 6)), c.CommandPath())
		if c.Short != "" {
			fmt.Fprintf(b, "%s\n\n", c.Short)
		}
		if c.Runnable() {
			fmt.Fprintf(b, "Usage: `%s`\n\n", c.UseLine())
		}
		if flags := flagTable(c.NonInheritedFlags()); flags != "" {
			b.WriteString(flags + "\n")
		}
		writeCommands(b, c, level+1)
	}
}

// flagTable writes the visible flags of set as a table, or nothing when it
// has none.
func flagTable(set *pflag.FlagSet) string {
	var rows []string
	set.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" || f.Name == "version" {
			return
		}
		name := "`--" + f.Name + "`"
		if f.Shorthand != "" {
			name += ", `-" + f.Shorthand + "`"
		}
		def := ""
		switch f.DefValue {
		case "", "[]", "false", "0", "0s":
		default:
			def = "`" + f.DefValue + "`"
		}
		rows = append(rows, fmt.Sprintf("| %s | %s | %s | %s |", name, f.Value.Type(), def, cell(f.Usage)))
	})
	if len(rows) == 0 {
		return ""
	}
	return "| Flag | Type | Default | What it does |\n|---|---|---|---|\n" + strings.Join(rows, "\n") + "\n"
}
