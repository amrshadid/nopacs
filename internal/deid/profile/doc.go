// Package profile is the language of de-identification profiles: a profile
// as data, its checks, its expansion into a frozen version, and the
// precedence by which one rule decides what happens to each element.
//
// A profile is the API's ProfileDocument, and its file form is that
// message's protobuf JSON (ParseDocument, FormatDocument), so a file, a
// request and a stored document are one thing. A document names bases,
// profiles the binary ships (Catalog, Shipped), and then its own rules: a
// selector, conditions and an action each, and pixel rules for images
// whose pixels may carry text.
//
// Checks. Expand checks every field of a document and answers every
// problem at once (Problems), each naming the rule by its position and
// name, the field by its path in the JSON names, and a fixed code a
// program can test. Nothing that cannot work is stored: a keyword the data
// dictionary does not know, a path through an element that is not a
// sequence, a regular expression that does not compile, an action that
// does not fit the VR its selector selects (a UID remapped in a date, a
// date shifted in a name), a constant that is not a value of its VR (PS3.5
// 6.2), a constant or template whose text is not printable ASCII or whose
// longest output is not a value of its VR, a template that reads a value
// as received, a regular expression whose compiled size passes its bound
// or the document's budget, a document whose version would not fit one
// answer, a base that is not available. The elements the pipeline handles
// in every file are refused and never selected (Pipeline, the one list the
// engine acts by too): the file meta information, which is written anew
// for every file (PS3.10 7.1), command and directory elements, items and
// group lengths, group 7FE0, whose pixel data only pixel rules change and
// whose other elements are removed, Specific Character Set, written for
// the text the file keeps, and the sequences and padding removed from
// every file (PS3.15 E.1.1). There is no action that runs outside code or calls an address,
// since each would be a way out for the values a profile removes.
//
// Warnings (Warnings) say what a valid document does that its author may
// not mean: a rule a more specific base rule overrides for some elements,
// and a rule that keeps what a base changes or removes.
//
// Expansion. A version stores the document expanded: each base's rules
// copied in, its own bases first, then the document's rules; every keyword,
// in a selector, a path or a template, written as its tag; the defaults
// made explicit; with each rule the VRs validation decided for its
// selector, and with the whole the language edition whose meaning it has.
// Compile reads that form by its structure alone and never the data
// dictionary, so a later library cannot change or refuse a stored
// version. Its canonical text (Canonical) has a SHA-256, the version's
// checksum. So a stored version never depends on what a later build ships:
// a new edition of a base is a new base name, and the data dictionary of a
// later library cannot change which element a stored rule names. The same
// document expands to the same text, and so the same checksum, in every
// run.
//
// Precedence. Of the rules whose selector matches an element and whose
// conditions all hold, the most specific selector decides (Tier: a path or
// a content item, then a tag or keyword, a private element, a private
// creator's block, a group, a value pattern, a VR); between equally
// specific ones the later rule, so a document's rules win over its bases',
// and a later base over an earlier, as an option of PS3.15 Annex E
// overrides the basic profile (PS3.15 E.3). A group, value pattern or VR
// rule selects private elements only when it says include_private, and a
// content item rule only its item's value element (IsContentValue). A
// value pattern reads the element's text (Location.Value), up to
// MaxPatternValue bytes; redact cuts its matches out of a value and keeps
// the rest. Both belong to language edition 2; a profile that uses
// neither is written as edition 1, as before either existed. An element no rule decides is removed when it
// is private (PS3.5 7.8), and otherwise follows the profile's unnamed
// default. Program
// decides this for the engine; matching an element reads only a Location
// and the Facts the conditions need, never a file.
//
// Versions. A Version is immutable: a change is a new version with the next
// number. Load reads a stored version and refuses one whose expanded text
// does not match its checksum.
//
// The bases of PS3.15 Annex E come from the standard's tables, which the
// DICOM library holds and NoPACs does not copy: each column of Table E.1-1
// (anonymize.ProfileTable) is read by the one adapter that turns its codes
// into actions (annexe.go), and the Retain Safe Private option is Table
// E.3.10-1 (anonymize.SafePrivateEntries). Clean Pixel Data has no rules:
// its work is the pixel rules and the gate that holds an image at risk of
// burned-in text no pixel rule matches, which every profile has, so the
// base is accepted and adds nothing. Clean Recognizable Visual Features
// removes faces, which this build does not do, and is refused as not
// available. Each base's name carries the
// edition of its tables (AnnexEEdition), and the package's tests hold each
// base's checksum, so a base of one name never changes meaning.
//
// A stored version's meaning is its expanded form together with the DICOM
// library the engine runs it with: the engine reads the library's data
// dictionary, IOD tables and UID registry as it processes a file, and
// records the library's version in each file it makes.
//
// The package does no I/O and reads no clock and no random source, so a
// profile means the same thing on every server and in every test.
package profile
