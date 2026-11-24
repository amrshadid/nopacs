package cli_test

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/cli"
)

// TestEveryExampleParses guards the examples in each command's help: every
// line that runs nopacs names a command that exists, with flags that command
// has and as many arguments as it takes. An example once used a flag that
// did not exist (--since for --from), and a person who copied it was told
// the flag is unknown.
func TestEveryExampleParses(t *testing.T) {
	var lines []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		// A line that ends in a backslash goes on in the next, as in a
		// shell.
		text := strings.ReplaceAll(c.Example, "\\\n", " ")
		for _, line := range strings.Split(text, "\n") {
			if words := exampleWords(line); len(words) > 0 {
				lines = append(lines, strings.Join(words, "\x00"))
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cli.Command(cli.Env{}))
	if len(lines) < 40 {
		t.Fatalf("only %d example lines found", len(lines))
	}
	for _, line := range lines {
		words := strings.Split(line, "\x00")
		// A command tree per line: parsing flags sets the command's
		// variables, which must not carry over to the next line.
		root := cli.Command(cli.Env{})
		cmd, rest, err := root.Find(words[1:])
		if err != nil || cmd == root {
			t.Errorf("%q: no such command (%v)", strings.Join(words, " "), err)
			continue
		}
		if err := cmd.ParseFlags(rest); err != nil {
			t.Errorf("%q: %v", strings.Join(words, " "), err)
			continue
		}
		if err := cmd.ValidateArgs(cmd.Flags().Args()); err != nil {
			t.Errorf("%q: %v", strings.Join(words, " "), err)
		}
	}
}

// exampleWords splits an example line into the words a shell would give
// nopacs, from the word "nopacs" on: variables set before it are dropped, a
// quoted word is one word without its quotes, a comment (from a "#" that
// starts a word) is dropped, and a line that does not run nopacs gives none.
func exampleWords(line string) []string {
	var words []string
	var word strings.Builder
	inWord, quote := false, rune(0)
scan:
	for _, r := range strings.TrimSpace(line) {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(r)
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '#' && !inWord:
			break scan
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	for i, w := range words {
		if w == "nopacs" {
			return words[i:]
		}
		if !strings.Contains(w, "=") {
			return nil
		}
	}
	return nil
}
