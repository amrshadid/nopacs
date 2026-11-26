package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Paging limits. The contract's page holds at most 1000 and 100 by default.
const (
	defaultLimit = 100
	maxPageSize  = 1000
)

// maxAddressFile is the contract's limit on a database address, 4 KiB. The
// file is read one byte past it, so a longer one is refused, not cut.
const maxAddressFile = 4096

// roleText is a role as a word; no role is "none".
func roleText(r v1.Role) string { return output.Role(r) }

// databaseText is a database type as a word.
func databaseText(t v1.DatabaseType) string { return output.Database(t) }

// stateText is a tenant state as a word.
func stateText(s v1.TenantState) string { return output.State(s) }

// outcomeText is an audit outcome as a word.
func outcomeText(o v1.AuditOutcome) string { return output.Outcome(o) }

// scopeText is a scope as words: "installation" or "tenant <name>".
func scopeText(sc *v1.Scope) string { return output.Scope(sc) }

// parseRole reads a role name as the person types it.
func parseRole(s string) (v1.Role, error) {
	switch strings.ToLower(s) {
	case "reader":
		return v1.Role_ROLE_READER, nil
	case "writer":
		return v1.Role_ROLE_WRITER, nil
	case "operator":
		return v1.Role_ROLE_OPERATOR, nil
	case "admin":
		return v1.Role_ROLE_ADMIN, nil
	}
	return 0, fault.New(fault.Invalid, fmt.Sprintf("--role %q is not a role; use reader, writer, operator or admin", output.Clean(s)))
}

// parseOutcome reads an audit outcome name.
func parseOutcome(s string) (v1.AuditOutcome, error) {
	switch strings.ToLower(s) {
	case "done":
		return v1.AuditOutcome_AUDIT_OUTCOME_DONE, nil
	case "refused":
		return v1.AuditOutcome_AUDIT_OUTCOME_REFUSED, nil
	case "failed":
		return v1.AuditOutcome_AUDIT_OUTCOME_FAILED, nil
	}
	return 0, fault.New(fault.Invalid, fmt.Sprintf("--outcome %q is not an outcome; use done, refused or failed", output.Clean(s)))
}

// parseSpan reads a length of time written as a number and a unit, such as 90d.
func parseSpan(s string) (time.Duration, error) { return output.ParseSpan(s) }

// parseTime reads the --since and --until forms: an age such as 24h or 7d
// (that long before now), a date (midnight in the zone times are shown in) or
// a full time with its offset (RFC 3339).
func parseTime(flag, s string, now time.Time, loc *time.Location) (time.Time, error) {
	if d, err := parseSpan(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fault.New(fault.Invalid, fmt.Sprintf("--%s %q is not a time; use a date (2026-11-17), a time with its offset (2026-11-17T09:30:00+03:00) or an age (24h, 7d)", flag, output.Clean(s)))
}

// checkKeyID refuses a value that is a whole key.
func checkKeyID(id string) error { return output.CheckKeyID(id) }

// readAddress reads a tenant's database address from the environment variable
// named by dbEnv or from the file named by dbFile, and from nowhere else. The
// messages name the variable or the file and never show any of the address.
func (a *app) readAddress(dbEnv, dbFile string) (string, error) {
	switch {
	case dbEnv != "":
		v := strings.TrimSpace(a.env.Getenv(dbEnv))
		if v == "" {
			return "", fault.New(fault.Invalid, "--db-env names "+dbEnv+", which is not set or is empty")
		}
		if len(v) > maxAddressFile {
			return "", fault.New(fault.Invalid, dbEnv+" holds an address longer than 4 KiB")
		}
		return v, nil
	case dbFile != "":
		f, err := os.Open(dbFile) // #nosec G304 -- the path is the person's own --db-file
		if err != nil {
			return "", fault.Wrap(fault.Invalid, err, "cannot read --db-file "+dbFile)
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, maxAddressFile+1))
		if err != nil {
			return "", fault.Wrap(fault.Invalid, err, "cannot read --db-file "+dbFile)
		}
		if len(b) > maxAddressFile {
			return "", fault.New(fault.Invalid, dbFile+" holds an address longer than 4 KiB")
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fault.New(fault.Invalid, "--db-file "+dbFile+" is empty")
		}
		return v, nil
	}
	return "", nil
}

// scopeFlags are the two ways to name a scope on the command line: a tenant,
// or the installation.
type scopeFlags struct {
	tenant       string
	installation bool
}

