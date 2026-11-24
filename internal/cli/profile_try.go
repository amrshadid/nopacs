package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The bounds of the files a trial run sends, the server's (profile.proto,
// TryProfile): checked here first, so a person hears which file is too
// large before anything is sent.
const (
	maxTrialFiles     = 32
	maxTrialFileBytes = 3 << 20
	maxTrialSentBytes = 3<<20 + 512<<10
	// maxWalkFiles bounds the files a directory's walk looks at, so a
	// directory of a million files is refused rather than read.
	maxWalkFiles = 4096
)

// problemsOfError returns the problems a refused document's error carries
// (its ProfileProblems detail), or none.
func problemsOfError(err error) []*v1.ProfileProblem {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return nil
	}
	for _, d := range ce.Details() {
		if m, err := connectproto.UnmarshalErrorDetail(d); err == nil {
			if p, ok := m.(*v1.ProfileProblems); ok {
				return p.GetProblems()
			}
		}
	}
	return nil
}

// trialProfileFlags are the flags that name the profile a trial run, an
// explanation or a description runs: a stored version, or a document.
type trialProfileFlags struct {
	version  string
	document string
	receiver string
	calling  string
	called   string
}

// add registers the flags.
func (f *trialProfileFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.version, "profile", "", "the stored version to run, NAME or NAME@VERSION (the latest without @)")
	fl.StringVar(&f.document, documentFlag, "", "an unsaved document to run, its file, or - to read it from standard input")
	fl.StringVar(&f.receiver, "receiver", "", "one of the tenant's receivers: the sent files are tried as arriving at it, its receiving checks and labels included; without it the verdict is de-identification's alone")
	fl.StringVar(&f.calling, "calling-aet", "", "the sender's AE title the sent files are tried as coming from")
	fl.StringVar(&f.called, "called-aet", "", "the AE title the sent files are tried as sent to")
	cmd.MarkFlagsMutuallyExclusive("profile", documentFlag)
	cmd.MarkFlagsOneRequired("profile", documentFlag)
}

// profile returns the profile the flags name.
func (a *app) trialProfile(f *trialProfileFlags) (*v1.TrialProfile, error) {
	if f.document != "" {
		doc, err := a.readDocument(f.document)
		if err != nil {
			return nil, err
		}
		return &v1.TrialProfile{Kind: &v1.TrialProfile_Document{Document: doc}}, nil
	}
	ref, err := parseVersionArg(f.version)
	if err != nil {
		return nil, err
	}
	return &v1.TrialProfile{Kind: &v1.TrialProfile_Version{Version: &v1.ProfileVersionName{Name: ref.Name, Version: ref.Version}}}, nil
}

// reception returns the reception the flags give.
func (f *trialProfileFlags) reception() *v1.TrialReception {
	return &v1.TrialReception{Receiver: f.receiver, CallingAeTitle: f.calling, CalledAeTitle: f.called}
}

