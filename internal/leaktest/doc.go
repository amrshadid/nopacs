// Package leaktest holds the seeded-identifier test: one test that builds the
// server and the command line from the tree, starts a real server, receives
// synthetic files whose identifiers are unmistakable, and then drives the
// API, the DICOMweb and download routes and the command line as masked keys
// of every grant level. It collects every surface a masked key or an
// operator's log can see (answers, errors, the server's log, the audit log,
// the event feed, the command line's output) and requires that no original
// identifier of a file appears on any of them, in its plain form or in a
// variant of it.
//
// The package has no code the binary runs: everything is in _test.go files.
// NOPACS_LEAKTEST_TREE names a module root to build from instead of this
// checkout, for a run against a changed copy. NOPACS_LEAKTEST_DUMP names a
// directory that receives every surface the run saw, in one file, to read.
package leaktest
