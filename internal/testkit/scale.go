package testkit

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The variables of the catalog's measurements at scale, which build a
// synthetic tenant of millions of instances and take from minutes to hours,
// so no make target that runs on every change sets them. docs/testing.md
// lists them.
const (
	// ScaleInstancesEnv is how many instances the catalog's scale tests
	// make (TestScaleCatalog in each database driver); unset, they skip.
	ScaleInstancesEnv = "NOPACS_SCALE_INSTANCES"
	// ScaleKeepEnv, set to anything, keeps the database a catalog scale
	// test made (a SQLite file, a Postgres schema) and logs where it is,
	// so its plans can be read again with sqlite3 or psql; the person who
	// set it removes it.
	ScaleKeepEnv = "NOPACS_SCALE_KEEP"
)

// ScaleInstances returns the instances ScaleInstancesEnv asks for, and
// skips tb when it is unset or under -short. A value that is not a
// positive number fails tb.
func ScaleInstances(tb testing.TB) int64 {
	tb.Helper()
	v := os.Getenv(ScaleInstancesEnv)
	if v == "" {
		tb.Skipf("%s is not set: the catalog is not measured at scale", ScaleInstancesEnv)
	}
	if testing.Short() {
		tb.Skip("millions of rows: skipped under -short")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		tb.Fatalf("%s is %q, not a positive number of instances", ScaleInstancesEnv, v)
	}
	return n
}

// ScaleKeep reports whether ScaleKeepEnv is set.
func ScaleKeep() bool { return os.Getenv(ScaleKeepEnv) != "" }

// Spread is the median, the 99th percentile and the longest of a set of
// timings, which a measurement reports instead of a mean that one slow
// probe would hide in.
type Spread struct {
	P50, P99, Max time.Duration
}

// SpreadOf returns the spread of d, which it sorts; zero for none.
func SpreadOf(d []time.Duration) Spread {
	if len(d) == 0 {
		return Spread{}
	}
	slices.Sort(d)
	at := func(q float64) time.Duration { return d[min(len(d)-1, int(q*float64(len(d))))] }
	return Spread{P50: at(0.50), P99: at(0.99), Max: d[len(d)-1]}
}

// String returns the spread in milliseconds, for a log.
func (s Spread) String() string {
	us := func(d time.Duration) string { return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 2, 64) }
	return "p50 " + us(s.P50) + " ms, p99 " + us(s.P99) + " ms, max " + us(s.Max) + " ms"
}

// ScaleSummaryEnv names a file a scale measurement appends its numbers to
// as a Markdown table, such as a CI job's step summary; unset, the table
// is only logged.
const ScaleSummaryEnv = "NOPACS_SCALE_SUMMARY"

// Summary collects a measurement's numbers, each logged as it is made, and
// writes them as one Markdown table at the end, so a run's results are
// read in one place rather than among its plans.
type Summary struct {
	tb    testing.TB
	title string
	rows  [][2]string
}

// NewSummary returns a summary titled title that writes itself when tb
// ends, whether it passed or not: a failed plan check still leaves the
// numbers measured before it.
func NewSummary(tb testing.TB, title string) *Summary {
	s := &Summary{tb: tb, title: title}
	tb.Cleanup(s.write)
	return s
}

// Add logs one measurement and keeps it for the table.
func (s *Summary) Add(what, value string) {
	s.tb.Helper()
	s.tb.Logf("%s: %s", what, value)
	s.rows = append(s.rows, [2]string{what, value})
}

// write logs the table and appends it to the file ScaleSummaryEnv names.
func (s *Summary) write() {
	var b strings.Builder
	b.WriteString("\n### " + s.title + "\n\n| Measurement | Value |\n|---|---|\n")
	for _, r := range s.rows {
		b.WriteString("| " + r[0] + " | " + r[1] + " |\n")
	}
	s.tb.Log(b.String())
	path := os.Getenv(ScaleSummaryEnv)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 G703 -- the file the person running the measurement named.
	if err != nil {
		s.tb.Errorf("%s: %v", ScaleSummaryEnv, err)
		return
	}
	if _, err := f.WriteString(b.String()); err != nil {
		s.tb.Errorf("%s: %v", ScaleSummaryEnv, err)
	}
	if err := f.Close(); err != nil {
		s.tb.Errorf("%s: %v", ScaleSummaryEnv, err)
	}
}
