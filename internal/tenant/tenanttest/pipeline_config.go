package tenanttest

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testStorages guards adding, reading, listing and removing storages: each
// type read back exactly, the S3 secret never returned (only that one is
// set), names unique, the overlap rules a precondition, a removed storage
// gone, and each change written with its audit entry.
func testStorages(e *penv) {
	d := dirStorage("fast-a", "studies/a")
	d.CapacityBytes = 1 << 40
	addAudit := e.audit()
	got, err := e.p.AddStorage(e.ctx, d, at(1), addAudit, nil)
	e.ok("AddStorage directory", err)
	want := d
	want.Created, want.Updated = at(1), at(1)
	if !reflect.DeepEqual(got, want) {
		e.Errorf("AddStorage answered %+v, want %+v", got, want)
	}
	if !e.auditIDs()[addAudit.ID] {
		e.Error("AddStorage wrote no audit entry")
	}
	s3 := s3Storage("bucket-a", "nopacs/lab-a/")
	got, err = e.p.AddStorage(e.ctx, s3, at(2), e.audit(), nil)
	e.ok("AddStorage s3", err)
	if got.S3.SealedSecret != nil || !got.S3.SecretSet {
		e.Errorf("AddStorage answered the secret (%q) or not that one is set (%v)", got.S3.SealedSecret, got.S3.SecretSet)
	}
	read, err := e.p.Storage(e.ctx, "bucket-a")
	e.ok("Storage", err)
	if read.S3.SealedSecret != nil || !read.S3.SecretSet || read.S3.AccessKeyID != "AKIDEXAMPLE" || read.S3.Prefix != "nopacs/lab-a/" {
		e.Errorf("Storage read back %+v", read.S3)
	}
	for _, s := range allPages(e, 1, func(p master.Page) ([]tenant.Storage, string, error) { return e.p.ListStorages(e.ctx, p) }) {
		if s.S3.SealedSecret != nil {
			e.Errorf("ListStorages answered the secret of %s", s.Name)
		}
	}

	failed := e.audit()
	_, err = e.p.AddStorage(e.ctx, dirStorage("fast-a", "elsewhere"), at(3), failed, nil)
	e.wantKind("AddStorage, name taken", err, fault.Conflict)
	if e.auditIDs()[failed.ID] {
		e.Error("a refused AddStorage wrote its audit entry")
	}
	for _, c := range []struct {
		what string
		s    tenant.Storage
	}{
		{"inside another directory", dirStorage("fast-b", "studies/a/inner")},
		{"containing another directory", dirStorage("fast-c", "studies")},
		{"the same directory in other case", dirStorage("fast-d", "Studies/A")},
		{"the tenant's whole directory", dirStorage("fast-e", "")},
		{"a prefix inside another's", s3Storage("bucket-b", "nopacs/lab-a/inner/")},
		{"the whole bucket", s3Storage("bucket-c", "")},
	} {
		_, err := e.p.AddStorage(e.ctx, c.s, at(3), e.audit(), nil)
		e.wantPrecondition("AddStorage "+c.what, err)
	}
	for _, s := range []tenant.Storage{dirStorage("fast-f", "studies/ab"), s3Storage("bucket-d", "nopacs/lab-b/")} {
		_, err := e.p.AddStorage(e.ctx, s, at(3), e.audit(), nil)
		e.ok("AddStorage beside another, not inside it", err)
	}
	other := s3Storage("bucket-e", "")
	other.S3.Bucket = "other-bucket"
	_, err = e.p.AddStorage(e.ctx, other, at(3), e.audit(), nil)
	e.ok("AddStorage, the whole of another bucket", err)

	for _, bad := range []tenant.Storage{
		dirStorage("Fast", "x"), dirStorage("con", "x"), dirStorage("ok", "../up"), dirStorage("ok", ".hidden"),
		{Name: "untyped"}, func() tenant.Storage { s := s3Storage("nosecret", "p/"); s.S3.SealedSecret = nil; return s }(),
		func() tenant.Storage { s := s3Storage("badprefix", "noslash"); return s }(),
		func() tenant.Storage { s := s3Storage("badbucket", ""); s.S3.Bucket = "-x"; return s }(),
		func() tenant.Storage { s := dirStorage("both", "x"); s.S3.Bucket = "b"; return s }(),
		func() tenant.Storage { s := dirStorage("neg", "x"); s.CapacityBytes = -1; return s }(),
	} {
		_, err := e.p.AddStorage(e.ctx, bad, at(3), e.audit(), nil)
		e.wantKind(fmt.Sprintf("AddStorage %+v", bad), err, fault.Invalid)
	}

	removed, err := e.p.RemoveStorage(e.ctx, "fast-a", e.audit(), nil)
	e.ok("RemoveStorage", err)
	if removed.Name != "fast-a" || removed.Directory.Path != "studies/a" {
		e.Errorf("RemoveStorage answered %+v, want the storage as it was", removed)
	}
	_, err = e.p.Storage(e.ctx, "fast-a")
	e.wantKind("Storage after RemoveStorage", err, fault.NotFound)
	_, err = e.p.RemoveStorage(e.ctx, "fast-a", e.audit(), nil)
	e.wantKind("RemoveStorage again", err, fault.NotFound)
}