// add registers --tenant and --installation on a command. what says what the
// scope is for, in the flags' help text.
func (f *scopeFlags) add(cmd *cobra.Command, forWhat string) {
	cmd.Flags().StringVar(&f.tenant, "tenant", "", "the tenant "+forWhat)
	cmd.Flags().BoolVar(&f.installation, "installation", false, "the installation itself "+forWhat+", not a tenant")
}

// scope turns the flags into the contract's Scope. The command marks the two
// flags as exclusive and one required, so exactly one is set.
func (f *scopeFlags) scope() *v1.Scope {
	if f.installation {
		return &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}
	}
	return &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: f.tenant}}
}

// optionalScope is the scope of a command where --tenant is optional: the
// tenant when named, else the installation.
func optionalScope(tenant string) *v1.Scope {
	if tenant != "" {
		return &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: tenant}}
	}
	return &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}
}

// listFlags are the paging flags of every list command.
type listFlags struct {
	limit int
	all   bool
}

// add registers --limit and --all.
func (f *listFlags) add(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.limit, "limit", defaultLimit, "the most items to fetch; when more exist the command says so")
	cmd.Flags().BoolVar(&f.all, "all", false, "fetch every item, page after page")
	cmd.MarkFlagsMutuallyExclusive("limit", "all")
}

// check refuses a limit that cannot be used.
func (f *listFlags) check() error {
	if f.limit < 1 {
		return fault.New(fault.Invalid, "--limit must be at least 1")
	}
	return nil
}

// fetchPages reads a list a page at a time until it has limit items (or every
// item, with all) and reports whether the server has more. The page size asked
// for is what is still needed, up to the contract's 1000, so --limit 100 is one
// call. A server that answers the token it was just given would make the loop
// run forever, so that is refused.
func fetchPages[T any](ctx context.Context, l listFlags, fetch func(ctx context.Context, size int32, token string) (items []T, next string, err error)) (items []T, more bool, err error) {
	token := ""
	for {
		size := maxPageSize
		if !l.all {
			size = min(l.limit-len(items), maxPageSize)
		}
		page, next, err := fetch(ctx, int32(size), token) // #nosec G115 -- size is at most 1000
		if err != nil {
			return nil, false, err
		}
		items = append(items, page...)
		if next == "" {
			return items, false, nil
		}
		if next == token {
			return nil, false, fault.New(fault.Internal, "the server answered the same page token twice")
		}
		if !l.all && len(items) >= l.limit {
			return items, true, nil
		}
		token = next
	}
}

// moreNote tells the person, on standard error so a pipe stays clean, that the
// list was cut short and how to get the rest.
func (a *app) moreNote(shown int) {
	_, _ = fmt.Fprintf(a.env.Stderr, "nopacs: more exist; showing the first %d; use --limit N or --all to see more\n", shown)
}

// listOut writes a fetched list: as a JSON document with the items under the
// contract's name and a "more" flag, or as a table by the given function. The
// note about more items goes to standard error only for the table, because a
// program reads "more" from the document.
func listOut[T proto.Message](a *app, field string, items []T, more bool, table func() error) error {
	if a.opts.json {
		return a.write(func(w io.Writer) error { return output.JSONList(w, field, items, more) })
	}
	if err := table(); err != nil {
		return err
	}
	if more {
		a.moreNote(len(items))
	}
	return nil
}

// call runs one API call for a command with its name in any error.
func call[T any](ctx context.Context, c *client.Client, cmd *cobra.Command, fn func(context.Context) (*T, error)) (*T, error) {
	return client.Call(ctx, c, what(cmd), fn)
}

// boundedInt32 converts the value of an integer flag to the 32 bits the contract
// carries, after checking that it lies in lo..hi: a value outside the range is
// refused as invalid with the bounds named, so a large number can never wrap
// into a valid one on its way to the server.
func boundedInt32(flag string, v, lo, hi int) (int32, error) {
	if v < lo || v > hi {
		return 0, fault.New(fault.Invalid, fmt.Sprintf("--%s %d is not from %d to %d", flag, v, lo, hi))
	}
	return int32(v), nil // #nosec G115 -- v is within lo..hi, and every bound passed is far below 2^31
}

// open makes the client for a command and returns it with the function that
// closes it.
func (a *app) open(cmd *cobra.Command) (*client.Client, func(), error) {
	c, err := a.client(cmd)
	if err != nil {
		return nil, nil, err
	}
	return c, c.Close, nil
}

