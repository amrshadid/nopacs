// Package naming is a receiver's naming rule: how the subject, session and
// scan labels of a new scan are made from the attributes of its first file.
//
// It is a package of its own because a rule has a written form that a person
// sets on a receiver and the server stores, and that form is parsed and
// checked when it is set, not when a file arrives: a mistake in a rule is
// refused at the command line instead of sending a whole study to
// quarantine. The parsed rule is then applied to every new scan.
//
// # The written form
//
// A rule is three templates, one per label, and optionally one pattern.
//
// A template is literal text and placeholders in braces. A placeholder names
// a DICOM attribute by keyword, {PatientID}, or by tag, {(0010,4000)}, or a
// part taken out by the pattern, {subject}. Any of them after '#',
// {#PatientID}, gives the keyed hash of the value instead of the value:
// HashLength characters that say nothing of the value to anyone without the
// tenant's key (HashText defines them). The key is a secret the caller
// holds; it passes a Hasher to Rule.Labels. Keywords start with an upper
// case letter and part names with a lower case one, so the two never clash.
// Only a short, explicit list of keywords is known (Keywords): the
// attributes a naming rule plausibly uses. Any other attribute is named by
// its tag. Literal text may hold only the characters a label may hold, so
// "{" and "}" need no escape.
//
// A pattern takes named parts out of one attribute's value, which is how
// sites tell a scanner where data goes today: the operator types
// "Proj^Sub01^Visit2" into Patient Comments, and the pattern
// ^(?P<project>[^^]+)\^(?P<subject>[^^]+)\^(?P<visit>[^^]+)$ gives three
// parts the templates use. The pattern must match the whole value.
//
// Parts may also be given from outside the file's attributes: an import
// reads named parts from each file's path, and its map may give one more
// (ParseWithParts, LabelsWithParts). A rule names the parts it may be
// given when it is parsed, so a template using a part nobody gives, or a
// part both the pattern and the import would fill, is refused when the
// rule is set.
//
// The default rule is subject sub-{#PatientID}, session
// ses-{#StudyInstanceUID}, scan scan-{#SeriesInstanceUID}: by default no
// label, and so no storage key, list or event, carries an identifier the
// sender gave. A site that wants readable labels writes its own rule, and
// its labels are then exactly as identifying as that rule. A default leans
// only on attributes every instance must carry with a value (PS3.3 Type 1),
// so a well-formed file is never quarantined for a date or a number it may
// leave empty. Patient ID, Type 2, is the one exception: a subject's label
// has to come from somewhere, and a file naming no patient is held for a
// person.
//
// # Attributes
//
// The rule reads attributes through Attributes, an interface the caller
// implements over the data set it decoded. This package does not parse
// DICOM. The caller gives each value as text with the padding the standard
// says is not part of the value removed (PS3.5 6.2: the trailing space or
// NUL that makes a value's length even, and the leading and trailing spaces
// of a number). Nothing else is done to a value: a missing or empty
// attribute, a value holding a character a label may not hold, or a result
// that is not a valid label is an error naming the label, the attribute and
// the fault, and the caller quarantines the file. A hashed value may hold
// any character, since only its hash reaches the label, but it too must be
// present and not empty: the empty string hashes as well as any value, and
// would make every file without the attribute one subject. A label is never
// cleaned up silently, because two different values could be cleaned into
// one label and their data mixed.
//
// # Hostile input
//
// A pattern is a Go regular expression, which runs in time linear in the
// length of its input (RE2 semantics, package regexp), so no value can make
// it take exponential time. The pattern is still bounded to MaxPatternLength
// bytes and the value it reads to MaxValueLength bytes, refused beyond that,
// so a rule and a file together bound the work done for one file. A
// template is bounded to MaxTemplateLength bytes.
//
// # What this package must never do
//
// It must never do I/O, read the clock, or import anything of NoPACs'
// outside the core (.golangci.yml, depguard "core"). It must never put an
// attribute's value in an error's text: before de-identification a value
// can be a patient's name or identifier, and an error's text reaches logs.
package naming
