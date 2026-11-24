package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// catalogOrderPrefix is what every name of a catalog order starts with.
const catalogOrderPrefix = "CATALOG_ORDER_"

// catalogFlags are the flags the three catalog lists share: how the rows are
// ordered and the span of time they were first archived in.
type catalogFlags struct {
	order    string
	from, to string
	prefix   string
	l        listFlags
}

// add registers the shared flags.
func (f *catalogFlags) add(cmd *cobra.Command, what string) {
	cmd.Flags().StringVar(&f.order, "order", "", "how to order the "+what+": "+strings.Join(enumNames(v1.CatalogOrder_name, catalogOrderPrefix), ", ")+" (default newest)")
	cmd.Flags().StringVar(&f.prefix, "label-prefix", "", "only the "+what+" whose label starts with this, ignoring case (order label)")
	cmd.Flags().StringVar(&f.from, "from", "", "only the "+what+" first archived at or after this time: a date, a time with its offset, or an age such as 7d (order newest)")
	cmd.Flags().StringVar(&f.to, "to", "", "only the "+what+" first archived before this time, written like --from (order newest)")
	enumCompletion(cmd, "order", v1.CatalogOrder_name, catalogOrderPrefix)
	f.l.add(cmd)
}

// read turns the shared flags into the contract's order and time bounds.
func (f *catalogFlags) read(a *app) (order v1.CatalogOrder, from, to *timestamppb.Timestamp, err error) {
	if err = f.l.check(); err != nil {
		return 0, nil, nil, err
	}
	if f.order != "" {
		if order, err = parseEnum[v1.CatalogOrder]("order", f.order, v1.CatalogOrder_name, catalogOrderPrefix); err != nil {
			return 0, nil, nil, err
		}
	}
	now := a.env.Now()
	if f.from != "" {
		t, err := parseTime("from", f.from, now, a.env.Location)
		if err != nil {
			return 0, nil, nil, err
		}
		from = timestamppb.New(t)
	}
	if f.to != "" {
		t, err := parseTime("to", f.to, now, a.env.Location)
		if err != nil {
			return 0, nil, nil, err
		}
		to = timestamppb.New(t)
	}
	if from != nil && to != nil && !to.AsTime().After(from.AsTime()) {
		return 0, nil, nil, fault.New(fault.Invalid, "--to must be after --from")
	}
	return order, from, to, nil
}

// rangeArgs requires between lo and hi positional arguments.
func rangeArgs(lo, hi int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < lo || len(args) > hi {
			return fault.New(fault.Invalid, fmt.Sprintf("wrong number of arguments (want %d to %d, got %d); usage: %s", lo, hi, len(args), cmd.UseLine()))
		}
		return nil
	}
}

// newCatalog builds the catalog noun.
func (a *app) newCatalog() *cobra.Command {
	return group("catalog", "List a tenant's subjects, sessions and scans",
		`List what a tenant's archive holds, as the catalog keeps it: its subjects,
the sessions of a subject and the scans of a session, a page at a time. A table
is the default; --json writes one document with "more" (whether the server has
further rows) and, when the server knows it without counting, "totalCount".
Counts and sizes are the catalog's running totals. Every command takes the
tenant first and needs the reader role on it.`,
		a.newCatalogSubjects(), a.newCatalogSessions(), a.newCatalogScans())
}

// totalNote says on standard error, for a table, how many rows match when the
// server knew it, with the noun in the singular for one.
func (a *app) totalNote(total *int64, one, many string) {
	if total == nil || a.opts.json {
		return
	}
	noun := many
	if *total == 1 {
		noun = one
	}
	_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: %d %s in all\n", *total, noun)
}

// catalogOut writes a fetched list as JSON with its total, or as a table.
// field is the JSON field of the items and the plural of their noun; one is
// its singular.
func catalogOut[T proto.Message](a *app, field, one string, items []T, more bool, total *int64, table func() error) error {
	if a.opts.json {
		return a.write(func(w io.Writer) error { return output.JSONListTotal(w, field, items, more, total) })
	}
	if err := table(); err != nil {
		return err
	}
	if more {
		a.moreNote(len(items))
	}
	a.totalNote(total, one, field)
	return nil
}

