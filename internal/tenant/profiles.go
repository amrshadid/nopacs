package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/master"
)

// The bounds of de-identification profiles and lookup tables.
const (
	// MaxProfileVersions is the most versions a profile may have; a
	// version past it is refused as a precondition failure.
	MaxProfileVersions = 10000
	// MaxProfileNoteLength bounds the note given with a version.
	MaxProfileNoteLength = 1 << 10
	// MaxProfileTextBytes bounds a version's two texts together, its
	// document and its expanded form: the API's message limit of 4 MiB
	// less 256 KiB for the other fields of an answer, so a version stored
	// can always be answered whole.
	MaxProfileTextBytes = 4<<20 - 256<<10
	// MaxLookupBatch is the most keys one AssignLookupCodes assigns.
	MaxLookupBatch = 1000
	// MaxLookupCodeLength is the longest code of a lookup table: 1 to 16
	// of A-Z, 0-9 and "_", which fits every text VR (PS3.5 6.2: CS's
	// repertoire is the narrowest).
	MaxLookupCodeLength = 16
)

// The details a store adds to the audit entry of a profile version's
// creation.
const (
	// DetailProfileVersion is the version answered: the one made, or the
	// latest when the document equaled it.
	DetailProfileVersion = "version"
	// DetailProfileCreated is "true" when the call made the version and
	// "false" when it answered the latest.
	DetailProfileCreated = "created"
	// DetailProfileChecksum is the version's checksum.
	DetailProfileChecksum = "checksum"
)

// ProfileRef names one version of a profile. The zero ref is none: an
// instance archived as received carries none.
type ProfileRef struct {
	// Name is the profile's name, under the rule of a tenant's name.
	Name string
	// Version is the version, from 1.
	Version int32
}

// IsZero reports whether r names no version.
func (r ProfileRef) IsZero() bool { return r == ProfileRef{} }

// String returns the ref as "name@version".
func (r ProfileRef) String() string { return r.Name + "@" + strconv.Itoa(int(r.Version)) }

// Validate returns an Invalid error unless r names a version: a profile's
// name and a version from 1 to MaxProfileVersions.
func (r ProfileRef) Validate() error {
	if err := ValidateName("profile", r.Name); err != nil {
		return err
	}
	if r.Version < 1 || r.Version > MaxProfileVersions {
		return invalid("a profile's version is 1 to " + strconv.Itoa(MaxProfileVersions))
	}
	return nil
}

// ProfileVersion is one stored version of a de-identification profile.
// Its texts are kept and answered byte for byte as they were given, so
// the checksum can be checked on every read: a version is never changed,
// and a change of a profile is a new version.
type ProfileVersion struct {
	// Name and Version name it.
	Name    string
	Version int32
	// Checksum is the SHA-256 of Expanded in lowercase hexadecimal.
	Checksum string
	// Note is the note given when it was made.
	Note string
	// CreatedByKeyID is the key that made it, its audit entry's
	// (master.AuditEntry.KeyID); empty for the local operator.
	CreatedByKeyID string
	// Created is when it was made.
	Created time.Time
	// Source is the canonical text of the document as given; Expanded the
	// canonical text of its expanded form, bases included. Both are nil
	// in a list.
	Source, Expanded []byte
}

// Ref returns the version's ref.
func (v ProfileVersion) Ref() ProfileRef { return ProfileRef{Name: v.Name, Version: v.Version} }

// NewProfileVersion is a version to store. The caller (the profile
// language) has checked and expanded the document; the store keeps the
// texts and decides the version's number.
type NewProfileVersion struct {
	// Name is the profile's name, under the rule of a tenant's name.
	Name string
	// Source and Expanded are the version's texts (ProfileVersion).
	Source, Expanded []byte
	// Checksum is the SHA-256 of Expanded in lowercase hexadecimal, which
	// the store checks.
	Checksum string
	// Note is why the version is made: at most MaxProfileNoteLength bytes
	// of UTF-8 without NUL.
	Note string
	// ExpectLatest, when not nil, is the version the caller expects to be
	// the profile's latest, 0 for a profile that does not exist yet: the
	// version is stored only when it is so.
	ExpectLatest *int32
}