// testStorageSetPartial guards SetStorage as the contract's update mask: only
// the fields named change, the secret is kept unless named, the access key
// id and the secret change together, a named secret is not empty, the type
// cannot change, and an unknown or empty mask is Invalid.
func testStorageSetPartial(e *penv) {
	_, err := e.p.AddStorage(e.ctx, s3Storage("bucket", "p/"), at(1), e.audit(), nil)
	e.ok("AddStorage", err)
	change := tenant.Storage{Name: "bucket", CapacityBytes: 5000, S3: tenant.S3Storage{Region: "eu-central-1"}}
	got, err := e.p.SetStorage(e.ctx, change, []tenant.StorageField{tenant.FieldCapacityBytes}, at(2), e.audit(), nil)
	e.ok("SetStorage capacity", err)
	if got.CapacityBytes != 5000 || got.S3.Region != "us-east-1" || !got.S3.SecretSet || !got.Updated.Equal(at(2)) || !got.Created.Equal(at(1)) {
		e.Errorf("SetStorage of the capacity answered %+v", got)
	}
	for _, c := range []struct {
		what   string
		change tenant.Storage
		fields []tenant.StorageField
	}{
		{"no field", change, nil},
		{"a field it does not have", change, []tenant.StorageField{"s3"}},
		{"the other type's field", change, []tenant.StorageField{tenant.FieldDirectoryPath}},
		{"the key id alone", tenant.Storage{Name: "bucket", S3: tenant.S3Storage{AccessKeyID: "NEWKEY"}}, []tenant.StorageField{tenant.FieldS3AccessKeyID}},
		{"the secret alone", tenant.Storage{Name: "bucket", S3: tenant.S3Storage{SealedSecret: []byte("x")}}, []tenant.StorageField{tenant.FieldS3SecretKey}},
		{"an empty secret", tenant.Storage{Name: "bucket", S3: tenant.S3Storage{AccessKeyID: "NEWKEY"}}, []tenant.StorageField{tenant.FieldS3AccessKeyID, tenant.FieldS3SecretKey}},
		{"a region made invalid", tenant.Storage{Name: "bucket"}, []tenant.StorageField{tenant.FieldS3Region}},
	} {
		_, err := e.p.SetStorage(e.ctx, c.change, c.fields, at(3), e.audit(), nil)
		e.wantKind("SetStorage, "+c.what, err, fault.Invalid)
	}
	got, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "bucket", S3: tenant.S3Storage{AccessKeyID: "NEWKEY", SealedSecret: []byte("new")}},
		[]tenant.StorageField{tenant.FieldS3AccessKeyID, tenant.FieldS3SecretKey}, at(4), e.audit(), nil)
	e.ok("SetStorage credentials", err)
	if got.S3.AccessKeyID != "NEWKEY" || !got.S3.SecretSet || got.CapacityBytes != 5000 {
		e.Errorf("SetStorage of the credentials answered %+v", got)
	}
	_, err = e.p.SetStorage(e.ctx, change, []tenant.StorageField{tenant.FieldCapacityBytes}, at(5), e.audit(), nil)
	e.ok("SetStorage", err)
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "missing"}, []tenant.StorageField{tenant.FieldCapacityBytes}, at(5), e.audit(), nil)
	e.wantKind("SetStorage of a storage not there", err, fault.NotFound)
}

