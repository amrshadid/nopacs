package backup

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Print writes what the restore did and, per scope, what it could not
// bring back, in plain words for the person who ran it. Every name and id
// read from the backup or a storage is printed with its control and format
// characters replaced (output.Clean), so a crafted name cannot move the
// cursor or pass one line off as another.
func (r Report) Print(w io.Writer) error {
	var b strings.Builder
	c := output.Clean
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	fmt.Fprintf(&b, "Restored installation %s from a backup made at %s.\n\n", c(r.Installation), ts(r.BackupTime))
	b.WriteString("What came back, each database as of its recovery point:\n")
	for _, s := range r.Sections {
		name := "master"
		if s.Tenant != "" {
			name = "tenant " + c(s.Tenant)
		}
		fmt.Fprintf(&b, "  %-24s as of %s: %d tables, %d rows (schema version %d)\n", name, ts(s.Point.SnapshotTime), s.Tables, s.Rows,
			s.Point.SchemaVersion)
	}
	fmt.Fprintf(&b, "\nKeys: all %d suspended. A key revoked, or a grant removed, after the backup would otherwise work\n", r.KeysSuspended)
	b.WriteString("again. Each stays refused; to give a holder a working key, the local operator, on this machine's socket,\n")
	b.WriteString("rotates it (nopacs key rotate ID): its successor holds the same grants and is not suspended.\n")
	for _, t := range r.Tenants {
		fmt.Fprintf(&b, "\nTenant %s:\n", c(t.Tenant))
		for _, o := range t.Owners {
			fmt.Fprintf(&b, "  storage %s taken over at owner generation %d\n", c(o.Storage), o.Generation)
		}
		fmt.Fprintf(&b, "  correction records read: %d\n", t.Records)
		if t.Added > 0 {
			fmt.Fprintf(&b, "    %d with no row in the backup: each made a correction, unresolved, whatever its record says\n"+
				"      (a delete holds its data hidden, and its arrivals held, until an admin undoes or purges it; a record\n"+
				"      is not trusted to have purged or undone anything)\n", t.Added)
		}
		if t.Disagreed > 0 {
			fmt.Fprintf(&b, "    %d that disagree with the backup's row: the row was kept as the backup holds it, and the\n"+
				"      server writes those records again from it at its start: %s", t.Disagreed, strings.Join(cleanAll(t.Disagreements), ", "))
			if t.Disagreed > len(t.Disagreements) {
				fmt.Fprintf(&b, " and %d more", t.Disagreed-len(t.Disagreements))
			}
			b.WriteString("\n")
		}
		if t.Unauthenticated > 0 {
			fmt.Fprintf(&b, "    %d that do not verify under this key file for this tenant (no MAC, or a wrong one): each was\n"+
				"      written by someone else or changed since, and was treated as if it were not there: %s",
				t.Unauthenticated, strings.Join(cleanAll(t.UnauthenticatedIDs), ", "))
			if t.Unauthenticated > len(t.UnauthenticatedIDs) {
				fmt.Fprintf(&b, " and %d more", t.Unauthenticated-len(t.UnauthenticatedIDs))
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "  catalog rebuilt from the storages by job %s (%s)", c(t.Rebuild.ID), c(string(t.Rebuild.State)))
		var left []string
		for _, x := range t.Tallies {
			if strings.HasPrefix(x.Key, "left_out|") || x.Key == tenant.TallyUnresolvedCorrection || x.Key == tenant.TallyUnauthenticatedCorrection {
				left = append(left, fmt.Sprintf("%s %d", c(x.Key), x.Count))
			}
		}
		if len(left) > 0 {
			fmt.Fprintf(&b, "; %s", strings.Join(left, ", "))
		}
		b.WriteString("\n  lost:\n")
		b.WriteString("    configuration changed after its recovery point (the storages show only what corrections did)\n")
		if len(t.Loss.Receptions) == 0 {
			b.WriteString("    receptions: none was unfinished at the recovery point\n")
		}
		for _, x := range t.Loss.Receptions {
			fmt.Fprintf(&b, "    receiver %s: %d receptions unfinished, %d files (%d bytes) acknowledged and not archived;\n"+
				"      they were in incoming and are gone: the sender must send them again\n", c(x.Receiver), x.Receptions, x.Files, x.Bytes)
		}
		fmt.Fprintf(&b, "    quarantine: %d held files (%d bytes) whose bytes were in incoming; their records came back,\n"+
			"      %d of them marked incoming lost\n", t.Loss.QuarantineFiles, t.Loss.QuarantineBytes, t.QuarantineMarked)
		b.WriteString("    events after its recovery point; the feed begins a new history, which tells every consumer\n")
	}
	b.WriteString("\nAudit entries written after each recovery point are gone; the restore's own entry is in the master's log.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// cleanAll returns each of s cleaned (output.Clean).
func cleanAll(s []string) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = output.Clean(x)
	}
	return out
}
