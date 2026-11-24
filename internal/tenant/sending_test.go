package tenant

import (
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// stow returns a stow sender as a store holds it: with a credential held.
func stow() Sender {
	return Sender{Name: "pacs", Type: SenderSTOW, STOW: STOWSender{URL: "https://pacs.example/dicom-web",
		Authentication: StowAuthBearer, CredentialSet: true}}.Normalize()
}

// TestSenderValidate guards the contract's rules of a sender's fields that
// the port holds: its name's length, its bounds, its host, a stow URL with
// neither user, query nor fragment, a credential exactly with an
// authentication that needs one, the header names refused, and TLS with a
// DIMSE sender's CA file.
func TestSenderValidate(t *testing.T) {
	d := Sender{Name: "orthanc", Type: SenderDIMSE, DIMSE: DIMSESender{Host: "pacs.example", Port: 104,
		CalledAETitle: "ORTHANC", CallingAETitle: "NOPACS"}}.Normalize()
	if err := d.Validate(); err != nil {
		t.Fatalf("a DIMSE sender with the defaults: %v", err)
	}
	if err := stow().Validate(); err != nil {
		t.Fatalf("a stow sender: %v", err)
	}
	bad := map[string]func(*Sender){
		"a name of 58":                  func(s *Sender) { s.Name = "s" + strings.Repeat("x", 57) },
		"17 associations":               func(s *Sender) { s.MaxAssociations = 17 },
		"a byte rate below 64 KiB":      func(s *Sender) { s.MaxBytesPerSecond = 1000 },
		"a timeout of a fraction":       func(s *Sender) { s.ConnectTimeout = 1500 * time.Millisecond },
		"a host with a zone":            func(s *Sender) { s.DIMSE.Host = "fe80::1%en0" },
		"a host with an underscore":     func(s *Sender) { s.DIMSE.Host = "pacs_1.example" },
		"TLS without a CA file":         func(s *Sender) { s.DIMSE.TLS = &SenderTLS{} },
		"a certificate without its key": func(s *Sender) { s.DIMSE.TLS = &SenderTLS{CAFile: "ca.pem", CertificateFile: "c.pem"} },
		"an AE title of 17":             func(s *Sender) { s.DIMSE.CalledAETitle = strings.Repeat("A", 17) },
	}
	for what, change := range bad {
		s := d
		change(&s)
		if fault.KindOf(s.Validate()) != fault.Invalid {
			t.Errorf("%s is not refused as invalid", what)
		}
	}
	badStow := map[string]func(*Sender){
		"a URL with a user":           func(s *Sender) { s.STOW.URL = "https://u:p@pacs.example/dicom-web" },
		"a URL with a query":          func(s *Sender) { s.STOW.URL = "https://pacs.example/dicom-web?x=1" },
		"a URL with a fragment":       func(s *Sender) { s.STOW.URL = "https://pacs.example/dicom-web#x" },
		"a URL port past 65535":       func(s *Sender) { s.STOW.URL = "https://pacs.example:99999999999999999999/dicom-web" },
		"a URL port of 0":             func(s *Sender) { s.STOW.URL = "https://pacs.example:0/dicom-web" },
		"an ftp URL":                  func(s *Sender) { s.STOW.URL = "ftp://pacs.example/" },
		"TLS with an http URL":        func(s *Sender) { s.STOW.URL = "http://pacs.example/"; s.STOW.TLS = &SenderTLS{} },
		"bearer with no credential":   func(s *Sender) { s.STOW.CredentialSet = false },
		"none with a credential":      func(s *Sender) { s.STOW.Authentication = StowAuthNone },
		"basic without a user":        func(s *Sender) { s.STOW.Authentication = StowAuthBasic },
		"a user with bearer":          func(s *Sender) { s.STOW.Username = "u" },
		"header Authorization":        func(s *Sender) { s.STOW.Authentication, s.STOW.HeaderName = StowAuthHeader, "authorization" },
		"a header starting Proxy-":    func(s *Sender) { s.STOW.Authentication, s.STOW.HeaderName = StowAuthHeader, "Proxy-Token" },
		"a request of 1001":           func(s *Sender) { s.STOW.MaxInstancesPerRequest = 1001 },
		"a sealed credential too big": func(s *Sender) { s.STOW.SealedCredential = make([]byte, MaxSealedCredentialLength+1) },
	}
	for what, change := range badStow {
		s := stow()
		change(&s)
		if fault.KindOf(s.Validate()) != fault.Invalid {
			t.Errorf("a stow sender with %s is not refused as invalid", what)
		}
	}
}

// TestApplySender guards the mask's rules: a field named is set to its
// value, one not named kept; the URL or the authentication only with the
// credential, unless it becomes none, which clears the held one; a field of
// the other type refused; a change of where it connects reported.
func TestApplySender(t *testing.T) {
	s := stow()
	out, dest, cleared, err := ApplySender(s, Sender{Enabled: true}, []SenderField{FieldSenderEnabled})
	if err != nil || !out.Enabled || dest || cleared || out.STOW.URL != s.STOW.URL {
		t.Errorf("enabled: %+v, %v, %v, %v", out, dest, cleared, err)
	}
	_, _, _, err = ApplySender(s, Sender{STOW: STOWSender{URL: "https://elsewhere.example/"}}, []SenderField{FieldSTOWURL})
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("a URL without the credential: %v", err)
	}
	out, dest, _, err = ApplySender(s, Sender{STOW: STOWSender{URL: "https://elsewhere.example/", SealedCredential: []byte("x")}},
		[]SenderField{FieldSTOWURL, FieldSTOWCredential})
	if err != nil || !dest || string(out.STOW.SealedCredential) != "x" {
		t.Errorf("a URL with its credential: %+v, %v, %v", out.STOW, dest, err)
	}
	_, _, _, err = ApplySender(s, Sender{}, []SenderField{FieldSTOWCredential})
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("an empty credential named: %v", err)
	}
	out, _, cleared, err = ApplySender(s, Sender{STOW: STOWSender{Authentication: StowAuthNone}}, []SenderField{FieldSTOWAuthentication})
	if err != nil || !cleared || out.STOW.CredentialSet {
		t.Errorf("authentication none: %+v, cleared %v, %v", out.STOW, cleared, err)
	}
	if _, _, _, err := ApplySender(s, Sender{}, []SenderField{FieldDIMSEHost}); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a field of the other type: %v", err)
	}
	if _, _, _, err := ApplySender(s, Sender{}, []SenderField{FieldSTOWMaxInstances}); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a bound named with no value: %v", err)
	}
	if _, _, _, err := ApplySender(s, Sender{}, nil); fault.KindOf(err) != fault.Invalid {
		t.Errorf("no field: %v", err)
	}
}