// testStorageInUse guards the rules that keep a storage's data where the
// catalog says: a storage a receiver names is not removed; one a reception
// in progress was identified to is not removed and its location does not
// change; once that reception ended with nothing archived it may go; one
// that holds an instance is never removed and its location never changes,
// though its capacity may. With several writers a removal and a commit or
// an identify that names the storage could each miss the other's rows; the
// storage's row is the lock they share. On SQLite's one writer they are
// ordered anyway, so this case passes either way here; a Postgres run
// exercises the lock.
func testStorageInUse(e *penv) {
	e.addReceiver("dimse-a", "store-a", 11112)
	_, err := e.p.RemoveStorage(e.ctx, "store-a", e.audit(), nil)
	e.wantPrecondition("RemoveStorage of a storage a receiver names", err)
	if err != nil && !strings.Contains(fault.Message(err), "dimse-a") {
		e.Errorf("the refusal %q does not say which receiver", fault.Message(err))
	}
	e.addStorage("store-b")
	e.addStorage("store-c")
	moved := tenant.Storage{Name: "store-b", Directory: tenant.DirectoryStorage{Path: "moved"}}
	r := e.stepTo(e.openReception("dimse-a", 1, at(10)), "identified", "store-b")
	_, err = e.p.RemoveStorage(e.ctx, "store-b", e.audit(), nil)
	e.wantPrecondition("RemoveStorage of a storage a reception in progress archives to", err)
	_, err = e.p.SetStorage(e.ctx, moved, []tenant.StorageField{tenant.FieldDirectoryPath}, at(20), e.audit(), nil)
	e.wantPrecondition("SetStorage of the path of a storage a reception in progress archives to", err)
	e.failReception(r)
	_, err = e.p.SetStorage(e.ctx, moved, []tenant.StorageField{tenant.FieldDirectoryPath}, at(20), e.audit(), nil)
	e.ok("SetStorage of the path of a storage whose reception ended with nothing archived", err)
	_, err = e.p.RemoveStorage(e.ctx, "store-b", e.audit(), nil)
	e.ok("RemoveStorage of a storage whose reception ended with nothing archived", err)

	r = e.openReception("dimse-a", 2, at(30))
	f := file(r, 2, 1)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(31))
	e.ok("AddReceivedFiles", err)
	r = e.stepTo(r, "identified", "store-c")
	e.commit(r, f)
	e.stepTo(r, "done", "store-c")
	_, err = e.p.RemoveStorage(e.ctx, "store-c", e.audit(), nil)
	e.wantPrecondition("RemoveStorage of a storage that holds an instance", err)
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "store-c", Directory: tenant.DirectoryStorage{Path: "moved-c"}},
		[]tenant.StorageField{tenant.FieldDirectoryPath}, at(40), e.audit(), nil)
	e.wantPrecondition("SetStorage of the path of a storage that holds an instance", err)
	_, err = e.p.SetStorage(e.ctx, tenant.Storage{Name: "store-c", CapacityBytes: 7},
		[]tenant.StorageField{tenant.FieldCapacityBytes}, at(40), e.audit(), nil)
	e.ok("SetStorage of the capacity of a storage that holds an instance", err)
}

// testIdentifyStorage guards that a reception is identified only to a
// storage the tenant has, checked in the step's transaction, so the rules
// of a storage in use cannot be passed by naming one that is not there.
func testIdentifyStorage(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.stepTo(e.openReception("a", 1, at(1)), "complete", "store")
	_, err := e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: "identify", At: at(100),
		SubjectLabel: "sub-01", SessionLabel: "ses-01", Storage: "nosuch"}, nil)
	e.wantPrecondition("identify to a storage the tenant does not have", err)
	if got, _, _ := e.p.Reception(e.ctx, r.ID); got.State != "complete" {
		e.Errorf("a refused identify moved the reception to %s", got.State)
	}
}

