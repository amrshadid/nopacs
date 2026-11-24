package master

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

var t0 = time.Date(2026, 11, 18, 9, 30, 0, 123456000, time.UTC)

func validTenant() Tenant {
	return Tenant{ID: "t1", Name: "lab", Driver: "sqlite", SealedAddress: []byte{1, 2, 3}, State: StateReady, Created: t0}
}

func validKey() Key {
	return Key{ID: "k1", Hash: make([]byte, 32), Holder: "a person", Created: t0}
}

func validEntry() AuditEntry {
	return AuditEntry{ID: "a1", Time: t0, Method: "CreateTenant", Outcome: OutcomeDone}
}

// wantKind fails the test unless err has the kind want; nil wants nil.
func wantKind(t *testing.T, what string, err error, want *fault.Kind) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Errorf("%s: %v, want no error", what, err)
		}
		return
	}
	if err == nil {
		t.Errorf("%s: no error, want %v", what, *want)
		return
	}
	if got := fault.KindOf(err); got != *want {
		t.Errorf("%s: kind %v (%v), want %v", what, got, err, *want)
	}
}

var invalidKind = fault.Invalid

// TestTenantNameRule guards the rule for a tenant's name: 1 to 63 of a-z, 0-9
// and -, starting with a letter, because the name appears in URLs and paths.
func TestTenantNameRule(t *testing.T) {
	good := []string{"a", "lab", "nyuad-mri", "a1", "z-", strings.Repeat("a", 63), "com10", "console", "lpt", "nul-1"}
	bad := []string{"", "1lab", "-lab", "Lab", "lab_1", "lab.1", "lab 1", "läb", strings.Repeat("a", 64), "../x",
		"con", "prn", "aux", "nul", "com0", "com9", "lpt1", "lpt9"}
	for _, name := range good {
		wantKind(t, "name "+strconv.Quote(name), ValidateTenantName(name), nil)
	}
	for _, name := range bad {
		wantKind(t, "name "+strconv.Quote(name), ValidateTenantName(name), &invalidKind)
	}
}

// TestTenantValidate guards each field of a tenant a store refuses before it
// writes anything.
func TestTenantValidate(t *testing.T) {
	wantKind(t, "valid tenant", validTenant().Validate(), nil)
	cases := map[string]func(*Tenant){
		"empty id":        func(x *Tenant) { x.ID = "" },
		"id with slash":   func(x *Tenant) { x.ID = "a/b" },
		"long id":         func(x *Tenant) { x.ID = strings.Repeat("x", 65) },
		"bad name":        func(x *Tenant) { x.Name = "Lab" },
		"no driver":       func(x *Tenant) { x.Driver = "" },
		"driver upper":    func(x *Tenant) { x.Driver = "SQLite" },
		"no address":      func(x *Tenant) { x.SealedAddress = nil },
		"huge address":    func(x *Tenant) { x.SealedAddress = make([]byte, MaxSealedAddressLength+1) },
		"unknown state":   func(x *Tenant) { x.State = "gone" },
		"long reason":     func(x *Tenant) { x.StateReason = strings.Repeat("r", MaxStateReasonLength+1) },
		"zero created":    func(x *Tenant) { x.Created = time.Time{} },
		"created in 1969": func(x *Tenant) { x.Created = time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC) },
		"year 10000":      func(x *Tenant) { x.Created = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	}
	for name, change := range cases {
		tn := validTenant()
		change(&tn)
		wantKind(t, name, tn.Validate(), &invalidKind)
	}
}

