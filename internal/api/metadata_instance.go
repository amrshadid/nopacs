package api

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/readat"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

const (
	// transferSyntaxImplicit is Implicit VR Little Endian (PS3.5 A.1), the
	// one syntax whose element headers are 8 bytes (PS3.5 7.1.2).
	transferSyntaxImplicit = "1.2.840.10008.1.2"
	// transferSyntaxDeflated is Deflated Explicit VR Little Endian (PS3.5
	// A.5): its data set is compressed, so no offset in the file names a
	// value.
	transferSyntaxDeflated = header.TransferSyntaxDeflated
	// transferSyntaxRoot starts every other transfer syntax the standard
	// registers, each of them explicit VR (PS3.5 7.1.2) with a 12-byte
	// header for a long value such as Pixel Data.
	transferSyntaxRoot = "1.2.840.10008.1.2."
	// trustedInline is the longest a value's JSON text may be for the
	// metadata file's data set to be answered without a look at the
	// instance's file: a value of that length may be longer than the 64 KiB
	// that makes it bulk data once it is as the file holds it.
	trustedInline = 16 << 10
)

// metaSource answers the headers of one scan's instances from the storage
// the scan is on, or from a replica in step where that storage cannot
// serve a file (checkedread).
type metaSource struct {
	// store reads the scan's metadata file, at key: a checkedread.Source's,
	// so every read of the file goes to the storage its first read chose.
	store blob.Store
	// stores opens the storages; storage and series are the scan's.
	stores          *checkedread.Stores
	storage, series string
	tenant          string
	path            label.Path
	// key is the key of the scan's metadata file.
	key string
	// limit is receiver.header-limit as the request began.
	limit int64
	// cur is the file of the instance read last, by its SOP Instance UID,
	// so the reads of one instance's file (its header, its frames, its
	// bytes) all go to the storage its first read chose.
	cur    instanceFile
	curUID string
}

// instanceFile is one instance's file as a metaSource reads it: a
// checkedread.Source, or, for a source built with no stores, the file on
// store alone (plainFile).
type instanceFile interface {
	// Read reads length bytes of the file from offset.
	Read(ctx context.Context, offset, length int64) (io.ReadCloser, error)
	// Stat returns what is known of the file.
	Stat(ctx context.Context) (blob.Info, error)
	// Store returns the file as a blob.Store, for package readat.
	Store() blob.Store
	// Mismatch records bytes of the file found not to be the catalog's.
	Mismatch(ctx context.Context, err error) error
}

// object returns the instance held as the object a read opens.
func (s *metaSource) object(held tenant.ScanInstance) checkedread.Object {
	return checkedread.InstanceObject(held, s.series, s.path)
}

// file returns the instance's file to read, the one read last again when
// it is the same instance.
func (s *metaSource) file(held tenant.ScanInstance) instanceFile {
	if s.cur == nil || s.curUID != held.SOPInstanceUID {
		if s.stores == nil {
			s.cur = plainFile{store: s.store, key: held.Key}
		} else {
			s.cur = s.stores.Source(s.object(held))
		}
		s.curUID = held.SOPInstanceUID
	}
	return s.cur
}

// readable returns ErrMarked for an instance a scrub marked that no replica
// in step can serve (checkedread.Stores.Readable), before anything of it is
// read or answered.
func (s *metaSource) readable(ctx context.Context, held tenant.ScanInstance) error {
	if s.stores == nil {
		return checkedread.Marked(held)
	}
	return s.stores.Readable(ctx, s.object(held))
}

// plainFile is an instance's file on one store, with no replica to turn
// to: what a metaSource built with no stores reads.
type plainFile struct {
	store blob.Store
	key   string
}

// Read reads the file.
func (f plainFile) Read(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	return f.store.Read(ctx, f.key, offset, length)
}

// Stat stats the file.
func (f plainFile) Stat(ctx context.Context) (blob.Info, error) { return f.store.Stat(ctx, f.key) }

// Store returns the store.
func (f plainFile) Store() blob.Store { return f.store }

// Mismatch records nothing: there is no replica.
func (plainFile) Mismatch(context.Context, error) error { return nil }

// uriFor returns the function that makes the BulkDataURI of the instance
// with the SOP Instance UID uid for a value at an offset and of a length
// in its file (metadata.proto, "Bulk data"). Every part of the path is a
// label or a UID, which hold only characters a path takes as they are.
func (s *metaSource) uriFor(uid string) func(offset, length int64) string {
	base := "/tenants/" + s.tenant + "/instances/" + s.path.Subject + "/" + s.path.Session + "/" + s.path.Scan + "/" + uid
	return func(offset, length int64) string {
		return base + "?offset=" + strconv.FormatInt(offset, 10) + "&length=" + strconv.FormatInt(length, 10)
	}
}