// testBindAddressClash guards the receivers that would contend for one port
// at the operating system: 0.0.0.0 with any IPv4 address, :: with any
// address, 0.0.0.0 with ::, and an empty address with any; and the ones
// that do not: two IPv4 addresses, an IPv4 and a specific IPv6 address. An
// IPv4-mapped IPv6 address is refused, so each address has one spelling.
func testBindAddressClash(e *penv) {
	e.addStorage("store")
	add := func(name, addr string, port int) error {
		r := receiverOf(name, "store", port)
		r.DIMSE.BindAddress = addr
		_, err := e.p.AddReceiver(e.ctx, r, T0, e.audit(), nil)
		return err
	}
	for i, c := range []struct{ first, second string }{
		{"0.0.0.0", "127.0.0.1"}, {"127.0.0.1", "0.0.0.0"}, {"0.0.0.0", "::"}, {"::", "0.0.0.0"},
		{"::", "::1"}, {"::", "192.0.2.1"}, {"", "::1"}, {"192.0.2.1", ""}, {"2001:db8::1", "2001:db8::1"},
	} {
		port := 12000 + i
		e.ok("AddReceiver "+c.first, add(fmt.Sprintf("first%d", i), c.first, port))
		e.wantPrecondition(fmt.Sprintf("AddReceiver %q beside %q on one port", c.second, c.first), add(fmt.Sprintf("second%d", i), c.second, port))
	}
	for i, c := range []struct{ first, second string }{
		{"127.0.0.1", "192.0.2.1"}, {"0.0.0.0", "::1"}, {"192.0.2.1", "2001:db8::1"},
	} {
		port := 13000 + i
		e.ok("AddReceiver "+c.first, add(fmt.Sprintf("apart%d", i), c.first, port))
		e.ok(fmt.Sprintf("AddReceiver %q beside %q on one port", c.second, c.first), add(fmt.Sprintf("beside%d", i), c.second, port))
	}
	e.wantKind("AddReceiver on an IPv4-mapped IPv6 address", add("mapped", "::ffff:192.0.2.9", 14000), fault.Invalid)
}

// testEndpointSpelling guards that an S3 endpoint is kept in one spelling
// (scheme and host in lowercase, no default port, no final slash), so two
// storages on one bucket cannot pass the overlap rule by spelling one
// endpoint two ways, and that an endpoint with a path, a query, user
// information or another scheme is refused.
func testEndpointSpelling(e *penv) {
	add := func(name, endpoint string) (tenant.Storage, error) {
		s := s3Storage(name, "")
		s.S3.Endpoint = endpoint
		return e.p.AddStorage(e.ctx, s, T0, e.audit(), nil)
	}
	got, err := add("s1", "HTTPS://S3.Example.org:443/")
	e.ok("AddStorage", err)
	if got.S3.Endpoint != "https://s3.example.org" {
		e.Errorf("an endpoint is kept as %q, want https://s3.example.org", got.S3.Endpoint)
	}
	for i, ep := range []string{"https://s3.example.org", "https://s3.example.org/", "https://S3.EXAMPLE.ORG", "https://s3.example.org:443"} {
		_, err := add(fmt.Sprintf("s%d", i+2), ep)
		e.wantPrecondition(fmt.Sprintf("AddStorage on %q beside https://s3.example.org on one bucket", ep), err)
	}
	if got, err := add("other-port", "https://s3.example.org:8443"); err != nil || got.S3.Endpoint != "https://s3.example.org:8443" {
		e.Errorf("an endpoint on another port = %q, %v", got.S3.Endpoint, err)
	}
	for _, ep := range []string{"https://s3.example.org/bucket", "https://s3.example.org?x=1", "https://user@s3.example.org",
		"ftp://s3.example.org", "https://s3.example.org#f", "s3.example.org", "https://s3.example.org:99999"} {
		_, err := add("bad", ep)
		e.wantKind("AddStorage on "+ep, err, fault.Invalid)
	}
}