// Validate returns an Invalid error unless v is a version a store keeps:
// a profile's name, two texts within MaxProfileTextBytes together, a
// checksum that is the SHA-256 of the expanded text, a note within its
// bound, and an expectation that is not negative.
func (v NewProfileVersion) Validate() error {
	if err := ValidateName("profile", v.Name); err != nil {
		return err
	}
	if len(v.Source) == 0 || len(v.Expanded) == 0 {
		return invalid("a profile version has its document and its expanded form")
	}
	if len(v.Source)+len(v.Expanded) > MaxProfileTextBytes {
		return invalid("a profile version's document and expanded form are at most " + strconv.Itoa(MaxProfileTextBytes) + " bytes together")
	}
	sum := sha256.Sum256(v.Expanded)
	if v.Checksum != hex.EncodeToString(sum[:]) {
		return invalid("a profile version's checksum is the SHA-256 of its expanded form, in lowercase hexadecimal")
	}
	if err := validateText("profile version's note", v.Note, 0, MaxProfileNoteLength); err != nil {
		return err
	}
	if v.ExpectLatest != nil && *v.ExpectLatest < 0 {
		return invalid("the latest version expected is 0 or more")
	}
	return nil
}

// ProfileSummary is a profile in a list: its name and its latest version.
type ProfileSummary struct {
	// Name is the profile's name.
	Name string
	// LatestVersion is its latest version.
	LatestVersion int32
	// LatestCreated is when its latest version was made.
	LatestCreated time.Time
}

// CreateOutcome is what CreateProfileVersion decides, given the profile's
// latest stored version (nil when it has none) and the version asked for.
// Every driver decides by it, in the transaction that stores the version,
// so the rules are stated once:
//
//   - a document byte for byte equal to the latest version's is answered
//     with that version, not stored again, whatever the expectation: a
//     call written again after its answer was lost is answered as the
//     first was (the API's CreateProfileVersion is idempotent);
//   - an expectation other than the latest version's number (0 when there
//     is none) is a precondition failure, so two people who each read
//     version 3 do not both store a version 4 unaware of each other;
//   - a profile at MaxProfileVersions is a precondition failure;
//   - otherwise the version is stored with the next number.
//
// next is the number to store, 0 when nothing is stored.
func CreateOutcome(latest *ProfileVersion, v NewProfileVersion) (next int32, err error) {
	if latest != nil && slices.Equal(latest.Source, v.Source) {
		return 0, nil
	}
	var have int32
	if latest != nil {
		have = latest.Version
	}
	if v.ExpectLatest != nil && *v.ExpectLatest != have {
		return 0, Precondition("the profile's latest version is " + strconv.Itoa(int(have)) + ", not the " +
			strconv.Itoa(int(*v.ExpectLatest)) + " expected")
	}
	if have >= MaxProfileVersions {
		return 0, Precondition("the profile has " + strconv.Itoa(MaxProfileVersions) + " versions, the most a profile may have")
	}
	return have + 1, nil
}

// CreatedDetails returns the audit entry of a version's creation with the
// details the store adds: the version answered, whether it was made, and
// its checksum.
func CreatedDetails(audit master.AuditEntry, v ProfileVersion, created bool) (master.AuditEntry, error) {
	a := audit.WithDetails(DetailProfileVersion, strconv.Itoa(int(v.Version)),
		DetailProfileCreated, strconv.FormatBool(created), DetailProfileChecksum, v.Checksum)
	return a, a.Validate()
}

// LookupKey is one value a lookup table is asked for: the table and the
// keyed hash of the value (naming's {#X} code), never the value.
type LookupKey struct {
	// Table is the table's name, under the rule of a tenant's name.
	Table string
	// Key is the keyed hash of the value: naming.HashLength characters of
	// lower case base32.
	Key string
}

// Validate returns an Invalid error unless k names a table and is a keyed
// hash's text.
func (k LookupKey) Validate() error {
	if err := ValidateName("lookup table", k.Table); err != nil {
		return err
	}
	if len(k.Key) != naming.HashLength || !allBytes(k.Key, func(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= '2' && b <= '7') }) {
		return invalid("a lookup key is the keyed hash of a value: " + strconv.Itoa(naming.HashLength) + " characters of lower case base32")
	}
	return nil
}

