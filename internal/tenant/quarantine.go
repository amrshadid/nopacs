package tenant

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/master"
)

// QuarantineReason is why a file was quarantined, from the API contract's
// fixed list. Its text is what a store keeps.
type QuarantineReason string

// The reasons.
const (
	QuarantineNotDICOM            QuarantineReason = "not_dicom"
	QuarantineSOPClassNotAccepted QuarantineReason = "sop_class_not_accepted"
	QuarantineTooLarge            QuarantineReason = "too_large"
	QuarantineBadUID              QuarantineReason = "bad_uid"
	QuarantineBadLabel            QuarantineReason = "bad_label"
	QuarantineLabelConflict       QuarantineReason = "label_conflict"
	QuarantineDifferentFile       QuarantineReason = "different_file"
	QuarantineUIDElsewhere        QuarantineReason = "uid_elsewhere"
	QuarantineStorageRefused      QuarantineReason = "storage_refused"
	QuarantineReadBackMismatch    QuarantineReason = "read_back_mismatch"
	// QuarantineHeaderTooLarge is a file whose header runs past the bound
	// the reader keeps to, so its UIDs could not be read. Unlike a file too
	// large, its bytes are kept, so a person can release or reject it.
	QuarantineHeaderTooLarge QuarantineReason = "header_too_large"
	// QuarantineIncomingLost is a file the store recorded whose copy in the
	// incoming area is gone when the pipeline came to read it: there are
	// no bytes left to keep, and the record says what was lost.
	QuarantineIncomingLost QuarantineReason = "incoming_lost"
	// QuarantineUIDMismatch is a file whose placing UIDs read from its
	// whole header differ from those its source read from its first bytes
	// to decide where it went (a STOW-RS part): a data set that repeats a
	// placing attribute or gives one out of the order of tags. Its bytes
	// are kept; a release places it by its whole header.
	QuarantineUIDMismatch QuarantineReason = "uid_mismatch"
	// QuarantineDeidFailed is a file its receiver's profile cannot
	// de-identify: a fixed cause (DeidCauses) under "cause", with the
	// element ("path") and rule ("rule") when there is one. Its original
	// bytes are kept; a release de-identifies it again.
	QuarantineDeidFailed QuarantineReason = "deid_failed"
	// QuarantineResidualValue is a file in which the check after
	// de-identification found a value the profile removed: the element,
	// "preamble" or "file" for bytes outside every element, or the label
	// ("subject", "session", "scan"), under "path", and the kind of value
	// ("kind": ResidualKinds), never the value.
	QuarantineResidualValue QuarantineReason = "residual_value"
	// QuarantineProfileRefused is a file a rule of the profile quarantines
	// (its action is quarantine): the rule's name under "rule".
	QuarantineProfileRefused QuarantineReason = "profile_refused"
	// QuarantineBurnedInRisk is an image whose pixels may carry burned-in
	// text, by its header as received, that no pixel rule of its
	// receiver's profile matches: why it is at risk under "cause"
	// (BurnedInCauses). Its original bytes are kept; a release
	// de-identifies it again.
	QuarantineBurnedInRisk QuarantineReason = "burned_in_risk"
	// QuarantineUnrouted is a file of a study its receiver's routing did
	// not route: no route chose it and the routing holds what none
	// chooses ("cause" no_match), a route could not be evaluated ("cause"
	// route_unusable, "route" its name), or another reception of the study
	// made its session on another storage meanwhile ("cause"
	// session_elsewhere). Its bytes are kept; a
	// release names the storage (Release.Storage), or runs the routes
	// again.
	QuarantineUnrouted QuarantineReason = "unrouted"
	// QuarantineRepairsDamaged is a file whose SOP Instance UID and
	// received checksum are those of an archived instance a scrub marked
	// damaged or missing (Scrubbing): the same file sent again, which can
	// repair it. It is held, not ignored as a duplicate, so the repair is a
	// person's decision (a replace, which clears the mark); "finding" names
	// the finding that marked the instance.
	QuarantineRepairsDamaged QuarantineReason = "repairs_damaged"
	// QuarantineWasDeleted is a file of data a correction deleted: its SOP
	// Instance UID, Series Instance UID or Study Instance UID is one a
	// delete named, or its subject label is one a subject's delete named
	// (Corrections.DeletedMatches). It is held, not archived, so a consent
	// withdrawn is not undone by the next send; "correction" names the
	// delete it matched.
	QuarantineWasDeleted QuarantineReason = "was_deleted"
)