// newCatalogSubjects builds "catalog subjects".
func (a *app) newCatalogSubjects() *cobra.Command {
	var f catalogFlags
	cmd := &cobra.Command{
		Use:   "subjects <tenant> [--label-prefix P] [--from T --to T] [--order O] [--limit N | --all]",
		Short: "List a tenant's subjects, newest first or by label",
		Long: `List a tenant's subjects with their session and instance counts and their
size. A page of 100 is shown by default; --limit N shows N and --all follows
every page. A label prefix needs the label order and a time span the newest
order, which is the default.`,
		Example: `  nopacs catalog subjects lab1
  nopacs catalog subjects lab1 --order label --label-prefix sub-a --all --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			order, from, to, err := f.read(a)
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			var total *int64
			items, more, err := fetchPages(ctx, f.l, func(ctx context.Context, size int32, token string) ([]*v1.Subject, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListSubjectsResponse, error) {
					return c.Catalog.ListSubjects(ctx, &v1.ListSubjectsRequest{Tenant: args[0], LabelPrefix: f.prefix, StartTime: from, EndTime: to, Order: order, PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				if res.TotalCount != nil {
					total = res.TotalCount
				}
				return res.GetSubjects(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return catalogOut(a, "subjects", "subject", items, more, total, func() error {
				rows := make([][]string, 0, len(items))
				for _, s := range items {
					rows = append(rows, []string{s.GetLabel(), count(s.GetSessionCount()), count(s.GetInstanceCount()), output.Human(s.GetSizeBytes()), a.when(s.GetArchiveTime())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"SUBJECT", "SESSIONS", "INSTANCES", "SIZE", "FIRST ARCHIVED"}, rows)
				})
			})
		}),
	}
	f.add(cmd, "subjects")
	return cmd
}

// count writes a count in decimal.
func count(n int64) string { return strconv.FormatInt(n, 10) }

// newCatalogSessions builds "catalog sessions".
func (a *app) newCatalogSessions() *cobra.Command {
	var f catalogFlags
	var dateFrom, dateTo, modality, receiver string
	cmd := &cobra.Command{
		Use:   "sessions <tenant> [<subject>] [--study-date-from D --study-date-to D] [--modality M] [--receiver R] [--order O] [--limit N | --all]",
		Short: "List a tenant's sessions, or one subject's",
		Long: `List the sessions of a tenant, or of one subject when it is named, with the
study date, the modalities, the scan and instance counts and the size. A study
date range (YYYYMMDD, both ends inclusive) comes with --order study-date; a
modality or a receiver comes with the newest order, and the server refuses a
combination it does not serve.`,
		Example: `  nopacs catalog sessions lab1 sub-3f9a
  nopacs catalog sessions lab1 --modality MR --from 7d
  nopacs catalog sessions lab1 --order study-date --study-date-from 20240101 --study-date-to 20241231`,
		Args: rangeArgs(1, 2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			order, from, to, err := f.read(a)
			if err != nil {
				return err
			}
			subject := ""
			if len(args) == 2 {
				subject = args[1]
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			var total *int64
			items, more, err := fetchPages(ctx, f.l, func(ctx context.Context, size int32, token string) ([]*v1.Session, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListSessionsResponse, error) {
					return c.Catalog.ListSessions(ctx, &v1.ListSessionsRequest{
						Tenant: args[0], SubjectLabel: subject, LabelPrefix: f.prefix, StudyDateStart: dateFrom, StudyDateEnd: dateTo,
						Modality: modality, Receiver: receiver, StartTime: from, EndTime: to, Order: order, PageSize: size, PageToken: token,
					})
				})
				if err != nil {
					return nil, "", err
				}
				if res.TotalCount != nil {
					total = res.TotalCount
				}
				return res.GetSessions(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return catalogOut(a, "sessions", "session", items, more, total, func() error {
				rows := make([][]string, 0, len(items))
				for _, s := range items {
					rows = append(rows, []string{
						s.GetSubjectLabel(), s.GetLabel(), output.Dash(s.GetStudyDate()), output.Dash(strings.Join(s.GetModalities(), ",")),
						count(s.GetScanCount()), count(s.GetInstanceCount()), output.Human(s.GetSizeBytes()), output.Dash(s.GetReceiver()), a.when(s.GetArchiveTime()),
					})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"SUBJECT", "SESSION", "STUDY DATE", "MODALITIES", "SCANS", "INSTANCES", "SIZE", "RECEIVER", "ARCHIVED"}, rows)
				})
			})
		}),
	}
	f.add(cmd, "sessions")
	cmd.Flags().StringVar(&dateFrom, "study-date-from", "", "only sessions whose study date is on or after this date, YYYYMMDD (order study-date)")
	cmd.Flags().StringVar(&dateTo, "study-date-to", "", "only sessions whose study date is on or before this date, YYYYMMDD (order study-date)")
	cmd.Flags().StringVar(&modality, "modality", "", "only sessions with a scan of this modality, such as MR (order newest)")
	cmd.Flags().StringVar(&receiver, "receiver", "", "only sessions first archived through the receiver of this name (order newest)")
	return cmd
}

// newCatalogScans builds "catalog scans".
func (a *app) newCatalogScans() *cobra.Command {
	var f catalogFlags
	var modality, description, receiver string
	cmd := &cobra.Command{
		Use:   "scans <tenant> [<subject> [<session>]] [--modality M] [--description-prefix P] [--receiver R] [--order O] [--limit N | --all]",
		Short: "List a tenant's scans, a subject's, or a session's",
		Long: `List scans with their modality, series number and description, instance count
and size. Name a subject to list its scans, by label unless --order says
otherwise, and a session of it to list that session's, newest first. A
description prefix comes with --order series-description; a modality or a
receiver with the newest order, and the server refuses a combination it does
not serve.`,
		Example: `  nopacs catalog scans lab1 sub-3f9a ses-81c2
  nopacs catalog scans lab1 sub-3f9a
  nopacs catalog scans lab1 --modality CT --json`,
		Args: rangeArgs(1, 3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			order, from, to, err := f.read(a)
			if err != nil {
				return err
			}
			subject, session := "", ""
			if len(args) > 1 {
				subject = args[1]
			}
			if len(args) > 2 {
				session = args[2]
			}
			if f.order == "" {
				order = scansOrder(subject, session, modality, receiver, from != nil || to != nil)
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			var total *int64
			items, more, err := fetchPages(ctx, f.l, func(ctx context.Context, size int32, token string) ([]*v1.Scan, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListScansResponse, error) {
					return c.Catalog.ListScans(ctx, &v1.ListScansRequest{
						Tenant: args[0], SubjectLabel: subject, SessionLabel: session, LabelPrefix: f.prefix, Modality: modality,
						SeriesDescriptionPrefix: description, Receiver: receiver, StartTime: from, EndTime: to, Order: order, PageSize: size, PageToken: token,
					})
				})
				if err != nil {
					return nil, "", err
				}
				if res.TotalCount != nil {
					total = res.TotalCount
				}
				return res.GetScans(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return catalogOut(a, "scans", "scan", items, more, total, func() error {
				rows := make([][]string, 0, len(items))
				for _, s := range items {
					rows = append(rows, []string{
						s.GetSubjectLabel(), s.GetSessionLabel(), s.GetLabel(), output.Dash(s.GetModality()), seriesNumber(s),
						output.Dash(s.GetSeriesDescription()), count(s.GetInstanceCount()), output.Human(s.GetSizeBytes()), a.when(s.GetArchiveTime()),
					})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"SUBJECT", "SESSION", "SCAN", "MODALITY", "SERIES", "DESCRIPTION", "INSTANCES", "SIZE", "ARCHIVED"}, rows)
				})
			})
		}),
	}
	f.add(cmd, "scans")
	cmd.Flags().StringVar(&modality, "modality", "", "only scans of this modality, such as MR (order newest)")
	cmd.Flags().StringVar(&description, "description-prefix", "", "only scans whose series description starts with this, ignoring case (order series-description)")
	cmd.Flags().StringVar(&receiver, "receiver", "", "only scans first archived through the receiver of this name (order newest)")
	return cmd
}

// scansOrder is the order "catalog scans" asks for when --order is not
// given. The scans of one subject are served by label only (the subject
// keeps no index of its scans by time), so a subject named alone, with no
// filter the newest order needs, is listed by label; everything else is
// listed newest first, and a combination no index serves is refused by the
// server with the combinations it serves.
func scansOrder(subject, session, modality, receiver string, span bool) v1.CatalogOrder {
	if subject != "" && session == "" && modality == "" && receiver == "" && !span {
		return v1.CatalogOrder_CATALOG_ORDER_LABEL
	}
	return v1.CatalogOrder_CATALOG_ORDER_UNSPECIFIED
}

// seriesNumber is a scan's Series Number, or a dash when it has none.
func seriesNumber(s *v1.Scan) string {
	if s.SeriesNumber == nil {
		return "-"
	}
	return strconv.Itoa(int(s.GetSeriesNumber()))
}