// instance answers the header of the instance held, whose line of the
// metadata file is in. The line must be the instance's: the same key, size
// and archived checksum the catalog holds (of the bytes stored, which for a
// de-identified instance are not those received).
func (s *metaSource) instance(ctx context.Context, held tenant.ScanInstance, in sidecar.Instance) (*v1.InstanceMetadata, error) {
	if err := s.readable(ctx, held); err != nil {
		// Its bytes are not what was archived, nor is its file's header.
		return nil, instanceFailure(err, s.path)
	}
	if in.Key != held.Key || in.Size != held.Size || (!held.ArchivedSHA256.IsZero() && in.SHA256 != held.ArchivedSHA256.String()) {
		return nil, fault.New(fault.DataLoss, messageBadSidecar)
	}
	ra := readat.New(ctx, s.file(held).Store(), held.Key, held.Size)
	// The line names the transfer syntax when it was written by a version
	// that keeps it; an older line is answered from the file's meta group.
	ts := in.TransferSyntax
	if ts == "" {
		var err error
		if ts, err = header.ReadTransferSyntax(ra, held.Size); err != nil {
			return nil, fileFailure(err)
		}
	}
	var err error
	m := &v1.InstanceMetadata{InstanceId: held.SOPInstanceUID, SopInstanceUid: held.SOPInstanceUID, TransferSyntaxUid: ts}
	if !held.Profile.IsZero() {
		m.Profile = &v1.ProfileVersionName{Name: held.Profile.Name, Version: held.Profile.Version}
	}
	deflated := ts == transferSyntaxDeflated
	m.BulkDataOmitted = deflated
	var js []byte
	if !deflated && in.Dataset != nil {
		js = datasetFromSidecar(in, ts, s.uriFor(held.SOPInstanceUID))
	}
	if js == nil {
		var tooLarge bool
		js, tooLarge, err = s.datasetFromFile(ctx, ra, held, deflated)
		if err != nil {
			return nil, err
		}
		if tooLarge {
			m.JsonTooLarge = true
			return m, nil
		}
	}
	if len(js) > MaxInstanceJSON {
		m.JsonTooLarge = true
		return m, nil
	}
	m.DicomJson = string(js)
	return m, nil
}

// datasetFromFile reads the header of the instance's file within the
// header bound and returns its DICOM JSON, and whether the header is too
// large to be read within the bound, in which case there is no JSON.
func (s *metaSource) datasetFromFile(ctx context.Context, ra io.ReaderAt, held tenant.ScanInstance, deflated bool) ([]byte, bool, error) {
	info, err := s.file(held).Stat(ctx)
	if err != nil {
		return nil, false, fileFailure(err)
	}
	if info.Size != held.Size {
		return nil, false, fault.New(fault.DataLoss, "the instance's file is not what was archived")
	}
	h, err := header.ReadWithin(ra, held.Size, s.limit)
	var tooLarge *header.TooLargeError
	if errors.As(err, &tooLarge) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fileFailure(err)
	}
	js, err := h.DICOMJSONBulk(s.uriFor(held.SOPInstanceUID), deflated)
	if err != nil {
		return nil, false, fault.Wrap(fault.DataLoss, err, "the instance's file is not what was archived")
	}
	return js, false, nil
}

// fileFailure returns the error of reading an instance's file: a file that
// is missing, shorter than archived or not a DICOM file is data loss, since
// the catalog holds it; anything else is the storage not answering.
func fileFailure(err error) error {
	var notDICOM *header.NotDICOMError
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return fault.Wrap(fault.DataLoss, err, "the instance's file is missing from the storage")
	case errors.As(err, &notDICOM), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return fault.Wrap(fault.DataLoss, err, "the instance's file is not what was archived")
	}
	return storageFailure(err, "the instance's file cannot be read")
}

// headerLength returns how long the header of a long element is in a file
// of the transfer syntax ts (PS3.5 7.1), and whether the syntax is one the
// standard registers, which are the ones this is certain of.
func headerLength(ts string) (int64, bool) {
	switch {
	case ts == transferSyntaxImplicit:
		return 8, true
	case strings.HasPrefix(ts, transferSyntaxRoot):
		return 12, true
	}
	return 0, false
}
