package testkit

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ForgeCursor returns a cursor of the list and binding of cursor, one a
// SQL driver's list returned, holding fields as its key: what a caller who
// was given a cursor can write, since a cursor ends with a checksum, not a
// signature. It reads the cursor as sqlkit.EncodeCursor writes it: fields
// of "length:text" (the version, the list, the binding, the key) and eight
// bytes of SHA-256.
func ForgeCursor(tb testing.TB, cursor string, fields ...string) string {
	tb.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) < 8 {
		tb.Fatalf("not a cursor: %q", cursor)
	}
	var parts []string
	for s := string(raw[:len(raw)-8]); s != "" && len(parts) < 3; {
		n, rest, ok := strings.Cut(s, ":")
		length, err := strconv.Atoi(n)
		if !ok || err != nil || length > len(rest) {
			tb.Fatalf("not a cursor: %q", cursor)
		}
		parts, s = append(parts, rest[:length]), rest[length:]
	}
	if len(parts) < 3 {
		tb.Fatalf("not a cursor: %q", cursor)
	}
	return sqlkit.EncodeCursor(parts[1], parts[2], fields...)
}

// CheckForgedCatalogCursors guards that every catalog list refuses, as the
// bad page token it is (Invalid, as any cursor it did not make), a cursor
// of its own list and filter whose key is outside the filter or holds what
// no key does: a text before or past a prefix or a range of study dates, a
// time past a range's end or no time a row has, an id that is not
// positive, an empty description, a label that is not folded, bytes that
// are not UTF-8 or a NUL (which Postgres refuses with an error, answered
// as the server's fault), a SOP Instance UID that is not one. Such a key
// would otherwise start the page outside the filter where the statement
// leaves a bound to the cursor. p is a tenant filled by a database
// driver's test catalog of at least thirty subjects, five sessions each,
// and v drawn from it (CatalogSample).
func CheckForgedCatalogCursors(t *testing.T, p tenant.CatalogReads, v CatalogSample) {
	t.Helper()
	ctx := context.Background()
	micros := func(at time.Time) string { return strconv.FormatInt(at.UnixMicro(), 10) }
	future := micros(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	end := v.SubjectKey.Created
	inRange := micros(end.Add(-time.Second))
	try := func(name string, list func(master.Page) (string, error), forged ...[]string) {
		t.Helper()
		next, err := list(master.Page{Limit: 1})
		if err != nil || next == "" {
			t.Fatalf("%s: the first page of one row gave no cursor (%v)", name, err)
		}
		if _, err := list(master.Page{Limit: 1, Cursor: next}); err != nil {
			t.Fatalf("%s: the cursor the list returned is refused: %v", name, err)
		}
		for _, fields := range forged {
			_, err := list(master.Page{Limit: 10, Cursor: ForgeCursor(t, next, fields...)})
			switch {
			case err == nil:
				t.Errorf("%s, a forged cursor of key %q: a page was answered, want Invalid", name, fields)
			case fault.KindOf(err) != fault.Invalid:
				t.Errorf("%s, a forged cursor of key %q: %v (%v), want Invalid", name, fields, err, fault.KindOf(err))
			}
		}
	}
	subjects := func(f tenant.SubjectFilter) func(master.Page) (string, error) {
		return func(pg master.Page) (string, error) { got, err := p.ListSubjects(ctx, f, pg); return got.Next, err }
	}
	sessions := func(f tenant.SessionFilter) func(master.Page) (string, error) {
		return func(pg master.Page) (string, error) { got, err := p.ListSessions(ctx, f, pg); return got.Next, err }
	}
	scans := func(f tenant.ScanFilter) func(master.Page) (string, error) {
		return func(pg master.Page) (string, error) { got, err := p.ListScans(ctx, f, pg); return got.Next, err }
	}
	subject := label.Fold(v.Subject)

	try("subjects by a label prefix", subjects(tenant.SubjectFilter{Order: tenant.OrderLabel, LabelPrefix: "sub-00001"}),
		[]string{""}, []string{"sub-000001"}, []string{"\xff"}, []string{"sub-00001\x00"}, []string{"SUB-000012"})
	try("subjects newest first before a time", subjects(tenant.SubjectFilter{Range: tenant.TimeRange{End: end}}),
		[]string{future, "1"}, []string{"9223372036854775807", "1"}, []string{"-9223372036854775808", "1"},
		[]string{inRange, "0"}, []string{inRange, "-1"})
	try("sessions by label", sessions(tenant.SessionFilter{Order: tenant.OrderLabel}),
		[]string{"\xff", "ses-1"}, []string{subject, "SES-1"}, []string{subject, "ses-1\x00"})
	try("a subject's sessions by a label prefix",
		sessions(tenant.SessionFilter{Order: tenant.OrderLabel, SubjectLabel: v.Subject, LabelPrefix: "ses"}),
		[]string{"sub-000001", "ses-1"}, []string{subject, "a"}, []string{subject, "\xff"})
	try("sessions by study date before an end", sessions(tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateEnd: "20200101"}),
		[]string{"20991231", "1"}, []string{"", "1"}, []string{"2015013", "1"}, []string{"20150230", "1"}, []string{"\xff", "1"},
		[]string{"20150101", "0"})
	try("sessions by study date after a start", sessions(tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateStart: "20150101"}),
		[]string{"20100101", "1"})
	try("the sessions of a receiver before a time",
		sessions(tenant.SessionFilter{Receiver: v.Receiver, Range: tenant.TimeRange{End: end}}),
		[]string{future, "1"}, []string{inRange, "0"})
	try("the sessions of a modality before a time",
		sessions(tenant.SessionFilter{Modality: v.Modality, Range: tenant.TimeRange{End: end}}),
		[]string{future, "1"})
	try("scans by a description prefix", scans(tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: "T1"}),
		[]string{"", "1"}, []string{"a", "1"}, []string{"t1\xff", "1"}, []string{"t1\x00", "1"}, []string{"T1 MPRAGE 5", "1"},
		[]string{"t1 mprage 5", "0"}, []string{"t1 mprage 5 ", "1"})
	try("scans by series description", scans(tenant.ScanFilter{Order: tenant.OrderSeriesDescription}),
		[]string{"", "1"}, []string{"\xff", "1"})
	try("a session's scans by a label prefix",
		scans(tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: v.Session[0], SessionLabel: v.Session[1], LabelPrefix: "scan"}),
		[]string{"sub-000001", label.Fold(v.Session[1]), "scan-1"}, []string{subject, label.Fold(v.Session[1]), "a"},
		[]string{subject, label.Fold(v.Session[1]), "scan-\xff"})
	try("scans by label", scans(tenant.ScanFilter{Order: tenant.OrderLabel}),
		[]string{subject, "\xff", "scan-1"}, []string{subject, "ses-1", "scan-1\x00"})
	try("the scans of a modality before a time", scans(tenant.ScanFilter{Modality: v.Modality, Range: tenant.TimeRange{End: end}}),
		[]string{future, "1"}, []string{inRange, "0"})

	held := func(pg master.Page) (string, error) {
		_, next, err := p.ListHeldInstances(ctx, v.Scan, pg)
		return next, err
	}
	try("a scan's instances", held, []string{"\xff"}, []string{"1.2.3\x00"}, []string{"not a uid"})
}
