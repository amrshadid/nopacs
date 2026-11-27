package tenant

import (
	"encoding/json"
	"testing"
	"time"
)

// markAt is the time of this file's events and failures.
var markAt = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

// TestSendRefusal guards the one rule of what may leave the archive: an
// instance archived as received, one whose profile version is recorded only
// in part, and one with an accepted pixel risk are each refused with their
// own code to a sender not marked, a de-identified instance is let out, and
// a marked sender is refused none of them; each code is one a job's failure
// may record, and counts as refused.
func TestSendRefusal(t *testing.T) {
	full := ProfileRef{Name: "research", Version: 3}
	cases := []struct {
		name string
		p    InstanceProvenance
		want JobFailureCode
	}{
		{"de-identified", InstanceProvenance{Profile: full}, ""},
		{"as received", InstanceProvenance{}, FailureAsReceived},
		{"a name without a version", InstanceProvenance{Profile: ProfileRef{Name: "research"}}, FailureNoProfileProvenance},
		{"a version without a name", InstanceProvenance{Profile: ProfileRef{Version: 3}}, FailureNoProfileProvenance},
		{"a version out of range", InstanceProvenance{Profile: ProfileRef{Name: "research", Version: MaxProfileVersions + 1}}, FailureNoProfileProvenance},
		{"a pixel risk accepted", InstanceProvenance{Profile: full, PixelRiskAccepted: true}, FailurePixelRiskAccepted},
		{"as received, a pixel risk said accepted", InstanceProvenance{PixelRiskAccepted: true}, FailureAsReceived},
	}
	for _, c := range cases {
		code, refused := SendRefusal(c.p, false)
		if code != c.want || refused != (c.want != "") {
			t.Errorf("%s, not marked: %q %v, want %q", c.name, code, refused, c.want)
		}
		if code, refused := SendRefusal(c.p, true); refused || code != "" {
			t.Errorf("%s, marked: refused %q", c.name, code)
		}
		if c.want != "" {
			f := JobFailure{SOPInstanceUID: "1.2.3", Code: c.want, Detail: "not sent", Time: markAt}
			if err := f.Validate(); err != nil || !c.want.Refused() {
				t.Errorf("%s: the code %q is not a refusal a job records: %v", c.name, c.want, err)
			}
		}
	}
	in := ScanInstance{Profile: full, PixelRiskAccepted: true}
	if got := in.Provenance(); got != (InstanceProvenance{Profile: full, PixelRiskAccepted: true}) {
		t.Errorf("a scan instance's provenance: %+v", got)
	}
}

// TestSenderAllowIdentifiedEvent guards the event of a sender's mark: it
// is valid only with a sender's name, and reads back as written.
func TestSenderAllowIdentifiedEvent(t *testing.T) {
	if _, err := NewEvent(&SenderAllowIdentifiedChanged{Sender: "Not A Name", AllowIdentified: true}, markAt); err == nil {
		t.Error("an event with a bad sender name was made")
	}
	e, err := NewEvent(&SenderAllowIdentifiedChanged{Sender: "pacs", AllowIdentified: true}, markAt)
	if err != nil {
		t.Fatal(err)
	}
	if e.Type() != EventSenderAllowIdentified || !EventSenderAllowIdentified.Valid() {
		t.Fatalf("the event's type %q", e.Type())
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(e.Payload()), &raw); err != nil || raw["sender"] != "pacs" || raw["allow_identified"] != true {
		t.Errorf("the payload %s", e.Payload())
	}
	p, ok := DecodeEvent(e.Type(), e.Payload())
	if got, is := p.(*SenderAllowIdentifiedChanged); !ok || !is || got.Sender != "pacs" || !got.AllowIdentified {
		t.Errorf("read back: %#v %v", p, ok)
	}
	if ServerEventType(EventSenderAllowIdentified) {
		t.Error("the mark's event is appended by the server, not written with its change")
	}
}

// TestRedirectsSender guards which fields move where a sender's bytes go,
// the ones a marked sender keeps for the installation's admin: host, port,
// AE titles, URL, every TLS field and a stow sender's account
// (authentication, user name, header name, credential); not its limits,
// timeouts or enabled state.
func TestRedirectsSender(t *testing.T) {
	for _, f := range []SenderField{FieldDIMSEHost, FieldDIMSEPeerPort, FieldDIMSECalledAETitle, FieldDIMSECallingAETitle,
		FieldDIMSESenderTLS, FieldDIMSETLSCAFile, FieldDIMSETLSCertFile, FieldDIMSETLSKeyFile, FieldDIMSETLSServerName,
		FieldSTOWURL, FieldSTOWTLS, FieldSTOWTLSCAFile, FieldSTOWTLSCertFile, FieldSTOWTLSKeyFile, FieldSTOWTLSServerName,
		FieldSTOWAuthentication, FieldSTOWUsername, FieldSTOWHeaderName, FieldSTOWCredential} {
		if !RedirectsSender(f) {
			t.Errorf("%s does not count as moving where the sender sends", f)
		}
	}
	for _, f := range []SenderField{FieldSenderEnabled, FieldSenderAllowIdentified, FieldSenderAssociations, FieldSenderByteRate,
		FieldSenderConnectTimeout, FieldSenderResponseTimeout, FieldSTOWMaxInstances, FieldSTOWMaxRequestBytes} {
		if RedirectsSender(f) {
			t.Errorf("%s counts as moving where the sender sends", f)
		}
	}
}

// TestApplySenderMark guards the mark as a field of a partial change: named,
// it is set and cleared; not named, it is kept.
func TestApplySenderMark(t *testing.T) {
	stored := Sender{Name: "pacs", Type: SenderDIMSE, Enabled: true, DIMSE: DIMSESender{Host: "pacs.lab.example", Port: 104,
		CalledAETitle: "PACS", CallingAETitle: "NOPACS"}}.Normalize()
	out, destination, _, err := ApplySender(stored, Sender{AllowIdentified: true}, []SenderField{FieldSenderAllowIdentified})
	if err != nil || !out.AllowIdentified || destination {
		t.Fatalf("set: %+v %v %v", out.AllowIdentified, destination, err)
	}
	kept, _, _, err := ApplySender(out, Sender{}, []SenderField{FieldSenderEnabled})
	if err != nil || !kept.AllowIdentified {
		t.Fatalf("a change that does not name it: %v %v", kept.AllowIdentified, err)
	}
	cleared, _, _, err := ApplySender(out, Sender{}, []SenderField{FieldSenderAllowIdentified})
	if err != nil || cleared.AllowIdentified {
		t.Fatalf("cleared: %v %v", cleared.AllowIdentified, err)
	}
}
