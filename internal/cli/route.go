package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// maxRuleSetFile bounds a rule set file read: 128 rules of conditions of
// 256 KiB together fit with room to spare.
const maxRuleSetFile = 4 << 20

// newRoute builds "route".
func (a *app) newRoute() *cobra.Command {
	return group("route", "Make, read and try a receiver's routing rule sets",
		`A receiver's routing is a rule set: a numbered version, never changed once
made, which the receiver runs once it is activated. Archive rules run in order
over the first file of a new session's first reception, as received: the first
whose condition holds chooses the storage and, optionally, the profile version
its files are de-identified with in place of the receiver's. Forward rules all
run: each whose condition holds forwards the reception to its senders, whole
or only the series its series filter holds for, optionally de-identified for
the destination by a profile that may only tighten the archive's.

Conditions are written in the condition language a search uses too, such as
  Modality in ("CT", "MR") and SeriesDescription contains "t1"
  calling_ae == "SCANNER1" or (0029,"SIEMENS CSA HEADER",10) present
An attribute the file does not hold makes a comparison false. No condition may
read who the patient is (Patient's Name and ID, birth date, any person's name).

A rule set is written as a JSON file (the API's RuleSet): create reads one,
export writes one to edit. explain shows, rule by rule, the comparison that
decided it, its tag and VR, never the file's values unless --show-values;
dry-run tries a version over the receiver's latest sessions; hits counts what
each rule routed. Creating and activating need admin on the tenant; explain and
dry-run operator; the rest reader.`,
		a.newRouteCreate(), a.newRouteGet(), a.newRouteList(), a.newRouteActivate(), a.newRouteExport(),
		a.newRouteExplain(), a.newRouteDryRun(), a.newRouteHits())
}

// readRuleSetFile reads a rule set file from a path, or standard input for
// "-", as the API's RuleSet in its JSON form, strictly: a field RuleSet does
// not have is refused, so a misspelled field is an error, never ignored.
func (a *app) readRuleSetFile(from string) (*v1.RuleSet, error) {
	var b []byte
	var err error
	if from == "-" {
		if a.env.Stdin == nil {
			return nil, fault.New(fault.Invalid, "- has no standard input to read")
		}
		b, err = io.ReadAll(io.LimitReader(a.env.Stdin, maxRuleSetFile+1))
	} else {
		var f *os.File
		if f, err = os.Open(from); err == nil { // #nosec G304 -- the person names the file to read.
			b, err = io.ReadAll(io.LimitReader(f, maxRuleSetFile+1))
			_ = f.Close()
		}
	}
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read "+from)
	}
	if len(b) > maxRuleSetFile {
		return nil, fault.New(fault.Invalid, from+" is larger than 4MiB")
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fault.New(fault.Invalid, from+" is not a JSON object: "+output.Clean(err.Error()))
	}
	// The schemaVersion "export" writes is the command line's, not the
	// rule set's: it is checked and dropped.
	if raw, ok := doc["schemaVersion"]; ok {
		var v int
		if json.Unmarshal(raw, &v) != nil || v != output.SchemaVersion {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%s is not of schemaVersion %d, which this command reads", from, output.SchemaVersion))
		}
		delete(doc, "schemaVersion")
		if b, err = json.Marshal(doc); err != nil {
			return nil, fault.Wrap(fault.Internal, err, "reading the rule set")
		}
	}
	rs := &v1.RuleSet{}
	if err := protojson.Unmarshal(b, rs); err != nil {
		return nil, fault.New(fault.Invalid, from+" is not a rule set (the API's RuleSet as JSON): "+output.Clean(err.Error()))
	}
	return rs, nil
}

// newRouteCreate builds "route create".
func (a *app) newRouteCreate() *cobra.Command {
	var activate bool
	var comment string
	cmd := &cobra.Command{
		Use:   "create <tenant> <receiver> FILE|- [--activate] [--comment TEXT]",
		Short: "Make a receiver's next rule set version from a file, and activate it",
		Long: `Make a receiver's next rule set version from a JSON file (the API's RuleSet:
archiveRules, quarantineUnmatched, forwardRules, comment), as "route export"
writes one. With --activate the receiver runs it at once, in the same change;
an activation is refused (exit 5, nothing stored) when a rule names a storage,
sender or profile version the tenant does not have, or a destination profile
would keep more of a file than the archive does. --comment replaces the file's.`,
		Example: `  nopacs route create lab1 scanner1 rules.json --activate --comment "MR to the research storage"
  nopacs route create lab1 scanner2 scanner1-rules.json`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			rs, err := a.readRuleSetFile(args[2])
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("comment") {
				rs.Comment = comment
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateRuleSetResponse, error) {
				return c.Routes.CreateRuleSet(ctx, &v1.CreateRuleSetRequest{Tenant: args[0], Receiver: args[1], RuleSet: rs, Activate: activate})
			})
			if err != nil {
				return err
			}
			return a.ruleSetOut(res.GetRuleSet())
		}),
	}
	cmd.Flags().BoolVar(&activate, "activate", false, "make the receiver run the new version at once")
	cmd.Flags().StringVar(&comment, "comment", "", "why the version is made, at most 1024 bytes on one line")
	return cmd
}

