package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// searchLevels are the levels "search --level" names.
var searchLevels = map[string]v1.SearchLevel{
	"scan": v1.SearchLevel_SEARCH_LEVEL_SCAN, "session": v1.SearchLevel_SEARCH_LEVEL_SESSION, "subject": v1.SearchLevel_SEARCH_LEVEL_SUBJECT,
}

// newSearch builds "search".
func (a *app) newSearch() *cobra.Command {
	var level, facet string
	var count bool
	var l listFlags
	cmd := &cobra.Command{
		Use:   "search <tenant> [<condition>] [--level scan|session|subject] [--count] [--facet FIELD] [--limit N | --all]",
		Short: "Find scans, sessions or subjects by the values their files hold",
		Long: `Search a tenant by a condition on the header values of its archived files, in
the condition language the routing rules of a receiver are written in:

  Modality in ("MR", "CT") and SeriesDescription contains "mprage"
  has scan (SliceThickness between 0.8 and 1.2)

Leaves compare a DICOM keyword or tag, or a catalog field (session.date,
subject.label, calling_ae, called_ae, session.receiver), with =, == (exact),
!=, in, <, <=, >, >=, between, prefix, like, contains, words, present or
absent; and, or and not join them; has and every reach across levels. A
condition no index serves is refused, naming the attribute to index. Quote the
condition for the shell.

--level chooses what is listed (scans by default). --count adds how many rows
match, counted up to the tenant's search.facet-rows; --facet FIELD lists the
values of a field of the rows listed with how many rows have each, and is
refused when more rows match than search.facet-rows. A key that may not see
original values is never shown a row archived as received. Without a condition
every row of the level is listed.`,
		Example: `  nopacs search lab1 'Modality == "MR" and SliceThickness < 1.5' --count
  nopacs search lab1 'has scan (Modality == "CT")' --level session --json
  nopacs search lab1 'session.date last 30 days' --facet Modality`,
		Args: rangeArgs(1, 2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			lv := v1.SearchLevel_SEARCH_LEVEL_SCAN
			if level != "" {
				var ok bool
				if lv, ok = searchLevels[level]; !ok {
					return fault.New(fault.Invalid, fmt.Sprintf("--level is scan, session or subject, not %q", output.Clean(level)))
				}
			}
			cond := ""
			if len(args) > 1 {
				cond = args[1]
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			var total *int64
			var lower bool
			var facets []*v1.FacetValue
			items, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]proto.Message, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SearchResponse, error) {
					return c.Search.Search(ctx, &v1.SearchRequest{Tenant: args[0], Level: lv, Query: cond, PageSize: size, PageToken: token,
						Count: count, Facet: facet})
				})
				if err != nil {
					return nil, "", err
				}
				if res.TotalCount != nil {
					total, lower = res.TotalCount, res.GetTotalIsLowerBound()
				}
				facets = res.GetFacet()
				var rows []proto.Message
				for _, s := range res.GetSubjects() {
					rows = append(rows, s)
				}
				for _, s := range res.GetSessions() {
					rows = append(rows, s)
				}
				for _, s := range res.GetScans() {
					rows = append(rows, s)
				}
				return rows, res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			field := map[v1.SearchLevel]string{v1.SearchLevel_SEARCH_LEVEL_SCAN: "scans", v1.SearchLevel_SEARCH_LEVEL_SESSION: "sessions",
				v1.SearchLevel_SEARCH_LEVEL_SUBJECT: "subjects"}[lv]
			if a.opts.json {
				return a.write(func(w io.Writer) error {
					if err := output.JSONListTotal(w, field, items, more, total); err != nil {
						return err
					}
					if facet == "" {
						return nil
					}
					_, err := fmt.Fprintf(w, "%s\n", facetJSON(facets))
					return err
				})
			}
			if err := a.write(func(w io.Writer) error { return searchTable(a, w, items) }); err != nil {
				return err
			}
			if facet != "" {
				rows := make([][]string, 0, len(facets))
				for _, f := range facets {
					rows = append(rows, []string{output.Clean(f.GetValue()), count64(f.GetCount())})
				}
				if err := a.write(func(w io.Writer) error { return output.Table(w, a.style(), []string{"VALUE", "ROWS"}, rows) }); err != nil {
					return err
				}
			}
			if more {
				a.moreNote(len(items))
			}
			if total != nil && lower {
				_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: at least %d in all\n", *total)
			} else {
				a.totalNote(total, "row", "rows")
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&level, "level", "", "what to list: scan (default), session or subject")
	cmd.Flags().BoolVar(&count, "count", false, "also count the rows that match, up to the tenant's search.facet-rows")
	cmd.Flags().StringVar(&facet, "facet", "", "list the values of this field with how many rows have each")
	_ = cmd.RegisterFlagCompletionFunc("level", cobra.FixedCompletions([]string{"scan", "session", "subject"}, cobra.ShellCompDirectiveNoFileComp))
	l.add(cmd)
	cmd.AddCommand(a.newSearchBackfill())
	return cmd
}

// count64 writes a count.
func count64(n int64) string { return strconv.FormatInt(n, 10) }

// facetJSON writes a facet as the JSON document "search --facet --json"
// adds after the rows.
func facetJSON(f []*v1.FacetValue) string {
	out := `{"facet":[`
	for i, v := range f {
		if i > 0 {
			out += ","
		}
		out += `{"value":` + strconv.Quote(v.GetValue()) + `,"count":` + count64(v.GetCount()) + `}`
	}
	return out + `]}`
}

// searchTable writes the rows of a search, each of the level it is.
func searchTable(a *app, w io.Writer, items []proto.Message) error {
	var scans, sessions, subjects [][]string
	for _, it := range items {
		switch s := it.(type) {
		case *v1.Scan:
			scans = append(scans, []string{s.GetSubjectLabel(), s.GetSessionLabel(), s.GetLabel(), output.Value(s.GetValuesWithheld(), s.GetModality()),
				seriesNumber(s), output.Value(s.GetValuesWithheld(), s.GetSeriesDescription()), count(s.GetInstanceCount()), output.Human(s.GetSizeBytes())})
		case *v1.Session:
			sessions = append(sessions, []string{s.GetSubjectLabel(), s.GetLabel(), output.Value(s.GetValuesWithheld(), s.GetStudyDate()),
				count(s.GetScanCount()), count(s.GetInstanceCount()), output.Human(s.GetSizeBytes())})
		case *v1.Subject:
			subjects = append(subjects, []string{s.GetLabel(), count(s.GetSessionCount()), count(s.GetInstanceCount()), output.Human(s.GetSizeBytes())})
		}
	}
	switch {
	case len(scans) > 0:
		return output.Table(w, a.style(), []string{"SUBJECT", "SESSION", "SCAN", "MODALITY", "SERIES", "DESCRIPTION", "INSTANCES", "SIZE"}, scans)
	case len(sessions) > 0:
		return output.Table(w, a.style(), []string{"SUBJECT", "SESSION", "DATE", "SCANS", "INSTANCES", "SIZE"}, sessions)
	case len(subjects) > 0:
		return output.Table(w, a.style(), []string{"SUBJECT", "SESSIONS", "INSTANCES", "SIZE"}, subjects)
	}
	return output.Table(w, a.style(), []string{"MATCHES"}, nil)
}
