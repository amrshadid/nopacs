package backup

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/snapshot"
)

// errBadLine is the one error of a plaintext that is not in its layout. The
// file opened under the cipher, so it was made with this key file; a line
// out of place is a writer's fault or a format this version does not read.
var errBadLine = errors.New("backup: the file is not in the layout of a backup")

// badLayout returns errBadLine with what was wrong.
func badLayout(what string) error {
	return fault.Wrap(fault.Invalid, errBadLine, "backup: the file is not in the layout of a backup: "+what)
}

// reader reads the plaintext line by line, in its layout, and is the
// snapshot.Source of the section it is in.
type reader struct {
	r *bufio.Reader
	// next is a line read ahead and not yet taken.
	next []byte
	// section is the digest of the section being read, nil outside one.
	section hash.Hash
	start   SectionStart
	cols    []snapshot.Column
	// tableName is the current table's.
	tableName string
	raw       []json.RawMessage
	tables    int
	rows      int64
	seqs      map[string]int64
	// inTable says the rows read are of the current table.
	inTable bool
	header  Header
	sums    []FooterSection
	footer  Footer
	// rowHook, when set, sees every row of every table as it is read.
	rowHook func(table string, cols []snapshot.Column, values []any)
}

// newReader returns a reader of r.
func newReader(r io.Reader) *reader {
	return &reader{r: bufio.NewReaderSize(r, 1<<16)}
}

// readLine returns the next line without its "\n", folding it into the
// section's digest; io.EOF at the end.
func (rd *reader) readLine() ([]byte, error) {
	if rd.next != nil {
		l := rd.next
		rd.next = nil
		return l, nil
	}
	var line []byte
	for {
		chunk, err := rd.r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxLine {
			return nil, badLayout("a line is longer than " + strconv.Itoa(MaxLine) + " bytes")
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, badLayout("the last line has no end")
		}
		return nil, err
	}
	if rd.section != nil {
		rd.section.Write(line)
	}
	return line[:len(line)-1], nil
}

// unread puts a line back to be read next.
func (rd *reader) unread(l []byte) { rd.next = l }

// kindOf returns the kind of an object line, or "" for a row.
func kindOf(l []byte) (string, error) {
	if len(l) > 0 && l[0] == '[' {
		return "", nil
	}
	var k struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(l, &k); err != nil || k.Kind == "" {
		return "", badLayout("a line is neither a row nor an object with a kind")
	}
	return k.Kind, nil
}

// object reads the next line as an object of kind into v.
func (rd *reader) object(kind string, v any) error {
	l, err := rd.readLine()
	if errors.Is(err, io.EOF) {
		return badLayout("it ends before its " + kind)
	}
	if err != nil {
		return err
	}
	k, err := kindOf(l)
	if err != nil {
		return err
	}
	if k != kind {
		return badLayout("a " + kind + " line was expected, and a " + k + " came")
	}
	if err := strictUnmarshal(l, v); err != nil {
		return badLayout("its " + kind + " line is not in its form")
	}
	return nil
}

// readHeader reads and checks the header.
func (rd *reader) readHeader() (Header, error) {
	var h Header
	if err := rd.object(kindHeader, &h); err != nil {
		return h, err
	}
	switch {
	case h.Format != Format && h.Format != ExportFormat:
		return h, badLayout("its format is neither " + Format + " nor " + ExportFormat)
	case h.Format == ExportFormat && h.Sections != 1:
		return h, badLayout("a tenant's export holds one section")
	case h.Version != FormatVersion:
		return h, fault.New(fault.Invalid, "backup: the file's layout is version "+strconv.Itoa(h.Version)+
			", and this version reads "+strconv.Itoa(FormatVersion))
	case h.Sections < 1 || h.Sections > MaxSections:
		return h, badLayout("its header announces no sections, or too many")
	}
	rd.header = h
	return h, nil
}

// nextSection reads the next section's first line, or, after the last
// section the header announced, the footer, checked against every
// section's digest, and the end of the file; false then.
func (rd *reader) nextSection() (SectionStart, bool, error) {
	if len(rd.sums) == rd.header.Sections {
		if err := rd.object(kindFooter, &rd.footer); err != nil {
			return SectionStart{}, false, err
		}
		if err := rd.checkFooter(); err != nil {
			return SectionStart{}, false, err
		}
		if _, err := rd.readLine(); !errors.Is(err, io.EOF) {
			return SectionStart{}, false, badLayout("something follows its footer")
		}
		return SectionStart{}, false, nil
	}
	rd.section = sha256.New()
	var s SectionStart
	if err := rd.object(kindSection, &s); err != nil {
		return s, false, err
	}
	// A backup's first section is the master's; an export's one section
	// is its tenant's.
	want := DatabaseTenant
	if len(rd.sums) == 0 && rd.header.Format == Format {
		want = DatabaseMaster
	}
	if s.Index != len(rd.sums) || s.Database != want {
		return s, false, badLayout("its sections are not the master's, then the tenants', in order")
	}
	if (s.Database == DatabaseMaster) != (s.Tenant == "") || (s.Tenant == "") != (s.TenantID == "") {
		return s, false, badLayout("a section names a tenant it is not, or no tenant it is")
	}
	rd.start, rd.tables, rd.rows, rd.seqs, rd.inTable = s, 0, 0, nil, false
	return s, true, nil
}

