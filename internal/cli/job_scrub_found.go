package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// scrubKindPrefix is what every name of a scrub finding's kind starts with.
const scrubKindPrefix = "SCRUB_FINDING_KIND_"

// findingWord writes a finding's kind as a word, such as "manifest-damaged".
func findingWord(k v1.ScrubFindingKind) string {
	return strings.ReplaceAll(enumWordOf(k.String(), scrubKindPrefix), "_", "-")
}

// findingScan writes the labels of the scan a finding belongs to, "-" for an
// orphan that names none.
func findingScan(f *v1.ScrubFinding) string {
	if f.GetSubjectLabel() == "" {
		return "-"
	}
	return f.GetSubjectLabel() + "/" + f.GetSessionLabel() + "/" + f.GetScanLabel()
}

// newJobFindings builds "job findings".
func (a *app) newJobFindings() *cobra.Command {
	var kinds []string
	var storage string
	var cleared bool
	var l listFlags
	cmd := &cobra.Command{
		Use:   "findings <tenant> [--kind KIND]... [--storage NAME] [--cleared]",
		Short: "List what scrubs found wrong with the stored objects",
		Long: `List the scrub findings of a tenant: the stored objects that are missing, damaged,
named by no scan, or that disagree with the catalog, by storage, then key, each
with the scan it belongs to. By default only the findings that hold now are
listed; --cleared adds those a later scrub read good again. An empty list means
no scrub has found anything wrong that still holds: "job scrub" makes one.

A KIND is one of: ` + strings.Join(enumNames(v1.ScrubFindingKind_name, scrubKindPrefix), ", ") + `.
  missing             the catalog and manifest name an object that is not there
  orphan              an object under the tenant's keys that nothing names
  damaged             an object's length or SHA-256 is not what was archived
  disagreeing         a manifest line disagrees with the catalog's row
  manifest-damaged    a scan's manifest is missing or is not the one the catalog holds

A missing or damaged instance is marked, and a read of it answers data loss
until a later scrub reads it good, a repair puts good bytes back, or a replace
from a file sent again settles it. The table never shows an object's key, which
holds a SOP Instance UID as received; --json carries the whole finding. The
reader role on the tenant is enough.`,
		Example: `  nopacs job findings lab1
  nopacs job findings lab1 --kind damaged --kind missing --storage main
  nopacs job findings lab1 --cleared --all --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			want, err := parseFindingKinds(kinds)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			findings, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.ScrubFinding, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListScrubFindingsResponse, error) {
					return c.Corrections.ListScrubFindings(ctx, &v1.ListScrubFindingsRequest{Tenant: args[0], Kinds: want, Storage: storage,
						IncludeCleared: cleared, PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetFindings(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "findings", findings, more, func() error {
				rows := make([][]string, 0, len(findings))
				for _, f := range findings {
					rows = append(rows, []string{f.GetId(), findingWord(f.GetKind()), output.Clean(f.GetStorage()), output.Clean(findingScan(f)),
						output.Dash(f.GetInstanceId()), a.when(f.GetFindTime()), a.whenOr(f.GetClearTime(), "holds")})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"ID", "KIND", "STORAGE", "SCAN", "INSTANCE", "FOUND", "CLEARED"}, rows)
				})
			})
		}),
	}
	fl := cmd.Flags()
	fl.StringArrayVar(&kinds, "kind", nil, "only the findings of this kind; repeat for more")
	fl.StringVar(&storage, "storage", "", "only the findings on the storage of this name")
	fl.BoolVar(&cleared, "cleared", false, "also list the findings a later scrub cleared")
	enumCompletion(cmd, "kind", v1.ScrubFindingKind_name, scrubKindPrefix)
	l.add(cmd)
	return cmd
}

// parseFindingKinds reads the --kind values of a list of findings.
func parseFindingKinds(kinds []string) ([]v1.ScrubFindingKind, error) {
	var want []v1.ScrubFindingKind
	for _, k := range kinds {
		v, err := parseEnum[v1.ScrubFindingKind]("kind", k, v1.ScrubFindingKind_name, scrubKindPrefix)
		if err != nil {
			return nil, err
		}
		want = append(want, v)
	}
	return want, nil
}

// newJobFinding builds "job finding".
func (a *app) newJobFinding() *cobra.Command {
	return &cobra.Command{
		Use:   "finding <tenant> <id>",
		Short: "Show one scrub finding: what was expected and what was found",
		Long: `Show one scrub finding, held or cleared: its kind, the storage and the scan it
belongs to, the size and SHA-256 the catalog or manifest holds and those of
the bytes found, the scrub jobs that found it and cleared it, and the repair
that mended it, if one did. The id is the one "job findings" lists.

The object's key is not shown, as in "job findings"; --json carries it. The
reader role on the tenant is enough. An id the tenant has no finding of is
exit 4.`,
		Example: `  nopacs job finding lab1 fnd_4Kp1
  nopacs job finding lab1 fnd_4Kp1 --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			// The contract lists findings and has no read by id, so the
			// list, cleared ones included, is read until the id is met.
			found, err := findFinding(ctx, c, cmd, args[0], args[1])
			if err != nil {
				return err
			}
			return messageOut(a, found, func(w io.Writer) error { return output.Details(w, a.style(), a.findingDetails(found)) })
		}),
	}
}