// DeidCauses are the causes a deid_failed record names: the
// de-identification engine's fixed causes (a date to shift and no subject,
// a lookup with no code, a value not of its VR, sequences too deep, a data
// set too large, the engine's own fault), and those of the pipeline around
// it: the file landed under a receiver that de-identifies and now names no
// profile (no_profile), the file was recorded as received and its receiver
// de-identifies now (recorded_as_received), the profile version cannot run in this build
// (profile_unusable), the data set cannot be read or written for
// de-identification (unreadable, unwritable), and a profile that leaves a
// placing UID other than the keyed remapping the database holds
// (uid_not_remapped); those of the work on pixels (the decoded image past
// deid.pixel-limit, a layout that cannot be filled, frames that do not
// decode, more bytes of native pixels than the frames stated, a pixel
// rule's rectangle wholly outside the image, compressed
// segments that are not searched under a profile that holds them, a
// detector finding more than text covers, an image at risk of burned-in
// text that no pixel template of the tenant fits); and those of the check after de-identification,
// which could not judge the file: too large, too many values to search
// for, or unreadable to its end (a deflated file among them); and a file
// whose own subject label is not its session's subject (subject_mismatch),
// which would be shifted and hashed by another subject identity.
var DeidCauses = []string{"no_subject", "lookup_missing", "bad_value", "too_deep", "too_large", "internal",
	"no_profile", "recorded_as_received", "profile_unusable", "unreadable", "unwritable", "uid_not_remapped",
	"check_too_large", "check_too_many_needles", "check_unreadable",
	"pixels_too_large", "pixels_unsupported", "pixels_undecodable", "pixel_region_outside", "pixel_text_unsearched",
	"detect_implausible", "frames_mismatch", "subject_mismatch", "no_pixel_template"}

// BurnedInCauses are the causes a burned_in_risk record names: Burned In
// Annotation (0028,0301) is YES; the image is a secondary capture, of a
// SOP class or a modality commonly drawn on, and Burned In Annotation is
// not NO; or it is the label or overview image of a whole slide.
var BurnedInCauses = []string{"burned_in_annotation", "secondary_capture", "modality", "sop_class", "slide_label"}

// UnroutedCauses are the causes an unrouted record names: no route chose
// the study and the routing holds what none chooses, a route could not be
// evaluated, another reception of the study made its session on another
// storage while this one was in flight, or the receiver was removed
// (UnroutedReceiverRemoved).
var UnroutedCauses = []string{"no_match", "route_unusable", "session_elsewhere", UnroutedReceiverRemoved}

// UnroutedReceiverRemoved is the cause of an unrouted record of a
// reception whose receiver was removed before its study had a session: its
// subject would be named under the receiver's namespace, gone with it, and
// is never named under another (fail closed).
const UnroutedReceiverRemoved = "receiver_removed"

// ResidualKinds are the kinds of value a residual_value record names: a
// person's name, an identifier or any other text, a date, a UID, a term of
// one of the profile's text lists, and an encapsulated document not of text
// kept under a profile with lists, which the terms could not be searched in.
var ResidualKinds = []string{"name", "identifier", "date", "uid", "list_term", "list_unsearchable"}