// TestJobProgressValidate guards what a progress write may say: the cursor
// moved forward past exactly what it counts, each refused and warned
// instance listed once between its cursors.
func TestJobProgressValidate(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	fail := func(uid string, code JobFailureCode) JobFailure {
		return JobFailure{SOPInstanceUID: uid, Code: code, Time: now}
	}
	ok := JobProgress{Fence: WorkFence{ID: "u1", Attempt: 1}, From: "2.25.1", To: "2.25.4", Sent: 1, Refused: 1, Warned: 1,
		Failures:     []JobFailure{fail("2.25.2", FailurePeerRefused), fail("2.25.3", FailurePeerWarning)},
		FailuresKept: DefaultFailuresKept, Now: now}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a progress write: %v", err)
	}
	bad := map[string]func(*JobProgress){
		"no move with counts":       func(p *JobProgress) { p.To = p.From },
		"a move back":               func(p *JobProgress) { p.To = "2.25.0" },
		"a refusal not listed":      func(p *JobProgress) { p.Refused = 2 },
		"a failure past the cursor": func(p *JobProgress) { p.Failures[0].SOPInstanceUID = "2.25.5" },
		"a failure twice":           func(p *JobProgress) { p.Failures[1] = fail("2.25.2", FailurePeerWarning) },
		"a bound of 10 kept":        func(p *JobProgress) { p.FailuresKept = 10 },
		"negative bytes":            func(p *JobProgress) { p.SentBytes = -1 },
		"an owed past the cursor":   func(p *JobProgress) { p.Owed = []string{"2.25.5"} },
		"an owed refused":           func(p *JobProgress) { p.Owed = []string{"2.25.2"} },
		"a settled after the cursor": func(p *JobProgress) {
			p.Settled, p.Sent = []string{"2.25.3"}, 2
		},
		"a settled not counted": func(p *JobProgress) { p.Settled = []string{"2.25.0", "2.25.05", "2.25.06", "2.25.07"} },
		"an instance owed and settled": func(p *JobProgress) {
			p.Owed, p.Settled, p.Sent = []string{"2.25.35"}, []string{"2.25.35"}, 2
		},
		"a settle-only write with counts past it": func(p *JobProgress) {
			p.To, p.Failures, p.Refused, p.Warned, p.Settled = p.From, nil, 0, 0, []string{"2.25.0"}
			p.Sent = 2
		},
	}
	good := map[string]func(*JobProgress){
		"an instance owed":                         func(p *JobProgress) { p.Owed = []string{"2.25.35"} },
		"a refusal for a meta that cannot be read": func(p *JobProgress) { p.Failures[0].Code = FailureMetaUnreadable },
		"a move past only owed instances": func(p *JobProgress) {
			p.Sent, p.Refused, p.Warned, p.Failures, p.Owed = 0, 0, 0, nil, []string{"2.25.2"}
		},
		"an owed instance settled refused": func(p *JobProgress) {
			p.Settled, p.Refused = []string{"2.25.0"}, 2
			p.Failures = append(p.Failures, fail("2.25.0", FailurePeerUnaccounted))
		},
		"a settle alone": func(p *JobProgress) {
			p.To, p.Failures, p.Refused, p.Warned, p.Sent, p.Settled = p.From, nil, 0, 0, 1, []string{"2.25.0"}
		},
	}
	for what, change := range good {
		p := ok
		p.Failures = append([]JobFailure(nil), ok.Failures...)
		change(&p)
		if err := p.Validate(); err != nil {
			t.Errorf("a progress write with %s is refused: %v", what, err)
		}
	}
	for what, change := range bad {
		p := ok
		p.Failures = append([]JobFailure(nil), ok.Failures...)
		change(&p)
		if fault.KindOf(p.Validate()) != fault.Invalid {
			t.Errorf("a progress write with %s is not refused", what)
		}
	}
}