// newProfileTry builds "profile try".
func (a *app) newProfileTry() *cobra.Command {
	var f trialProfileFlags
	var records []string
	var failOnHeld, changedOnly, local bool
	cmd := &cobra.Command{
		Use:   "try (<tenant> (--profile NAME[@VERSION] | --document FILE|-) [FILE|DIR ...] [--record ID ...] | --local --document FILE|- FILE|DIR ...)",
		Short: "Run a profile over sample files and show what it would do, storing nothing",
		Long: `Run a profile over sample files exactly as a receiver naming it would: each file
is de-identified, written in memory, checked after for every value the profile
removed, and shown element by element with its value before and after, the
action, and the rule (or base) that decided it. Each file ends passed (it would
be archived) or held, with the quarantine reason and its fixed cause. Nothing is
stored, and none of the tenant's secrets is used: each run has keys of its own,
so remapped UIDs, hashes, shifted dates and codes never equal the archive's
(only their form carries over), and no lookup table is read (every lookup
shows UNASSIGNED; one that would hold a file without a code makes the file
undecided). The server's audit entry keeps the counts per rule and reason,
never a value.

With --receiver, and for held files (their receiver), the receiver's own
checks run too (placing UIDs, SOP classes, its naming rule and labels), so
"passed" means that receiver would archive the file; without one the answer
says the verdict is de-identification's alone.

Files are named as paths; a directory is read recursively for its DICOM files.
At most 32 files of up to 3MiB each, 3.5MiB together, go in one run. --record
tries the held file of a quarantine record instead (repeat for more).

The values shown are the files' own and may identify a person: they go to this
terminal only. --changed-only shows only the elements that change.
--fail-on-held exits 5 when any file would be held or is undecided, or when no
receiver could name the profile, for scripts and agents.

--local runs the engine and the check after in this command, with no server
and no tenant: every argument is a file or a directory, and the profile is a
document (--document), or a version's export ("profile export", or its --json
answer, which names the version). The answer is the same as a server's for
files tried with no receiver, under keys made for the run and thrown away, with
every lookup UNASSIGNED; nothing is sent anywhere and nothing is written. A file
of up to 64MiB is read at a time, so a directory of any number of files can be
tried; receiver.header-limit and deid.pixel-limit take their defaults. For
people and agents iterating on a profile offline.`,
		Example: `  nopacs profile try lab1 --profile research@2 ./samples/
  nopacs profile try lab1 --document draft.json scan1.dcm --json
  nopacs profile try --local --document draft.json ./samples/ --changed-only
  nopacs profile try lab1 --profile research --record 01J9Z3K4 --changed-only`,
		Args: minimumArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if local {
				if f.version != "" || f.receiver != "" || len(records) > 0 {
					return fault.New(fault.Invalid, "--local runs without a server: give --document, not --profile, --receiver or --record")
				}
				res, err := a.tryLocal(ctx, &f, args)
				if err != nil {
					return err
				}
				return a.trialOut(res, changedOnly, failOnHeld)
			}
			p, err := a.trialProfile(&f)
			if err != nil {
				return err
			}
			files, err := collectFiles(args[1:])
			if err != nil {
				return err
			}
			if len(files)+len(records) == 0 {
				return fault.New(fault.Invalid, "name at least one file or directory, or give --record")
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.TryProfileResponse, error) {
				return c.Profiles.TryProfile(ctx, &v1.TryProfileRequest{Tenant: args[0], Profile: p, Files: files, QuarantineRecords: records, Reception: f.reception()})
			})
			if err != nil {
				return a.withProblems(err)
			}
			return a.trialOut(res, changedOnly, failOnHeld)
		}),
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&local, "local", false, "run the engine and the check in this command, with no server: every argument is a file or directory, the profile a --document or a version's export")
	cmd.Flags().StringArrayVar(&records, "record", nil, "the id of a quarantine record whose held file to try; repeat for more")
	cmd.Flags().BoolVar(&failOnHeld, "fail-on-held", false, "exit 5 when any file would be held or is undecided, or no receiver could name the profile")
	cmd.Flags().BoolVar(&changedOnly, "changed-only", false, "show only the elements whose value or presence changes")
	return cmd
}

// trialOut writes a trial run's answer and, with failOnHeld, fails when a
// file would be held or is undecided, or no receiver could name the
// profile.
func (a *app) trialOut(res *v1.TryProfileResponse, changedOnly, failOnHeld bool) error {
	if err := messageOut(a, res, func(w io.Writer) error { return a.trialText(w, res, changedOnly) }); err != nil {
		return err
	}
	if !failOnHeld {
		return nil
	}
	s := res.GetSummary()
	switch {
	case res.GetReceptionRefusal() != "":
		return fault.New(fault.Precondition, "no receiver could name this profile: "+output.Clean(res.GetReceptionRefusal()))
	case s.GetHeld() > 0 || s.GetUndecided() > 0:
		return fault.New(fault.Precondition, fmt.Sprintf("%d of %d files would be held, and %d depend on a lookup table", s.GetHeld(), s.GetFiles(), s.GetUndecided()))
	}
	return nil
}

// minimumArgs requires at least n positional arguments.
func minimumArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return fault.New(fault.Invalid, fmt.Sprintf("wrong number of arguments (want at least %d, got %d); usage: %s", n, len(args), cmd.UseLine()))
		}
		return nil
	}
}

// ruleText names the rule an element was decided by: its name or its
// position, with its base; "default" when no rule decided.
func ruleText(rule int32, name, origin string) string {
	if rule == 0 {
		return "default"
	}
	t := name
	if t == "" {
		t = "rule " + strconv.Itoa(int(rule))
	}
	if origin != "" {
		t += " (" + origin + ")"
	}
	return t
}

