package check

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/amrshadid/go-dicom/config"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// The defaults of Options.
const (
	// DefaultMinLength is the shortest form searched for in a script
	// written with spaces: a shorter one matches inside ordinary words.
	DefaultMinLength = 4
	// DefaultMinUnspaced is the shortest form searched for in a script
	// written without spaces, where a name of one character is a name.
	DefaultMinUnspaced = 1
	// DefaultMaxNeedles is the most needles one file is searched for, as
	// many as the terms the engine's clean takes for a file.
	DefaultMaxNeedles = 4096
	// DefaultMaxNeedleBytes is the longest needle, the longest value the
	// engine's clean reads.
	DefaultMaxNeedleBytes = 64 << 10
	// DefaultMaxFileBytes is the largest file read.
	DefaultMaxFileBytes = 1 << 30
	// DefaultMaxValueBytes is the longest text value read into memory; a
	// binary value longer than this stays in the file and is searched for
	// only in the raw bytes.
	DefaultMaxValueBytes = 16 << 20
	// DefaultMaxFindings is the most findings a report lists.
	DefaultMaxFindings = 16
	// DefaultMaxFormBytes is the most bytes of written forms, as text and
	// in every encoding, one file is searched for: the automata take some
	// 20 bytes of memory a byte of them, so a few tens of megabytes at
	// most. Real needles take a few kilobytes.
	DefaultMaxFormBytes = 2 << 20
)

// maxDepth is the deepest nesting of sequences read: as deep as the engine
// walks a data set.
const maxDepth = 64

// Options bound a check. A zero field takes its default.
type Options struct {
	// MinLength is the shortest form searched for, in characters, and
	// the shortest raw encoding, in bytes.
	MinLength int
	// MinUnspaced is the shortest form searched for in a script written
	// without spaces (Han, Hiragana, Katakana, Hangul), in characters.
	MinUnspaced int
	// MaxNeedles is the most needles a file is searched for.
	MaxNeedles int
	// MaxNeedleBytes is the longest needle, in bytes.
	MaxNeedleBytes int
	// MaxFileBytes is the largest file read.
	MaxFileBytes int64
	// MaxValueBytes is the longest value read into memory.
	MaxValueBytes uint32
	// MaxFindings is the most findings a report lists; Report.Total counts
	// them all.
	MaxFindings int
	// MaxFormBytes is the most bytes of written forms searched for, as
	// text and in every encoding together.
	MaxFormBytes int
	// Memory, when not nil, is the server's bound on de-identification's
	// memory, which what the search holds (Checker.reservation: a window
	// of the file, one value, the automata) is taken from once the file's
	// elements are read and before it is searched, and given back when the
	// check returns: a file that needs more than the whole bound is
	// refused (CauseTooLarge), and one that does not fit beside the others
	// waits for room until ctx ends. Nil bounds each file by MaxFileBytes
	// alone.
	Memory Memory
}

