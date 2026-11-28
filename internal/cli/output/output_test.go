package output_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// TestClean guards the line between the network and the terminal: control
// characters of every kind, an escape and a newline among them, become "?",
// while ordinary text, including non-ASCII, is untouched.
func TestClean(t *testing.T) {
	cases := map[string]string{
		"plain":               "plain",
		"tenant-é":            "tenant-é",
		"\x1b[2Jscreen":       "?[2Jscreen",
		"two\nlines":          "two?lines",
		"tab\there":           "tab?here",
		"bell\x07":            "bell?",
		"c1\u009bcontrol":     "c1?control",
		"bad\xffutf8":         "bad?utf8",
		"return\rcarriage":    "return?carriage",
		"\u202eright-to-left": "?right-to-left",
		"zero\u200bwidth":     "zero?width",
	}
	for in, want := range cases {
		if got := output.Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTime guards how a time is shown: in the given zone, with its offset, and
// "-" when absent.
func TestTime(t *testing.T) {
	zone := time.FixedZone("EAT", 3*3600)
	ts := timestamppb.New(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	if got := output.Time(ts, zone); got != "2026-11-17 15:00:00 +03:00" {
		t.Errorf("Time = %q", got)
	}
	if got := output.Time(nil, zone); got != "-" {
		t.Errorf("Time(nil) = %q", got)
	}
}

// TestTableAlignsAndCleans guards the table: columns line up by characters
// (not bytes), there is no trailing space, a cell from the network is
// cleaned, and bold appears only when asked for and does not move the columns.
func TestTableAlignsAndCleans(t *testing.T) {
	header := []string{"NAME", "NOTE"}
	rows := [][]string{{"café", "one"}, {"x\x1b[31m", "two"}, {"longer-name", ""}}
	var plain bytes.Buffer
	if err := output.Table(&plain, output.Style{}, header, rows); err != nil {
		t.Fatal(err)
	}
	want := "NAME         NOTE\ncafé         one\nx?[31m       two\nlonger-name  \n"
	// The last cell of the last row is empty, so its line ends after the
	// padding of the previous column; trailing spaces are not added after a
	// last column, and that row's first column is the widest, so it has two.
	if plain.String() != want {
		t.Errorf("table:\n%q\nwant:\n%q", plain.String(), want)
	}
	var bold bytes.Buffer
	if err := output.Table(&bold, output.Style{Color: true}, header, rows); err != nil {
		t.Fatal(err)
	}
	stripped := strings.NewReplacer("\x1b[1m", "", "\x1b[0m", "").Replace(bold.String())
	if stripped != want {
		t.Errorf("bold changed the layout:\n%q\nwant:\n%q", stripped, want)
	}
	if !strings.HasPrefix(bold.String(), "\x1b[1mNAME\x1b[0m") || strings.Contains(strings.Split(bold.String(), "\n")[1], "\x1b") {
		t.Errorf("only the header is bold: %q", bold.String())
	}
}

// TestDetailsAligns guards the labeled lines: values start in one column.
func TestDetailsAligns(t *testing.T) {
	var b bytes.Buffer
	if err := output.Details(&b, output.Style{}, []output.Pair{{Label: "ID", Value: "k1"}, {Label: "Last used", Value: "never\n"}}); err != nil {
		t.Fatal(err)
	}
	if want := "ID:        k1\nLast used: never?\n"; b.String() != want {
		t.Errorf("details %q, want %q", b.String(), want)
	}
}

// TestJSONMessage guards the shape of a document: schemaVersion first, then
// the message's fields under the contract's JSON names, unset fields written
// as empty values, and the same bytes every time.
func TestJSONMessage(t *testing.T) {
	msg := &v1.GetTenantResponse{Tenant: &v1.Tenant{Name: "lab1", DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE}}
	var a, b bytes.Buffer
	for _, w := range []*bytes.Buffer{&a, &b} {
		if err := output.JSONMessage(w, msg); err != nil {
			t.Fatal(err)
		}
	}
	want := "{\n  \"schemaVersion\": 1,\n  \"tenant\": {\n    \"name\": \"lab1\",\n    \"databaseType\": \"DATABASE_TYPE_SQLITE\",\n    \"state\": \"TENANT_STATE_UNSPECIFIED\",\n    \"stateReason\": \"\",\n    \"createTime\": null,\n    \"stateTime\": null,\n    \"quota\": null\n  }\n}\n"
	if a.String() != want || b.String() != want {
		t.Errorf("document:\n%s\nwant:\n%s", a.String(), want)
	}
	var empty bytes.Buffer
	if err := output.JSONMessage(&empty, &v1.RemoveGrantResponse{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(empty.String(), "{\n  \"schemaVersion\": 1,\n  \"previousRole\": \"ROLE_UNSPECIFIED\"") {
		t.Errorf("empty-valued message: %s", empty.String())
	}
}

// TestJSONList guards a list document: an array, never null, when there are no
// items, and the "more" flag.
func TestJSONList(t *testing.T) {
	var b bytes.Buffer
	if err := output.JSONList[*v1.Tenant](&b, "tenants", nil, true); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int   `json:"schemaVersion"`
		Tenants       []any `json:"tenants"`
		More          bool  `json:"more"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("%v\n%s", err, b.String())
	}
	if doc.SchemaVersion != 1 || doc.Tenants == nil || len(doc.Tenants) != 0 || !doc.More {
		t.Errorf("document %+v from\n%s", doc, b.String())
	}
}

// TestJSONValue guards the document of a command with no message of the API:
// schemaVersion is added in front, and a value that is not an object is
// refused.
func TestJSONValue(t *testing.T) {
	var b bytes.Buffer
	if err := output.JSONValue(&b, struct {
		Version string `json:"version"`
	}{"v1"}); err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"schemaVersion\": 1,\n  \"version\": \"v1\"\n}\n"; b.String() != want {
		t.Errorf("document %q, want %q", b.String(), want)
	}
	if err := output.JSONValue(&b, []int{1}); err == nil {
		t.Error("an array was accepted as a document")
	}
}

// TestQuarantineReasonWords guards that every reason the contract defines has
// words of its own, lowercase and spaced (so a new reason is never shown as
// "unknown" or as its enum name), and that one this client does not know is
// shown as one it cannot name.
func TestQuarantineReasonWords(t *testing.T) {
	for n, name := range v1.QuarantineReason_name {
		if n == 0 {
			continue
		}
		want := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, "QUARANTINE_REASON_")), "_", " ")
		if got := output.QuarantineReason(v1.QuarantineReason(n)); got != want {
			t.Errorf("%s is worded %q, want %q", name, got, want)
		}
	}
	for r, want := range map[v1.QuarantineReason]string{
		v1.QuarantineReason_QUARANTINE_REASON_HEADER_TOO_LARGE: "header too large",
		v1.QuarantineReason_QUARANTINE_REASON_INCOMING_LOST:    "incoming lost",
		v1.QuarantineReason(999):                               "a reason this client cannot name",
		v1.QuarantineReason_QUARANTINE_REASON_UNSPECIFIED:      "unknown",
	} {
		if got := output.QuarantineReason(r); got != want {
			t.Errorf("reason %d: %q, want %q", r, got, want)
		}
	}
}