// trialText writes a trial run for a person: per file its verdict, then a
// table of its elements, then the summary.
func (a *app) trialText(w io.Writer, res *v1.TryProfileResponse, changedOnly bool) error {
	head := []output.Pair{{Label: "Checksum", Value: res.GetChecksum()}}
	if v := res.GetVersion(); v != nil {
		head = append([]output.Pair{{Label: "Profile", Value: v.GetName() + "@" + strconv.Itoa(int(v.GetVersion()))}}, head...)
	} else {
		head = append([]output.Pair{{Label: "Profile", Value: "unsaved document"}}, head...)
	}
	if r := res.GetReceptionRefusal(); r != "" {
		head = append(head, output.Pair{Label: "A receiver refuses it", Value: output.Clean(r)})
	}
	if err := output.Details(w, a.style(), head); err != nil {
		return err
	}
	for _, r := range res.GetResults() {
		name := output.Clean(r.GetName())
		if r.GetQuarantineRecord() != "" {
			name = "record " + r.GetQuarantineRecord()
		}
		verdict := "passed"
		if r.GetDeidentificationOnly() {
			verdict += " (de-identification only: no receiver's checks ran)"
		} else if r.GetReceiver() != "" {
			verdict += " at receiver " + r.GetReceiver()
		}
		if r.GetVerdict() == v1.TrialVerdict_TRIAL_VERDICT_UNDECIDED {
			verdict = "undecided: depends on a lookup table the trial does not read"
		}
		if r.GetVerdict() == v1.TrialVerdict_TRIAL_VERDICT_HELD {
			verdict = "held: " + r.GetReason() + " (" + r.GetCause()
			if r.GetPath() != "" {
				verdict += " at " + r.GetPath()
			}
			if r.GetRule() != "" {
				verdict += ", rule " + r.GetRule()
			}
			verdict += ")"
		}
		_, _ = fmt.Fprintf(w, "\n%s: %s\n", a.style().Bold(name), verdict)
		rows := [][]string{}
		for _, e := range r.GetElements() {
			if changedOnly && !e.GetChanged() {
				continue
			}
			after := output.Clean(e.GetAfter())
			if !e.GetPresentAfter() {
				after = "(absent)"
			}
			if r.GetVerdict() == v1.TrialVerdict_TRIAL_VERDICT_HELD {
				after = "-"
			}
			rows = append(rows, []string{e.GetPath(), output.Dash(e.GetKeyword()), e.GetAction(), output.Dash(output.Clean(e.GetBefore())), output.Dash(after),
				ruleText(e.GetRule(), e.GetRuleName(), e.GetOrigin())})
		}
		if len(rows) > 0 {
			if err := output.Table(w, a.style(), []string{"PATH", "KEYWORD", "ACTION", "BEFORE", "AFTER", "RULE"}, rows); err != nil {
				return err
			}
		}
		if n := r.GetElementsOmitted(); n > 0 {
			_, _ = fmt.Fprintf(w, "(%d more elements not shown; the answer is bounded)\n", n)
		}
		if n := r.GetLookupsUnassigned(); n > 0 {
			_, _ = fmt.Fprintf(w, "(%d lookup(s) UNASSIGNED: a receiver would assign the table's next code)\n", n)
		}
	}
	s := res.GetSummary()
	_, _ = fmt.Fprintf(w, "\n%d file(s): %d passed, %d held, %d undecided\n", s.GetFiles(), s.GetPassed(), s.GetHeld(), s.GetUndecided())
	for _, c := range s.GetByReason() {
		_, _ = fmt.Fprintf(w, "  held %s: %d\n", c.GetKey(), c.GetCount())
	}
	_, _ = fmt.Fprintln(w, "Keyed values (remapped UIDs, hashes, shifted dates, codes) come from keys made for this trial, never the archive's.")
	return nil
}