// versionFlag registers --version on cmd.
func versionFlag(cmd *cobra.Command, v *int, what string) {
	cmd.Flags().IntVar(v, "version", 0, what)
}

// newRouteGet builds "route get".
func (a *app) newRouteGet() *cobra.Command {
	var version int
	cmd := &cobra.Command{
		Use:   "get <tenant> <receiver> [--version N]",
		Short: "Show the rule set a receiver runs, or one of its versions",
		Long: `Show a receiver's rule set: the version it runs, or --version N. Each rule is
shown with its condition, as written; with --json the API's RuleSet.`,
		Example: `  nopacs route get lab1 scanner1
  nopacs route get lab1 scanner1 --version 2 --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			v, err := boundedInt32("version", version, 0, 1000000)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetRuleSetResponse, error) {
				return c.Routes.GetRuleSet(ctx, &v1.GetRuleSetRequest{Tenant: args[0], Receiver: args[1], Version: v})
			})
			if err != nil {
				return err
			}
			return a.ruleSetOut(res.GetRuleSet())
		}),
	}
	versionFlag(cmd, &version, "the version to show; 0, the default, for the one the receiver runs")
	return cmd
}

// ruleSetOut writes a rule set: the API's message with --json, and
// otherwise its rules as tables.
func (a *app) ruleSetOut(rs *v1.RuleSet) error {
	return messageOut(a, rs, func(w io.Writer) error { return a.ruleSetTable(w, rs) })
}

// profileText writes a profile version reference, or what stands for none.
func profileText(p *v1.ProfileVersionRef, none string) string {
	if p == nil {
		return none
	}
	return p.GetName() + "@" + strconv.Itoa(int(p.GetVersion()))
}

// whenText writes a condition, or what an empty one holds for.
func whenText(s, empty string) string {
	if s == "" {
		return empty
	}
	return output.Clean(s)
}

// ruleSetTable writes a rule set's heading, its archive rules and its
// forward rules.
func (a *app) ruleSetTable(w io.Writer, rs *v1.RuleSet) error {
	state := ""
	if rs.GetActive() {
		state = " (running)"
	}
	if _, err := fmt.Fprintf(w, "Rule set %s@%d%s, made %s by %s\n", output.Clean(rs.GetReceiver()), rs.GetVersion(), state,
		a.when(rs.GetCreateTime()), output.Dash(rs.GetCreatedByKey())); err != nil {
		return err
	}
	if rs.GetComment() != "" {
		if _, err := fmt.Fprintf(w, "Comment: %s\n", output.Clean(rs.GetComment())); err != nil {
			return err
		}
	}
	rows := make([][]string, 0, len(rs.GetArchiveRules()))
	for i, r := range rs.GetArchiveRules() {
		rows = append(rows, []string{strconv.Itoa(i + 1), r.GetName(), whenText(r.GetWhen(), "every study"), r.GetStorage(),
			profileText(r.GetProfile(), "receiver's")})
	}
	if _, err := fmt.Fprintln(w, "\nArchive rules (the first that holds chooses):"); err != nil {
		return err
	}
	if err := output.Table(w, a.style(), []string{"#", "RULE", "WHEN", "STORAGE", "PROFILE"}, rows); err != nil {
		return err
	}
	otherwise := "the receiver's storage and profile"
	if rs.GetQuarantineUnmatched() {
		otherwise = "held as unrouted"
	}
	if _, err := fmt.Fprintf(w, "A study no archive rule chooses: %s\n\nForward rules (each that holds forwards):\n", otherwise); err != nil {
		return err
	}
	rows = rows[:0]
	for _, r := range rs.GetForwardRules() {
		rows = append(rows, []string{r.GetName(), whenText(r.GetWhen(), "every reception"), whenText(r.GetSeries(), "every series"),
			strings.Join(r.GetSenders(), ", "), profileText(r.GetDestinationProfile(), "as archived")})
	}
	return output.Table(w, a.style(), []string{"RULE", "WHEN", "SERIES", "SENDERS", "COPY PROFILE"}, rows)
}

// newRouteList builds "route list".
func (a *app) newRouteList() *cobra.Command {
	var lf listFlags
	cmd := &cobra.Command{
		Use:   "list <tenant> <receiver> [--limit N | --all]",
		Short: "List a receiver's rule set versions, the newest first",
		Long: `List a receiver's rule set versions, the newest first, each with when and by
which key it was made, how many rules it has, its comment, and which one the
receiver runs.`,
		Example: `  nopacs route list lab1 scanner1`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := lf.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			items, more, err := fetchPages(ctx, lf, func(ctx context.Context, size int32, token string) ([]*v1.RuleSet, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListRuleSetsResponse, error) {
					return c.Routes.ListRuleSets(ctx, &v1.ListRuleSetsRequest{Tenant: args[0], Receiver: args[1], PageSize: size, PageToken: token})
				})
				return res.GetRuleSets(), res.GetNextPageToken(), err
			})
			if err != nil {
				return err
			}
			return listOut(a, "ruleSets", items, more, func() error {
				return a.write(func(w io.Writer) error {
					rows := make([][]string, 0, len(items))
					for _, rs := range items {
						running := ""
						if rs.GetActive() {
							running = "yes"
						}
						rows = append(rows, []string{strconv.Itoa(int(rs.GetVersion())), running, a.when(rs.GetCreateTime()),
							output.Dash(rs.GetCreatedByKey()), strconv.Itoa(len(rs.GetArchiveRules())), strconv.Itoa(len(rs.GetForwardRules())),
							output.Clean(rs.GetComment())})
					}
					return output.Table(w, a.style(), []string{"VERSION", "RUNNING", "MADE", "BY", "ARCHIVE", "FORWARD", "COMMENT"}, rows)
				})
			})
		}),
	}
	lf.add(cmd)
	return cmd
}

// newRouteActivate builds "route activate".
func (a *app) newRouteActivate() *cobra.Command {
	return &cobra.Command{
		Use:   "activate <tenant> <receiver> <version>",
		Short: "Make a receiver run one of its rule set versions, or none (0)",
		Long: `Make a receiver run one of its rule set versions: an older one to go back to
it, or 0 to run none, every study then going as the receiver says. It applies
to receptions identified from then on; a session already made keeps where it
went. The activation is checked as "route create --activate" checks one.`,
		Example: `  nopacs route activate lab1 scanner1 2
  nopacs route activate lab1 scanner1 0`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[2])
			if err != nil {
				return fault.New(fault.Invalid, "the version is a whole number, 0 for none")
			}
			v, err := boundedInt32("version", n, 0, 1000000)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ActivateRuleSetResponse, error) {
				return c.Routes.ActivateRuleSet(ctx, &v1.ActivateRuleSetRequest{Tenant: args[0], Receiver: args[1], Version: v})
			})
			if err != nil {
				return err
			}
			if res.GetRuleSet() == nil {
				return messageOut(a, res, func(w io.Writer) error {
					_, err := fmt.Fprintf(w, "The receiver %s runs no rule set: every study goes as the receiver says.\n", output.Clean(args[1]))
					return err
				})
			}
			return a.ruleSetOut(res.GetRuleSet())
		}),
	}
}

// newRouteExport builds "route export".
func (a *app) newRouteExport() *cobra.Command {
	var version int
	var to string
	cmd := &cobra.Command{
		Use:   "export <tenant> <receiver> [--version N] [--to FILE]",
		Short: "Write a receiver's rule set to a file, to edit and give to create",
		Long: `Write a receiver's rule set, the version it runs or --version N, as the JSON
file "route create" reads: its rules, its default and its comment, without what
the server answers (its version and times). To standard output, or --to.`,
		Example: `  nopacs route export lab1 scanner1 --to rules.json`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			v, err := boundedInt32("version", version, 0, 1000000)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetRuleSetResponse, error) {
				return c.Routes.GetRuleSet(ctx, &v1.GetRuleSetRequest{Tenant: args[0], Receiver: args[1], Version: v})
			})
			if err != nil {
				return err
			}
			rs := res.GetRuleSet()
			edit := &v1.RuleSet{ArchiveRules: rs.GetArchiveRules(), QuarantineUnmatched: rs.GetQuarantineUnmatched(),
				ForwardRules: rs.GetForwardRules(), Comment: rs.GetComment()}
			// Only what is set, so the file holds what create reads and
			// nothing the server answers.
			text, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(edit)
			if err != nil {
				return fault.Wrap(fault.Internal, err, "writing the rule set")
			}
			b := bytes.NewBuffer(append(text, '\n'))
			if to == "" {
				return a.write(func(w io.Writer) error { _, err := w.Write(b.Bytes()); return err })
			}
			if err := os.WriteFile(to, b.Bytes(), 0o600); err != nil {
				return fault.Wrap(fault.Invalid, err, "cannot write --to "+to)
			}
			return nil
		}),
	}
	versionFlag(cmd, &version, "the version to write; 0, the default, for the one the receiver runs")
	cmd.Flags().StringVar(&to, "to", "", "the file to write; standard output when not given")
	return cmd
}