// TestRetryRules guards the rules a retry is listed by: a job or scan
// with nothing refused or remaining has nothing to retry, a job not fully
// listed always has; a retried scan starts after its cursor, or at its own
// start when its refused were not all listed.
func TestRetryRules(t *testing.T) {
	if !(JobCounts{Instances: 2, InstancesSent: 1, InstancesWarned: 1, Listed: true}).NothingToRetry() {
		t.Error("a job that sent everything has something to retry")
	}
	for _, c := range []JobCounts{{Instances: 2, InstancesSent: 2}, {Instances: 2, InstancesSent: 1, Listed: true},
		{Instances: 2, InstancesSent: 1, InstancesRefused: 1, Listed: true}} {
		if c.NothingToRetry() {
			t.Errorf("%+v has nothing to retry", c)
		}
	}
	if got := RetryStart("2.25.1", "2.25.5", 0); got != "2.25.5" {
		t.Errorf("RetryStart after a cursor = %q", got)
	}
	if got := RetryStart("2.25.1", "", 0); got != "2.25.1" {
		t.Errorf("RetryStart of a scan never started = %q", got)
	}
	if got := RetryStart("2.25.1", "2.25.5", 3); got != "2.25.1" {
		t.Errorf("RetryStart of a scan whose refused were not all listed = %q", got)
	}
}

// TestProgressExcess guards the growth of a scan's totals: none while a
// write fits the count, the instances and bytes past it otherwise.
func TestProgressExcess(t *testing.T) {
	c := JobCounts{Instances: 3, Bytes: 300, InstancesSent: 2, BytesSent: 200}
	if n, b := ProgressExcess(c, JobProgress{Sent: 1, SentBytes: 100}); n != 0 || b != 0 {
		t.Errorf("a write that fits grows the count by %d, %d", n, b)
	}
	if n, b := ProgressExcess(c, JobProgress{Sent: 2, SentBytes: 150, Refused: 1, RefusedBytes: 90}); n != 2 || b != 140 {
		t.Errorf("a write past the count grows it by %d, %d; want 2, 140", n, b)
	}
}

// TestTakesProgress guards which jobs a unit's write is recorded into:
// running ones and canceled ones, never one that ended otherwise.
func TestTakesProgress(t *testing.T) {
	for _, s := range JobStates() {
		want := s == JobQueued || s == JobRunning || s == JobCanceled
		if s.TakesProgress() != want {
			t.Errorf("%s takes progress: %v, want %v", s, s.TakesProgress(), want)
		}
	}
}

