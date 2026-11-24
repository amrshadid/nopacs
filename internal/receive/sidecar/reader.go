package sidecar

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/uid"
)

// Reader reads a sidecar as a stream, one instance at a time.
type Reader struct {
	r     *bufio.Reader
	start bool
	done  bool
	last  string
	// version is the version its first line states, 0 for a reader that
	// starts at an instance's line (NewReaderFrom).
	version int
	// scan is the header of a sidecar of version 3.
	scan *ScanHeader
}

// Version returns the version of the sidecar, known once Next has read
// its first line: 0 before, and for a reader started at an instance's line.
func (r *Reader) Version() int { return r.version }

// Scan returns the scan's header, known once Next has read the first line of
// a sidecar of version 3; nil before, for older versions, and for a reader
// started at an instance's line.
func (r *Reader) Scan() *ScanHeader { return r.scan }

// parseHead reads a sidecar's first line (without its newline): its version
// and, for version 3, its scan's header.
func parseHead(h []byte) (int, *ScanHeader, error) {
	switch s := string(h); {
	case s == header[:len(header)-1]:
		return VersionFrames, nil, nil
	case s == headerV1[:len(headerV1)-1]:
		return 1, nil, nil
	case strings.HasPrefix(s, headV3) && strings.HasSuffix(s, tailV3):
		var sh ScanHeader
		dec := json.NewDecoder(strings.NewReader(s[len(headV3) : len(s)-len(tailV3)]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&sh); err != nil || dec.More() {
			return 0, nil, fmt.Errorf("%w: header", ErrFormat)
		}
		if err := sh.Validate(); err != nil {
			return 0, nil, err
		}
		// Written once in one form: the line must be what the writer writes
		// for these values, so a header has no second spelling.
		if enc, err := json.Marshal(sh); err != nil || headV3+string(enc)+tailV3 != s {
			return 0, nil, fmt.Errorf("%w: header", ErrFormat)
		}
		return Version, &sh, nil
	}
	return 0, nil, fmt.Errorf("%w: header", ErrFormat)
}

// NewReader returns a reader of the sidecar r holds.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next instance and its line as written, or io.EOF after
// the last. A sidecar not in the form Writer writes is ErrFormat.
func (r *Reader) Next() (Instance, []byte, error) {
	if r.done {
		return Instance{}, nil, io.EOF
	}
	if !r.start {
		h, err := r.line()
		if err != nil {
			return Instance{}, nil, fmt.Errorf("%w: header", ErrFormat)
		}
		if r.version, r.scan, err = parseHead(h); err != nil {
			return Instance{}, nil, err
		}
		r.start = true
	}
	line, err := r.line()
	if err != nil {
		return Instance{}, nil, err
	}
	if string(line) == footer[:len(footer)-1] {
		if err := r.end(); err != nil {
			return Instance{}, nil, err
		}
		r.done = true
		if r.last == "" {
			return Instance{}, nil, fmt.Errorf("%w: no instances", ErrFormat)
		}
		return Instance{}, nil, io.EOF
	}
	line = bytes.TrimSuffix(line, []byte(","))
	var in Instance
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Instance{}, nil, fmt.Errorf("%w: an instance's line", ErrFormat)
	}
	if string(in.Dataset) == "null" {
		in.Dataset = nil
	}
	if err := check(in, r.version); err != nil {
		return Instance{}, nil, err
	}
	if r.version >= VersionFrames && in.NeedsFrames() {
		return Instance{}, nil, fmt.Errorf("%w: %w", ErrFormat, ErrIncomplete)
	}
	if r.last != "" && in.SOPInstanceUID <= r.last {
		return Instance{}, nil, fmt.Errorf("%w: %w", ErrFormat, ErrOrder)
	}
	r.last = in.SOPInstanceUID
	return in, line, nil
}

// end checks what follows the footer: nothing, or the index line and then
// nothing.
func (r *Reader) end() error {
	if _, err := r.r.ReadByte(); errors.Is(err, io.EOF) {
		return nil
	}
	_ = r.r.UnreadByte()
	line, err := r.lineWithin(maxIndexLine)
	if err != nil {
		return err
	}
	if _, err := decodeIndex(line); err != nil {
		return err
	}
	if _, err := r.r.ReadByte(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: bytes after the end", ErrFormat)
	}
	return nil
}

// decodeIndex decodes the index line, which must be in the one form.
func decodeIndex(line []byte) ([]IndexEntry, error) {
	var il indexLine
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&il); err != nil || len(il.Index) == 0 {
		return nil, fmt.Errorf("%w: the index", ErrFormat)
	}
	prev := int64(0)
	for i, e := range il.Index {
		if e.Offset <= prev || uid.Check(e.UID) != nil || (i > 0 && e.UID <= il.Index[i-1].UID) {
			return nil, fmt.Errorf("%w: the index is out of order", ErrFormat)
		}
		prev = e.Offset
	}
	return il.Index, nil
}

// ParseIndex returns the index a sidecar ends with, given its last bytes
// and whether they are the whole file. complete is false when tail is too
// short to hold the whole index line, and a longer tail should be read;
// when it is true and entries is nil, the sidecar has no index (a small
// one, or one written before indexes existed) or its index is not in its
// form, and a reader starts at the first line.
func ParseIndex(tail []byte, whole bool) (entries []IndexEntry, complete bool) {
	if len(tail) == 0 || tail[len(tail)-1] != '\n' || bytes.HasSuffix(tail, []byte("\n"+footer)) {
		return nil, true
	}
	body := tail[:len(tail)-1]
	i := bytes.LastIndexByte(body, '\n')
	if i < 0 && !whole {
		return nil, false
	}
	entries, err := decodeIndex(body[i+1:])
	if err != nil {
		return nil, true
	}
	return entries, true
}

// Locate returns the offset to start reading at for the instance with the
// SOP Instance UID uid: the line of the last indexed instance not after it.
func Locate(entries []IndexEntry, uid string) int64 {
	i := sort.Search(len(entries), func(i int) bool { return entries[i].UID > uid })
	if i == 0 {
		return entries[0].Offset
	}
	return entries[i-1].Offset
}

// NewReaderFrom returns a reader of the sidecar that r holds from the first
// byte of an instance's line, an offset an index gives, to the end.
func NewReaderFrom(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), start: true}
}

// line reads one line without its newline, at most maxLine bytes.
func (r *Reader) line() ([]byte, error) { return r.lineWithin(maxLine) }

// lineWithin reads one line without its newline, at most limit bytes.
func (r *Reader) lineWithin(limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > limit {
			return nil, fmt.Errorf("%w: a line is longer than %d bytes", ErrFormat, limit)
		}
		switch {
		case err == nil:
			return out[:len(out)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil, fmt.Errorf("%w: cut short", ErrFormat)
		default:
			return nil, fmt.Errorf("sidecar: read: %w", err)
		}
	}
}
