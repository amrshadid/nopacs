// Package label decides whether a string may be a subject, session or scan
// label. A label is a name for people and a segment of a storage key, and on
// a directory storage it becomes a directory's name, so a label this package
// accepts must be a safe single path segment on Linux, macOS and Windows,
// whichever file system the storage happens to sit on.
//
// It is a package of its own because every other part of the core that
// handles a label (the naming rule, which makes labels, and the layout,
// which puts them in keys) applies the same rule, and one rule written once
// cannot drift between them.
//
// # The rule
//
// A label is 1 to 128 bytes of A-Z, a-z, 0-9, ".", "_" and "-"; it does not
// start with a dot (a hidden file on Unix, and "." and ".." are the current
// and parent directory), does not end with a dot (the Windows shell and user
// interface drop a trailing period, so "a." and "a" would be one directory),
// and is not a Windows reserved device name such as CON or COM1, in any case,
// alone or followed by an extension. The reserved names and the trailing
// period are from Microsoft's "Naming Files, Paths, and Namespaces"
// (learn.microsoft.com/windows/win32/fileio/naming-a-file).
//
// A value that breaks the rule is refused with the fault named. It is never
// cleaned up: a label changed silently would put data where nobody asked
// for it, and two different values could be cleaned into the same label.
//
// # Case
//
// Two labels that differ only in case, "Sub01" and "sub01", are different
// labels here and on Linux and S3, but one directory on the default file
// systems of macOS and Windows: the second would be written into the first.
// This package does not refuse either, because it sees one label at a time.
// Fold gives the form two labels share when they would collide, so the
// catalog, which knows the labels already held at a level, can refuse a new
// label whose folded form is held by a different one.
//
// # What this package must never do
//
// It must never do I/O, read the clock, or import anything of NoPACs'
// outside the core: it is part of the core (.golangci.yml, depguard "core").
// It must never put the refused value in an error's text: before
// de-identification a value can be a patient's identifier, and an error's
// text reaches logs. The value stays in the error's Value field for the
// caller that is entitled to it.
package label
