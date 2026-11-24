package tenanttest

import (
	"reflect"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// stowReceiverOf returns a stow receiver with the contract's defaults that
// takes requests from the keys named.
func stowReceiverOf(name, storage string, keys ...string) tenant.Receiver {
	return tenant.Receiver{
		Name: name, Type: tenant.ReceiverSTOW, Storage: storage,
		STOW:        tenant.STOWReceiver{Keys: keys, MaxRequestBytes: tenant.DefaultMaxRequestBytes},
		QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes,
	}
}

// testSTOWReceivers guards the stow receiver as a store keeps it: added and
// read back field for field beside a DIMSE receiver, on no port; a key
// listed by two stow receivers of the tenant refused, so a request is never
// taken by a choice between two; a change of its keys and largest request
// kept; a field of the other type refused in a change; and a stow receiver
// with a DIMSE field refused as invalid.
func testSTOWReceivers(e *penv) {
	e.addStorage("store")
	e.addReceiver("dimse", "store", 11112)
	r := stowReceiverOf("web", "store", "key-a", "key-b")
	r.Enabled = true
	r.MaxFileBytes = 1 << 20
	got, err := e.p.AddReceiver(e.ctx, r, at(1), e.audit(), nil)
	e.ok("AddReceiver stow", err)
	want := r
	want.Created, want.Updated = at(1), at(1)
	if !reflect.DeepEqual(got, want) {
		e.Errorf("AddReceiver answered\n%+v\nwant\n%+v", got, want)
	}
	read, err := e.p.Receiver(e.ctx, "web")
	e.ok("Receiver", err)
	if !reflect.DeepEqual(read, want) {
		e.Errorf("Receiver read back\n%+v\nwant\n%+v", read, want)
	}

	_, err = e.p.AddReceiver(e.ctx, stowReceiverOf("web2", "store", "key-c", "key-b"), at(2), e.audit(), nil)
	e.wantPrecondition("AddReceiver stow listing a key another stow receiver lists", err)
	_, err = e.p.AddReceiver(e.ctx, stowReceiverOf("web3", "store", "key-c"), at(2), e.audit(), nil)
	e.ok("AddReceiver stow with keys of its own", err)
	_, err = e.p.AddReceiver(e.ctx, stowReceiverOf("closed", "store"), at(2), e.audit(), nil)
	e.ok("AddReceiver stow listing no key", err)

	change := tenant.Receiver{Name: "web", STOW: tenant.STOWReceiver{Keys: []string{"key-c"}}}
	_, err = e.p.SetReceiver(e.ctx, change, []tenant.ReceiverField{tenant.FieldSTOWKeys}, at(3), e.audit(), nil)
	e.wantPrecondition("SetReceiver stow to a key another stow receiver lists", err)
	change = tenant.Receiver{Name: "web", STOW: tenant.STOWReceiver{Keys: []string{"key-d"}, MaxRequestBytes: 1 << 30}}
	set, err := e.p.SetReceiver(e.ctx, change, []tenant.ReceiverField{tenant.FieldSTOWKeys, tenant.FieldSTOWMaxRequest}, at(3), e.audit(), nil)
	e.ok("SetReceiver stow keys and largest request", err)
	if !reflect.DeepEqual(set.STOW, tenant.STOWReceiver{Keys: []string{"key-d"}, MaxRequestBytes: 1 << 30}) || set.Type != tenant.ReceiverSTOW {
		e.Errorf("SetReceiver kept %+v", set.STOW)
	}
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "web", DIMSE: tenant.DIMSEReceiver{Port: 104}},
		[]tenant.ReceiverField{tenant.FieldDIMSEPort}, at(4), e.audit(), nil)
	e.wantKind("SetReceiver stow naming a DIMSE field", err, fault.Invalid)
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", STOW: tenant.STOWReceiver{Keys: []string{"key-e"}}},
		[]tenant.ReceiverField{tenant.FieldSTOWKeys}, at(4), e.audit(), nil)
	e.wantKind("SetReceiver dimse naming a stow field", err, fault.Invalid)

	bad := stowReceiverOf("bad", "store", "key-f")
	bad.DIMSE.Port = 104
	_, err = e.p.AddReceiver(e.ctx, bad, at(5), e.audit(), nil)
	e.wantKind("AddReceiver stow with a port", err, fault.Invalid)
	bad = stowReceiverOf("bad", "store", "key f")
	_, err = e.p.AddReceiver(e.ctx, bad, at(5), e.audit(), nil)
	e.wantKind("AddReceiver stow with a key id that is not one", err, fault.Invalid)
	bad = stowReceiverOf("bad", "store", "key-g", "key-g")
	_, err = e.p.AddReceiver(e.ctx, bad, at(5), e.audit(), nil)
	e.wantKind("AddReceiver stow listing a key twice", err, fault.Invalid)
	bad = stowReceiverOf("bad", "store", "key-h")
	bad.STOW.MaxRequestBytes = 0
	_, err = e.p.AddReceiver(e.ctx, bad, at(5), e.audit(), nil)
	e.wantKind("AddReceiver stow with no largest request", err, fault.Invalid)
}