// ValidateLookupKeys returns an Invalid error unless keys is a batch
// AssignLookupCodes takes: 1 to MaxLookupBatch valid keys.
func ValidateLookupKeys(keys []LookupKey) error {
	if len(keys) == 0 || len(keys) > MaxLookupBatch {
		return invalid("a lookup assigns 1 to " + strconv.Itoa(MaxLookupBatch) + " keys at once")
	}
	for _, k := range keys {
		if err := k.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// LookupCodeText is the code a table assigns as its n-th: n in decimal
// (the API's LookupAction: "the next number of the table, in decimal from
// 1"). It is a code of the form ValidLookupCode keeps while n has at most
// MaxLookupCodeLength digits.
func LookupCodeText(n int64) string { return strconv.FormatInt(n, 10) }

// ValidLookupCode reports whether code is a lookup table's code: 1 to
// MaxLookupCodeLength of A-Z, 0-9 and "_".
func ValidLookupCode(code string) bool {
	return code != "" && len(code) <= MaxLookupCodeLength && allBytes(code, func(b byte) bool {
		return (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
	})
}

// Deidentification is what the tenant's database keeps for
// de-identification: its profiles' versions and its lookup tables. The
// tenant's keys for it (SecretUID, SecretDate, beside SecretNaming) are
// Secrets.
type Deidentification interface {
	Profiles
	LookupTables
}

// Profiles keeps a tenant's de-identification profiles, each a list of
// immutable versions.
//
// Under several writers (Postgres) a driver keeps CreateProfileVersion's
// compare, precondition and insert in one transaction, serialized with
// every other change of the tenant's configuration, so two calls at once
// for one profile store at most one version each in turn, the second
// answered as CreateOutcome says against the first's.
type Profiles interface {
	// CreateProfileVersion stores v as the profile's next version (1 for
	// a profile it does not have) and returns it with its texts, and true;
	// or returns the latest version and false, storing nothing, when v's
	// document equals it (CreateOutcome, which also says what is a
	// precondition failure). now is when it is made, and the audit
	// entry's key is the version's creator. The audit entry is written
	// either way, with the details CreatedDetails adds.
	CreateProfileVersion(ctx context.Context, v NewProfileVersion, now time.Time, audit master.AuditEntry, guard Guard) (ProfileVersion, bool, error)
	// ProfileVersion returns a version with its texts as stored: version
	// 0 is the latest. A profile or a version it does not have is
	// NotFound; a negative version is Invalid.
	ProfileVersion(ctx context.Context, name string, version int32) (ProfileVersion, error)
	// ListProfiles returns a page of the tenant's profiles in order of
	// name, each with its latest version.
	ListProfiles(ctx context.Context, page master.Page) ([]ProfileSummary, string, error)
	// ListProfileVersions returns a page of a profile's versions, newest
	// first, without their texts. A profile the tenant does not have is
	// NotFound. The cursor is bound to the profile's name.
	ListProfileVersions(ctx context.Context, name string, page master.Page) ([]ProfileVersion, string, error)
	// InstanceProfile returns the profile version that de-identified the
	// held instance of that SOP Instance UID, as its commit recorded it
	// (ConfirmedInstance.Profile): the zero ref for one archived as
	// received. An instance the tenant does not hold is NotFound.
	InstanceProfile(ctx context.Context, sopInstanceUID string) (ProfileRef, error)
}

// LookupTables keeps a tenant's lookup tables: from the keyed hash of a
// value to the code that replaces it. A table is made by its first code.
// Codes are assigned by the server's work, once per value; the work is
// not a person's change, and writes no audit entry. A person's codes are
// imported (LookupImports), each import a change with its audit entry.
type LookupTables interface {
	// LookupCode returns the code the table holds for the key, and
	// whether it holds one.
	LookupCode(ctx context.Context, k LookupKey) (string, bool, error)
	// AssignLookupCodes returns the codes the tables hold for keys, in
	// their order, first giving each key a table holds no code for that
	// table's next code (LookupCodeText of one more than the most it has
	// assigned), all in one transaction or none. A key named twice gets
	// one code. A key once given a code keeps it: assigning it again
	// answers the same code, so a file processed again, by a step run
	// again or a release, gets the codes it got before. Under several
	// writers the tables of a batch are taken in order of name, and a
	// table's codes are assigned one transaction at a time, so codes are
	// unique and without gaps whatever runs at once. now is when they are
	// assigned.
	AssignLookupCodes(ctx context.Context, keys []LookupKey, now time.Time) ([]string, error)
	LookupImports
}
