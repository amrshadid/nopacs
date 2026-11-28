package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// maxExplainFile is the largest file "route explain" sends: the server's
// bound.
const maxExplainFile = 64 << 20

// newRouteExplain builds "route explain".
func (a *app) newRouteExplain() *cobra.Command {
	var version int
	var file, instance, calling, called string
	var show bool
	cmd := &cobra.Command{
		Use:   "explain <tenant> <receiver> (--file FILE.dcm [--calling-aet AE] [--called-aet AE] | --instance SUBJECT/SESSION/SCAN[/UID]) [--version N] [--show-values]",
		Short: "Show what a rule set decides for one file, rule by rule",
		Long: `Show what a receiver's rule set (the version it runs, or --version N) decides
for one file: a DICOM file read from --file, as the receiver would receive it
from --calling-aet, or an instance the archive holds (--instance, its scan's
first instance without a UID). For each rule: its result (true, false, unknown
or unusable) and the comparison that decided it, with the attribute's tag, VR
and whether the file holds it. The values the comparison saw are hidden unless
--show-values, which is recorded in the audit log and refused to a masked key.
An archived header is de-identified, so a comparison on an attribute its profile
removed is unknown. Needs the operator role on the tenant.`,
		Example: `  nopacs route explain lab1 scanner1 --file study/0001.dcm --calling-aet SCANNER1
  nopacs route explain lab1 scanner1 --instance sub-01/ses-01/scan-01 --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			v, err := boundedInt32("version", version, 0, 1000000)
			if err != nil {
				return err
			}
			req := &v1.ExplainRouteRequest{Tenant: args[0], Receiver: args[1], Version: v, ShowValues: show,
				CallingAeTitle: calling, CalledAeTitle: called}
			switch {
			case (file == "") == (instance == ""):
				return fault.New(fault.Invalid, "give one of --file and --instance")
			case file != "":
				b, err := readExplained(file, maxExplainFile)
				if err != nil {
					return err
				}
				req.Source = &v1.ExplainRouteRequest_File{File: b}
			default:
				parts := strings.Split(instance, "/")
				if len(parts) < 3 || len(parts) > 4 {
					return fault.New(fault.Invalid, "--instance is SUBJECT/SESSION/SCAN or SUBJECT/SESSION/SCAN/UID")
				}
				in := &v1.RouteInstance{SubjectLabel: parts[0], SessionLabel: parts[1], ScanLabel: parts[2]}
				if len(parts) == 4 {
					in.InstanceId = parts[3]
				}
				req.Source = &v1.ExplainRouteRequest_Instance{Instance: in}
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ExplainRouteResponse, error) {
				return c.Routes.ExplainRoute(ctx, req)
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.explainTable(w, res, show) })
		}),
	}
	versionFlag(cmd, &version, "the version to explain; 0, the default, for the one the receiver runs")
	cmd.Flags().StringVar(&file, "file", "", "a DICOM file to explain, as the receiver would receive it")
	cmd.Flags().StringVar(&instance, "instance", "", "an archived instance: SUBJECT/SESSION/SCAN, or with /UID")
	cmd.Flags().StringVar(&calling, "calling-aet", "", "the calling AE title --file is explained as received from")
	cmd.Flags().StringVar(&called, "called-aet", "", "the called AE title --file is explained as received with")
	cmd.Flags().BoolVar(&show, "show-values", false, "show the values each deciding comparison saw (audited; refused to a masked key)")
	return cmd
}

// readExplained reads the file an explanation sends, at most limit bytes.
func readExplained(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read "+path)
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fault.New(fault.Invalid, path+" is not a file of at most "+strconv.FormatInt(limit>>20, 10)+"MiB")
	}
	return readBounded(path, info.Size())
}

// truthWord names a condition's result.
func truthWord(t v1.RouteTruth) string {
	return strings.ToLower(strings.TrimPrefix(t.String(), "ROUTE_TRUTH_"))
}

// phaseWord names a rule's phase.
func phaseWord(p v1.RulePhase) string {
	return strings.ToLower(strings.TrimPrefix(p.String(), "RULE_PHASE_"))
}

// explainTable writes an explanation: the decision, then each rule's
// result and the comparison that decided it.
func (a *app) explainTable(w io.Writer, res *v1.ExplainRouteResponse, show bool) error {
	d := res.GetDecision()
	var verdict string
	switch {
	case d.GetUnrouted() != "":
		verdict = "held as unrouted (" + d.GetUnrouted() + ")"
		if d.GetArchiveRule() != "" {
			verdict += ", at rule " + output.Clean(d.GetArchiveRule())
		}
	case d.GetArchiveUnknown():
		verdict = "unknown: a rule before any that holds reads a value the header does not keep"
	case d.GetArchiveRule() != "":
		verdict = "archive rule " + output.Clean(d.GetArchiveRule()) + " to " + d.GetStorage() + ", profile " + profileText(d.GetProfile(), "receiver's")
	default:
		verdict = "no archive rule: the receiver's storage " + output.Dash(d.GetStorage())
	}
	forward := output.Dash(strings.Join(d.GetForwardRules(), ", "))
	if len(d.GetForwardUnknown()) > 0 {
		forward += " (unknown: " + strings.Join(d.GetForwardUnknown(), ", ") + ")"
	}
	if _, err := fmt.Fprintf(w, "Version %d: %s\nForward rules that hold: %s\n\n", res.GetVersion(), verdict, forward); err != nil {
		return err
	}
	header := []string{"PHASE", "RULE", "RESULT", "DECIDED BY", "TAG", "VR", "PRESENT"}
	if show {
		header = append(header, "VALUES")
	}
	rows := make([][]string, 0, len(res.GetRules()))
	for _, r := range res.GetRules() {
		dc := r.GetDecidedBy()
		by := "no condition"
		switch {
		case r.GetUnusable() != "":
			by = r.GetUnusable()
		case dc != nil:
			by = dc.GetComparison()
		}
		present := ""
		if dc != nil {
			present = "no"
			if dc.GetPresent() {
				present = "yes"
			}
		}
		row := []string{phaseWord(r.GetPhase()), r.GetRule(), truthWord(r.GetTruth()), by, output.Dash(dc.GetTag()), output.Dash(dc.GetVr()), output.Dash(present)}
		if show {
			row = append(row, strings.Join(dc.GetValues(), `\`))
		}
		rows = append(rows, row)
	}
	if err := output.Table(w, a.style(), header, rows); err != nil {
		return err
	}
	if !show {
		_, err := fmt.Fprintln(w, "Values are hidden; --show-values shows them (audited).")
		return err
	}
	return nil
}

// newRouteDryRun builds "route dry-run".
func (a *app) newRouteDryRun() *cobra.Command {
	var version, sessions int
	cmd := &cobra.Command{
		Use:   "dry-run <tenant> <receiver> [--version N] [--sessions N]",
		Short: "Try a rule set version on a receiver's latest sessions",
		Long: `Try a rule set version (the one the receiver runs, or --version N) on the
receiver's latest sessions (--sessions, 100 by default, at most 1000), and show
for each the rules that routed it beside the rules the version would choose:
same, changed, or unknown. It reads one metadata file per scan and the
session's AE titles, and changes nothing. The archive's headers are
de-identified, so a session whose result depends on a value its profile removed
is unknown, never counted as matching or not. Needs the operator role.`,
		Example: `  nopacs route create lab1 scanner1 new.json
  nopacs route dry-run lab1 scanner1 --version 4 --sessions 200`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			v, err := boundedInt32("version", version, 0, 1000000)
			if err != nil {
				return err
			}
			n, err := boundedInt32("sessions", sessions, 1, 1000)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DryRunRoutesResponse, error) {
				return c.Routes.DryRunRoutes(ctx, &v1.DryRunRoutesRequest{Tenant: args[0], Receiver: args[1], Version: v, Sessions: n})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				rows := make([][]string, 0, len(res.GetSessions()))
				for _, s := range res.GetSessions() {
					would := output.Dash(s.GetArchiveRule())
					if s.GetArchiveUnknown() {
						would = "unknown"
					}
					fwd := strings.Join(s.GetForwardRules(), ", ")
					if len(s.GetForwardUnknown()) > 0 {
						fwd += " ?" + strings.Join(s.GetForwardUnknown(), ", ?")
					}
					outcome := strings.ToLower(strings.TrimPrefix(s.GetOutcome().String(), "DRY_RUN_OUTCOME_"))
					if s.GetUnreadable() != "" {
						outcome += ": " + s.GetUnreadable()
					}
					rows = append(rows, []string{s.GetSubjectLabel() + "/" + s.GetSessionLabel(),
						output.Dash(s.GetRecordedArchiveRule()) + " @" + strconv.Itoa(int(s.GetRecordedVersion())),
						output.Dash(strings.Join(s.GetRecordedForwardRules(), ", ")), would, output.Dash(fwd), outcome})
				}
				if err := output.Table(w, a.style(), []string{"SESSION", "WENT BY", "FORWARDED BY", "WOULD GO BY", "WOULD FORWARD BY", "OUTCOME"}, rows); err != nil {
					return err
				}
				_, err := fmt.Fprintf(w, "Version %d: %d the same, %d changed, %d unknown\n", res.GetVersion(), res.GetSame(), res.GetChanged(), res.GetUnknown())
				return err
			})
		}),
	}
	versionFlag(cmd, &version, "the version to try; 0, the default, for the one the receiver runs")
	cmd.Flags().IntVar(&sessions, "sessions", 100, "how many of the latest sessions to try it on, 1 to 1000")
	return cmd
}

// newRouteHits builds "route hits".
func (a *app) newRouteHits() *cobra.Command {
	var version int
	cmd := &cobra.Command{
		Use:   "hits <tenant> <receiver> [--version N]",
		Short: "Count the receptions each rule routed",
		Long: `Count, for each rule of a receiver's rule sets (every version, or --version
N), the receptions it routed and the series its forwards sent, and when it last
routed one, from the record each reception keeps. "(default)" is a reception no
archive rule chose.`,
		Example: `  nopacs route hits lab1 scanner1 --version 3`,
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
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetRouteHitsResponse, error) {
				return c.Routes.GetRouteHits(ctx, &v1.GetRouteHitsRequest{Tenant: args[0], Receiver: args[1], Version: v})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				rows := make([][]string, 0, len(res.GetHits()))
				for _, h := range res.GetHits() {
					rule := h.GetRule()
					if rule == "" {
						rule = "(default)"
					}
					rows = append(rows, []string{strconv.Itoa(int(h.GetVersion())), phaseWord(h.GetPhase()), rule,
						strconv.FormatInt(h.GetReceptions(), 10), strconv.FormatInt(h.GetSeries(), 10), a.when(h.GetLastTime())})
				}
				return output.Table(w, a.style(), []string{"VERSION", "PHASE", "RULE", "RECEPTIONS", "SERIES", "LAST"}, rows)
			})
		}),
	}
	versionFlag(cmd, &version, "the version to count; 0, the default, for every version")
	return cmd
}
