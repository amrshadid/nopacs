package backup

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// The plaintext's form (doc.go).
const (
	// Format names a backup of an installation.
	Format = "nopacs-backup"
	// ExportFormat names a tenant's export: one tenant's section, no
	// master's, under a transfer key (ExportTenant).
	ExportFormat = "nopacs-tenant-export"
	// FormatVersion is the version of the layout this package writes.
	FormatVersion = 1
	// MaxLine is the longest line a reader takes, its "\n" included: a row
	// longer than this cannot be backed up, and a reader holds no more of
	// one line in memory.
	MaxLine = 64 << 20
	// MaxSections bounds the sections a header may announce.
	MaxSections = 100000
)

// The databases a section holds.
const (
	DatabaseMaster = "master"
	DatabaseTenant = "tenant"
)

// The line kinds.
const (
	kindHeader    = "header"
	kindSection   = "section"
	kindTable     = "table"
	kindSequences = "sequences"
	kindEnd       = "end"
	kindFooter    = "footer"
)

// Header is a backup's first line.
type Header struct {
	Kind         string    `json:"kind"`
	Format       string    `json:"format"`
	Version      int       `json:"version"`
	Installation string    `json:"installation"`
	Time         time.Time `json:"time"`
	KeyVersion   int64     `json:"key_version"`
	Sections     int       `json:"sections"`
}

// SectionStart begins a section.
type SectionStart struct {
	Kind     string `json:"kind"`
	Index    int    `json:"index"`
	Database string `json:"database"`
	Tenant   string `json:"tenant"`
	TenantID string `json:"tenant_id"`
	Driver   string `json:"driver"`
}

// Owner is one storage's owner generation in a recovery point.
type Owner struct {
	Storage    string `json:"storage"`
	Generation int64  `json:"generation"`
}

// Point is a section's recovery point as the file writes it.
type Point struct {
	SnapshotTime   time.Time `json:"snapshot_time"`
	SchemaVersion  int       `json:"schema_version"`
	EventHistory   string    `json:"event_history"`
	LastCorrection string    `json:"last_correction"`
	LastReception  string    `json:"last_reception"`
	Owners         []Owner   `json:"owners"`
	KeyVersion     int64     `json:"key_version"`
}

// SectionEnd ends a section.
type SectionEnd struct {
	Kind   string `json:"kind"`
	Index  int    `json:"index"`
	Tables int    `json:"tables"`
	Rows   int64  `json:"rows"`
	Point  Point  `json:"point"`
}