// checkFooter compares the footer with the digests read.
func (rd *reader) checkFooter() error {
	if len(rd.footer.Sections) != len(rd.sums) {
		return badLayout("its footer lists another number of sections than it holds")
	}
	for i, f := range rd.footer.Sections {
		if f != rd.sums[i] {
			return fault.New(fault.Invalid, "backup: section "+strconv.Itoa(i)+"'s SHA-256 or counts are not what the footer says")
		}
	}
	return nil
}

// NextTable implements snapshot.Source: the rows of a table not read are
// passed over.
func (rd *reader) NextTable() (snapshot.Table, bool, error) {
	for {
		l, err := rd.readLine()
		if errors.Is(err, io.EOF) {
			return snapshot.Table{}, false, badLayout("it ends inside a section")
		}
		if err != nil {
			return snapshot.Table{}, false, err
		}
		k, err := kindOf(l)
		if err != nil {
			return snapshot.Table{}, false, err
		}
		switch k {
		case "":
			if !rd.inTable {
				return snapshot.Table{}, false, badLayout("a row comes before its table")
			}
			if err := rd.decodeRow(l, nil); err != nil {
				return snapshot.Table{}, false, err
			}
			continue
		case kindTable:
			return rd.table(l)
		case kindSequences:
			var s sequencesLine
			if err := strictUnmarshal(l, &s); err != nil || s.Sequences == nil {
				return snapshot.Table{}, false, badLayout("its sequences line is not in its form")
			}
			rd.seqs, rd.inTable = s.Sequences, false
			return snapshot.Table{}, false, nil
		}
		return snapshot.Table{}, false, badLayout("a " + k + " line comes inside a section")
	}
}

// table parses a table's line and makes it the current table.
func (rd *reader) table(l []byte) (snapshot.Table, bool, error) {
	var tl tableLine
	if err := strictUnmarshal(l, &tl); err != nil || tl.Name == "" || len(tl.Columns) == 0 {
		return snapshot.Table{}, false, badLayout("a table line is not in its form")
	}
	t := snapshot.Table{Name: tl.Name}
	for _, c := range tl.Columns {
		k := snapshot.Kind(c[1])
		if c[0] == "" || !k.Valid() {
			return snapshot.Table{}, false, badLayout("a column of " + tl.Name + " has no name or no kind")
		}
		t.Columns = append(t.Columns, snapshot.Column{Name: c[0], Kind: k})
	}
	rd.cols, rd.inTable, rd.tableName = t.Columns, true, t.Name
	rd.tables++
	return t, true, nil
}

// NextRow implements snapshot.Source.
func (rd *reader) NextRow(values []any) (bool, error) {
	l, err := rd.readLine()
	if errors.Is(err, io.EOF) {
		return false, badLayout("it ends inside a section")
	}
	if err != nil {
		return false, err
	}
	if len(l) == 0 || l[0] != '[' {
		rd.unread(l)
		return false, nil
	}
	if err := rd.decodeRow(l, values); err != nil {
		return false, err
	}
	return true, nil
}

// decodeRow decodes a row of the current table into values (or a scratch
// slice when values is nil) and counts it.
func (rd *reader) decodeRow(l []byte, values []any) error {
	rd.raw = rd.raw[:0]
	if err := json.Unmarshal(l, &rd.raw); err != nil || len(rd.raw) != len(rd.cols) {
		return badLayout("a row of a table is not one value per column")
	}
	if values == nil {
		values = make([]any, len(rd.cols))
	}
	for i, c := range rd.cols {
		v, err := decodeValue(rd.raw[i], c.Kind)
		if err != nil {
			return err
		}
		values[i] = v
	}
	rd.rows++
	if rd.rowHook != nil {
		rd.rowHook(rd.tableName, rd.cols, values)
	}
	return nil
}

// Sequences implements snapshot.Source.
func (rd *reader) Sequences() (map[string]int64, error) {
	if rd.seqs == nil {
		return nil, badLayout("a section's sequences are read before its tables are done")
	}
	return rd.seqs, nil
}

// endSection reads a section's last line, after its sequences, and keeps
// its digest; it checks the counts against what was read.
func (rd *reader) endSection() (SectionEnd, FooterSection, error) {
	if rd.seqs == nil {
		for {
			_, ok, err := rd.NextTable()
			if err != nil {
				return SectionEnd{}, FooterSection{}, err
			}
			if !ok {
				break
			}
		}
	}
	var e SectionEnd
	if err := rd.object(kindEnd, &e); err != nil {
		return e, FooterSection{}, err
	}
	if e.Index != rd.start.Index || e.Tables != rd.tables || e.Rows != rd.rows {
		return e, FooterSection{}, badLayout("a section's end does not count what it holds")
	}
	f := FooterSection{Index: rd.start.Index, Tenant: rd.start.Tenant, Tables: rd.tables, Rows: rd.rows,
		SHA256: hex.EncodeToString(rd.section.Sum(nil))}
	rd.section = nil
	rd.sums = append(rd.sums, f)
	return e, f, nil
}