// The fault words a quarantine record's details carry under "fault", the API
// contract's, each listed with the reasons that use it (the label conflict
// faults are in catalog.go).
const (
	// FaultMissing, FaultEmpty and FaultBadUID: an attribute absent, empty,
	// or not a UID (QuarantineBadUID, QuarantineBadLabel).
	FaultMissing = "missing"
	FaultEmpty   = "empty"
	FaultBadUID  = "bad_uid"
	// FaultTooLong, FaultBadCharacter, FaultNoMatch and FaultBadLabel: a
	// value too long for a label, one with a character a label may not
	// hold, a pattern that does not match, a label made that is not valid
	// (QuarantineBadLabel).
	FaultTooLong      = "too_long"
	FaultBadCharacter = "bad_character"
	FaultNoMatch      = "no_match"
	FaultBadLabel     = "bad_label"
	// The storage faults (QuarantineStorageRefused), the contract's
	// StorageFault values in lowercase without their prefix.
	FaultUnreachable       = "unreachable"
	FaultTLSFailed         = "tls_failed"
	FaultRefused           = "refused"
	FaultNotFound          = "not_found"
	FaultWrongAnswer       = "wrong_answer"
	FaultAddressNotAllowed = "address_not_allowed"
)

// reasonDetails are the detail keys each reason may carry, the contract's,
// each with the check of its value. Every value is from a closed list, a
// number, a UID as received, a storage's name, a checksum, or an
// attribute's keyword or tag as the naming package parses it: nothing a
// file's value could be written into, so a detail never carries one.
var reasonDetails = map[QuarantineReason]map[string]func(string) bool{
	QuarantineNotDICOM:            {"offset": isNumber},
	QuarantineSOPClassNotAccepted: {"sop_class_uid": isUID},
	QuarantineTooLarge:            {"limit": isNumber},
	QuarantineBadUID: {
		"attribute": oneOf("SOPInstanceUID", "SOPClassUID", "StudyInstanceUID", "SeriesInstanceUID"),
		"fault":     oneOf(FaultMissing, FaultEmpty, FaultBadUID),
	},
	QuarantineBadLabel: {
		"label":     oneOf("subject", "session", "scan"),
		"attribute": func(v string) bool { return v == "" || isAttribute(v) },
		"fault":     oneOf(FaultMissing, FaultEmpty, FaultTooLong, FaultBadCharacter, FaultBadUID, FaultNoMatch, FaultBadLabel),
	},
	QuarantineLabelConflict: {
		"label":      oneOf("subject", "session", "scan", "instance"),
		"fault":      oneOf(FaultHeldByAnotherSession, FaultTwoSubjectsForOneStudy, FaultHeldByAnotherScan, FaultCaseCollision, FaultHeldByCorrection),
		"correction": func(v string) bool { return ValidateCorrectionID(v) == nil },
	},
	QuarantineDifferentFile:    {},
	QuarantineUIDElsewhere:     {"study_instance_uid": isUID, "series_instance_uid": isUID},
	QuarantineStorageRefused:   {"storage": isName, "fault": oneOf(FaultUnreachable, FaultTLSFailed, FaultRefused, FaultNotFound, FaultWrongAnswer, FaultAddressNotAllowed)},
	QuarantineReadBackMismatch: {"storage": isName, "read_sha256": isHex64},
	QuarantineHeaderTooLarge:   {"limit": isNumber},
	QuarantineIncomingLost:     {},
	QuarantineUIDMismatch:      {"attribute": oneOf("SOPInstanceUID", "SOPClassUID", "StudyInstanceUID", "SeriesInstanceUID")},
	QuarantineDeidFailed:       {"cause": oneOf(DeidCauses...), "path": isElementPath, "rule": isRuleName},
	QuarantineResidualValue:    {"path": func(v string) bool { return isElementPath(v) || slices.Contains(rawPlaces, v) }, "kind": oneOf(ResidualKinds...)},
	QuarantineProfileRefused:   {"rule": isRuleName},
	QuarantineBurnedInRisk:     {"cause": oneOf(BurnedInCauses...)},
	QuarantineUnrouted:         {"cause": oneOf(UnroutedCauses...), "route": isName},
	QuarantineRepairsDamaged:   {"finding": func(v string) bool { return master.ValidateID("finding id", v) == nil }},
	QuarantineWasDeleted:       {"correction": func(v string) bool { return ValidateCorrectionID(v) == nil }},
}