// testReceiverDeidentification guards a receiver's choice of
// de-identification as a store keeps it: a receiver added with a profile
// version reads it back, one added without it reads back as received (the
// zero ref, as every receiver stored before the choice existed), a change
// of the choice alone sets it and changes nothing else, and a choice that
// is not a version is refused as invalid.
func testReceiverDeidentification(e *penv) {
	e.addStorage("store")
	plain := e.addReceiver("plain", "store", 11112)
	if !plain.Profile.IsZero() || plain.Deidentifies() {
		e.Errorf("a receiver added without a profile reads back with %v", plain.Profile)
	}
	r := receiverOf("deid", "store", 11113)
	r.Profile = tenant.ProfileRef{Name: "research", Version: 3}
	got, err := e.p.AddReceiver(e.ctx, r, at(1), e.audit(), nil)
	e.ok("AddReceiver with a profile", err)
	read, err := e.p.Receiver(e.ctx, "deid")
	e.ok("Receiver", err)
	if got.Profile != r.Profile || read.Profile != r.Profile {
		e.Errorf("a receiver's profile kept as %v and read back as %v, want %v", got.Profile, read.Profile, r.Profile)
	}

	want := read
	want.Profile = tenant.ProfileRef{}
	want.Updated = at(2)
	set, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "deid"}, []tenant.ReceiverField{tenant.FieldDeidentification}, at(2), e.audit(), nil)
	e.ok("SetReceiver as received", err)
	if !reflect.DeepEqual(set, want) {
		e.Errorf("SetReceiver as received answered\n%+v\nwant\n%+v", set, want)
	}
	set, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "plain", Profile: tenant.ProfileRef{Name: "strict", Version: 1}},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, at(3), e.audit(), nil)
	e.ok("SetReceiver to a profile", err)
	if set.Profile != (tenant.ProfileRef{Name: "strict", Version: 1}) || set.DIMSE.Port != 11112 {
		e.Errorf("SetReceiver to a profile kept %v on port %d", set.Profile, set.DIMSE.Port)
	}

	plainRule := naming.Spec{Subject: "sub-{PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"}
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "plain", Naming: &plainRule}, []tenant.ReceiverField{tenant.FieldNaming}, at(3), e.audit(), nil)
	e.wantKind("SetReceiver of a receiver that de-identifies to a naming rule with an unhashed placeholder", err, fault.Precondition)
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "deid", Naming: &plainRule}, []tenant.ReceiverField{tenant.FieldNaming}, at(3), e.audit(), nil)
	e.ok("SetReceiver of a receiver that archives as received to a naming rule with an unhashed placeholder", err)
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "deid", Profile: tenant.ProfileRef{Name: "strict", Version: 1}},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, at(3), e.audit(), nil)
	e.wantKind("SetReceiver to de-identify a receiver whose naming rule has an unhashed placeholder", err, fault.Precondition)

	for what, ref := range map[string]tenant.ProfileRef{
		"version 0":     {Name: "research"},
		"no name":       {Version: 1},
		"a bad name":    {Name: "Research", Version: 1},
		"too new a one": {Name: "research", Version: tenant.MaxProfileVersions + 1},
	} {
		bad := receiverOf("bad", "store", 11114)
		bad.Profile = ref
		_, err := e.p.AddReceiver(e.ctx, bad, at(4), e.audit(), nil)
		e.wantKind("AddReceiver with a profile of "+what, err, fault.Invalid)
	}
}