// TestKeyValidate guards the key rules: a 32-byte hash, a holder of 1 to 256
// bytes of UTF-8, an expiry after creation, and a new key that is neither
// used nor revoked.
func TestKeyValidate(t *testing.T) {
	wantKind(t, "valid key", validKey().Validate(), nil)
	k := validKey()
	k.Holder = strings.Repeat("é", 128) // 256 bytes
	wantKind(t, "holder of 256 bytes", k.Validate(), nil)
	k.Expires = t0.Add(time.Hour)
	wantKind(t, "with expiry", k.Validate(), nil)

	cases := map[string]func(*Key){
		"short hash":         func(x *Key) { x.Hash = make([]byte, 31) },
		"no holder":          func(x *Key) { x.Holder = "" },
		"holder 257 bytes":   func(x *Key) { x.Holder = strings.Repeat("é", 128) + "x" },
		"holder not utf-8":   func(x *Key) { x.Holder = "a\xffb" },
		"holder with nul":    func(x *Key) { x.Holder = "a\x00b" },
		"expiry at creation": func(x *Key) { x.Expires = t0 },
		"already used":       func(x *Key) { x.LastUsed = t0 },
		"already revoked":    func(x *Key) { x.Revoked = t0 },
	}
	for name, change := range cases {
		key := validKey()
		change(&key)
		wantKind(t, name, key.Validate(), &invalidKind)
	}
}

// TestGrantValidate guards that a grant names a valid key, a valid scope and
// one of the four roles, and that the empty role is not one.
func TestGrantValidate(t *testing.T) {
	wantKind(t, "installation grant", Grant{KeyID: "k", Scope: Installation, Role: RoleAdmin}.Validate(), nil)
	wantKind(t, "tenant grant", Grant{KeyID: "k", Scope: TenantScope("t1"), Role: RoleReader}.Validate(), nil)
	wantKind(t, "no role", Grant{KeyID: "k", Role: ""}.Validate(), &invalidKind)
	wantKind(t, "unknown role", Grant{KeyID: "k", Role: "owner"}.Validate(), &invalidKind)
	wantKind(t, "bad tenant id", Grant{KeyID: "k", Scope: TenantScope("a b"), Role: RoleReader}.Validate(), &invalidKind)
	wantKind(t, "no key", Grant{Scope: Installation, Role: RoleReader}.Validate(), &invalidKind)
	if Installation.String() != "installation" || TenantScope("t1").String() != "tenant/t1" {
		t.Errorf("scope strings: %q, %q", Installation.String(), TenantScope("t1").String())
	}
}

// TestSettingRules guards a setting's name (1 to 128 of a-z 0-9 . _ -) and
// value (at most 64 KiB of UTF-8).
func TestSettingRules(t *testing.T) {
	wantKind(t, "dotted name", ValidateSettingName("api.max_message-size"), nil)
	wantKind(t, "128 bytes", ValidateSettingName(strings.Repeat("a", 128)), nil)
	for _, name := range []string{"", "Upper", "a b", "a/b", strings.Repeat("a", 129)} {
		wantKind(t, "name "+strconv.Quote(name), ValidateSettingName(name), &invalidKind)
	}
	wantKind(t, "empty value", ValidateSettingValue(""), nil)
	wantKind(t, "64 KiB value", ValidateSettingValue(strings.Repeat("v", 64<<10)), nil)
	wantKind(t, "64 KiB + 1", ValidateSettingValue(strings.Repeat("v", 64<<10+1)), &invalidKind)
	wantKind(t, "not utf-8", ValidateSettingValue("\xc3"), &invalidKind)
}