// testReceivers guards receivers: one read back exactly with every field of
// its type, names unique, a missing storage and a port taken on an
// overlapping address are preconditions, a partial change sets only what it
// names (any caller and a callers list exclude each other), and a removed
// receiver is gone.
func testReceivers(e *penv) {
	e.addStorage("store")
	r := receiverOf("full", "store", 11112)
	r.Enabled = true
	r.DIMSE.BindAddress = "192.0.2.1"
	r.DIMSE.Callers = []tenant.CallerRule{{CallingAETitle: "SCANNER1"}, {AddressRange: "10.20.0.0/24"}, {CallingAETitle: "CT 2", AddressRange: "2001:db8::/48"}}
	r.DIMSE.TLS = &tenant.ReceiverTLS{CertificateFile: "cert.pem", PrivateKeyFile: "key.pem"}
	r.DIMSE.SOPClasses = []string{"1.2.840.10008.5.1.4.1.1.4", "1.2.840.10008.5.1.4.1.1.2"}
	r.DIMSE.TransferSyntaxes = []string{"1.2.840.10008.1.2.1"}
	r.Naming = &naming.Spec{Subject: "sub-{#PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{SeriesNumber}"}
	got, err := e.p.AddReceiver(e.ctx, r, at(1), e.audit(), nil)
	e.ok("AddReceiver", err)
	want := r
	want.Created, want.Updated = at(1), at(1)
	if !reflect.DeepEqual(got, want) {
		e.Errorf("AddReceiver answered\n%+v\nwant\n%+v", got, want)
	}
	read, err := e.p.Receiver(e.ctx, "full")
	e.ok("Receiver", err)
	if !reflect.DeepEqual(read, want) {
		e.Errorf("Receiver read back\n%+v\nwant\n%+v", read, want)
	}

	_, err = e.p.AddReceiver(e.ctx, receiverOf("full", "store", 11113), at(2), e.audit(), nil)
	e.wantKind("AddReceiver, name taken", err, fault.Conflict)
	_, err = e.p.AddReceiver(e.ctx, receiverOf("nostore", "missing", 11113), at(2), e.audit(), nil)
	e.wantPrecondition("AddReceiver on a storage the tenant does not have", err)
	same := receiverOf("same-addr", "store", 11112)
	same.DIMSE.BindAddress = "192.0.2.1"
	_, err = e.p.AddReceiver(e.ctx, same, at(2), e.audit(), nil)
	e.wantPrecondition("AddReceiver on a port and address taken", err)
	_, err = e.p.AddReceiver(e.ctx, receiverOf("every-addr", "store", 11112), at(2), e.audit(), nil)
	e.wantPrecondition("AddReceiver on a port taken, on every address", err)
	apart := receiverOf("other-addr", "store", 11112)
	apart.DIMSE.BindAddress = "192.0.2.2"
	_, err = e.p.AddReceiver(e.ctx, apart, at(2), e.audit(), nil)
	e.ok("AddReceiver on the same port and another address", err)

	for _, bad := range []func(*tenant.Receiver){
		func(x *tenant.Receiver) { x.QuietPeriod = 4e9 },
		func(x *tenant.Receiver) { x.QuietPeriod = 5500e6 },
		func(x *tenant.Receiver) { x.MaxOpenTime = x.QuietPeriod - 1 },
		func(x *tenant.Receiver) { x.MaxFileBytes = 1023 },
		func(x *tenant.Receiver) { x.DIMSE.Port = 0 },
		func(x *tenant.Receiver) { x.DIMSE.AETitle = " PADDED" },
		func(x *tenant.Receiver) { x.DIMSE.AETitle = "SEVENTEEN-LETTERS" },
		func(x *tenant.Receiver) { x.DIMSE.BindAddress = "2001:DB8::1" },
		func(x *tenant.Receiver) { x.DIMSE.Callers = []tenant.CallerRule{{}} },
		func(x *tenant.Receiver) { x.DIMSE.Callers = []tenant.CallerRule{{AddressRange: "10.20.0.1/24"}} },
		func(x *tenant.Receiver) {
			x.DIMSE.AnyCaller, x.DIMSE.Callers = true, []tenant.CallerRule{{CallingAETitle: "A"}}
		},
		func(x *tenant.Receiver) {
			x.DIMSE.TLS = &tenant.ReceiverTLS{CertificateFile: "../cert", PrivateKeyFile: "k"}
		},
		func(x *tenant.Receiver) { x.DIMSE.SOPClasses = []string{"1.2.03"} },
		func(x *tenant.Receiver) { x.Naming = &naming.Spec{Subject: "{Nope}", Session: "s", Scan: "x"} },
		func(x *tenant.Receiver) { x.Type = "dicomweb" },
	} {
		x := receiverOf("bad", "store", 11120)
		bad(&x)
		_, err := e.p.AddReceiver(e.ctx, x, at(2), e.audit(), nil)
		e.wantKind(fmt.Sprintf("AddReceiver %+v", x), err, fault.Invalid)
	}

	change := tenant.Receiver{Name: "full", DIMSE: tenant.DIMSEReceiver{AnyCaller: true, Port: 11114}}
	got, err = e.p.SetReceiver(e.ctx, change, []tenant.ReceiverField{tenant.FieldDIMSEAnyCaller, tenant.FieldDIMSEPort, tenant.FieldNaming, tenant.FieldDIMSETLS}, at(3), e.audit(), nil)
	e.ok("SetReceiver", err)
	if !got.DIMSE.AnyCaller || got.DIMSE.Callers != nil || got.DIMSE.Port != 11114 || got.Naming != nil || got.DIMSE.TLS != nil ||
		got.DIMSE.AETitle != "ARCHIVE" || !slices.Equal(got.DIMSE.SOPClasses, r.DIMSE.SOPClasses) || !got.Updated.Equal(at(3)) {
		e.Errorf("SetReceiver answered %+v", got)
	}
	got, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "full", DIMSE: tenant.DIMSEReceiver{Callers: []tenant.CallerRule{{CallingAETitle: "ONLY"}}}},
		[]tenant.ReceiverField{tenant.FieldDIMSECallers}, at(4), e.audit(), nil)
	e.ok("SetReceiver callers", err)
	if got.DIMSE.AnyCaller || len(got.DIMSE.Callers) != 1 {
		e.Errorf("naming the callers did not clear any caller: %+v", got.DIMSE)
	}
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "full", DIMSE: tenant.DIMSEReceiver{TLS: &tenant.ReceiverTLS{CertificateFile: "c.pem"}}},
		[]tenant.ReceiverField{tenant.FieldDIMSETLSCert}, at(4), e.audit(), nil)
	e.wantKind("SetReceiver of a certificate with no key", err, fault.Invalid)
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "full", DIMSE: tenant.DIMSEReceiver{Port: 11112}},
		[]tenant.ReceiverField{tenant.FieldDIMSEPort}, at(4), e.audit(), nil)
	e.ok("SetReceiver back to a port free on its address", err)
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "full"}, []tenant.ReceiverField{tenant.FieldDIMSEBindAddress}, at(4), e.audit(), nil)
	e.wantPrecondition("SetReceiver to every address on a port another receiver holds", err)
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "full"}, []tenant.ReceiverField{"dimse"}, at(4), e.audit(), nil)
	e.wantKind("SetReceiver naming a type's whole message", err, fault.Invalid)

	removed, err := e.p.RemoveReceiver(e.ctx, "full", e.audit(), nil)
	e.ok("RemoveReceiver", err)
	if removed.Name != "full" {
		e.Errorf("RemoveReceiver answered %+v", removed)
	}
	_, err = e.p.Receiver(e.ctx, "full")
	e.wantKind("Receiver after RemoveReceiver", err, fault.NotFound)
	_, err = e.p.RemoveReceiver(e.ctx, "full", e.audit(), nil)
	e.wantKind("RemoveReceiver again", err, fault.NotFound)
}

