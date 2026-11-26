// Package query is the condition language a receiver's routing and a
// tenant's search share: one parser, one tree, and two ways to use it.
//
// A condition is text such as
//
//	Modality in ("CT", "MR") and SeriesDescription contains "t1"
//	  and not (0029,"SIEMENS CSA HEADER",10) present
//
// Parse turns it into a tree (Node) within fixed bounds (MaxLength,
// MaxDepth, MaxLeaves, MaxListValues, MaxValueLength), each refusal a
// *SyntaxError that names the character where the text went wrong; Print
// writes a tree back in its one canonical form, and Parse of that form
// gives the same tree.
//
// Routing evaluates a tree over one file's data set as go-dicom gives it
// (Eval). The result has three values: true, false, and unknown, which a dry
// run over de-identified headers gives for an attribute the profile removed,
// so a dry run never claims a result it cannot know (Kleene's logic: an
// "and" with a false child is false whatever the others are). An attribute
// the file does not hold makes a comparison false, never an error.
//
// Search compiles a tree to the WHERE clause of a query over the search
// tables (Compile). Every value is a bound parameter and every column name
// comes from the Registry, never from the query's text, so no query text
// reaches SQL. A query no index serves is refused, naming the attribute to
// index. What differs between databases is behind Dialect; this package
// holds the two (Postgres, SQLite), which write SQL text only, and each
// database driver passes its own to the compiler, so the SQL the package's
// tests run against each database is the SQL the drivers run.
//
// A QIDO-RS search (PS3.18 8.3.4, matching per PS3.4 C.2.2.2) maps onto the
// same tree (FromQIDO), so a search through DICOMweb and one through the
// archive's own API mean the same thing.
package query