// testDeidentifiedRows guards how a received file and a quarantine record
// say they were recorded: Deidentified is kept and read back on each, and
// copied to the files of a retry; a release of a record recorded as
// received through a receiver that de-identifies joins no reception (the
// pipeline lands it again, remapped and keyed), while one through a
// receiver that archives as received joins with its flag; a record
// recorded de-identified is refused through a receiver that archives as
// received (a precondition failure, the record still held) and joins with
// its flag through one that de-identifies.
func testDeidentifiedRows(e *penv) {
	e.addStorage("store")
	e.addReceiver("plain", "store", 11112)
	deid := receiverOf("deid", "store", 11113)
	deid.Profile = tenant.ProfileRef{Name: "research", Version: 1}
	_, err := e.p.AddReceiver(e.ctx, deid, at(1), e.audit(), nil)
	e.ok("AddReceiver de-identifying", err)

	r := e.openReception("plain", 1, at(1))
	f := file(r, 1, 1)
	f.Deidentified = true
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f, file(r, 1, 2)}, at(2))
	e.ok("AddReceivedFiles", err)
	files, _, err := e.p.ReceivedFiles(e.ctx, r.ID, master.Page{Limit: 10})
	e.ok("ReceivedFiles", err)
	marked := 0
	for _, g := range files {
		if g.Deidentified {
			marked++
		}
	}
	if len(files) != 2 || marked != 1 {
		e.Errorf("%d files read back, %d of them de-identified; want 2 and 1", len(files), marked)
	}

	plainRecord, deidRecord := e.differentFile("plain", "", 7), e.differentFile("deid", "", 8)
	deidRecord.Deidentified = true
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{plainRecord, deidRecord})
	e.ok("HoldFiles", err)
	if got, _ := e.p.QuarantinedFile(e.ctx, deidRecord.ID); !got.Deidentified {
		e.Error("a record made de-identified reads back as received")
	}
	if got, _ := e.p.QuarantinedFile(e.ctx, plainRecord.ID); got.Deidentified {
		e.Error("a record made as received reads back de-identified")
	}
	pp := e.parts()
	got, err := pp.ReleaseFile(e.ctx, tenant.Release{ID: plainRecord.ID, Mode: tenant.ReleaseAsIs, Receiver: "deid", ReceptionID: "rel-a", Now: at(10)}, e.audit(), nil)
	e.ok("ReleaseFile as received through a receiver that de-identifies", err)
	if got.ReleaseReception != "" {
		e.Errorf("a record as received released through a receiver that de-identifies joined %s", got.ReleaseReception)
	}
	_, err = pp.ReleaseFile(e.ctx, tenant.Release{ID: deidRecord.ID, Mode: tenant.ReleaseAsIs, Receiver: "plain", ReceptionID: "rel-b", Now: at(11)}, e.audit(), nil)
	e.wantKind("ReleaseFile de-identified through a receiver that archives as received", err, fault.Precondition)
	if still, _ := e.p.QuarantinedFile(e.ctx, deidRecord.ID); still.State != tenant.QuarantineHeld || !still.ContentKept {
		e.Errorf("after a refused release the record is %s, kept %v", still.State, still.ContentKept)
	}
	got, err = pp.ReleaseFile(e.ctx, tenant.Release{ID: deidRecord.ID, Mode: tenant.ReleaseAsIs, Receiver: "deid", ReceptionID: "rel-b", Now: at(11)}, e.audit(), nil)
	e.ok("ReleaseFile de-identified through a receiver that de-identifies", err)
	if got.ReleaseReception == "" {
		e.Fatal("a record made de-identified joined no reception")
	}
	joined, _, err := e.p.ReceivedFiles(e.ctx, got.ReleaseReception, master.Page{Limit: 10})
	e.ok("ReceivedFiles of the release", err)
	found := false
	for _, g := range joined {
		if g.SOPInstanceUID == deidRecord.SOPInstanceUID {
			found = true
			if !g.Deidentified || !g.Released {
				e.Errorf("the released file reads %+v, want it released and de-identified", g)
			}
		}
	}
	if !found {
		e.Error("the released file is not among its reception's files")
	}
}