// testReceiverNameHeld guards the contract's rule that a removed receiver's
// name is not given to a new one while a reception or a quarantined file
// names it, so a name in a record always means one receiver.
func testReceiverNameHeld(e *penv) {
	e.addReceiver("by-reception", "store", 11112)
	e.openReception("by-reception", 1, at(1))
	e.addReceiver("by-quarantine", "store", 11113)
	_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{{ID: e.id("q"), Reason: tenant.QuarantineNotDICOM,
		Receiver: "by-quarantine", Quarantined: at(1)}})
	e.ok("HoldFiles", err)
	e.addReceiver("unused", "store", 11114)
	for _, n := range []string{"by-reception", "by-quarantine", "unused"} {
		_, err := e.p.RemoveReceiver(e.ctx, n, e.audit(), nil)
		e.ok("RemoveReceiver", err)
	}
	for _, n := range []string{"by-reception", "by-quarantine"} {
		_, err := e.p.AddReceiver(e.ctx, receiverOf(n, "store", 11115), at(2), e.audit(), nil)
		e.wantKind("AddReceiver with the name of a removed receiver records name", err, fault.Conflict)
	}
	_, err = e.p.AddReceiver(e.ctx, receiverOf("unused", "store", 11115), at(2), e.audit(), nil)
	e.ok("AddReceiver with the name of a removed receiver nothing names", err)
}

