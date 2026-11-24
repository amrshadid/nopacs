// Package importpath holds the rules of the paths an import reads: the
// rule of a path relative to an import root (Check), the order a walk
// visits paths in (Compare), when two jobs' paths read the same files
// (Overlap), and the layouts that read a file's path for the named parts a
// naming rule uses (Layout, Resolver).
//
// It is a package of its own, beside package naming, because its rules
// belong to the import alone and three places must agree on them: the
// handlers that check a job when it is made, the store that keeps a
// unit's cursor, and the worker that walks the tree. A cursor written in
// one order and compared in another would skip files or hand them in
// twice.
//
// # Paths
//
// A path is relative to its root, its segments joined by "/": each 1 to
// MaxSegmentLength bytes of UTF-8, none "." or "..", none holding "\", ":"
// or NUL (a separator or a drive on another system, or the end of a C
// string); no empty segment and no leading or trailing "/"; at most
// MaxPathLength bytes, so a store can keep it in an index. An entry a walk
// meets whose path is longer (CheckEntry) is skipped as too long, and
// recorded under the deepest of its ancestors that fits (Fitting). A path
// can hold what identifies a person (a folder named after a patient), so
// no error of this package holds a path or a segment's text: it names a
// segment by its number.
//
// # Layouts
//
// A layout is either plain, reading nothing of a path, or an RE2 regular
// expression over a file's path, matched whole, whose named groups are
// parts ({project}, {session}, {scan}) a naming rule's templates use as
// they use a header pattern's parts. One pattern describes any tree. A
// tree that does not hold every part a site's labels need (one with no
// subject folder, say) takes the missing part from a map: a table, read by
// package dirimport/mapping, that gives one part's value by the values of
// others. A path the layout does not match is skipped as outside the
// layout; a path whose key the map does not list is skipped as unmapped,
// so a folder the source system no longer knows is not brought back. The
// path and the map only name: which session and scan a file belongs to is
// still its UIDs.
//
// # What this package must never do
//
// It must never do I/O or import anything of NoPACs' outside the core
// (.golangci.yml, depguard "core"). Where a path leads through links is
// for the caller that opens it, segment by segment, without following any.
package importpath