// TestSenderDestination guards what an audit entry records of where a
// sender sends: a DIMSE peer's host and port, a stow URL's host and port
// when it names one, never more of the URL, and always within an audit
// detail's bound.
func TestSenderDestination(t *testing.T) {
	for _, c := range []struct {
		s    Sender
		want string
	}{
		{Sender{Type: SenderDIMSE, DIMSE: DIMSESender{Host: "pacs.example", Port: 104}}, "pacs.example:104"},
		{Sender{Type: SenderDIMSE, DIMSE: DIMSESender{Host: "2001:db8::1", Port: 104}}, "[2001:db8::1]:104"},
		{Sender{Type: SenderSTOW, STOW: STOWSender{URL: "https://pacs2.example/dicom-web"}}, "pacs2.example"},
		{Sender{Type: SenderSTOW, STOW: STOWSender{URL: "https://pacs2.example:8443/dicom-web"}}, "pacs2.example:8443"},
		{Sender{Type: SenderSTOW, STOW: STOWSender{URL: "https://pacs2.example:99999999999999999999/x"}}, ""},
	} {
		if got := c.s.Destination(); got != c.want {
			t.Errorf("Destination = %q, want %q", got, c.want)
		}
	}
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	if got := (Sender{Type: SenderDIMSE, DIMSE: DIMSESender{Host: long, Port: 65535}}).Destination(); got != long {
		t.Errorf("the destination of a host of 253 bytes is %d bytes, want the host alone", len(got))
	}
}

// TestEndState guards the state a job ends in: failed with instances never
// sent, partial with refusals and the rest sent, done otherwise.
func TestEndState(t *testing.T) {
	for _, c := range []struct {
		counts JobCounts
		want   JobState
	}{
		{JobCounts{Instances: 3, InstancesSent: 2, InstancesWarned: 1}, JobDone},
		{JobCounts{Instances: 3, InstancesSent: 2, InstancesRefused: 1}, JobPartial},
		{JobCounts{Instances: 3, InstancesSent: 1, InstancesRefused: 1}, JobFailed},
		{JobCounts{}, JobDone},
	} {
		if got, _ := EndState(c.counts); got != c.want {
			t.Errorf("EndState(%+v) = %s, want %s", c.counts, got, c.want)
		}
	}
}

// TestForwardJobID guards a forward's id: the same for one reception and
// sender, another for another, and an id a store keeps.
func TestForwardJobID(t *testing.T) {
	a, b := ForwardJobID("rc1", "orthanc"), ForwardJobID("rc1", "pacs")
	if a != ForwardJobID("rc1", "orthanc") || a == b || master.ValidateID("job id", a) != nil {
		t.Errorf("forward ids %q and %q", a, b)
	}
	if u := JobUnitID(strings.Repeat("j", master.MaxIDLength), "123456789"); master.ValidateID("work id", u) != nil {
		t.Errorf("a unit's id %q is not an id a store keeps", u)
	}
	if len(SendKind(strings.Repeat("s", MaxSenderNameLength))) > 63 {
		t.Error("the kind of the longest sender's work is longer than a kind")
	}
}

// TestJobEventsRoundTrip guards the job and sender events: each is written
// and read back as it was, and one whose counts do not add up is refused.
func TestJobEventsRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	j := Job{ID: "j1", Sender: "orthanc", State: JobRunning, Counts: JobCounts{Instances: 4, InstancesSent: 1, Bytes: 40, BytesSent: 10}}
	for _, mk := range []func() (EventRecord, error){
		func() (EventRecord, error) { return JobStepEvent(JobQueued, j, at) },
		func() (EventRecord, error) { return JobProgressEvent(j, at) },
		func() (EventRecord, error) {
			return NewEvent(&SenderHealthChanged{Sender: "orthanc", State: SenderDown, DownReason: SenderDownPeerFailing,
				Fault: SenderFaultUnreachable, Since: at, FailedConnections: 3, NextTry: at.Add(time.Minute)}, at)
		},
	} {
		e, err := mk()
		if err != nil {
			t.Fatal(err)
		}
		p, ok := DecodeEvent(e.Type(), e.Payload())
		if !ok || p == nil || p.EventType() != e.Type() {
			t.Errorf("%s does not read back: %v %v", e.Type(), p, ok)
		}
	}
	j.Counts.InstancesSent = 5
	if _, err := JobProgressEvent(j, at); fault.KindOf(err) != fault.Invalid {
		t.Errorf("counts that do not add up: %v", err)
	}
	if !ServerEventType(EventSenderHealth) || ServerEventType(EventJobStep) || ServerEventType(EventJobProgress) {
		t.Error("a sender's health is the server's to append; a job's events are written with their change")
	}
}
