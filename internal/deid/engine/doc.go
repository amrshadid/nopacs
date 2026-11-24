// Package engine de-identifies a DICOM data set by a profile: a pure
// function from a data set, an expanded profile version and a tenant's
// keys to the data set changed in place and a record of what was done to
// each element and why.
//
// The data set is the DICOM library's (go-dicom's dataset.Dataset), read
// and changed only through its exported calls; this package parses and
// encodes no DICOM. The profile comes from the profile language
// (internal/deid/profile) through one small boundary, Decider: the
// decision for an element (the action with its arguments, the rule, its
// origin and reason, the VRs its selector was checked for), the set rules
// and the templates.
//
// Every element is visited: the top level, every item of every sequence
// (to MaxDepth), every private block, whose creator is resolved in the
// data set the element is in and compared exactly once its padding is
// removed. A private creator element follows its block: it is kept when an
// element of its block is, and removed otherwise. A content item is any
// item with a Value Type (0040,A040), wherever it sits, and its elements
// carry its concept name and value type to the profile's content item
// rules. Some elements are treated by fixed rules whatever the profile
// says, the ones the profile language refuses to name, from the one list
// both read (profile.Pipeline): the file meta information, group lengths,
// command elements (group 0000), the directory elements of group 0004,
// items, digital signatures, trailing padding, the Original Attributes,
// Encrypted Attributes and MAC Parameters Sequences, and every element of
// group 7FE0 but the pixel data and the table of its frames are removed
// (PS3.15 E.1.1); the pixel data is kept, since only pixel rules change it
// and this engine applies none; and Specific Character Set (0008,0005) is
// removed at every depth and written once at the top level after every
// change: ISO_IR 192 when any text left is outside the default
// repertoire, none otherwise. The library holds the text it decoded in
// UTF-8; text under another declaration, or that is not UTF-8, refuses the
// file.
//
// The floor (floor.go). The attributes that name or identify the person
// (Patient's Name and ID, other IDs, birth date and time, addresses,
// residence and telephone numbers, and every person's name: an element of
// VR PN, a standard attribute the data dictionary gives VR PN whatever VR
// the file wrote, and a private value of unknown VR that reads as a name)
// are never kept, whatever the profile decides: an action that would keep
// one becomes remove-unless-required with a dummy. Their values are
// needles whenever they are removed or changed.
//
// Needles (needles.go). A value a rule removes or changes is a needle,
// kept in memory for clean and the check after, only when it can identify
// someone by itself: a person's name, an identifier (an attribute of the
// floor, or of the rows of PS3.15 Table E.1-1 whose value identifies a
// person, a place, a device, an order or a study; a GPS coordinate; an
// attribute the data dictionary does not know; any text a rule hashes or
// looks up), a date, a UID the standard does not register. A time of day,
// a number, a coded string, a description or comment, and a private value
// but a name, a date or a UID, are removed as the profile says and are no
// needle: a needle that identifies no one makes false holds on ordinary
// words and numbers, and a name typed into a description is found through
// the name's own needle. Of a sequence removed whole, the values the
// profile or the floor would remove where they are, by the same classes;
// of one covered whole (the floor's, the identification code sequences,
// the issuers' sequences), at any depth, every value but a number, a code,
// a time or a coding scheme's name. A date of only a year, or a year and
// month, is matched only as a date of that precision, never as the year of
// a full date. A date of only zeros is removed as its rule says and is no
// needle: it identifies no one, and is found inside ordinary decimals.
// What a person's rule chose by a pattern is an identifier needle too: each
// distinct text a value pattern matched in a value its rule removes or
// changes, and each distinct text redact cuts (redact.go), searched for
// whole, and as a whole word when it begins or ends with a letter, so a
// copy of a redacted MRN kept elsewhere in the file holds it. A value a
// value pattern would read and cannot (too long, not in memory, holding
// an undecoded escape), and one with more distinct matches than the bound,
// refuse the file: none is passed unread or with a needle dropped.
//
// Redact (redact.go) cuts the matches of its pattern out of a text value,
// part by part and a name component by component, again until none is
// left, and refuses the file when the value left is not one of its VR or
// the matches pass their bound. On an element the floor covers it is
// lowered like keep, since it keeps what it does not cut.
//
// Clean removes the file's needles from free text: each value, each part
// of a name (of one or two letters only as a whole word; of a script
// written without spaces at any length) and each date in its common
// written forms, in any case, rune by rune, again until nothing more is
// found. Its work for a file is bounded, past which the file is refused.
//
// Values of VR UN (unknown.go). A UN value that begins with an item is a
// sequence the DICOM library kept as bytes (PS3.5 6.2.2): it is parsed
// (network.DecodeUNSequence) within the engine's own bounds, its text
// decoded into UTF-8 by the character set the file was written in, which
// the caller names (Reception.CharacterSet), and walked like any sequence;
// a kept one is written back as VR SQ. One that is not items, or whose
// text cannot be decoded, and a UN value not in memory, are never kept:
// each refuses the file. Text holding an ISO 2022 escape, which no decoder
// turned into UTF-8, is never kept or cleaned. Under the Retain Safe
// Private base, a kept private element held as UN that PS3.15 Table
// E.3.10-1 lists takes the table's VR when its value is what that VR says
// and is not taken for a name (keepSafePrivate). A stored version runs
// only with the Annex E bases of this build's edition (New).
//
// Keyed actions. A UID is remapped to "2.25." and a version 8 UUID from
// HMAC-SHA-256 of it under the tenant's UID key (RemapUID); a UID the
// standard registers is kept (Registered), the public coding schemes of
// PS3.16 Table 8-1 among them (SNOMED CT, LOINC, UCUM, RadLex). The UID
// of a vocabulary (Coding Scheme UID, Context Group Extension Creator
// UID, Context UID, Mapping Resource UID: vocabularyUIDTags) that is not
// registered is local, mostly the institution's own root, so it is
// remapped even where the profile would keep it. A date is moved back by the
// subject's shift, from 1 to the profile's most days, keyed by the
// tenant's date key over the subject identity (the Patient ID with its
// issuer, or with the reception's namespace, Reception.Namespace, when the
// file names none: naming.SubjectIdentity), exactly the bytes the subject
// label's code is the keyed hash of (ShiftDays), so a subject has one
// shift; the time of day is kept, and the shift is written nowhere. A job
// over an archive, whose files no longer hold the Patient ID, gives the
// subject itself (Reception.Subject, the instance's subject label), which
// keys the shift in its place; a reception never does. A
// year, or a year and month, that the shift leaves as it was is emptied.
// A hash and a template's "{#X}" are the naming rule's keyed hash of the
// value (naming.HashText) under the tenant's naming key; of the top-level
// Patient ID, of the subject identity, so a hashed Patient ID, and its
// lookup code, follow the subject and its issuer, and equal the default
// subject label's code.
//
// Fails closed. Everything is worked out before the data set is changed:
// a file the profile cannot process returns a *Failure with a fixed Cause,
// and the data set is left as it was, never half processed; a panic is
// recovered as a Failure, the changes made so far undone; a lookup table
// assigns a code only once the plan has succeeded. A value an action
// writes is checked against its VR (PS3.5 6.2) first.
//
// What happens to an element whose VR in the file is not one its rule was
// checked for (the VRs validation stored with the rule, never this
// build's dictionary): keep, remove, empty, dummy and the IOD-dependent
// removal apply to any VR; an action that reads or writes the value
// refuses the file. An element the file holds as UN is treated as the
// data dictionary's VR for a standard tag.
//
// The action that removes an element unless its IOD requires it reads the
// attribute's Type in the IOD of the file's SOP class from the DICOM
// library's IOD tables (Types, IODTypes). PS3.15 Table E.1-1's codes are
// not resolved here: the profile language turns them into actions when it
// builds the Annex E bases, so there is one resolution of each.
//
// What a stored profile version does depends on its expanded form and on
// the DICOM library the engine runs with: its data dictionary (the VR of
// an element a file holds as UN, which attributes are names), its IOD
// tables (remove-unless-required) and its UID registry (which UIDs stay).
// So the library's version is part of what a version meant for a file,
// and each file records it, with this build's, in De-identification
// Method (0012,0063).
//
// The package does no I/O but through the Lookups the caller passes, reads
// no clock and no random source: the same file under the same profile and
// keys always comes out byte for byte the same.
package engine
