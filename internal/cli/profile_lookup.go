package cli

import (
	"context"
	"encoding/csv"
	"errors"
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

// The bounds of an import, the server's (deid.proto, ImportLookupCodes).
const (
	maxLookupEntries = 10000
	maxLookupValue   = 1 << 10
	// maxLookupFile bounds the file read: every entry at its longest.
	maxLookupFile = maxLookupEntries * (maxLookupValue + 64)
)

// newProfileLookup builds "profile lookup".
func (a *app) newProfileLookup() *cobra.Command {
	return group("lookup", "Fill and list the lookup tables a profile's lookup rules read",
		`A lookup rule replaces a value by the code a table of the tenant gives it. The
table is keyed by the keyed hash of each value under the tenant's naming key,
never the value. Codes are assigned in order on first sight (on_missing assign),
or chosen by a person and imported here before the files arrive, for a rule
that holds a file whose value has no code (on_missing quarantine).

import needs the admin role on the tenant; list needs operator. Neither shows a
value or a code.`,
		a.newLookupImport(), a.newLookupList())
}

// lookupColumns are the columns of an import's file, in order: the first
// two always, the others when the file has them.
var lookupColumns = []string{"value", "code", "issuer", "namespace"}

// readLookupCSV reads an import's file: each record a value and its code,
// and, in a file of three or four columns, the Issuer of Patient ID of a
// Patient ID value and the namespace of one with no issuer, empty for
// none; every record has the file's first record's count of columns, and
// a first record of the column names (any case) is taken as a header.
func readLookupCSV(path string) ([]*v1.LookupEntry, error) {
	f, err := os.Open(path) // #nosec G304 -- the person names the file to import.
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read "+path)
	}
	defer func() { _ = f.Close() }()
	r := csv.NewReader(io.LimitReader(f, maxLookupFile+1))
	r.FieldsPerRecord = 0
	r.ReuseRecord = true
	notCSV := func(why string) error {
		return fault.New(fault.Invalid, fmt.Sprintf("%s is not a CSV of value and code, and optionally issuer and namespace: %s", path, why))
	}
	var out []*v1.LookupEntry
	for line := 1; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, notCSV(csvProblem(err))
		}
		if len(rec) < 2 || len(rec) > len(lookupColumns) {
			return nil, notCSV("line " + strconv.Itoa(line) + " has " + strconv.Itoa(len(rec)) + " columns, not 2 to 4")
		}
		if line == 1 && isLookupHeader(rec) {
			continue
		}
		if len(out) == maxLookupEntries {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%s holds more than %d entries, the most one import takes; split it", path, maxLookupEntries))
		}
		e := &v1.LookupEntry{Value: rec[0], Code: strings.TrimSpace(rec[1])}
		if len(rec) > 2 && rec[2] != "" {
			e.Issuer = &v1.PatientIDIssuer{Issuer: rec[2]}
		}
		if len(rec) > 3 {
			e.Namespace = strings.TrimSpace(rec[3])
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, fault.New(fault.Invalid, path+" holds no entry")
	}
	return out, nil
}

// isLookupHeader reports whether rec is the column names of an import's
// file, in their order, in any case.
func isLookupHeader(rec []string) bool {
	for i, v := range rec {
		if !strings.EqualFold(strings.TrimSpace(v), lookupColumns[i]) {
			return false
		}
	}
	return true
}

// csvProblem says where a CSV is wrong without quoting any of it: a value
// of the file may identify a person.
func csvProblem(err error) string {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return "line " + strconv.Itoa(pe.Line) + ", " + pe.Err.Error()
	}
	return "it cannot be read"
}

// newLookupImport builds "profile lookup import".
func (a *app) newLookupImport() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <tenant> <table> FILE",
		Short: "Import chosen codes into a lookup table from a CSV of value,code[,issuer[,namespace]]",
		Long: `Import the codes a person chose into a lookup table, from a CSV file of two
columns: the original value and its code. A first line "value,code" is a header.
A code is 1 to 16 of A-Z, 0-9 and _. The server stores each value as its keyed
hash, never as given, with its padding removed as an LO value's (spaces before
and after).

For a table a lookup rule on the Patient ID reads, a third column "issuer" gives
each Patient ID's Issuer of Patient ID (0010,0021) and a fourth, "namespace",
the receiver namespace of a Patient ID that comes with no issuer: a Patient ID
is unique only within its issuer, so the entry is stored by the keyed hash of
the subject identity, the Patient ID with its issuer or namespace, which is
what such a rule meets in a file from that issuer. An entry with both empty is
stored by its value alone, as in a file of two columns. Every line has the
first line's number of columns.

All or nothing, at most 10000 entries: an entry whose value the table already
gives the same code is counted unchanged. One value with two codes in the file,
or one code for two values, refuses the whole import naming the entry (its
place among the entries, the header not counted; exit 2). Values the table
gives another code, or codes it gives another value, refuse it too (exit 5),
answered by how many conflict, never which: an import cannot tell whether a
given person is in the table. Nothing is stored either way. One key makes at
most 10 imports an hour, stored or refused (exit 7 past it). The answer counts
what was added and what was unchanged; every import's audit entry, written
before the answer, counts the same, with no value and no code. Needs the admin
role on the tenant.`,
		Example: `  nopacs profile lookup import lab1 subjects cohort-codes.csv
  nopacs profile lookup import lab1 subjects cohort-codes.csv --json`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			entries, err := readLookupCSV(args[2])
			if err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ImportLookupCodesResponse, error) {
				return c.Deid.ImportLookupCodes(ctx, &v1.ImportLookupCodesRequest{Tenant: args[0], Table: args[1], Entries: entries})
			})
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error {
				return output.Details(w, a.style(), []output.Pair{
					{Label: "Table", Value: res.GetTable()},
					{Label: "Added", Value: strconv.Itoa(int(res.GetAdded()))},
					{Label: "Unchanged", Value: strconv.Itoa(int(res.GetUnchanged()))},
					{Label: "Codes in the table", Value: strconv.FormatInt(res.GetCodes(), 10)},
				})
			})
		}),
	}
	return cmd
}

// newLookupList builds "profile lookup list".
func (a *app) newLookupList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use:     "list <tenant>",
		Short:   "List a tenant's lookup tables with how many codes each holds",
		Long:    `List a tenant's lookup tables by name, each with how many codes it holds, imported and assigned together. No value and no code is shown. Needs the operator role on the tenant.`,
		Example: `  nopacs profile lookup list lab1`,
		Args:    exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			items, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.LookupTable, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListLookupTablesResponse, error) {
					return c.Deid.ListLookupTables(ctx, &v1.ListLookupTablesRequest{Tenant: args[0], PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetTables(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "tables", items, more, func() error {
				rows := make([][]string, 0, len(items))
				for _, t := range items {
					rows = append(rows, []string{t.GetName(), strconv.FormatInt(t.GetCodes(), 10)})
				}
				return a.write(func(w io.Writer) error { return output.Table(w, a.style(), []string{"TABLE", "CODES"}, rows) })
			})
		}),
	}
	l.add(cmd)
	return cmd
}
