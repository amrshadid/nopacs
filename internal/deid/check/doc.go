// Package check decides whether a de-identified file still holds any of the
// values de-identification took out of it. It is a verdict, never an edit:
// it reads the file and changes nothing.
//
// The values searched for are the engine's needles (engine.Needles): the
// values a profile removed or replaced that can identify someone by
// themselves (names, identifiers, dates, UIDs; never a time of day, a
// number but a GPS coordinate, a coded string or a description), and the
// values of the attributes that name or identify the person, which the
// engine never keeps (PS3.15 Table E.1-1). The file is read by the DICOM library's codec-free reader
// (filereader/attributes), a different path from the data set the engine
// changed, so a value one reader hides from the other is still seen. A
// file the reader did not read whole (truncated, a sequence or item cut
// short, sequences nested past the reader's deepest) is not judged.
//
// Two searches of the file, each one pass for every form at once (an
// Aho-Corasick automaton):
//
//   - the text: every element's value decoded under its data set's
//     Specific Character Set as the library decodes it (an item without
//     one takes its parent's): the file meta information, private
//     elements and every item of every sequence at any depth, the top
//     level's Pixel Data excepted; a value that may be text the file did not say was text
//     (VR UN, OB, or none) and the preamble also as UTF-8 and under the
//     character set the file was received in;
//   - the raw bytes of the whole file but the value of the top level's
//     Pixel Data, Float Pixel Data and Double Float Pixel Data (the
//     preamble and the file meta information included), for each written
//     form encoded in UTF-8 and in the received character set (all of it,
//     and each of its character sets alone), its ISO 2022 escape sequences
//     taken out: what a parser could miss, such as text in a binary value.
//
// An element of group 7FE0 inside a sequence item is a finding whatever
// it holds (an identifier, raw): de-identification removes pixels there
// (an icon, a vendor's thumbnail), an image no pixel rule filled, so one
// found is never passed.
//
// Each needle is searched for in the forms people write it in, the ones
// the engine's clean removes (engine.NameForms, engine.DateForms), so the
// two cannot drift apart: a person's name whole, by component and word
// (PS3.5 6.2.1) and as "JOHN DOE", "DOE, JOHN" and the like; a date in its
// numeric forms, compact, with the month's name, as an ordinal, in
// Chinese, Japanese and Korean; a UID whole. Text is folded before it is
// compared (engine.Fold: letter case, and full-width ASCII as ASCII), and
// runs of white space are one space. Each form is held to its edges
// (engine.Edge): a form beginning with a digit is not matched after a
// digit, and one ending with a digit not before one, so an identifier is
// not found inside a longer number such as a remapped UID; a full date may
// be followed by digits, as by a time; a name's form holding a part of one
// or two letters only as a whole word, so LE^AN is not in "SCAN LEFT"; an
// ambiguous date form only standing alone as a date, though a run that
// reads as the date ("5.3.24" in "version 5.3.24") is taken for it; a UID
// not as the end of a longer UID, but as its start, since a UID made by
// appending to the original shows the original. UIDs the standard defines
// identify no one and are not searched for.
//
// A form shorter than Options.MinLength characters is skipped, since it
// matches inside ordinary words, and counted. A form in a script written
// without spaces between words (Han, Hiragana, Katakana, Hangul) is judged
// by Options.MinUnspaced instead, one character by default, as the engine
// removes such a name part from free text at any length. In the raw bytes
// such a form is searched for at any length too, but not in the value of a
// standard element of a binary VR (a LUT, an overlay, an encapsulated
// document), whose numbers spell any two or three bytes by chance; a
// private binary value, where vendors put text, is searched. Text in a
// 7-bit set of two-byte characters (ISO 2022 IR 87 and IR 159), whose
// bytes are ASCII letters (林 is "NS"), is matched in the raw bytes only
// inside a run the file shifted into the set by its escape sequence.
//
// Text is compared in Unicode Normalization Form C (UAX #15, profile.NFC),
// as the engine's clean and value patterns compare it: a name written with
// a combining mark ("e" and U+0301) is found for a needle written composed
// ("é"), and the reverse. In the raw bytes, which no reader normalized,
// each form is searched for in UTF-8 both composed and decomposed (NFD).
// A match in the raw bytes that a following combining mark composes with
// in every received character set ("RENE" then U+0301, "RENÉ" in NFC) is
// another word in NFC, as clean judges it, and is not a finding.
//
// The result names where a value was found (the element's path, as the
// engine's record writes it: tags as eight hexadecimal digits, item
// numbers in brackets, "preamble" for the first 128 bytes) and what kind of
// value it was (name, identifier, date, UID), never the value, so it can
// be the detail of a quarantine and go into a log or an event.
//
// Bounded: a file larger than Options.MaxFileBytes, a value longer than
// Options.MaxValueBytes, more needles than Options.MaxNeedles, a needle
// longer than Options.MaxNeedleBytes, or written forms of more than
// Options.MaxFormBytes together (as text and in every encoding) is refused
// with a fixed cause, never passed. The work is a step or two of an
// automaton for each byte of the text and of the file, plus what a fixed
// budget allows, the same whatever the size: one unit for each form ending
// at a position, and for each rune of a form checked against a received
// character set before it is written in it (the library's encoder takes
// the square of a text's length when it cannot write it); past the
// budget, a second or two of work, the file is refused as too large.
//
// The file is never held whole. The reader holds its elements with their
// values in place (text values, and binary values of at most 1 KiB; the
// image data and longer binary values stay in the file), about the header
// the engine held. The text is searched one value at a time; a value of
// unknown kind longer than the 256 KiB decoded under a character set is
// read as a stream of UTF-8. The raw bytes are read a window of 1 MiB at a
// time, each window beginning with the end of the one before (the longest
// raw form and a few bytes for its edges), so a form across the edge
// between two windows is found whole in the second, and an ISO 2022 run is
// followed from one window into the next. The memory of a check is a
// window, one value (at most Options.MaxValueBytes, a few copies of it as
// it is decoded and folded, counted at up to three bytes of UTF-8 for each
// byte that is not ASCII), and the automata, counted at 64 bytes for each
// byte of the folded forms and of the raw forms as written (together at
// most MaxFormBytes): about 1.1 MiB for an image whatever its size.
// Options.Memory is charged that (Checker.reservation), once the raw forms
// are written.
package check
