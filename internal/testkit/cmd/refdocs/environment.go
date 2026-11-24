package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// programVars are the variables the two programs read. Each is a default for
// a flag of the same meaning, so a script sets it once instead of repeating
// the flag. renderEnvironment refuses one the source no longer mentions.
var programVars = map[string]string{
	"NOPACS_CA_FILE":         "`nopacs`: the file of certificate authorities to trust for `--server`, instead of the system's. Default of `--ca-file`.",
	"NOPACS_CONFIG":          "`nopacs`: the file that holds the saved contexts, instead of `$XDG_CONFIG_HOME/nopacs/contexts.json`. It must be in a private directory of your own.",
	"NOPACS_CONTEXT":         "`nopacs`: the name of the saved context to use. Default of `--context`.",
	"NOPACS_DATA_DIR":        "both programs: the installation's data directory. Default of `--data-dir`.",
	"NOPACS_KEY":             "`nopacs`: the API key to send with `--server`. A command that uses a saved context is refused while it is set, so an exported key never travels to whichever context is current. The alternative is `--key-file`; a key is never an argument.",
	"NOPACS_SERVER":          "`nopacs`: the address of a server on the network, `https://host:port`. Default of `--server`.",
	"NOPACS_TLS_CLIENT_CERT": "`nopacs`: the PEM file of the TLS client certificate to present to `--server`. Default of `--tls-client-cert`.",
	"NOPACS_TLS_CLIENT_KEY":  "`nopacs`: the PEM file of that certificate's private key. Default of `--tls-client-key`.",
}

// internalVars are variables a test, a proof script or the interruption
// harness sets for a process it starts, to carry a value across the process
// boundary. They are no setting of anyone's: a person never sets them.
var internalVars = map[string]string{
	"NOPACS_PROOF_AUTH":              "Carries a key from a proof script to the header of one `curl` call, so it is not in the process list.",
	"NOPACS_PROOF_CRASH":             "Carries a Postgres address from a proof script to the commands it kills, named by `--db-env`.",
	"NOPACS_PROOF_DSN":               "Carries a Postgres address from a proof script to `tenant create --db-env`.",
	"NOPACS_PROOF_REMOTE":            "Carries an address that cannot be reached from a proof script to `tenant create --db-env`, to see the refusal.",
	"NOPACS_RECEIVE_CRASH_DEID":      "Tells the crash test's child process which de-identification crash point to stop at.",
	"NOPACS_RECEIVE_CRASH_RELEASE":   "Tells the crash test's child process which release crash point to stop at.",
	"NOPACS_RECEIVE_CRASH_ROUTE":     "Tells the crash test's child process which routing crash point to stop at.",
	"NOPACS_RECEIVE_CRASH_UNMATCHED": "Tells the crash test's child process which crash point of an unrouted file to stop at.",
	"NOPACS_TEST_SILENT_DB":          "Carries a database address that never answers to a test of `init`, named by `--master-env`.",
}

var (
	envName = regexp.MustCompile(`\bNOPACS_[A-Z0-9_]*[A-Z0-9]\b`)
	// docRow is a row of a table of docs/develop/testing.md whose first cell
	// names variables in backticks.
	docRow = regexp.MustCompile("^\\| (`NOPACS_[^|]*) \\| ([^|]*) \\|")
)

// scanVars returns every variable named in the source the programs, tests and
// scripts are made of, and whether a program (not a test) names it.
func scanVars(root string) (all map[string]bool, program map[string]bool, err error) {
	all, program = map[string]bool{}, map[string]bool{}
	for _, top := range []string{"cmd", "internal", "scripts", "Makefile"} {
		err = filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && path == filepath.Join(root, top) {
					return fs.SkipAll
				}
				return err
			}
			if d.IsDir() {
				if d.Name() == "refdocs" || d.Name() == "node_modules" || d.Name() == "__pycache__" {
					return fs.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".go" && ext != ".py" && ext != ".sh" && d.Name() != "Makefile" {
				return nil
			}
			text, err := os.ReadFile(path) //nolint:gosec // G122: a development tool reading the repository's own checkout, which nobody swaps for links meanwhile.
			if err != nil {
				return err
			}
			isProgram := ext == ".go" && !strings.HasSuffix(path, "_test.go") && !strings.Contains(filepath.ToSlash(path), "/testkit/")
			for _, name := range envName.FindAllString(string(text), -1) {
				all[name] = true
				if isProgram {
					program[name] = true
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return all, program, nil
}

// testDocs reads the variables docs/develop/testing.md describes: the first
// column names them, the second says what each does.
func testDocs(root string) (map[string]string, error) {
	text, err := os.ReadFile(filepath.Join(root, "docs", "develop", "testing.md"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(text), "\n") {
		m := docRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, name := range envName.FindAllString(m[1], -1) {
			out[name] = strings.TrimSpace(m[2])
		}
	}
	return out, nil
}

// renderEnvironment lists the variables of the programs, then those of tests
// and checks, from what the source names.
func renderEnvironment(root string) (string, error) {
	all, program, err := scanVars(root)
	if err != nil {
		return "", err
	}
	docs, err := testDocs(root)
	if err != nil {
		return "", err
	}
	for name := range programVars {
		if !program[name] {
			return "", fmt.Errorf("programVars lists %s, which no program source names", name)
		}
	}
	for name := range internalVars {
		if !all[name] {
			return "", fmt.Errorf("internalVars lists %s, which no source names", name)
		}
	}
	var tests []string
	for name := range all {
		if _, ok := programVars[name]; ok {
			continue
		}
		if _, ok := internalVars[name]; ok {
			continue
		}
		if _, ok := docs[name]; !ok {
			return "", fmt.Errorf("%s is named in the source and described nowhere: add a row to docs/develop/testing.md, or to internalVars if it only carries a value to a process a test starts", name)
		}
		tests = append(tests, name)
	}
	slices.Sort(tests)

	var b strings.Builder
	b.WriteString("Every variable beginning `NOPACS_` that the source names. A flag always beats its variable.\n\n" +
		"## Read by the programs\n\n| Variable | What it does |\n|---|---|\n")
	for _, name := range slices.Sorted(keys(programVars)) {
		fmt.Fprintf(&b, "| `%s` | %s |\n", name, cell(programVars[name]))
	}
	b.WriteString("\nNo variable holds a secret except `NOPACS_KEY`; a database address or a storage credential is named by a flag " +
		"(`--db-env`, `--master-env`, `--credential-env`) that points at a variable of the caller's own choosing.\n\n" +
		"## Read by tests, checks and proofs\n\n" +
		"These are for people who change the code. A test that needs an outside tool or a service skips when its variable is unset, " +
		"unless the variable that requires it is set. How each is used is in [Testing](../develop/testing.md).\n\n" +
		"| Variable | What it does |\n|---|---|\n")
	for _, name := range tests {
		fmt.Fprintf(&b, "| `%s` | %s |\n", name, cell(docs[name]))
	}
	b.WriteString("\n## Set by a test or a script for a process it starts\n\n" +
		"Nobody sets these by hand.\n\n| Variable | What it does |\n|---|---|\n")
	for _, name := range slices.Sorted(keys(internalVars)) {
		fmt.Fprintf(&b, "| `%s` | %s |\n", name, cell(internalVars[name]))
	}
	return b.String(), nil
}

// keys returns the keys of m as a sequence for slices.Sorted.
func keys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
