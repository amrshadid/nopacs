package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a search reads, and its bound: rows are read from the catalog in
// the order an index serves, page by page through its cursor, and kept
// when every key of the search matches them; the first offset kept are
// passed over, the next limit answered, and one more read to know whether
// more remain. Nothing collects every match.

// webMaxPassed is the most rows one search reads and does not keep: rows
// an index served that a key the index does not hold turned down. Past it
// the search is refused as too broad (PS3.18 10.6.3, 413), so a search the
// catalog cannot narrow never reads a whole tenant.
const webMaxPassed = 10000

// errTooBroad is a search that passed over webMaxPassed rows.
var errTooBroad = &dicomweb.Error{Status: http.StatusRequestEntityTooLarge,
	Message: "the search reads more than " + strconv.Itoa(webMaxPassed) + " rows it does not keep; narrow it with a key the catalog matches by index (a study, a series, a subject's PatientID, a StudyDate range, a Modality)"}

// iterator yields rows one at a time.
type iterator[T any] interface {
	next(ctx context.Context) (T, bool, error)
}

// lister is one list of the catalog: a page of rows and the cursor of the
// next, empty after the last.
type lister[T any] func(ctx context.Context, page master.Page) ([]T, string, error)

// pages iterates a list of the catalog through its cursor.
type pages[T any] struct {
	list   lister[T]
	size   int
	buf    []T
	cursor string
	ended  bool
}

// pagesOf iterates list in pages of size rows, at most master.MaxPageLimit.
func pagesOf[T any](list lister[T], size int) *pages[T] {
	return &pages[T]{list: list, size: min(max(size, 1), master.MaxPageLimit)}
}

// next returns the next row.
func (it *pages[T]) next(ctx context.Context) (T, bool, error) {
	var zero T
	for len(it.buf) == 0 {
		if it.ended {
			return zero, false, nil
		}
		rows, next, err := it.list(ctx, master.Page{Cursor: it.cursor, Limit: it.size})
		if err != nil {
			return zero, false, err
		}
		it.buf, it.cursor, it.ended = rows, next, next == ""
	}
	row := it.buf[0]
	it.buf = it.buf[1:]
	return row, true, nil
}

// rowsOf iterates rows already found, each looked up by a UID.
type rowsOf[T any] struct{ rows []T }

// next returns the next row.
func (it *rowsOf[T]) next(context.Context) (T, bool, error) {
	var zero T
	if len(it.rows) == 0 {
		return zero, false, nil
	}
	row := it.rows[0]
	it.rows = it.rows[1:]
	return row, true, nil
}

// none iterates nothing: a search whose key no row can match.
type none[T any] struct{}

// next returns nothing.
func (none[T]) next(context.Context) (T, bool, error) {
	var zero T
	return zero, false, nil
}

// walk returns the rows of it that keep keeps, after the first offset of
// them, at most limit, and whether more remain. A search that turns down
// more than webMaxPassed rows is errTooBroad.
func walk[T any](ctx context.Context, it iterator[T], keep func(T) (bool, error), offset, limit int) ([]T, bool, error) {
	var (
		out             []T
		skipped, passed int
	)
	for {
		row, ok, err := it.next(ctx)
		if err != nil || !ok {
			return out, false, err
		}
		kept, err := keep(row)
		if err != nil {
			return nil, false, err
		}
		switch {
		case !kept:
			if passed++; passed > webMaxPassed {
				return nil, false, errTooBroad
			}
		case skipped < offset:
			skipped++
		case len(out) < limit:
			out = append(out, row)
		default:
			return out, true, nil
		}
	}
}

// pageSize is the size of a page read for a search: what its answer needs,
// so a small search reads one small page.
func pageSize(q dicomweb.Query) int { return q.Offset + q.Limit + 1 }

// webMaxMetadataRead is the most bytes of metadata files one search reads,
// counted as they come from the storage: the lines of instances it matches
// and answers (the first line of each study and series it answers is read
// once a result, at most a page of them). Past it the
// search is refused as too broad (PS3.18 10.6.3, 413), so a search of
// instances whose lines are long, or that matches on attributes over many
// scans, never reads more of the storage than this however its keys fall.
const webMaxMetadataRead = 256 << 20

// errReadTooMuch is a search that read webMaxMetadataRead bytes of
// metadata files.
var errReadTooMuch = &dicomweb.Error{Status: http.StatusRequestEntityTooLarge,
	Message: "the search reads more than " + strconv.Itoa(webMaxMetadataRead>>20) + " MiB of the scans' metadata files; narrow it to fewer scans or instances, or ask for fewer attributes"}

// errMetadataBudget is the read past webMaxMetadataRead, which stops the
// read that met it.
var errMetadataBudget = errors.New("api: the search's bound on metadata read is met")