// findFinding pages through a tenant's findings, cleared ones included, for
// the one with this id.
func findFinding(ctx context.Context, c *client.Client, cmd *cobra.Command, tenant, id string) (*v1.ScrubFinding, error) {
	token := ""
	for {
		res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListScrubFindingsResponse, error) {
			return c.Corrections.ListScrubFindings(ctx, &v1.ListScrubFindingsRequest{Tenant: tenant, IncludeCleared: true, PageSize: maxPageSize, PageToken: token})
		})
		if err != nil {
			return nil, err
		}
		for _, f := range res.GetFindings() {
			if f.GetId() == id {
				return f, nil
			}
		}
		next := res.GetNextPageToken()
		if next == "" || next == token {
			return nil, fault.New(fault.NotFound, fmt.Sprintf("the tenant has no scrub finding %q", output.Clean(id)))
		}
		token = next
	}
}

// findingDetails lists one finding as labeled values.
func (a *app) findingDetails(f *v1.ScrubFinding) []output.Pair {
	size := func(present bool, n int64) string {
		if !present {
			return "-"
		}
		return fmt.Sprintf("%d bytes", n)
	}
	pairs := []output.Pair{
		{Label: "ID", Value: f.GetId()},
		{Label: "Kind", Value: findingWord(f.GetKind())},
		{Label: "State", Value: map[bool]string{true: "cleared", false: "holds"}[f.GetClearTime() != nil]},
		{Label: "Storage", Value: output.Clean(f.GetStorage())},
	}
	if f.GetReplicaOf() != "" {
		pairs = append(pairs, output.Pair{Label: "Replica of", Value: f.GetReplicaOf()})
	}
	pairs = append(pairs,
		output.Pair{Label: "Scan", Value: output.Clean(findingScan(f))},
		output.Pair{Label: "Instance", Value: output.Dash(f.GetInstanceId())},
		output.Pair{Label: "Revision", Value: fmt.Sprintf("%d", f.GetRevision())},
		output.Pair{Label: "Expected size", Value: size(f.ExpectedSize != nil, f.GetExpectedSize())},
		output.Pair{Label: "Found size", Value: size(f.FoundSize != nil, f.GetFoundSize())},
		output.Pair{Label: "Expected SHA-256", Value: output.Dash(f.GetExpectedSha256())},
		output.Pair{Label: "Found SHA-256", Value: output.Dash(f.GetFoundSha256())},
		output.Pair{Label: "Found by", Value: f.GetJob()},
		output.Pair{Label: "Found", Value: a.when(f.GetFindTime())},
		output.Pair{Label: "Cleared", Value: a.whenOr(f.GetClearTime(), "-")},
		output.Pair{Label: "Cleared by", Value: output.Dash(f.GetClearJob())})
	if f.GetRepairJob() != "" {
		pairs = append(pairs, output.Pair{Label: "Repaired by", Value: f.GetRepairJob()})
	}
	return pairs
}
