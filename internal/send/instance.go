package send

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// errMissing is an instance whose object the storage does not hold: data
// loss, as a read of it reports it.
var errMissing = errors.New("send: the instance's file is not on its storage")

// errNoMeta is an instance whose file meta information cannot be read, so
// the transfer syntax its data set is in is not known (PS3.10 7.1).
var errNoMeta = errors.New("send: the instance's file meta information cannot be read")

// errMetaDisagrees is an instance whose file meta information names
// another SOP class or instance than its data set (PS3.10 7.1).
var errMetaDisagrees = errors.New("send: the instance's file meta information and its data set name different SOP classes or instances")

// errStreamClosed is what the copy of an instance is told when its reader
// stops reading before the end.
var errStreamClosed = errors.New("send: the instance's reader was closed before its end")

// stream is the bytes of one archived instance as they are read and
// checked (checkedread.CopyChecked): a reader that fails with the
// mismatch, before the last piece, when the storage's bytes are not what
// the catalog holds. Close stops the copy and waits for it, so the
// buffers it was lent are free once Close returns.
type stream struct {
	pr   *io.PipeReader
	body io.Closer
	done chan struct{}
	err  error
}

// openStream opens the instance in from st, its bytes checked against its
// length and archived SHA-256 as they are read into bufs. A storage that
// does not hold the object is errMissing; an instance a scrub marked is
// checkedread.ErrMarked, before its storage is opened.
func openStream(ctx context.Context, st Storages, in tenant.ScanInstance, bufs [2][]byte) (*stream, error) {
	if err := checkedread.Marked(in); err != nil {
		// A scrub found its bytes damaged: not one is read or sent.
		return nil, err
	}
	store, err := st.Get(ctx, in.Storage)
	if err != nil {
		return nil, err
	}
	body, err := store.Read(ctx, in.Key, in.Offset, in.Length)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return nil, errMissing
	case err != nil:
		return nil, err
	}
	pr, pw := io.Pipe()
	s := &stream{pr: pr, body: body, done: make(chan struct{})}
	sum := in.ArchivedSHA256
	go func() {
		defer close(s.done)
		_, err := checkedread.CopyChecked(body, in.Length, &sum, bufs, func(b []byte) error {
			_, err := pw.Write(b)
			return err
		})
		s.err = err
		_ = pw.CloseWithError(err) // A nil error is the end of the instance.
	}()
	return s, nil
}

// Read reads the checked bytes.
func (s *stream) Read(b []byte) (int, error) { return s.pr.Read(b) }

// Close stops the copy, waits for it, and closes the storage's body.
func (s *stream) Close() error {
	_ = s.pr.CloseWithError(errStreamClosed)
	<-s.done
	return s.body.Close()
}

// damaged reports, once the copy has ended, whether it found the bytes
// not to be what was archived.
func (s *stream) damaged() bool {
	select {
	case <-s.done:
		return checkedread.Mismatched(s.err)
	default:
		return false
	}
}

// failed returns, once the copy has ended, the error it ended with that
// is the storage's (not a mismatch, and not the reader closing it); nil
// otherwise.
func (s *stream) failed() error {
	select {
	case <-s.done:
	default:
		return nil
	}
	if s.err == nil || checkedread.Mismatched(s.err) || errors.Is(s.err, errStreamClosed) || errors.Is(s.err, io.ErrClosedPipe) {
		return nil
	}
	return s.err
}

// meta is what an instance's file meta information says of its data set.
type meta struct {
	sopClass, sopInstance, syntax string
}

// metaStart is where a Part 10 file's meta group's first element begins:
// after the 128-byte preamble and "DICM" (PS3.10 7.1). The group length
// (0002,0000), the first element, is 12 bytes in Explicit VR Little Endian,
// so the data set begins at metaStart+12 plus its value.
const metaStart = 132

// dataSetOf reads the start of an instance's stream, at most metaPrefix
// bytes into prefix, and returns what its file meta information says and
// a reader of the data set that follows it, as stored. The data set begins
// where the group length (0002,0000) says the meta group ends (PS3.10 7.1),
// which go-dicom's reader of the group must agree with; a file whose group
// length is absent, whose group ends past the bytes read (an element the
// reader passed over), or whose SOP class or transfer syntax is missing or
// not a UID (PS3.5 9.1), is errNoMeta: where its data set starts, or what
// it is, is not known, and it is not sent.
func dataSetOf(s *stream, size int64, prefix []byte) (meta, io.Reader, error) {
	n := int(min(size, int64(len(prefix))))
	if _, err := io.ReadFull(s, prefix[:n]); err != nil {
		return meta{}, nil, err
	}
	if n < metaStart+12 || string(prefix[metaStart:metaStart+6]) != "\x02\x00\x00\x00UL" {
		return meta{}, nil, errNoMeta
	}
	start := int64(metaStart+12) + int64(binary.LittleEndian.Uint32(prefix[metaStart+8:metaStart+12]))
	if start > int64(n) {
		return meta{}, nil, errNoMeta
	}
	dfr := filereader.NewDCMFileReader(filebase.NewFileReader(bytes.NewReader(prefix[:n])))
	if dfr.ReadPreamble() != nil || dfr.ReadDICMPrefix() != nil {
		return meta{}, nil, errNoMeta
	}
	fm, err := dfr.ReadFileMetaInfo()
	if err != nil || fm == nil || dfr.GetPosition() != start {
		return meta{}, nil, errNoMeta
	}
	trim := func(v string) string { return strings.TrimRight(v, "\x00 ") }
	m := meta{sopClass: trim(fm.MediaStorageSOPClassUID), sopInstance: trim(fm.MediaStorageSOPInstanceUID), syntax: trim(fm.TransferSyntaxUID)}
	if !validUID(m.sopClass) || !validUID(m.syntax) {
		return meta{}, nil, errNoMeta
	}
	// The C-STORE's Affected SOP Class UID is the data set's own SOP Class
	// UID (0008,0016) (PS3.7 9.3.1.1), read as received from the start of
	// the data set; PS3.10 7.1 has the meta group's (0002,0002) and
	// (0002,0003) repeat the data set's, and a file whose two disagree is
	// not sent, since which class it is cannot be told.
	id, err := header.Identify(prefix[:n], int64(n) == size)
	switch {
	case err != nil || !validUID(id.SOPClassUID):
		return meta{}, nil, errNoMeta
	case id.SOPClassUID != m.sopClass || (id.SOPInstanceUID != "" && m.sopInstance != "" && id.SOPInstanceUID != m.sopInstance):
		return meta{}, nil, errMetaDisagrees
	}
	m.sopClass, m.sopInstance = id.SOPClassUID, id.SOPInstanceUID
	return m, io.MultiReader(bytes.NewReader(prefix[start:n]), s), nil
}

// validUID reports whether uid is a UID a failure of a job may record
// (PS3.5 9.1), by the store's own rule.
func validUID(uid string) bool {
	return uid != "" && (tenant.JobFailure{SOPInstanceUID: "1.2", Code: tenant.FailurePeerRefused, SOPClassUID: uid,
		Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}).Validate() == nil
}
