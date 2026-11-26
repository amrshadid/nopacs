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
	// src names where the rows came from: a backup's file, or the databases
	// a site restored by its own point-in-time recovery.
	src := "the backup"
	if r.ReconcileOnly {
		src = "the restored database"
		fmt.Fprintf(&b, "Reconciled installation %s, whose databases the site restored by its own point-in-time recovery,\n"+
			"with its storages. No backup was read: each database's recovery point is the newest rows it holds.\n\n", c(r.Installation))
		b.WriteString("What the restored databases hold:\n")
		for _, s := range r.Sections {
			name := "master"
			if s.Tenant == "" {
				fmt.Fprintf(&b, "  %-24s %d tables, %d rows (schema version %d)\n", name, s.Tables, s.Rows, s.Point.SchemaVersion)
				continue
			}
			name = "tenant " + c(s.Tenant)
			fmt.Fprintf(&b, "  %-24s %d tables, %d rows (schema version %d); newest correction %s, newest reception %s\n", name, s.Tables, s.Rows,
				s.Point.SchemaVersion, orNone(c(s.Point.LastCorrection)), orNone(c(s.Point.LastReception)))
		}
	} else {
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
	}
	since := "after the backup"
	if r.ReconcileOnly {
		since = "after the recovery point"
	}
	fmt.Fprintf(&b, "\nKeys: all %d suspended. A key revoked, or a grant removed, %s would otherwise work\n", r.KeysSuspended, since)
	b.WriteString("again. Each stays refused until the local operator, on this machine's socket, reviews the keys\n")
	b.WriteString("(nopacs key list) and reinstates those that should work (nopacs key reinstate ID... or --all).\n")
	for _, t := range r.Tenants {
		fmt.Fprintf(&b, "\nTenant %s:\n", c(t.Tenant))
		if t.Fenced != "" {
			// The reason says what to do: leave the storage to the
			// installation that writes it, --take-over only for a stale claim.
			fmt.Fprintf(&b, "  NOT reconciled, and left fenced (read only, serving no data): %s.\n"+
				"    Its rows are as %s holds them; nothing was written to its storages.\n", strings.TrimSuffix(c(t.Fenced), "."), src)
			continue
		}
		for _, o := range t.Owners {
			fmt.Fprintf(&b, "  storage %s taken over at owner generation %d\n", c(o.Storage), o.Generation)
		}
		fmt.Fprintf(&b, "  correction records read: %d\n", t.Records)
		if t.Added > 0 {
			fmt.Fprintf(&b, "    %d with no row in %s: each made a correction as its record says (a delete abandoned\n"+
				"      or undone shows its data; any other holds its data hidden, and its arrivals held, unresolved until an\n"+
				"      admin undoes or purges it: a record is not trusted to have purged anything)\n", t.Added, src)
		}
		if t.Moved > 0 {
			fmt.Fprintf(&b, "    %d whose row %s holds behind its record: each row was moved to the state its record says\n"+
				"      (one prepared before its request was answered and published since is unresolved, its purge left to\n"+
				"      an admin; a delete undone since is undone, its data shown)\n", t.Moved, src)
		}
		if t.Held > 0 {
			fmt.Fprintf(&b, "  %d deletes %s holds committed are kept, each unresolved: none is purged until an admin\n"+
				"    confirms it, since an undo made %s whose record was not rewritten cannot be told\n"+
				"    apart. Review each (nopacs correction list <tenant> --kind delete); undo one that was undone\n"+
				"    (nopacs correction undo), or confirm one by purging it now (nopacs correction purge)\n", t.Held, src, since)
		}
		if t.Disagreed > 0 {
			fmt.Fprintf(&b, "    %d that disagree with %s's row: the row was kept as %s holds it, and the\n"+
				"      server writes those records again from it at its start: %s", t.Disagreed, src, src, strings.Join(cleanAll(t.Disagreements), ", "))
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
		if len(t.HoldsAdopted) > 0 {
			fmt.Fprintf(&b, "  %d holds found active on the storages with no row in %s, each placed after its recovery point,\n"+
				"    are active again (their reason is not on a storage: only its digest): %s\n", len(t.HoldsAdopted), src,
				strings.Join(cleanAll(t.HoldsAdopted), ", "))
		}
		if t.HoldsRefused > 0 {
			fmt.Fprintf(&b, "  %d hold records do not verify under this key file for this tenant, or are not in their form: each\n"+
				"    makes no hold: %s", t.HoldsRefused, strings.Join(cleanAll(t.HoldsRefusedRecords), ", "))
			if t.HoldsRefused > len(t.HoldsRefusedRecords) {
				fmt.Fprintf(&b, " and %d more", t.HoldsRefused-len(t.HoldsRefusedRecords))
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
				"      they were in incoming at the recovery point and are not taken back: the sender must send them again\n",
				c(x.Receiver), x.Receptions, x.Files, x.Bytes)
		}
		if r.ReconcileOnly {
			fmt.Fprintf(&b, "    quarantine: %d held files (%d bytes) whose bytes were in incoming; %d of them marked incoming lost,\n"+
				"      their bytes missing there or not the recorded ones; the others' are there whole and can still be released\n",
				t.Loss.QuarantineFiles, t.Loss.QuarantineBytes, t.QuarantineMarked)
		} else {
			fmt.Fprintf(&b, "    quarantine: %d held files (%d bytes) whose bytes were in incoming; their records came back,\n"+
				"      %d of them marked incoming lost\n", t.Loss.QuarantineFiles, t.Loss.QuarantineBytes, t.QuarantineMarked)
		}
		b.WriteString("    events after its recovery point; the feed begins a new history, which tells every consumer\n")
		b.WriteString(CredentialsText(t.Credentials))
	}
	if r.ReconcileOnly {
		b.WriteString("\nAudit entries written after each recovery point are gone; the reconcile's own entry is in the master's log.\n")
	} else {
		b.WriteString("\nAudit entries written after each recovery point are gone; the restore's own entry is in the master's log.\n")
		b.WriteString("The network listener's certificate and key are files named when the server starts, not in a backup:\n" +
			"give them again if they were in the data directory.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// CredentialsText returns the lines that name the TLS files a tenant's
// receivers and senders name that the data directory lacks, by name only,
// or "" when none is missing: a backup or an export holds none, and a
// receiver without its files does not listen, a sender cannot connect.
func CredentialsText(creds []Credential) string {
	if len(creds) == 0 {
		return ""
	}
	var b strings.Builder
	c := output.Clean
	fmt.Fprintf(&b, "  external credentials to re-enter: files kept in the data directory, never in a backup; put each in %s\n", c(creds[0].Dir))
	for _, x := range creds {
		fmt.Fprintf(&b, "    %s %s: %s\n", x.Kind, c(x.Name), strings.Join(cleanAll(x.Files), ", "))
	}
	return b.String()
}

// orNone returns s, or "none" when it is empty.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// cleanAll returns each of s cleaned (output.Clean).
func cleanAll(s []string) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = output.Clean(x)
	}
	return out
}