// rawPlaces are where outside any element a residual value can be found,
// as a residual_value record's "path" names them: a file's preamble, or
// bytes between elements; or the label the file was to be archived under.
var rawPlaces = []string{"preamble", "file", "subject", "session", "scan"}

// ValidElementPath reports whether v is an element's path as a quarantine
// record's detail takes it (isElementPath).
func ValidElementPath(v string) bool { return isElementPath(v) || slices.Contains(rawPlaces, v) }

// ValidRuleName reports whether v names a profile's rule as a quarantine
// record's detail takes it (isRuleName).
func ValidRuleName(v string) bool { return isRuleName(v) }

// maxElementPath bounds an element's path in a detail: 64 sequences deep,
// each a tag and an item number, with room.
const maxElementPath = 2048

// isElementPath reports whether v is where an element is in a data set as
// the de-identification engine writes it: tags of eight upper-case
// hexadecimal digits, each inside a sequence followed by its item number in
// brackets and a slash, such as "00081115[0]/0020000E". Nothing of a value
// can be written in that alphabet but hexadecimal digits, which a tag's
// form bounds.
func isElementPath(v string) bool {
	if v == "" || len(v) > maxElementPath {
		return false
	}
	for _, step := range strings.Split(v, "/") {
		t, item, nested := strings.Cut(step, "[")
		if len(t) != 8 || !allBytes(t, func(b byte) bool { return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'F') }) {
			return false
		}
		if nested && (!strings.HasSuffix(item, "]") || !isNumber(strings.TrimSuffix(item, "]"))) {
			return false
		}
	}
	return true
}

// isRuleName reports whether v names a rule of a profile: its name (1 to
// 64 of a-z, 0-9, ".", "_" and "-", starting with a letter or digit), or
// "rule " and its position for one without a name.
func isRuleName(v string) bool {
	if n, ok := strings.CutPrefix(v, "rule "); ok {
		return isNumber(n)
	}
	if v == "" || len(v) > 64 || (v[0] < 'a' || v[0] > 'z') && (v[0] < '0' || v[0] > '9') {
		return false
	}
	return allBytes(v, func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '.' || b == '_' || b == '-'
	})
}

// oneOf returns a check that a value is one of words.
func oneOf(words ...string) func(string) bool {
	return func(v string) bool { return slices.Contains(words, v) }
}

// isNumber reports whether v is a decimal count of at most 20 digits.
func isNumber(v string) bool {
	return v != "" && len(v) <= 20 && allBytes(v, func(b byte) bool { return b >= '0' && b <= '9' })
}

// isUID reports whether v is a UID (PS3.5 9.1).
func isUID(v string) bool { return uid.Valid(v) }

// isName reports whether v follows the tenant name rule.
func isName(v string) bool { return ValidateName("storage", v) == nil }

