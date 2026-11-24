package tenant

import (
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
)

// sha returns a checksum whose every byte is b.
func sha(b byte) SHA256 {
	var s SHA256
	for i := range s {
		s[i] = b
	}
	return s
}

// TestNormalizeEndpoint guards the one spelling of an S3 endpoint: every
// way of writing one endpoint gives one text, and every form that could
// stand for an endpoint two ways, or carry something that is not one (a
// number taken for a name, a zone, an IPv4-mapped address, an encoded byte),
// is refused.
func TestNormalizeEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://s3.example.org":        "https://s3.example.org",
		"HTTPS://S3.Example.ORG/":       "https://s3.example.org",
		"https://s3.example.org.":       "https://s3.example.org",
		"https://s3.example.org:443":    "https://s3.example.org",
		"https://s3.example.org:0443":   "https://s3.example.org",
		"http://minio.lab:80":           "http://minio.lab",
		"http://minio.lab:00080":        "http://minio.lab",
		"http://minio.lab:9000":         "http://minio.lab:9000",
		"http://minio.lab:09000":        "http://minio.lab:9000",
		"https://xn--bcher-kva.example": "https://xn--bcher-kva.example",
		"http://192.168.1.1":            "http://192.168.1.1",
		"http://192.0.2.7:9000/":        "http://192.0.2.7:9000",
		"https://[2001:DB8::1]:443/":    "https://[2001:db8::1]",
		"http://[2001:DB8:0::1]":        "http://[2001:db8::1]",
		"http://[2001:db8:0:0:0:0:0:1]": "http://[2001:db8::1]",
		"https://[2001:db8::1]:8443":    "https://[2001:db8::1]:8443",
	} {
		got, err := NormalizeEndpoint(in)
		if err != nil || got != want {
			t.Errorf("NormalizeEndpoint(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "s3.example.org", "ftp://s3.example.org", "https://", "https://u:p@s3.example.org",
		"https://s3.example.org/b", "https://s3.example.org/?", "https://s3.example.org?a=1", "https://s3.example.org#f",
		"https://s3.example.org:0", "https://s3.example.org:65536", "https://s3 .example.org", "https://s3.example.org:443:443",
		"https://s3.example.org:", "https://s3%2Eexample.org", "https://b%C3%BCcher.example", "https://s3.example.org/%2F",
		"https://s3.example.org//", "http://3232235777", "http://0xc0.0xa8.1.1", "http://192.168.001.001",
		"http://[::ffff:192.168.1.1]", "http://[fe80::1%25en0]", "http://[192.168.1.1]", "http://-bad.example",
		"http://bad-.example", "http://a..b", "http://" + strings.Repeat("a", 64) + ".example",
		"http://" + strings.Repeat("a.", 127) + "com", "https://" + strings.Repeat("a", MaxEndpointLength)} {
		if got, err := NormalizeEndpoint(in); fault.KindOf(err) != fault.Invalid {
			t.Errorf("NormalizeEndpoint(%q) = %q, %v; want Invalid", in, got, err)
		}
	}
}

// TestStoragesOverlap guards the overlap rule: directories under one root
// whose paths are equal or nested by whole segments, case folded; S3
// storages on one endpoint and bucket whose prefixes nest; and nothing
// across roots, buckets, endpoints or types.
func TestStoragesOverlap(t *testing.T) {
	dir := func(root, path string) Storage {
		return Storage{Type: StorageDirectory, Directory: DirectoryStorage{Root: root, Path: path}}
	}
	s3 := func(ep, bucket, prefix string) Storage {
		return Storage{Type: StorageS3, S3: S3Storage{Endpoint: ep, Bucket: bucket, Prefix: prefix}}
	}
	for _, c := range []struct {
		a, b Storage
		want bool
	}{
		{dir("fast", "a"), dir("fast", "a"), true},
		{dir("fast", "a"), dir("fast", "a/b"), true},
		{dir("fast", "A/B"), dir("fast", "a"), true},
		{dir("fast", ""), dir("fast", "x"), true},
		{dir("fast", "a"), dir("fast", "ab"), false},
		{dir("fast", "a"), dir("slow", "a"), false},
		{s3("https://e", "b", ""), s3("https://e", "b", "p/"), true},
		{s3("https://e", "b", "p/"), s3("https://e", "b", "p/q/"), true},
		{s3("https://e", "b", "p/"), s3("https://e", "b", "q/"), false},
		{s3("https://e", "b", ""), s3("https://e", "c", ""), false},
		{s3("https://e", "b", ""), s3("https://f", "b", ""), false},
		{dir("fast", ""), s3("https://e", "b", ""), false},
	} {
		if got := StoragesOverlap(c.a, c.b); got != c.want {
			t.Errorf("StoragesOverlap(%+v, %+v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestReceiversClash guards which bind addresses contend for one port at
// the operating system, and that a different port never clashes.
func TestReceiversClash(t *testing.T) {
	r := func(addr string, port int) Receiver {
		return Receiver{Type: ReceiverDIMSE, DIMSE: DIMSEReceiver{Port: port, BindAddress: addr}}
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"", "192.0.2.1", true}, {"192.0.2.1", "192.0.2.1", true}, {"0.0.0.0", "127.0.0.1", true}, {"127.0.0.1", "0.0.0.0", true},
		{"0.0.0.0", "::", true}, {"::", "::1", true}, {"::", "192.0.2.1", true}, {"2001:db8::1", "::", true},
		{"192.0.2.1", "192.0.2.2", false}, {"0.0.0.0", "::1", false}, {"192.0.2.1", "2001:db8::1", false}, {"::1", "::2", false},
	} {
		if got := ReceiversClash(r(c.a, 104), r(c.b, 104)); got != c.want {
			t.Errorf("ReceiversClash(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if ReceiversClash(r("", 104), r("", 105)) {
		t.Error("receivers on two ports clash")
	}
	x := Receiver{Name: "x", Type: ReceiverDIMSE, Storage: "s", DIMSE: DIMSEReceiver{Port: 104, AETitle: "A", BindAddress: "::ffff:192.0.2.1"},
		QuietPeriod: DefaultQuietPeriod, MaxOpenTime: DefaultMaxOpenTime, MaxFileBytes: DefaultMaxFileBytes}
	if fault.KindOf(x.Validate()) != fault.Invalid {
		t.Error("an IPv4-mapped bind address was accepted")
	}
}

// TestDecideClaim guards every branch of the claim's rules, the one place
// they are stated: no row; another reception's claim, whatever its study
// and series; this reception's claim with the same checksum, another
// checksum, or another series; a held instance the same file (by another
// reception or by this one), a different file, or elsewhere.
func TestDecideClaim(t *testing.T) {
	c := InstanceClaim{SOPInstanceUID: "1.2.3", StudyUID: "1.1", SeriesUID: "1.1.1", SHA256: sha(1)}
	row := func(held bool, by string, s SHA256, series string) *InstanceRow {
		return &InstanceRow{StudyUID: "1.1", SeriesUID: series, SHA256: s, By: by, Held: held}
	}
	other := c
	other.SHA256 = sha(2)
	for _, x := range []struct {
		what   string
		c      InstanceClaim
		row    *InstanceRow
		kind   DuplicateKind
		action ClaimAction
		by     string
	}{
		{"no row", c, nil, ClaimedNow, ClaimInsert, ""},
		{"held by another, same", c, row(true, "r0", sha(1), "1.1.1"), SameFile, ClaimNothing, "r0"},
		{"held by me, same", c, row(true, "me", sha(1), "1.1.1"), ArchivedBefore, ClaimNothing, "me"},
		{"held by me, different", other, row(true, "me", sha(1), "1.1.1"), DifferentFile, ClaimNothing, "me"},
		{"held, different", other, row(true, "r0", sha(1), "1.1.1"), DifferentFile, ClaimNothing, "r0"},
		{"held, another series", c, row(true, "r0", sha(1), "1.1.2"), UIDElsewhere, ClaimNothing, "r0"},
		{"claimed by me, same", c, row(false, "me", sha(1), "1.1.1"), ClaimedBefore, ClaimNothing, "me"},
		{"claimed by me, different", other, row(false, "me", sha(1), "1.1.1"), DifferentFile, ClaimNothing, "me"},
		{"claimed by me, another series", c, row(false, "me", sha(1), "1.1.2"), UIDElsewhere, ClaimNothing, "me"},
		{"claimed by another", c, row(false, "r9", sha(1), "1.1.1"), Claimed, ClaimNothing, "r9"},
		{"claimed by another, another series", c, row(false, "r9", sha(1), "1.1.2"), Claimed, ClaimNothing, "r9"},
	} {
		d, a := DecideClaim("me", x.c, x.row)
		if d.Kind != x.kind || a != x.action || d.HeldBy != x.by {
			t.Errorf("%s: %v %v by %q, want %v %v by %q", x.what, d.Kind, a, d.HeldBy, x.kind, x.action, x.by)
		}
	}
}

// TestJudgeRow guards the read of the duplicate check: a held instance is
// compared, a claim not confirmed is Claimed whatever its study, and no row
// is not held.
func TestJudgeRow(t *testing.T) {
	q := DuplicateQuery{SOPInstanceUID: "1.2.3", StudyUID: "1.1", SeriesUID: "1.1.1", SHA256: sha(1)}
	for _, c := range []struct {
		what string
		row  *InstanceRow
		want DuplicateKind
	}{
		{"none", nil, NotHeld},
		{"held", &InstanceRow{StudyUID: "1.1", SeriesUID: "1.1.1", SHA256: sha(1), Held: true}, SameFile},
		{"held, another checksum", &InstanceRow{StudyUID: "1.1", SeriesUID: "1.1.1", SHA256: sha(2), Held: true}, DifferentFile},
		{"held elsewhere", &InstanceRow{StudyUID: "9.9", SeriesUID: "1.1.1", SHA256: sha(1), Held: true}, UIDElsewhere},
		{"claimed", &InstanceRow{StudyUID: "1.1", SeriesUID: "1.1.1", SHA256: sha(1), By: "r"}, Claimed},
		{"claimed elsewhere", &InstanceRow{StudyUID: "9.9", SeriesUID: "1.1.1", SHA256: sha(1), By: "r"}, Claimed},
	} {
		if got := JudgeRow(q, c.row); got.Kind != c.want {
			t.Errorf("%s: %v, want %v", c.what, got.Kind, c.want)
		}
	}
}

// TestQuarantineDetailsClosed guards that a record's details are each from
// a closed list or of a form no file's value can be written into: a name
// in the shape of a word, or of a keyword, is refused; so is an element's
// path that is not tags and item numbers, a rule's name that is not one,
// and a kind of value or a cause not in the list.
func TestQuarantineDetailsClosed(t *testing.T) {
	base := QuarantinedFile{ID: "q1", Receiver: "a", Size: 1, Quarantined: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, c := range []struct {
		reason  QuarantineReason
		details map[string]string
		ok      bool
	}{
		{QuarantineBadUID, map[string]string{"attribute": "StudyInstanceUID", "fault": FaultMissing}, true},
		{QuarantineBadUID, map[string]string{"attribute": "DoeJohn", "fault": "john_doe"}, false},
		{QuarantineBadUID, map[string]string{"attribute": "PatientName", "fault": FaultEmpty}, false},
		{QuarantineBadLabel, map[string]string{"label": "session", "attribute": "(0010,4000)", "fault": FaultNoMatch}, true},
		{QuarantineBadLabel, map[string]string{"label": "session", "attribute": "PatientID", "fault": FaultTooLong}, true},
		{QuarantineBadLabel, map[string]string{"label": "session", "attribute": "", "fault": FaultBadLabel}, true},
		{QuarantineBadLabel, map[string]string{"label": "session", "attribute": "john_doe", "fault": FaultEmpty}, false},
		{QuarantineBadLabel, map[string]string{"label": "patient", "fault": FaultEmpty}, false},
		{QuarantineLabelConflict, map[string]string{"label": "subject", "fault": FaultTwoSubjectsForOneStudy}, true},
		{QuarantineLabelConflict, map[string]string{"label": "subject", "fault": "doe"}, false},
		{QuarantineStorageRefused, map[string]string{"storage": "store", "fault": FaultUnreachable}, true},
		{QuarantineStorageRefused, map[string]string{"storage": "store", "fault": "connection reset by peer"}, false},
		{QuarantineNotDICOM, map[string]string{"offset": "128"}, true},
		{QuarantineNotDICOM, map[string]string{"offset": "DOE"}, false},
		{QuarantineDeidFailed, map[string]string{"cause": "no_subject", "path": "00081115[0]/00080023", "rule": "shift-dates"}, true},
		{QuarantineDeidFailed, map[string]string{"cause": "bad_value", "rule": "rule 3"}, true},
		{QuarantineDeidFailed, map[string]string{"cause": "check_unreadable"}, true},
		{QuarantineDeidFailed, map[string]string{"cause": "Doe^John"}, false},
		{QuarantineDeidFailed, map[string]string{"cause": "bad_value", "path": "00100010=DOE"}, false},
		{QuarantineDeidFailed, map[string]string{"cause": "bad_value", "path": "0010001g"}, false},
		{QuarantineDeidFailed, map[string]string{"cause": "bad_value", "path": "00081115[x]/00100010"}, false},
		{QuarantineDeidFailed, map[string]string{"cause": "bad_value", "rule": "Doe John"}, false},
		{QuarantineResidualValue, map[string]string{"path": "00100010", "kind": "name"}, true},
		{QuarantineResidualValue, map[string]string{"path": "preamble", "kind": "identifier"}, true},
		{QuarantineResidualValue, map[string]string{"path": "00100010", "kind": "DOE"}, false},
		{QuarantineResidualValue, map[string]string{"path": "00100010", "kind": "name", "value": "DOE"}, false},
		{QuarantineProfileRefused, map[string]string{"rule": "refuse-sr"}, true},
		{QuarantineProfileRefused, map[string]string{"rule": "-refuse"}, false},
	} {
		q := base
		q.Reason, q.Details = c.reason, c.details
		if err := q.Validate(); (err == nil) != c.ok {
			t.Errorf("%s %v: %v, want accepted %v", c.reason, c.details, err, c.ok)
		}
	}
}

// TestRetryDelay guards the delay between attempts: it starts at
// RetryBase, doubles, and stops growing at RetryMax.
func TestRetryDelay(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: RetryBase, 2: 2 * RetryBase, 3: 4 * RetryBase, 9: 256 * RetryBase, 10: RetryMax, 1000: RetryMax} {
		if got := RetryDelay(attempt); got != min(want, RetryMax) {
			t.Errorf("RetryDelay(%d) = %v, want %v", attempt, got, min(want, RetryMax))
		}
	}
}

// TestScanCommitKeys guards that a commit's keys are the layout's keys of
// its scan and its instances' UIDs, so the catalog never records a place a
// reader would not find.
func TestScanCommitKeys(t *testing.T) {
	p := label.Path{Subject: "sub-01", Session: "ses-01", Scan: "scan-01"}
	key, err := layout.InstanceKey(p, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	c := ScanCommit{ReceptionID: "r1", StudyUID: "1.1", SeriesUID: "1.1.1", SubjectLabel: p.Subject, SessionLabel: p.Session,
		ScanLabel: p.Scan, Storage: "store", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Instances: []ConfirmedInstance{{SOPInstanceUID: "1.2.3", Key: key, Size: 1, ArchivedSHA256: sha(1)}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("a commit with its own keys: %v", err)
	}
	for what, change := range map[string]func(*ScanCommit){
		"another scan's key":   func(c *ScanCommit) { c.Instances[0].Key = "data/sub-01/ses-01/scan-02/1.2.3.dcm" },
		"another UID's key":    func(c *ScanCommit) { c.Instances[0].Key = "data/sub-01/ses-01/scan-01/1.2.4.dcm" },
		"a manifest's key":     func(c *ScanCommit) { c.Instances[0].Key = "meta/sub-01/ses-01/scan-01/manifest.json" },
		"no archived checksum": func(c *ScanCommit) { c.Instances[0].ArchivedSHA256 = SHA256{} },
		"an instance twice":    func(c *ScanCommit) { c.Instances = append(c.Instances, c.Instances[0]) },
		"a bad label":          func(c *ScanCommit) { c.ScanLabel = "scan/01" },
	} {
		x := c
		x.Instances = append([]ConfirmedInstance(nil), c.Instances...)
		change(&x)
		if fault.KindOf(x.Validate()) != fault.Invalid {
			t.Errorf("a commit with %s was accepted", what)
		}
	}
}

// TestApplyStorage guards a partial change of a storage: only the fields
// named change, the endpoint is kept normalized, and a location field is
// reported as one.
func TestApplyStorage(t *testing.T) {
	stored := Storage{Name: "s", Type: StorageS3, S3: S3Storage{Endpoint: "https://e.org", Region: "r", Bucket: "bkt",
		AccessKeyID: "AK", SecretSet: true}}
	got, location, err := ApplyStorage(stored, Storage{S3: S3Storage{Endpoint: "HTTPS://E.ORG:443/", Region: "x"}},
		[]StorageField{FieldS3Endpoint})
	if err != nil || got.S3.Endpoint != "https://e.org" || got.S3.Region != "r" || !location {
		t.Errorf("ApplyStorage = %+v, %v, %v", got.S3, location, err)
	}
	if _, location, _ := ApplyStorage(stored, Storage{CapacityBytes: 5}, []StorageField{FieldCapacityBytes}); location {
		t.Error("a change of the capacity was reported as one of the location")
	}
}

// TestValidateIncoming guards a place in the incoming area: relative
// segments only, none empty or hidden, so it never leaves the area.
func TestValidateIncoming(t *testing.T) {
	for _, ok := range []string{"rc1/f.dcm", "a/b/c_d-e.f"} {
		if err := validateIncoming(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "a//b", "../x", "a/.hidden", "a\\b", "a/b c"} {
		if validateIncoming(bad) == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