// messageOut writes the answer of a call: the JSON document of the response
// when --json is set, and otherwise what the table function writes.
func messageOut(a *app, m proto.Message, table func(w io.Writer) error) error {
	if a.opts.json {
		return a.write(func(w io.Writer) error { return output.JSONMessage(w, m) })
	}
	return a.write(table)
}

// timeRange reads the --since and --until of a list command into the
// contract's bounds: either may be absent, and when both are given the end
// must be after the start, since a list that can never hold anything is a
// mistake the person should hear about.
func (a *app) timeRange(since, until string) (from, to *timestamppb.Timestamp, err error) {
	now := a.env.Now()
	if since != "" {
		t, err := parseTime("since", since, now, a.env.Location)
		if err != nil {
			return nil, nil, err
		}
		from = timestamppb.New(t)
	}
	if until != "" {
		t, err := parseTime("until", until, now, a.env.Location)
		if err != nil {
			return nil, nil, err
		}
		to = timestamppb.New(t)
	}
	if from != nil && to != nil && !to.AsTime().After(from.AsTime()) {
		return nil, nil, fault.New(fault.Invalid, "--until must be after --since")
	}
	return from, to, nil
}

// parseSize reads a size a flag takes, a whole number of bytes or one with a
// binary unit such as 200GiB, the form the settings take.
func parseSize(flag, s string) (int64, error) {
	n, err := settings.ParseSize(s)
	if err != nil {
		return 0, fault.New(fault.Invalid, fmt.Sprintf("--%s %q %s", flag, output.Clean(s), err))
	}
	return n, nil
}

// enumNames lists the names a person may type for an enum's values, each the
// value's name without its prefix, in lowercase with hyphens, in the order of
// the values and without the unspecified one, which is never a choice.
func enumNames(values map[int32]string, prefix string) []string {
	nums := make([]int32, 0, len(values))
	for n := range values {
		if n != 0 {
			nums = append(nums, n)
		}
	}
	slices.Sort(nums)
	names := make([]string, len(nums))
	for i, n := range nums {
		names[i] = strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(values[n], prefix)), "_", "-")
	}
	return names
}

// parseEnum reads the name of an enum value as a person types it, one of
// enumNames, and says the choices when it is not one.
func parseEnum[E ~int32](flag, s string, values map[int32]string, prefix string) (E, error) {
	want := strings.ToLower(s)
	for n, name := range values {
		if n != 0 && strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, prefix)), "_", "-") == want {
			return E(n), nil
		}
	}
	return 0, fault.New(fault.Invalid, fmt.Sprintf("--%s %q is not one of: %s", flag, output.Clean(s), strings.Join(enumNames(values, prefix), ", ")))
}

// enumCompletion offers an enum's names to the shell for a flag.
func enumCompletion(cmd *cobra.Command, flag string, values map[int32]string, prefix string) {
	_ = cmd.RegisterFlagCompletionFunc(flag, cobra.FixedCompletions(enumNames(values, prefix), cobra.ShellCompDirectiveNoFileComp))
}

// checkReason refuses, before anything is sent, a reason the contract would
// refuse: empty or only spaces. The server says the rest, such as the length.
func checkReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fault.New(fault.Invalid, "--reason must say why, in words for whoever reads the record later")
	}
	return nil
}

// confirmTyped is the one guard of a command that removes data for good: with
// --yes it passes; on a terminal it asks for the id to be typed again and
// passes only for the same text; otherwise it refuses with exit 2, saying what
// would have happened, and nothing is sent. done is the past participle of
// what the command does ("rejected") and effect says what it would do.
func (a *app) confirmTyped(yes bool, id, done, effect string) error {
	if yes {
		return nil
	}
	if a.env.Stdin == nil || !a.terminal() {
		return fault.New(fault.Invalid, fmt.Sprintf("nothing was %s: this would %s; run it again with --yes to do it", done, effect))
	}
	_, _ = fmt.Fprintf(a.env.Stderr, "This would %s.\nType the id %s again to go on: ", output.Clean(effect), output.Clean(id))
	line, err := bufio.NewReader(io.LimitReader(a.env.Stdin, 4096)).ReadString('\n')
	if err != nil && line == "" {
		return fault.New(fault.Invalid, fmt.Sprintf("nothing was %s: no confirmation was typed", done))
	}
	if strings.TrimSpace(line) != id {
		return fault.New(fault.Invalid, fmt.Sprintf("nothing was %s: the id typed is not the id given", done))
	}
	return nil
}
