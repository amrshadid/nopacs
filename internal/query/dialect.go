package query

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The dialects of the two databases. They write SQL text only and import
// no database library: a driver passes its own to a Compiler, and the
// package's tests run the SQL they write against each database itself.
var (
	// Postgres writes the text matches Postgres serves, over text columns
	// of COLLATE "C" (byte order, which a btree serves for a LIKE prefix)
	// with a pg_trgm GIN index on each folded column that has a trigram
	// index.
	Postgres Dialect = postgresDialect{}
	// SQLite writes the text matches SQLite serves, over text columns of
	// the BINARY collation with an FTS5 table of the trigram tokenizer
	// (an external content table keyed by the search table's rowid) for
	// each folded column that has a trigram index.
	SQLite Dialect = sqliteDialect{}
)

// postgresDialect writes LIKE with ESCAPE '\' for every text match: a
// btree on a column of COLLATE "C" serves a prefix, pg_trgm's GIN index a
// pattern or a term inside percent signs.
type postgresDialect struct{}

// Placeholder returns $n.
func (postgresDialect) Placeholder(n int) string { return "$" + strconv.Itoa(n) }

// TextMatch returns a LIKE of the match's pattern.
func (postgresDialect) TextMatch(m TextMatch, bind func(any) string) string {
	var p string
	switch m.Op {
	case OpPrefix:
		p = EscapeLike(m.Text) + "%"
	case OpLike:
		p = GlobToLike(m.Text)
	case OpContains:
		p = "%" + EscapeLike(m.Text) + "%"
	}
	return m.Column + " LIKE " + bind(p) + ` ESCAPE '\'`
}

// TrigramServes reports whether the text holds a run of three letters or
// digits: pg_trgm makes its trigrams from the words of a pattern, a word
// being a run of letters and digits, and a LIKE pattern whose words are
// all shorter than three gives no trigram, which the GIN index answers by
// reading every entry it holds (pg_trgm, "Index Support").
func (postgresDialect) TrigramServes(text string) bool {
	run := 0
	for _, r := range text {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			run = 0
			continue
		}
		if run++; run >= MinContains {
			return true
		}
	}
	return false
}

// sqliteDialect writes SQLite's text matches. Its LIKE ignores the case of
// ASCII letters and, with an ESCAPE clause, is served by no index, so no
// match here is a LIKE: a prefix is a range of the column, which a btree
// on a BINARY column serves whatever characters the prefix holds; an exact
// pattern is a GLOB (case-sensitive), which only rechecks rows another
// match found. A contains goes through the column's FTS5 trigram table,
// its term quoted as an FTS5 string, and is rechecked on the folded column
// itself: FTS5 folds case by its own table (Unicode's simple case folding)
// at index and query time, which may join two characters Fold keeps apart,
// and the recheck keeps the answer the one routing gives. A pattern on a
// folded column goes through the same table's GLOB, which the trigram
// tokenizer serves and checks on the text it reads.
type sqliteDialect struct{}

// Placeholder returns "?".
func (sqliteDialect) Placeholder(int) string { return "?" }

// TextMatch returns the match in the form SQLite's indexes serve.
func (sqliteDialect) TextMatch(m TextMatch, bind func(any) string) string {
	switch {
	case m.Op == OpContains:
		return "(" + m.Row + " IN (SELECT rowid FROM " + m.Trigram + " WHERE " + m.Trigram + " MATCH " +
			bind(`"`+strings.ReplaceAll(m.Text, `"`, `""`)+`"`) + ") AND instr(" + m.Column + ", " + bind(m.Text) + ") > 0)"
	case m.Op == OpLike && m.Folded:
		return m.Row + " IN (SELECT rowid FROM " + m.Trigram + " WHERE " + m.Name + " GLOB " + bind(globPattern(m.Text)) + ")"
	case m.Op == OpPrefix:
		lo, hi, bounded := PrefixRange(m.Text)
		if !bounded {
			return m.Column + " >= " + bind(lo)
		}
		return "(" + m.Column + " >= " + bind(lo) + " AND " + m.Column + " < " + bind(hi) + ")"
	}
	return m.Column + " GLOB " + bind(globPattern(m.Text))
}

// TrigramServes reports whether the text holds three characters: the FTS5
// trigram tokenizer indexes every run of three characters, whatever they
// are, and a MATCH of fewer returns no row without an error (FTS5, "The
// Trigram Tokenizer").
func (sqliteDialect) TrigramServes(text string) bool {
	return utf8.RuneCountInString(text) >= MinContains
}

// PrefixRange returns the range of text that starts with p, in the order
// of its UTF-8 bytes, which is the order of its code points: lo is p, and
// hi is p with its last character replaced by the one after it, so every
// text t with lo <= t < hi starts with p and every text that starts with p
// is in the range. When p is only U+10FFFF characters, the last there is,
// no text is above the range and bounded is false. A btree serves the
// range on any column ordered by bytes, whatever characters p holds; a
// GLOB or LIKE prefix that starts with a character special to it is not
// served.
func PrefixRange(p string) (lo, hi string, bounded bool) {
	rs := []rune(p)
	for i := len(rs) - 1; i >= 0; i-- {
		next := rs[i] + 1
		if next == 0xD800 {
			// The surrogates are not characters; UTF-8 has no encoding
			// for them.
			next = 0xE000
		}
		if next <= unicode.MaxRune {
			return p, string(rs[:i]) + string(next), true
		}
	}
	return p, "", false
}

// globPattern returns a pattern of "*" and "?" as a GLOB pattern: the two
// wildcards kept, "[" made literal.
func globPattern(p string) string { return strings.ReplaceAll(p, "[", "[[]") }
