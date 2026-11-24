package sqlkit

import (
	"reflect"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestTimeCursor guards the one time cursor both drivers page newest-first
// lists by: a cursor made after a row reads back as that row's time to the
// microsecond and its id, the first page has none, a cursor of another
// binding is Invalid, and a filter's bound writes as its microseconds or as
// nothing for no bound.
func TestTimeCursor(t *testing.T) {
	at := time.Date(2026, 11, 18, 9, 30, 0, 123456789, time.UTC)
	c := TimeCursor("list", "b1", at, "rc-7")
	pos, err := PageAfterTime(master.Page{Limit: 10, Cursor: c}, "list", "b1")
	if err != nil || pos == nil || !pos.At.Equal(at.Truncate(time.Microsecond)) || pos.ID != "rc-7" {
		t.Fatalf("PageAfterTime = %+v, %v", pos, err)
	}
	if pos, err := PageAfterTime(master.Page{Limit: 10}, "list", "b1"); pos != nil || err != nil {
		t.Errorf("the first page = %+v, %v", pos, err)
	}
	if _, err := PageAfterTime(master.Page{Limit: 10, Cursor: c}, "list", "b2"); fault.KindOf(err) != fault.Invalid {
		t.Errorf("another binding's cursor: %v", err)
	}
	if MicrosText(time.Time{}) != "" || MicrosText(at) != "1794994200123456" {
		t.Errorf("MicrosText = %q, %q", MicrosText(time.Time{}), MicrosText(at))
	}
}

// TestCut guards the page cut every list makes from limit+1 rows.
func TestCut(t *testing.T) {
	rows, last, more := Cut([]int{1, 2, 3}, 2)
	if !reflect.DeepEqual(rows, []int{1, 2}) || last != 2 || !more {
		t.Errorf("Cut of 3 to 2 = %v, %d, %v", rows, last, more)
	}
	if rows, _, more := Cut([]int{1, 2}, 2); len(rows) != 2 || more {
		t.Errorf("Cut of 2 to 2 = %v, %v", rows, more)
	}
}

// TestReceiverConfig guards that a receiver's configuration reads back as
// it was written, every field, with and without a naming rule and TLS, so
// the one stored format serves both drivers.
func TestReceiverConfig(t *testing.T) {
	spec := naming.Spec{Subject: "PatientID", Session: "StudyDate", Scan: "SeriesNumber", PatternAttribute: "StudyDescription", Pattern: "(.*)"}
	full := tenant.Receiver{DIMSE: tenant.DIMSEReceiver{AETitle: "ARCHIVE", AnyCaller: true,
		Callers:    []tenant.CallerRule{{CallingAETitle: "SCANNER1", AddressRange: "192.0.2.0/24"}},
		TLS:        &tenant.ReceiverTLS{CertificateFile: "c.pem", PrivateKeyFile: "k.pem"},
		SOPClasses: []string{"1.2.840.10008.5.1.4.1.1.4"}, TransferSyntaxes: []string{"1.2.840.10008.1.2.1"}}, Naming: &spec}
	for _, r := range []tenant.Receiver{full, {DIMSE: tenant.DIMSEReceiver{AETitle: "PLAIN"}}} {
		config, namingText, err := ReceiverConfigText(r)
		if err != nil {
			t.Fatal(err)
		}
		var got tenant.Receiver
		if err := ReadReceiverConfig(&got, config, namingText); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.DIMSE, r.DIMSE) || !reflect.DeepEqual(got.Naming, r.Naming) {
			t.Errorf("read back %+v %+v, want %+v %+v", got.DIMSE, got.Naming, r.DIMSE, r.Naming)
		}
	}
	if err := ReadReceiverConfig(&tenant.Receiver{}, "{", ""); fault.KindOf(err) != fault.Internal {
		t.Errorf("a column that does not read: %v", err)
	}
}

// TestToSHA256 guards the reading of a checksum column: 32 bytes are the
// checksum, anything else (NULL included) is none.
func TestToSHA256(t *testing.T) {
	b := make([]byte, 32)
	b[0] = 7
	if s := ToSHA256(b); s[0] != 7 || s.IsZero() {
		t.Errorf("32 bytes read as %v", s)
	}
	if !ToSHA256(nil).IsZero() || !ToSHA256(b[:31]).IsZero() {
		t.Error("a short or NULL column is not none")
	}
}

// TestDetails guards the one encoding of an audit entry's details both
// drivers store: keys sorted whatever the map's order, no details written
// as {} and read back as nil, and a column that is not JSON an Internal
// fault, not a silent empty map.
func TestDetails(t *testing.T) {
	text, err := EncodeDetails(map[string]string{"b": "2", "a": "1"})
	if err != nil || text != `{"a":"1","b":"2"}` {
		t.Fatalf("encoded %q, %v", text, err)
	}
	back, err := DecodeDetails(text)
	if err != nil || !reflect.DeepEqual(back, map[string]string{"a": "1", "b": "2"}) {
		t.Fatalf("decoded %v, %v", back, err)
	}
	if text, _ := EncodeDetails(nil); text != "{}" {
		t.Fatalf("no details encoded as %q", text)
	}
	if back, err := DecodeDetails("{}"); err != nil || back != nil {
		t.Fatalf("{} decoded as %v, %v", back, err)
	}
	if _, err := DecodeDetails("not json"); fault.KindOf(err) != fault.Internal {
		t.Fatalf("a broken column: %v", err)
	}
}

// TestNullText guards that no value is NULL and any other is itself.
func TestNullText(t *testing.T) {
	if NullText("") != nil || NullText("t1") != "t1" {
		t.Fatal("NullText")
	}
}

// TestDistinctTypes guards that a type named twice in a read of events is
// read once, in the order it was first named.
func TestDistinctTypes(t *testing.T) {
	step, held, health := tenant.EventReceptionStep, tenant.EventFileQuarantined, tenant.EventReceiverHealth
	got := DistinctTypes([]tenant.EventType{held, step, held, health, step})
	if !reflect.DeepEqual(got, []tenant.EventType{held, step, health}) {
		t.Fatalf("got %v", got)
	}
	if DistinctTypes(nil) != nil {
		t.Fatal("no types gave some")
	}
}

// TestReleaseEntry guards a release's audit entry: it names the reception
// the file joined, and is the entry as given when it joined none.
func TestReleaseEntry(t *testing.T) {
	a := master.AuditEntry{Details: map[string]string{"reason": "read again"}}
	same, err := ReleaseEntry(a, "")
	if err != nil || !reflect.DeepEqual(same, a) {
		t.Fatalf("joined none: %+v, %v", same, err)
	}
	if e, _ := ReleaseEntry(a, "rc1"); e.Details[tenant.DetailReleaseReception] != "rc1" || e.Details["reason"] != "read again" {
		t.Fatalf("joined rc1: %+v", e.Details)
	}
}