// testChangeGuards guards the Guard and the audit entry of every change a
// key asks for, configuration and decisions alike: a Guard that refuses
// undoes the change and its entry; one that allows lets the change be made
// once, with its entry, and runs after the change is written and before the
// commit. A Guard may be called more than once for one change (Guard), so
// what is asserted is the effect, once, not the number of calls.
func testChangeGuards(e *penv) {
	refusal := fault.New(fault.Denied, "a test's refusal")
	e.addStorage("kept")
	e.addReceiver("kept-r", "kept", 11112)
	e.addReceiver("decisions", "decide", 11113)
	retried := e.failReception(e.openReception("decisions", 1, at(1)))
	discarded := e.failReception(e.openReception("decisions", 2, at(1)))
	released, rejected := e.heldFile("decisions", 10, 1), e.heldFile("decisions", 20, 2)
	_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{released, rejected})
	e.ok("HoldFiles", err)
	type op struct {
		name string
		do   func(tenant.Guard, master.AuditEntry) error
		made func() bool
	}
	storageThere := func(n string) bool { _, err := e.p.Storage(e.ctx, n); return err == nil }
	receiverThere := func(n string) bool { _, err := e.p.Receiver(e.ctx, n); return err == nil }
	ops := []op{
		{"AddStorage", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.AddStorage(e.ctx, dirStorage("new", "new"), T0, a, g)
			return err
		}, func() bool { return storageThere("new") }},
		{"SetStorage", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.SetStorage(e.ctx, tenant.Storage{Name: "kept", CapacityBytes: 9}, []tenant.StorageField{tenant.FieldCapacityBytes}, T0, a, g)
			return err
		}, func() bool { s, _ := e.p.Storage(e.ctx, "kept"); return s.CapacityBytes == 9 }},
		{"AddReceiver", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.AddReceiver(e.ctx, receiverOf("new-r", "new", 11119), T0, a, g)
			return err
		}, func() bool { return receiverThere("new-r") }},
		{"SetReceiver", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "kept-r", Enabled: true}, []tenant.ReceiverField{tenant.FieldEnabled}, T0, a, g)
			return err
		}, func() bool { r, _ := e.p.Receiver(e.ctx, "kept-r"); return r.Enabled }},
		{"RemoveReceiver", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.RemoveReceiver(e.ctx, "kept-r", a, g)
			return err
		}, func() bool { return !receiverThere("kept-r") }},
		{"RemoveStorage", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.RemoveStorage(e.ctx, "kept", a, g)
			return err
		}, func() bool { return !storageThere("kept") }},
		{"RetryReception", func(g tenant.Guard, a master.AuditEntry) error {
			_, _, err := e.p.RetryReception(e.ctx, retried.ID, "retry-made", "", T0, a, g)
			return err
		}, func() bool { _, _, err := e.p.Reception(e.ctx, "retry-made"); return err == nil }},
		{"DiscardReception", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.DiscardReception(e.ctx, discarded.ID, "not wanted", T0, a, g)
			return err
		}, func() bool { r, _, _ := e.p.Reception(e.ctx, discarded.ID); return r.DiscardReason != "" }},
		{"ReleaseQuarantined", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.ReleaseQuarantined(e.ctx, released.ID, tenant.ReleaseAsIs, "", "release-made", T0, a, g)
			return err
		}, func() bool {
			q, _ := e.p.QuarantinedFile(e.ctx, released.ID)
			return q.State == tenant.QuarantineReleased
		}},
		{"RejectQuarantined", func(g tenant.Guard, a master.AuditEntry) error {
			_, err := e.p.RejectQuarantined(e.ctx, rejected.ID, "not wanted", T0, a, g)
			return err
		}, func() bool {
			q, _ := e.p.QuarantinedFile(e.ctx, rejected.ID)
			return q.State == tenant.QuarantineRejected
		}},
	}
	for _, o := range ops {
		var calls atomic.Int32
		a := e.audit()
		err := o.do(func(context.Context) error { calls.Add(1); return refusal }, a)
		e.wantKind(o.name+" with a Guard that refuses", err, fault.Denied)
		if o.made() || e.auditIDs()[a.ID] || calls.Load() < 1 {
			e.Errorf("%s with a Guard that refuses: made %v, entry written %v, Guard ran %d times", o.name, o.made(), e.auditIDs()[a.ID], calls.Load())
		}
		var seenInside bool
		a = e.audit()
		err = o.do(func(context.Context) error { seenInside = o.made(); return nil }, a)
		e.ok(o.name+" with a Guard that allows", err)
		if !o.made() || !e.auditIDs()[a.ID] || seenInside {
			e.Errorf("%s with a Guard that allows: made %v, entry written %v, visible while the Guard ran %v", o.name, o.made(), e.auditIDs()[a.ID], seenInside)
		}
	}
}