// FooterSection is one section as the footer lists it.
type FooterSection struct {
	Index  int    `json:"index"`
	Tenant string `json:"tenant"`
	Tables int    `json:"tables"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

// Footer is a backup's last line.
type Footer struct {
	Kind     string          `json:"kind"`
	Sections []FooterSection `json:"sections"`
}

// tableLine is a table's line.
type tableLine struct {
	Kind    string      `json:"kind"`
	Name    string      `json:"name"`
	Columns [][2]string `json:"columns"`
}

// sequencesLine is a section's sequences.
type sequencesLine struct {
	Kind      string           `json:"kind"`
	Sequences map[string]int64 `json:"sequences"`
}

// nonUTF8 is a text value that is not UTF-8, written as its bytes.
type nonUTF8 struct {
	Bytes string `json:"bytes"`
}

// pointOf returns a recovery point as the file writes it.
func pointOf(p snapshot.Point, keyVersion int64) Point {
	out := Point{SnapshotTime: p.Time, SchemaVersion: p.Version, EventHistory: p.EventHistory, LastCorrection: p.LastCorrection,
		LastReception: p.LastReception, Owners: []Owner{}, KeyVersion: keyVersion}
	for _, o := range p.Owners {
		out.Owners = append(out.Owners, Owner{Storage: o.Storage, Generation: o.Generation})
	}
	return out
}

// writer writes the plaintext; it is the snapshot.Sink of the section it is
// in.
type writer struct {
	w *bufio.Writer
	// section is the digest of the section being written, nil outside one.
	section hash.Hash
	start   SectionStart
	cols    []snapshot.Column
	tables  int
	rows    int64
	done    []FooterSection
	buf     bytes.Buffer
	enc     *json.Encoder
	row     []any
}

// newWriter returns a writer onto w.
func newWriter(w io.Writer) *writer {
	pw := &writer{w: bufio.NewWriterSize(w, 1<<16)}
	pw.enc = json.NewEncoder(&pw.buf)
	pw.enc.SetEscapeHTML(false)
	return pw
}

// line writes v as one line, into the section's digest when in one.
func (pw *writer) line(v any) error {
	pw.buf.Reset()
	if err := pw.enc.Encode(v); err != nil {
		return fault.Wrap(fault.Internal, err, "backup: a line cannot be encoded")
	}
	if pw.buf.Len() > MaxLine {
		return fault.New(fault.Invalid, "backup: a row of "+pw.where()+" is longer than a backup line may be")
	}
	if pw.section != nil {
		pw.section.Write(pw.buf.Bytes())
	}
	if _, err := pw.w.Write(pw.buf.Bytes()); err != nil {
		return fault.Wrap(fault.Unavailable, err, "backup: the file cannot be written")
	}
	return nil
}

// where names the section being written, for a message.
func (pw *writer) where() string {
	if pw.start.Database == DatabaseMaster {
		return "the master database"
	}
	return "the tenant " + pw.start.Tenant
}

// header writes the header, of a backup unless h names the export's format.
func (pw *writer) header(h Header) error {
	if h.Format != ExportFormat {
		h.Format = Format
	}
	h.Kind, h.Version = kindHeader, FormatVersion
	return pw.line(h)
}

// begin writes a section's first line.
func (pw *writer) begin(s SectionStart) error {
	s.Kind = kindSection
	pw.section, pw.start, pw.tables, pw.rows = sha256.New(), s, 0, 0
	return pw.line(s)
}

// Table implements snapshot.Sink.
func (pw *writer) Table(t snapshot.Table) error {
	l := tableLine{Kind: kindTable, Name: t.Name}
	for _, c := range t.Columns {
		if !c.Kind.Valid() {
			return fault.New(fault.Internal, "backup: a column of "+t.Name+" has no kind")
		}
		l.Columns = append(l.Columns, [2]string{c.Name, string(c.Kind)})
	}
	pw.cols = t.Columns
	pw.tables++
	return pw.line(l)
}

// Row implements snapshot.Sink.
func (pw *writer) Row(values []any) error {
	if len(values) != len(pw.cols) {
		return fault.New(fault.Internal, "backup: a row has another count of values than its table has columns")
	}
	pw.row = pw.row[:0]
	for i, v := range values {
		e, err := encodeValue(v, pw.cols[i].Kind)
		if err != nil {
			return fault.New(fault.Internal, "backup: "+pw.cols[i].Name+" of "+pw.where()+": "+err.Error())
		}
		pw.row = append(pw.row, e)
	}
	pw.rows++
	return pw.line(pw.row)
}

// Sequences implements snapshot.Sink.
func (pw *writer) Sequences(seqs map[string]int64) error {
	if seqs == nil {
		seqs = map[string]int64{}
	}
	return pw.line(sequencesLine{Kind: kindSequences, Sequences: seqs})
}

// end writes a section's last line and keeps its digest for the footer.
func (pw *writer) end(p Point) (FooterSection, error) {
	if err := pw.line(SectionEnd{Kind: kindEnd, Index: pw.start.Index, Tables: pw.tables, Rows: pw.rows, Point: p}); err != nil {
		return FooterSection{}, err
	}
	f := FooterSection{Index: pw.start.Index, Tenant: pw.start.Tenant, Tables: pw.tables, Rows: pw.rows,
		SHA256: hex.EncodeToString(pw.section.Sum(nil))}
	pw.section = nil
	pw.done = append(pw.done, f)
	return f, nil
}

// footer writes the footer and flushes.
func (pw *writer) footer() error {
	if err := pw.line(Footer{Kind: kindFooter, Sections: pw.done}); err != nil {
		return err
	}
	if err := pw.w.Flush(); err != nil {
		return fault.Wrap(fault.Unavailable, err, "backup: the file cannot be written")
	}
	return nil
}

// encodeValue returns a value as its line holds it.
func encodeValue(v any, k snapshot.Kind) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch k {
	case snapshot.KindInt:
		if x, ok := v.(int64); ok {
			return x, nil
		}
	case snapshot.KindText:
		if x, ok := v.(string); ok {
			if !utf8.ValidString(x) {
				return nonUTF8{Bytes: base64.StdEncoding.EncodeToString([]byte(x))}, nil
			}
			return x, nil
		}
	case snapshot.KindBytes:
		if x, ok := v.([]byte); ok {
			return base64.StdEncoding.EncodeToString(x), nil
		}
	case snapshot.KindBool:
		if x, ok := v.(bool); ok {
			return x, nil
		}
	case snapshot.KindTime:
		if x, ok := v.(time.Time); ok {
			return x.UnixMicro(), nil
		}
	case snapshot.KindFloat:
		if x, ok := v.(float64); ok {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return nil, errNotFinite
			}
			return x, nil
		}
	}
	return nil, errValueKind
}

// The errors of a value that does not fit its column.
var (
	errNotFinite = fault.New(fault.Invalid, "a number that is not finite has no JSON form")
	errValueKind = fault.New(fault.Internal, "a value is not of its column's kind")
)

// decodeValue returns a value of a line as the Go type of its kind.
func decodeValue(raw json.RawMessage, k snapshot.Kind) (any, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	switch k {
	case snapshot.KindInt, snapshot.KindTime:
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return nil, errBadValue
		}
		if k == snapshot.KindTime {
			return time.UnixMicro(n).UTC(), nil
		}
		return n, nil
	case snapshot.KindText:
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s, nil
		}
		var b nonUTF8
		if err := strictUnmarshal(raw, &b); err != nil {
			return nil, errBadValue
		}
		out, err := base64.StdEncoding.DecodeString(b.Bytes)
		if err != nil || utf8.Valid(out) {
			return nil, errBadValue
		}
		return string(out), nil
	case snapshot.KindBytes:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, errBadValue
		}
		out, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, errBadValue
		}
		return out, nil
	case snapshot.KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, errBadValue
		}
		return b, nil
	case snapshot.KindFloat:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, errBadValue
		}
		return f, nil
	}
	return nil, errBadValue
}

// errBadValue is a value that is not in the form its column's kind writes.
var errBadValue = fault.New(fault.Invalid, "backup: a value is not in the form of its column's kind")

// strictUnmarshal decodes one JSON object into v, refusing an unknown field
// or anything after it.
func strictUnmarshal(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errBadLine
	}
	return nil
}