// newProfileExplain builds "profile explain".
func (a *app) newProfileExplain() *cobra.Command {
	var f trialProfileFlags
	var record string
	cmd := &cobra.Command{
		Use:   "explain <tenant> (--profile NAME[@VERSION] | --document FILE|-) (FILE | --record ID)",
		Short: "Show which rule decides each element of a file, and why",
		Long: `For one file, show for each element the rule (or base) that decides it, how
that rule selects the element (place, tag, private element, private block,
group, VR: from the most specific), its reason, and every other rule that
matches it with why it does not decide: its conditions do not hold, a more
specific rule decides, a later rule of the same kind decides, or the engine's
floor (which never keeps a person's name or identifiers) overrides it. No value
of the file is shown. Nothing is stored.`,
		Example: `  nopacs profile explain lab1 --profile research@2 scan1.dcm
  nopacs profile explain lab1 --document draft.json --record 01J9Z3K4 --json`,
		Args: rangeArgs(1, 2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if (len(args) == 2) == (record != "") {
				return fault.New(fault.Invalid, "name one file, or give --record, one of the two")
			}
			p, err := a.trialProfile(&f)
			if err != nil {
				return err
			}
			req := &v1.ExplainProfileRequest{Tenant: args[0], Profile: p, QuarantineRecord: record, Reception: f.reception()}
			if len(args) == 2 {
				files, err := collectFiles(args[1:2])
				if err != nil {
					return err
				}
				if len(files) != 1 {
					return fault.New(fault.Invalid, "explain takes one file")
				}
				req.File = files[0]
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ExplainProfileResponse, error) {
				return c.Profiles.ExplainProfile(ctx, req)
			})
			if err != nil {
				return a.withProblems(err)
			}
			return messageOut(a, res, func(w io.Writer) error {
				if res.GetRefusal() != "" {
					_, _ = fmt.Fprintf(w, "The engine refuses the file: %s %s\n", res.GetRefusal(), res.GetRefusalPath())
				}
				rows := [][]string{}
				for _, e := range res.GetElements() {
					others := []string{}
					for _, o := range e.GetOthers() {
						others = append(others, ruleText(o.GetRule(), o.GetRuleName(), o.GetOrigin())+": "+o.GetWhyNot())
					}
					if n := e.GetOthersOmitted(); n > 0 {
						others = append(others, "+"+strconv.Itoa(int(n))+" more")
					}
					rows = append(rows, []string{e.GetPath(), output.Dash(e.GetKeyword()), e.GetAction(), ruleText(e.GetRule(), e.GetRuleName(), e.GetOrigin()),
						output.Dash(e.GetSelectedBy()), output.Dash(strings.Join(others, "; ")), output.Clean(e.GetReason())})
				}
				if err := output.Table(w, a.style(), []string{"PATH", "KEYWORD", "ACTION", "DECIDED BY", "SELECTED BY", "NOT DECIDING", "WHY"}, rows); err != nil {
					return err
				}
				if n := res.GetElementsOmitted(); n > 0 {
					_, _ = fmt.Fprintf(w, "(%d more elements not shown; the answer is bounded)\n", n)
				}
				return nil
			})
		}),
	}
	f.add(cmd)
	cmd.Flags().StringVar(&record, "record", "", "the id of a quarantine record whose held file to explain")
	return cmd
}

// newProfileDiff builds "profile diff".
func (a *app) newProfileDiff() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <tenant> <name>[@<version>] <name>[@<version>]",
		Short: "Show what changes from one version to another",
		Long: `Show what changes from the first version to the second (either may be of another
profile; without @VERSION, the latest): the bases added and removed, the
defaults, each rule added, removed or changed, and the effect: for each
selector with its conditions, the action that wins before and after. Equal
checksums treat every file alike, and no effect is listed. The effect is judged
rule against rule; "profile try" over sample files shows it element by element.`,
		Example: `  nopacs profile diff lab1 research@1 research@2
  nopacs profile diff lab1 research strict --json`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			from, err := parseVersionArg(args[1])
			if err != nil {
				return err
			}
			to, err := parseVersionArg(args[2])
			if err != nil {
				return err
			}
			req := &v1.DiffProfileVersionsRequest{Tenant: args[0], Name: from.Name, FromVersion: from.Version, ToVersion: to.Version}
			if to.Name != from.Name {
				req.ToName = to.Name
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DiffProfileVersionsResponse, error) {
				return c.Profiles.DiffProfileVersions(ctx, req)
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.diffText(w, res) })
		}),
	}
	return cmd
}