// TestAuditEntryValidate guards the bounds of an audit entry: the reason at
// most 1 KiB, at most 16 details with keys of 64 bytes and values of 256, a
// key or a local user but not both, and a tenant name that need not be a
// valid name, since it records what a request said.
func TestAuditEntryValidate(t *testing.T) {
	wantKind(t, "valid entry", validEntry().Validate(), nil)
	e := validEntry()
	e.TenantName = "Not A Valid Name ../"
	e.Reason = strings.Repeat("r", MaxAuditReasonLength)
	e.Details = map[string]string{}
	for i := range MaxAuditDetails {
		e.Details[strings.Repeat("k", MaxAuditDetailKeyLength-3)+strconv.Itoa(100+i)] = strings.Repeat("v", MaxAuditDetailValueLength)
	}
	wantKind(t, "entry at every bound", e.Validate(), nil)

	cases := map[string]func(*AuditEntry){
		"no id":           func(x *AuditEntry) { x.ID = "" },
		"zero time":       func(x *AuditEntry) { x.Time = time.Time{} },
		"key and user":    func(x *AuditEntry) { x.KeyID, x.LocalUser = "k", "501" },
		"bad key id":      func(x *AuditEntry) { x.KeyID = "k k" },
		"no method":       func(x *AuditEntry) { x.Method = "" },
		"bad outcome":     func(x *AuditEntry) { x.Outcome = "ok" },
		"long reason":     func(x *AuditEntry) { x.Reason = strings.Repeat("r", MaxAuditReasonLength+1) },
		"long object":     func(x *AuditEntry) { x.Object = strings.Repeat("o", MaxAuditFieldLength+1) },
		"long tenant":     func(x *AuditEntry) { x.TenantName = strings.Repeat("t", MaxAuditFieldLength+1) },
		"empty detail":    func(x *AuditEntry) { x.Details = map[string]string{"": "v"} },
		"long detail key": func(x *AuditEntry) { x.Details = map[string]string{strings.Repeat("k", 65): "v"} },
		"long detail val": func(x *AuditEntry) { x.Details = map[string]string{"k": strings.Repeat("v", 257)} },
		"17 details": func(x *AuditEntry) {
			x.Details = map[string]string{}
			for i := range 17 {
				x.Details["k"+strconv.Itoa(i)] = "v"
			}
		},
	}
	for name, change := range cases {
		entry := validEntry()
		change(&entry)
		wantKind(t, name, entry.Validate(), &invalidKind)
	}
}

// TestWithRoleChange guards the details a grant change adds to its audit
// entry, "none" for a side with no grant, and that the caller's map is left
// as it was.
func TestWithRoleChange(t *testing.T) {
	e := validEntry()
	e.Details = map[string]string{"scope": "installation"}
	got := e.WithRoleChange("", RoleReader)
	if got.Details[DetailOldRole] != "none" || got.Details[DetailNewRole] != "reader" || got.Details["scope"] != "installation" {
		t.Errorf("details = %v", got.Details)
	}
	if len(e.Details) != 1 {
		t.Errorf("the caller's details were changed: %v", e.Details)
	}
	removed := e.WithRoleChange(RoleAdmin, "")
	if removed.Details[DetailOldRole] != "admin" || removed.Details[DetailNewRole] != "none" {
		t.Errorf("details = %v", removed.Details)
	}
	full := validEntry()
	full.Details = map[string]string{}
	for i := range MaxAuditDetails {
		full.Details["k"+strconv.Itoa(i)] = "v"
	}
	wantKind(t, "full details before a role change", full.ValidateRoleChange(), &invalidKind)
}

// TestAuditFilterValidate guards the filter of an audit listing: a valid key
// id and outcome, and an end time after the start time.
func TestAuditFilterValidate(t *testing.T) {
	wantKind(t, "zero filter", AuditFilter{}.Validate(), nil)
	wantKind(t, "full filter", AuditFilter{TenantName: "lab", KeyID: "k", Outcome: OutcomeRefused, Since: t0, Until: t0.Add(time.Second)}.Validate(), nil)
	wantKind(t, "bad outcome", AuditFilter{Outcome: "x"}.Validate(), &invalidKind)
	wantKind(t, "bad key", AuditFilter{KeyID: "a b"}.Validate(), &invalidKind)
	wantKind(t, "end before start", AuditFilter{Since: t0, Until: t0}.Validate(), &invalidKind)
}

// TestPageValidate guards that a page's limit is 1 to 1000: zero and above
// are refused rather than read as "everything".
func TestPageValidate(t *testing.T) {
	wantKind(t, "1", Page{Limit: 1}.Validate(), nil)
	wantKind(t, "1000", Page{Limit: 1000}.Validate(), nil)
	wantKind(t, "0", Page{Limit: 0}.Validate(), &invalidKind)
	wantKind(t, "-1", Page{Limit: -1}.Validate(), &invalidKind)
	wantKind(t, "1001", Page{Limit: 1001}.Validate(), &invalidKind)
}