// withDefaults returns o with every zero field set to its default.
func (o Options) withDefaults() Options {
	if o.MinLength <= 0 {
		o.MinLength = DefaultMinLength
	}
	if o.MinUnspaced <= 0 {
		o.MinUnspaced = DefaultMinUnspaced
	}
	if o.MaxNeedles <= 0 {
		o.MaxNeedles = DefaultMaxNeedles
	}
	if o.MaxNeedleBytes <= 0 {
		o.MaxNeedleBytes = DefaultMaxNeedleBytes
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.MaxValueBytes == 0 {
		o.MaxValueBytes = DefaultMaxValueBytes
	}
	if o.MaxFindings <= 0 {
		o.MaxFindings = DefaultMaxFindings
	}
	if o.MaxFormBytes <= 0 {
		o.MaxFormBytes = DefaultMaxFormBytes
	}
	return o
}

// Kind is the kind of value a finding is: what the needle found was.
type Kind string

// The kinds of value.
const (
	// KindName is a person's name or a part of it.
	KindName Kind = "name"
	// KindIdentifier is an identifier.
	KindIdentifier Kind = "identifier"
	// KindDate is a date, or a date and time.
	KindDate Kind = "date"
	// KindUID is a UID.
	KindUID Kind = "uid"
	// KindListTerm is a term of one of the profile's text lists.
	KindListTerm Kind = "list_term"
)

// kindOf maps the engine's kind of needle to a finding's.
func kindOf(k engine.NeedleKind) Kind {
	switch k {
	case engine.NeedleName:
		return KindName
	case engine.NeedleDate:
		return KindDate
	case engine.NeedleUID:
		return KindUID
	}
	return KindIdentifier
}

// Finding is a needle found in the file. It names where and what kind of
// value, never the value.
type Finding struct {
	// Path is the element the value was found in, as the engine's record
	// writes a path ("00081115[0]/00100010"); "preamble" for the first
	// 128 bytes of the file; "file" for bytes between elements.
	Path string
	// Kind is what the value was.
	Kind Kind
	// Raw reports that it was found in the raw bytes, not in the decoded
	// text of an element.
	Raw bool
}

// Report is what a check searched for and found. It holds no value.
type Report struct {
	// Needles is the needles given.
	Needles int
	// Searched is the distinct written forms searched for.
	Searched int
	// Skipped is the forms not searched for as too short.
	Skipped int
	// Findings are the first findings, one per path and kind, at most
	// Options.MaxFindings; empty when the file passed.
	Findings []Finding
	// Total counts the findings, those not listed included.
	Total int
}

// ReasonResidual is the quarantine reason of a file in which a needle was
// found.
const ReasonResidual = "residual_value"

// ReasonFailed is the quarantine reason of a file the check refused: the
// engine's reason for a file de-identification could not finish.
const ReasonFailed = engine.ReasonFailed

// Residual is the error of a file in which a needle was found. Its text
// names the first finding's kind and path, never a value.
type Residual struct {
	// First is the first finding.
	First Finding
	// Total counts the findings.
	Total int
}

// Error says what kind of value was found where.
func (r *Residual) Error() string {
	return fmt.Sprintf("deid check: %s", r.Detail())
}

// Reason returns the quarantine reason, residual_value.
func (r *Residual) Reason() string { return ReasonResidual }

// Detail returns the quarantine detail: the first finding's kind and
// path, and how many there were.
func (r *Residual) Detail() string {
	s := string(r.First.Kind) + " at " + r.First.Path
	if r.Total > 1 {
		s += fmt.Sprintf(" (%d found)", r.Total)
	}
	return s
}

// Cause is the fixed reason a file was refused without a verdict.
type Cause string

// The causes of a refusal.
const (
	// CauseTooLarge: the file is larger than MaxFileBytes, its search
	// needs more than the whole of Options.Memory, or it holds a value
	// longer than MaxValueBytes, sequences nested deeper than the
	// check walks, or forms ending on one another so densely that the
	// search would pass its budget of work.
	CauseTooLarge Cause = "check_too_large"
	// CauseTooManyNeedles: more needles than MaxNeedles, one longer than
	// MaxNeedleBytes, or written forms of more than MaxFormBytes.
	CauseTooManyNeedles Cause = "check_too_many_needles"
	// CauseUnreadable: the reader could not read the file to its end
	// (truncated, malformed, a sequence or item cut short, sequences
	// nested past the reader's deepest), or it is in the Deflated
	// Explicit VR Little Endian transfer syntax (PS3.5 A.5), whose raw
	// bytes are compressed and cannot be searched.
	CauseUnreadable Cause = "check_unreadable"
)

// Refusal is a file the check could not judge. It is never a pass: the
// file is held like one that failed.
type Refusal struct {
	// Cause is why.
	Cause Cause
}

// Error names the cause.
func (r *Refusal) Error() string { return "deid check: " + string(r.Cause) }

// Reason returns the quarantine reason, deid_failed.
func (r *Refusal) Reason() string { return ReasonFailed }

// Detail returns the quarantine detail, the cause.
func (r *Refusal) Detail() string { return string(r.Cause) }

// Checker searches de-identified files for their needles. It holds only
// its options and is safe for use by many goroutines at once.
type Checker struct {
	opts  Options
	seams seams
}

// seams are what a test of this package changes in a Checker.
type seams struct {
	// afterParse runs once a file is read, before it is searched.
	afterParse func()
	// work is told the units of the work budget a search spent.
	work func(spent int64)
	// refusedAt is told how far into the text a search had read when its
	// budget refused the file.
	refusedAt func(at int)
	// library is told the encodings of each call to the library's encoder.
	library func(encodings []string)
	// window, when not zero, is the bytes of a stream read and searched at
	// a time in place of the window constant, so a small file has edges
	// between windows to put a match across.
	window int
}

// New returns a Checker bounded by opts.
func New(opts Options) *Checker { return &Checker{opts: opts.withDefaults()} }

// Check searches a de-identified file for needles: its size bytes read
// from src, which the caller opened (an *os.File of the pipeline's
// temporary file, with its size from Stat). characterSet is the Specific
// Character Set (0008,0005) the file was received in, its values joined by
// a backslash (engine.Reception.CharacterSet), nil or "" for none: the raw
// bytes are searched in it as well as in UTF-8.
//
// It returns nil when no needle is in the file, a *Residual when one is, a
// *Refusal when the file or the needles pass a bound or the file cannot be
// read, an error of src, or ctx's error. The Report is filled in every
// case as far as the check got.
//
// Every element is searched, the record of the de-identification
// included; CheckWritten is the check of a file the engine just made.
func (c *Checker) Check(ctx context.Context, src io.ReaderAt, size int64, needles engine.Needles, characterSet *string) (Report, error) {
	return c.CheckWritten(ctx, src, size, needles, engine.Written{}, characterSet)
}

// CheckWritten is Check of a file the engine made in the run whose record
// gave needles and written (engine.Record.Written): the record of the
// de-identification at the file's top level, where it holds exactly what
// written says, is not searched, so a word of it (a code meaning, the
// method's name, a version) is not taken for a removed value it equals
// (writtenElements says why that lets no received value through). Every
// other element is searched as Check searches it.
func (c *Checker) CheckWritten(ctx context.Context, src io.ReaderAt, size int64, needles engine.Needles, written engine.Written, characterSet *string) (rep Report, err error) {
	rep.Needles = len(needles)
	set, err := c.forms(needles)
	if err != nil {
		return rep, err
	}
	rep.Searched, rep.Skipped = len(set.forms), set.skipped
	if size < 0 || size > c.opts.MaxFileBytes {
		return rep, &Refusal{Cause: CauseTooLarge}
	}
	defer func() {
		// The reader works on hostile input; a panic of it is a file it
		// cannot read, never a pass and never a crash of the server.
		if p := recover(); p != nil {
			rep.Findings, rep.Total = nil, 0
			err = &Refusal{Cause: CauseUnreadable}
		}
	}()
	f, err := c.parse(src, size, written)
	if err != nil {
		return rep, err
	}
	// The library logs a declaration it cannot read and a form a
	// character set cannot write; per file, that would be noise.
	quiet := config.WithLogger(ctx, slog.New(slog.DiscardHandler))
	fs := newFindings(c.opts.MaxFindings)
	s := newSearcher(ctx, quiet, f, set, sourceEncodings(characterSet), c.opts, fs, c.seams)
	if c.seams.work != nil {
		defer func() { c.seams.work(workBudget - s.budget) }()
	}
	// The raw forms are written before the memory is taken, so what is
	// taken counts their automaton as built, not a guess of it.
	if err := s.encodeRaw(); err != nil {
		return rep, err
	}
	if err := s.prepareLists(); err != nil {
		return rep, err
	}
	if c.opts.Memory != nil {
		release, err := c.opts.Memory.Reserve(ctx, c.reservation(size, f, set, s.rawBytes, s.rawLongest)+s.listsBytes())
		if err != nil {
			return rep, err
		}
		defer release()
	}
	if c.seams.afterParse != nil {
		c.seams.afterParse()
	}
	if f.nestedPixels != "" {
		fs.add(Finding{Path: f.nestedPixels, Kind: KindIdentifier, Raw: true})
	}
	if err := s.search(); err != nil {
		return rep, err
	}
	rep.Findings, rep.Total = fs.list, fs.total
	if fs.total > 0 {
		return rep, &Residual{First: fs.list[0], Total: fs.total}
	}
	return rep, nil
}

// findings collects the findings of a file, one per path and kind.
type findings struct {
	max   int
	seen  map[Finding]bool
	list  []Finding
	total int
}

func newFindings(limit int) *findings {
	return &findings{max: limit, seen: map[Finding]bool{}}
}

// add records a finding unless one of its path and kind is recorded.
func (f *findings) add(x Finding) {
	key := Finding{Path: x.Path, Kind: x.Kind}
	if f.seen[key] {
		return
	}
	f.seen[key] = true
	f.total++
	if len(f.list) < f.max {
		f.list = append(f.list, x)
	}
}