// diffText writes a comparison for a person.
func (a *app) diffText(w io.Writer, res *v1.DiffProfileVersionsResponse) error {
	name := func(v *v1.ProfileVersion) string { return v.GetName() + "@" + strconv.Itoa(int(v.GetVersion())) }
	same := "no"
	if res.GetSameEffect() {
		same = "yes: every file is treated alike"
	}
	unnamed := func(u v1.UnnamedElements) string {
		return strings.ToLower(strings.TrimPrefix(u.String(), "UNNAMED_ELEMENTS_"))
	}
	pairs := []output.Pair{
		{Label: "From", Value: name(res.GetFrom()) + " (" + res.GetFrom().GetChecksum()[:min(12, len(res.GetFrom().GetChecksum()))] + ")"},
		{Label: "To", Value: name(res.GetTo()) + " (" + res.GetTo().GetChecksum()[:min(12, len(res.GetTo().GetChecksum()))] + ")"},
		{Label: "Same effect", Value: same},
		{Label: "Bases added", Value: output.Dash(strings.Join(res.GetBasesAdded(), ", "))},
		{Label: "Bases removed", Value: output.Dash(strings.Join(res.GetBasesRemoved(), ", "))},
	}
	if res.GetUnnamedFrom() != res.GetUnnamedTo() {
		pairs = append(pairs, output.Pair{Label: "Unnamed elements", Value: unnamed(res.GetUnnamedFrom()) + " -> " + unnamed(res.GetUnnamedTo())})
	}
	if res.GetDateShiftDaysFrom() != res.GetDateShiftDaysTo() {
		pairs = append(pairs, output.Pair{Label: "Largest date shift", Value: strconv.Itoa(int(res.GetDateShiftDaysFrom())) + " -> " + strconv.Itoa(int(res.GetDateShiftDaysTo())) + " days"})
	}
	if res.GetBasesReordered() {
		pairs = append(pairs, output.Pair{Label: "Bases reordered", Value: "yes: a later base's rule wins over an earlier's"})
	}
	if res.GetUnlistedChange() {
		pairs = append(pairs, output.Pair{Label: "Unlisted change", Value: "yes: the versions differ in what this comparison does not itemize; try them on files"})
	}
	if n := res.GetPixelRulesChanged(); n > 0 {
		pairs = append(pairs, output.Pair{Label: "Pixel rules changed", Value: strconv.Itoa(int(n))})
	}
	if err := output.Details(w, a.style(), pairs); err != nil {
		return err
	}
	if len(res.GetRules()) > 0 {
		rows := [][]string{}
		for _, r := range res.GetRules() {
			change := strings.ToLower(strings.TrimPrefix(r.GetChange().String(), "PROFILE_CHANGE_"))
			rule := r.GetTo()
			if rule == nil {
				rule = r.GetFrom()
			}
			origin := r.GetOrigin()
			if origin == "" {
				origin = "own"
			}
			rows = append(rows, []string{change, origin, output.Dash(rule.GetName()), oneofText(rule.GetSelector()), actionOrDash(r.GetFrom()), actionOrDash(r.GetTo())})
		}
		_, _ = fmt.Fprintln(w, "\nRules:")
		if err := output.Table(w, a.style(), []string{"CHANGE", "ORIGIN", "NAME", "SELECTOR", "FROM", "TO"}, rows); err != nil {
			return err
		}
		if n := res.GetRulesOmitted(); n > 0 {
			_, _ = fmt.Fprintf(w, "(%d more rules not shown)\n", n)
		}
	}
	if len(res.GetEffects()) > 0 {
		rows := [][]string{}
		for _, e := range res.GetEffects() {
			sel := oneofText(e.GetSelector())
			if n := len(e.GetWhen()); n > 0 {
				sel += " when " + strconv.Itoa(n) + " condition(s)"
			}
			from, to := "-", "-"
			if e.GetOrderChanged() {
				from, to = "rules of different conditions", "win in another order"
			}
			if e.GetFrom() != nil {
				from = oneofText(e.GetFrom())
			}
			if e.GetTo() != nil {
				to = oneofText(e.GetTo())
			}
			rows = append(rows, []string{sel, from, to})
		}
		_, _ = fmt.Fprintln(w, "\nEffect:")
		if err := output.Table(w, a.style(), []string{"SELECTOR", "FROM", "TO"}, rows); err != nil {
			return err
		}
		if n := res.GetEffectsOmitted(); n > 0 {
			_, _ = fmt.Fprintf(w, "(%d more changes of effect not shown)\n", n)
		}
	}
	return nil
}

// actionOrDash writes a rule's action, or a dash for no rule.
func actionOrDash(r *v1.ProfileRule) string {
	if r == nil {
		return "-"
	}
	return oneofText(r.GetAction())
}
