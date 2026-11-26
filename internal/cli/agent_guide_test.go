package cli_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/cli"
)

// listedCommands returns the path of every command of the tree the agent
// guide must list, walked here on its own: every command but a hidden one and
// the ones cobra adds.
func listedCommands() []string {
	var out []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			out = append(out, sub.CommandPath())
			walk(sub)
		}
	}
	walk(cli.Command(cli.Env{}))
	return out
}

// TestAgentGuideListsEveryCommand guards the agent guide: its --json lists
// every command of the tree, each once, none more, with its usage line,
// summary and flags; its version and the exit codes are there; a hidden
// command is not; the text form names every command's usage; and it needs
// no server.
func TestAgentGuideListsEveryCommand(t *testing.T) {
	want := listedCommands()
	if len(want) < 100 {
		t.Fatalf("the tree has %d commands; the walk is broken", len(want))
	}
	doc := document(t, runWith(map[string]string{}, "agent-guide", "--json"))
	if doc["guideVersion"] != float64(cli.AgentGuideVersion) || len(doc["exitCodes"].([]any)) != 8 || !strings.Contains(doc["values"].(string), "--show-values") {
		t.Errorf("the agent guide's head: version %v, exit codes %v, values %v", doc["guideVersion"], doc["exitCodes"], doc["values"])
	}
	listed := map[string]map[string]any{}
	for _, c := range doc["commands"].([]any) {
		m := c.(map[string]any)
		path := m["path"].(string)
		if listed[path] != nil {
			t.Errorf("%q is listed twice", path)
		}
		listed[path] = m
	}
	for _, p := range want {
		if listed[p] == nil {
			t.Errorf("the agent guide does not list %q", p)
		}
	}
	if len(listed) != len(want) {
		t.Errorf("the agent guide lists %d commands, the tree has %d", len(listed), len(want))
	}
	if listed["nopacs init"] != nil || listed["nopacs help"] != nil {
		t.Error("the agent guide lists a hidden or a built-in command")
	}
	create := listed["nopacs key create"]
	if create == nil || !strings.HasPrefix(create["usage"].(string), "nopacs key create") || create["summary"] == "" {
		t.Fatalf("key create: %v", create)
	}
	var masked bool
	for _, f := range create["flags"].([]any) {
		if f.(map[string]any)["name"] == "--masked" {
			masked = true
		}
	}
	if !masked {
		t.Errorf("key create's flags lack --masked: %v", create["flags"])
	}

	text := runWith(map[string]string{}, "agent-guide")
	if text.code != 0 || text.stderr != "" || !strings.HasPrefix(text.stdout, "nopacs agent guide, version 1") {
		t.Fatalf("agent-guide: %+v", text)
	}
	for _, p := range want {
		if !strings.Contains(text.stdout, "\n"+p) {
			t.Errorf("the text guide does not name %q", p)
		}
	}
}