// scanLines reads one scan's metadata file forward, one instance's line at
// a time, from the place of its index nearest the instance asked for.
type scanLines struct {
	src   *metaSource
	close func()
	wrap  func(io.Reader) io.Reader
	rd    *sidecar.Reader
	rc    io.Closer
	// last is the UID of the line read last, and held that line, which a
	// search reads twice: to match it, then to answer it.
	last string
	held sidecar.Instance
}

// lines holds the metadata file a request reads now, and counts the bytes
// it has read of every one. One scan's file is open at a time: a request
// that moves to another scan closes the one before, so a search over many
// scans holds one open body, one read buffer and one line, never one per
// scan it passed.
type lines struct {
	x    *webRequest
	path label.Path
	cur  *scanLines
	// read is the bytes of metadata files read so far, and max its bound,
	// 0 for none.
	read, max int64
}

// newLines returns the metadata files of a request, none open yet, read
// without a bound.
func (x *webRequest) newLines() *lines {
	return &lines{x: x}
}

// of returns the metadata file of a scan when the request moves to it,
// closing the one open before; the storages are the request's, each opened
// once.
func (l *lines) of(ctx context.Context, s tenant.CatalogScan) (*scanLines, error) {
	path := pathOf(s)
	if l.cur != nil && l.path == path {
		return l.cur, nil
	}
	l.closeAll()
	src, closeSrc, err := l.x.c.metadataSource(ctx, l.x.p, l.x.tenant, s, path, l.x.stores)
	if err != nil {
		return nil, err
	}
	l.cur, l.path = &scanLines{src: src, close: closeSrc, wrap: l.count}, path
	return l.cur, nil
}

// closeAll closes the file open.
func (l *lines) closeAll() {
	if sl := l.cur; sl != nil {
		sl.reset()
		sl.close()
		l.cur = nil
	}
}

// exceeded reports whether the request met its bound on metadata read.
func (l *lines) exceeded() bool { return l.max > 0 && l.read >= l.max }

// count wraps a body of a metadata file so its bytes count toward the
// request's bound, and a read past it stops with errMetadataBudget.
func (l *lines) count(r io.Reader) io.Reader { return &countedReader{r: r, l: l} }

// countedReader is a body whose bytes count toward a request's bound.
type countedReader struct {
	r io.Reader
	l *lines
}

// Read reads from the body until the bound is met.
func (c *countedReader) Read(p []byte) (int, error) {
	if c.l.exceeded() {
		return 0, errMetadataBudget
	}
	n, err := c.r.Read(p)
	c.l.read += int64(n)
	return n, err
}

// reset closes the body open, so the next line is found from the index.
func (sl *scanLines) reset() {
	if sl.rc != nil {
		_ = sl.rc.Close()
	}
	sl.rd, sl.rc = nil, nil
}

// line returns the line of the instance with the SOP Instance UID uid. The
// UID read last is answered again without a read; a UID after it is read
// forward; any other starts again from the file's index.
func (sl *scanLines) line(ctx context.Context, uid string) (sidecar.Instance, error) {
	if sl.rd != nil && uid == sl.last {
		return sl.held, nil
	}
	if sl.rd != nil && uid > sl.last {
		in, err := nextInstance(sl.rd, uid)
		if err != nil {
			sl.reset()
			return in, err
		}
		sl.last, sl.held = uid, in
		return in, nil
	}
	sl.reset()
	rd, rc, in, err := sl.src.lineOf(ctx, uid, sl.wrap)
	if err != nil {
		return in, err
	}
	sl.rd, sl.rc, sl.last, sl.held = rd, rc, uid, in
	return in, nil
}

// first returns the data set of the first instance's line of the scan's
// metadata file, as DICOM JSON keyed by tag: what a search answers of a
// study or a series beyond what the catalog holds. A line without its data
// set (omitted for its size) answers nothing.
func (sl *scanLines) first(ctx context.Context) (map[string]json.RawMessage, error) {
	rc, err := sl.src.store.Read(ctx, sl.src.key, 0, -1)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return nil, fault.Wrap(fault.DataLoss, err, messageNoSidecar)
	case err != nil:
		return nil, storageFailure(err, "the scan's metadata file cannot be read")
	}
	defer func() { _ = rc.Close() }()
	// One line a result, and at most a page's results: not counted toward
	// the bound on what a search reads, which is for the lines it matches,
	// so a search of studies is never cut for the length of their headers.
	in, _, err := sidecar.NewReader(rc).Next()
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, sidecar.ErrFormat), errors.Is(err, sidecar.ErrOrder):
		return nil, fault.Wrap(fault.DataLoss, err, messageBadSidecar)
	case err != nil:
		return nil, storageFailure(err, "the scan's metadata file cannot be read")
	}
	return datasetMembers(in.Dataset), nil
}

// datasetMembers returns the members of a line's data set, or nil for a
// line without one.
func datasetMembers(ds json.RawMessage) map[string]json.RawMessage {
	if len(ds) == 0 {
		return nil
	}
	var set map[string]json.RawMessage
	if json.Unmarshal(ds, &set) != nil {
		return nil
	}
	delete(set, keyPaddingMember)
	return set
}