// testConfigPagingWithDeletes guards the storages and receivers lists
// against rows removed ahead of and behind the position reached between
// pages, with others added: each present throughout comes back exactly
// once, in order of name.
func testConfigPagingWithDeletes(e *penv) {
	var names []string
	for i := range 20 {
		n := fmt.Sprintf("s%02d", i)
		e.addStorage(n)
		names = append(names, n)
	}
	seen := map[string]int{}
	var order []string
	removed := map[string]bool{}
	page := master.Page{Limit: 3}
	for round := 0; ; round++ {
		items, next, err := e.p.ListStorages(e.ctx, page)
		e.ok("ListStorages", err)
		for _, s := range items {
			seen[s.Name]++
			order = append(order, s.Name)
		}
		if next == "" {
			break
		}
		page.Cursor = next
		for _, n := range []string{order[0], names[len(names)-1-round]} {
			if !removed[n] && (seen[n] > 0 || n > order[len(order)-1]) {
				_, err := e.p.RemoveStorage(e.ctx, n, e.audit(), nil)
				e.ok("RemoveStorage", err)
				removed[n] = true
			}
		}
		e.addStorage(fmt.Sprintf("t%02d", round))
	}
	for _, n := range names {
		if !removed[n] && seen[n] != 1 {
			e.Errorf("%s, present throughout, came back %d times", n, seen[n])
		}
	}
	if !slices.IsSorted(order) {
		e.Errorf("storages out of order: %q", order)
	}
	if len(removed) < 4 {
		e.Errorf("only %d storages were removed, so the test tested little", len(removed))
	}
	_, cursor, err := e.p.ListStorages(e.ctx, master.Page{Limit: 1})
	e.ok("ListStorages", err)
	_, _, err = e.p.ListReceivers(e.ctx, master.Page{Limit: 1, Cursor: cursor})
	e.wantKind("ListReceivers with a storages cursor", err, fault.Invalid)
}