// isHex64 reports whether v is a SHA-256 in lowercase hexadecimal.
func isHex64(v string) bool {
	return len(v) == 64 && allBytes(v, func(b byte) bool { return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') })
}

// isAttribute reports whether v is an attribute a naming rule may name: a
// keyword of the naming package's table or a tag written (gggg,eeee),
// checked by the naming package's own parser.
func isAttribute(v string) bool {
	_, err := naming.ParseAttribute(v)
	return err == nil
}

// Valid reports whether r is one of the reasons.
func (r QuarantineReason) Valid() bool {
	_, ok := reasonDetails[r]
	return ok
}

// QuarantineState is what has been decided about a quarantined file.
type QuarantineState string

// The states.
const (
	QuarantineHeld     QuarantineState = "held"
	QuarantineReleased QuarantineState = "released"
	QuarantineRejected QuarantineState = "rejected"
)

// ReleaseMode is how a released file runs through the pipeline again.
type ReleaseMode string

// ReleaseAsIs runs it by the pipeline's rules now, the one mode of this
// version. Replacing an archived instance with a released file deletes
// archived data, which needs a recorded reason and proof of the copy that
// replaces it; it will be an operation of its own, not a mode of release.
const ReleaseAsIs ReleaseMode = "as_is"

// MaxQuarantineDetails bounds a record's details, the contract's.
const MaxQuarantineDetails = 16

// QuarantinedFile is the record of one quarantined file.
type QuarantinedFile struct {
	// ID is the record's id, given by the caller.
	ID string
	// Reason is why it was quarantined.
	Reason QuarantineReason
	// Details are more about the reason, under the keys the reason allows.
	Details map[string]string
	// State is what has been decided. A record is held when made.
	State QuarantineState
	// Receiver names the receiver it arrived at.
	Receiver string
	// ReceptionID names its reception, empty when its study was not known.
	ReceptionID string
	// Caller is who sent it.
	Caller Caller
	// Size is its size in bytes as written, or read before the bound.
	Size int64
	// SHA256 is its checksum, zero when its content was not kept whole.
	SHA256 SHA256
	// KeptSHA256 is the checksum of the file held under its UID, for
	// QuarantineDifferentFile only.
	KeptSHA256 SHA256
	// The UIDs as received, each empty when it could not be read.
	SOPClassUID, SOPInstanceUID, StudyUID, SeriesUID, TransferSyntaxUID string
	// Incoming is where its bytes are in the incoming area, empty when
	// they were never kept.
	Incoming string
	// ContentKept says its bytes are held: true while held with Incoming
	// set, false once rejected.
	ContentKept bool
	// Quarantined is when it was quarantined.
	Quarantined time.Time
	// Decided is when it was released or rejected, zero while held.
	Decided time.Time
	// ReleaseMode, ReleaseReception, ReleaseReceiver and ReleaseReason are
	// set when it is released.
	ReleaseMode ReleaseMode
	// ReleaseReception is the reception its release joined.
	ReleaseReception string
	// ReleaseReceiver is the receiver its release ran through.
	ReleaseReceiver string
	// ReleaseStorage is the storage its release named (Release.Storage),
	// empty when it named none.
	ReleaseStorage string
	// ReleaseProfile is the profile version its release named
	// (Release.Profile), zero when it named none.
	ReleaseProfile ProfileRef
	// RejectReason is why it was rejected.
	RejectReason string
	// ReleaseReason is why it was released, as the person gave it; empty
	// when none was given or it was not released.
	ReleaseReason string
	// ReleasedFrom names the record whose release quarantined it again.
	ReleasedFrom string
	// Deidentified says the record was made at a receiver that
	// de-identifies: its placing UIDs are remapped and its checksum keyed
	// (as a received file's, ReceivedFile.Deidentified), and only its bytes
	// in the incoming area are the original's.
	Deidentified bool
	// Import, when not nil, is the import unit that handed in a file held
	// as it arrived (it never became a received file) and its path: the
	// store writes the arrival, decided quarantined, with the record
	// (Importing). Written only; a read answers nil.
	Import *ImportArrival
	// ImportJob and ImportPath name the import job that handed the file in
	// and its path under the root, empty for a file that came any other
	// way. Answered by a store, which sets them (Importing); ignored in a
	// write.
	ImportJob, ImportPath string
}

// Validate returns an Invalid error unless q can be recorded as a new held
// file.
func (q QuarantinedFile) Validate() error {
	if err := master.ValidateID("quarantine id", q.ID); err != nil {
		return err
	}
	allowed, ok := reasonDetails[q.Reason]
	if !ok {
		return invalid("a quarantine reason is one of the contract's codes")
	}
	if len(q.Details) > MaxQuarantineDetails {
		return invalid("a quarantine record has at most " + strconv.Itoa(MaxQuarantineDetails) + " details")
	}
	for k, v := range q.Details {
		check, ok := allowed[k]
		if !ok {
			return invalid("the detail " + strconv.Quote(k) + " is not one the reason " + string(q.Reason) + " carries")
		}
		if !check(v) {
			return invalid("the detail " + strconv.Quote(k) + " does not have the form its reason states")
		}
	}
	if q.State != "" && q.State != QuarantineHeld {
		return invalid("a quarantined file is recorded held")
	}
	if err := ValidateName("receiver", q.Receiver); err != nil {
		return err
	}
	if q.ReceptionID != "" {
		if err := master.ValidateID("reception id", q.ReceptionID); err != nil {
			return err
		}
	}
	if err := q.Caller.Validate(); err != nil {
		return err
	}
	if q.Size < 0 {
		return invalid("a file's size is not negative")
	}
	if !q.KeptSHA256.IsZero() && q.Reason != QuarantineDifferentFile {
		return invalid("a kept checksum is recorded for a different file only")
	}
	if q.Reason == QuarantineDifferentFile && (q.KeptSHA256.IsZero() || q.SHA256.IsZero()) {
		return invalid("a different file is recorded with both checksums")
	}
	for _, u := range []struct{ what, v string }{
		{"SOP Class UID", q.SOPClassUID}, {"SOP Instance UID", q.SOPInstanceUID}, {"Study Instance UID", q.StudyUID},
		{"Series Instance UID", q.SeriesUID}, {"Transfer Syntax UID", q.TransferSyntaxUID},
	} {
		if u.v != "" {
			if err := validateUID(u.what, u.v); err != nil {
				return err
			}
		}
	}
	if q.ContentKept != (q.Incoming != "") {
		return invalid("a quarantined file whose content is kept says where it is, and only then")
	}
	if q.Incoming != "" {
		if err := validateIncoming(q.Incoming); err != nil {
			return err
		}
	}
	if q.Reason == QuarantineTooLarge && q.ContentKept {
		return invalid("a file too large is never kept")
	}
	if q.Reason == QuarantineIncomingLost && q.ContentKept {
		return invalid("a file whose incoming copy is lost has no bytes to keep")
	}
	if q.Reason == QuarantineHeaderTooLarge && !q.ContentKept {
		return invalid("a file whose header is too large to read is kept, so a person can decide on it")
	}
	if q.ReleasedFrom != "" {
		if err := master.ValidateID("released-from id", q.ReleasedFrom); err != nil {
			return err
		}
	}
	if q.Import != nil {
		if q.ReleasedFrom != "" {
			return invalid("a file an import hands in is not a released one")
		}
		if err := q.Import.Validate(); err != nil {
			return err
		}
	}
	return master.ValidateTime("quarantine time", q.Quarantined)
}

// ValidateQuarantineBatch returns an Invalid error unless the batch is within
// MaxQuarantineBatch and every record is valid.
func ValidateQuarantineBatch(files []QuarantinedFile) error {
	if len(files) > MaxQuarantineBatch {
		return invalid("at most 1000 quarantined files are recorded at once")
	}
	for _, q := range files {
		if err := q.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Receivable reports whether the quarantined file carries what a received
// file needs (every UID valid, a checksum and its place in the incoming
// area), so a release can put it into a reception of its study. A file
// that is not receivable joins no reception when released, and is checked
// again by its receiver's rules as a file that just arrived.
func (q QuarantinedFile) Receivable() bool {
	for _, u := range []string{q.SOPInstanceUID, q.SeriesUID, q.StudyUID, q.SOPClassUID, q.TransferSyntaxUID} {
		if !uid.Valid(u) {
			return false
		}
	}
	return !q.SHA256.IsZero() && q.Incoming != ""
}

// JoinsAt reports whether a release of q through a receiver that
// de-identifies (deidentifies) or not puts it into a reception of its
// study as a received file: when it is receivable, unless it was recorded
// as received and the receiver de-identifies now. Such a record holds the
// original's UIDs and checksum, which a row of a receiver that
// de-identifies must never hold; it joins nothing, and the pipeline checks
// its bytes again as a file that just arrived, which records it remapped
// and keyed, and so de-identifies it. A record made de-identified is
// released only through a receiver that de-identifies (ReleasableAt) and
// joins with its flag; a receiver changed to archive as received after
// the release holds it (deid_failed, no_profile), never archives it as it
// was.
func (q QuarantinedFile) JoinsAt(deidentifies bool) bool {
	return q.Receivable() && (q.Deidentified || !deidentifies)
}

// ReleasableAt returns a precondition failure when q may not be released
// through a receiver that de-identifies (deidentifies) or not, nil
// otherwise: a record made at a receiver that de-identifies is released
// only through one that does. Released through one that archives as
// received, a file that joins no reception (a header past the bound, not
// DICOM, a placing UID missing) would land again under it and be archived
// as it was received, identified.
func (q QuarantinedFile) ReleasableAt(deidentifies bool) error {
	if q.Deidentified && !deidentifies {
		return Precondition("a file held at a receiver that de-identifies is released only through one that does")
	}
	return nil
}

// ReleaseAllowed returns a precondition failure when rel may not release q
// through a receiver that de-identifies (deidentifies) or not, nil
// otherwise: q is not releasable there (QuarantinedFile.ReleasableAt), or
// rel names a profile version and the receiver archives as received (a
// version named replaces a profile; it never turns de-identification on,
// since the receiver's choice decided how the file landed).
func ReleaseAllowed(q QuarantinedFile, rel Release, deidentifies bool) error {
	if err := q.ReleasableAt(deidentifies); err != nil {
		return err
	}
	if !rel.Profile.IsZero() && !deidentifies {
		return Precondition("a release names a profile version only through a receiver that de-identifies")
	}
	return nil
}

// HeldBytes is what a record adds to the quarantine's held bytes: its size
// while its content is kept, nothing otherwise.
func (q QuarantinedFile) HeldBytes() int64 {
	if q.State == QuarantineHeld && q.ContentKept {
		return q.Size
	}
	return 0
}

// QuarantineFilter narrows ListQuarantined. A zero field keeps every record.
type QuarantineFilter struct {
	State          QuarantineState
	Reason         QuarantineReason
	Receiver       string
	ReceptionID    string
	StudyUID       string
	SOPInstanceUID string
	// Since and Until bound the quarantine time: at or after, and before.
	Since, Until time.Time
}

// Validate returns an Invalid error when f cannot be applied.
func (f QuarantineFilter) Validate() error {
	switch f.State {
	case "", QuarantineHeld, QuarantineReleased, QuarantineRejected:
	default:
		return invalid("a quarantine state is held, released or rejected")
	}
	if f.Reason != "" && !f.Reason.Valid() {
		return invalid("a quarantine reason is one of the contract's codes")
	}
	if f.Receiver != "" {
		if err := ValidateName("receiver", f.Receiver); err != nil {
			return err
		}
	}
	if f.ReceptionID != "" {
		if err := master.ValidateID("reception id", f.ReceptionID); err != nil {
			return err
		}
	}
	for _, u := range []struct{ what, v string }{{"Study Instance UID", f.StudyUID}, {"SOP Instance UID", f.SOPInstanceUID}} {
		if u.v != "" {
			if err := validateUID(u.what, u.v); err != nil {
				return err
			}
		}
	}
	return validateRange(f.Since, f.Until)
}

// ValidateRelease returns an Invalid error unless the arguments of
// ReleaseQuarantined are well formed.
func ValidateRelease(id string, mode ReleaseMode, receiver, receptionID string, now time.Time) error {
	if err := master.ValidateID("quarantine id", id); err != nil {
		return err
	}
	if mode != ReleaseAsIs {
		return invalid("a release's mode is as_is")
	}
	if receiver != "" {
		if err := ValidateName("receiver", receiver); err != nil {
			return err
		}
	}
	if err := master.ValidateID("reception id", receptionID); err != nil {
		return err
	}
	return master.ValidateTime("release time", now)
}
