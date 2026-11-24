package receive

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Checker is the check after de-identification (package check): a read of
// the de-identified file separate from the engine's, which searches every
// element left, and the raw bytes, for the values the profile removed (the
// engine's needles). It is a verdict, never an edit. Its error decides the
// file: nil passes it; a *check.Residual holds it as residual_value, a
// *check.Refusal as deid_failed; any other error says nothing of the file,
// and the step is tried again.
type Checker interface {
	// CheckWritten reads the de-identified file, size bytes of src, for
	// needles; written is the record of the de-identification the same
	// run of the engine wrote (engine.Record.Written), not searched where
	// the file holds exactly it; characterSet is the received file's
	// Specific Character Set, as the engine was given it.
	CheckWritten(ctx context.Context, src io.ReaderAt, size int64, needles engine.Needles, written engine.Written, characterSet *string) (check.Report, error)
}

// DeidKeys returns the tenant's keys of de-identification: its UID key,
// date key and naming key, as bytes. The pipeline asks once and keeps
// them, since a tenant's secrets are never replaced.
type DeidKeys func(ctx context.Context) (engine.Keys, error)

// errNoKeys is a file at a receiver that de-identifies when the pipeline
// was given no keys: it is not recorded until it can be, never recorded as
// received.
var errNoKeys = fault.New(fault.Internal, "the reception pipeline is given no keys of de-identification, and a receiver de-identifies")

// errNoCheck is a de-identified file when the pipeline was given no check
// after de-identification: it is never archived unchecked.
var errNoCheck = fault.New(fault.Internal, "the reception pipeline is given no check after de-identification, and a receiver de-identifies")

// deid is what the pipeline keeps to de-identify: the tenant's keys, read
// once, and an engine per profile version, compiled once (a version never
// changes).
type deid struct {
	mu      sync.Mutex
	keys    *engine.Keys
	engines map[tenant.ProfileRef]*engine.Engine
	// sums are the checksums of the expanded texts of the versions in
	// engines, which name each version whatever database holds it.
	sums map[tenant.ProfileRef]string
	// pixelLimit is the pixel bound last read and when (pixelLimit).
	pixelLimit liveLimit
}

// deidKeys returns the tenant's keys of de-identification, read once.
func (p *Pipeline) deidKeys(ctx context.Context) (engine.Keys, error) {
	p.deid.mu.Lock()
	defer p.deid.mu.Unlock()
	if p.deid.keys != nil {
		return *p.deid.keys, nil
	}
	if p.cfg.DeidKeys == nil {
		return engine.Keys{}, errNoKeys
	}
	k, err := p.cfg.DeidKeys(ctx)
	if err != nil {
		return engine.Keys{}, err
	}
	if len(k.UID) < engine.MinKeyLength {
		return engine.Keys{}, fault.New(fault.Internal, "the tenant's UID key is shorter than a key")
	}
	p.deid.keys = &k
	return k, nil
}

// errProfileUnusable is a stored profile version this build cannot run:
// the file is held, never archived as it is.
var errProfileUnusable = errors.New("the profile version cannot run in this build")

// engineFor returns the engine of a profile version, compiled once. A
// version the build cannot run is errProfileUnusable; a database that
// cannot be read is its own error.
func (p *Pipeline) engineFor(ctx context.Context, ref tenant.ProfileRef) (*engine.Engine, error) {
	p.deid.mu.Lock()
	en := p.deid.engines[ref]
	p.deid.mu.Unlock()
	if en != nil {
		return en, nil
	}
	keys, err := p.deidKeys(ctx)
	if err != nil {
		return nil, err
	}
	store, ok := p.store.(tenant.Deidentification)
	if !ok {
		return nil, fault.New(fault.Internal, "the tenant's database keeps no profiles")
	}
	v, err := store.ProfileVersion(ctx, ref.Name, ref.Version)
	if fault.KindOf(err) == fault.NotFound {
		return nil, errProfileUnusable
	}
	if err != nil {
		return nil, err
	}
	// Loaded through the profile language, which checks the expanded text
	// against its checksum: a version changed in the database after it was
	// made is never run.
	version, err := profile.Load(v.Name, v.Version, v.Source, v.Expanded, v.Checksum)
	if err != nil {
		return nil, errProfileUnusable
	}
	en, err = engine.New(version.Expanded(), keys, engine.Options{Lookups: lookups{store: store, now: p.now}, Method: ref.String()})
	if err != nil {
		return nil, errProfileUnusable
	}
	p.deid.mu.Lock()
	if p.deid.engines == nil {
		p.deid.engines = map[tenant.ProfileRef]*engine.Engine{}
	}
	p.deid.engines[ref] = en
	if p.deid.sums == nil {
		p.deid.sums = map[tenant.ProfileRef]string{}
	}
	p.deid.sums[ref] = v.Checksum
	p.deid.mu.Unlock()
	return en, nil
}

// profileSum returns the checksum of a profile version's expanded text when
// the version has run in this process, which it has for any file it
// de-identified, and "" otherwise: a line then holds no checksum for it
// rather than one read for the purpose.
func (p *Pipeline) profileSum(ref tenant.ProfileRef) string {
	p.deid.mu.Lock()
	defer p.deid.mu.Unlock()
	return p.deid.sums[ref]
}

// lookups reaches the tenant's lookup tables for the engine.
type lookups struct {
	store tenant.LookupTables
	now   func() time.Time
}

// Code returns the code a table holds for a key.
func (l lookups) Code(ctx context.Context, table, key string) (string, bool, error) {
	return l.store.LookupCode(ctx, tenant.LookupKey{Table: table, Key: key})
}

// Assign returns the codes the tables hold for keys, assigning the missing
// ones, all at once.
func (l lookups) Assign(ctx context.Context, keys []engine.LookupKey) ([]string, error) {
	ks := make([]tenant.LookupKey, len(keys))
	for i, k := range keys {
		ks[i] = tenant.LookupKey{Table: k.Table, Key: k.Key}
	}
	return l.store.AssignLookupCodes(ctx, ks, l.now())
}

// remapPlacingUID returns the UID that replaces a placing UID (Study,
// Series or SOP Instance UID) of a file at a receiver that de-identifies:
// the engine's keyed remapping, the one the de-identified file holds, so
// every row of the database holds what the archived file does and nothing
// the original could be matched by. There is no exception for a UID under
// the standard's root: no instance of a patient's data is named by one
// (a file that names itself so is held as bad_uid before this), and an
// exception here would let the original's UID into the rows.
func remapPlacingUID(keys engine.Keys, u string) string {
	if u == "" {
		return u
	}
	return engine.RemapUID(keys.UID, u)
}

// underStandardRoot reports whether u is under the root of the UIDs the
// standard itself issues (PS3.5 9.1, "1.2.840.10008."): never the UID of a
// study, series or instance of a patient.
func underStandardRoot(u string) bool { return strings.HasPrefix(u, standardRoot) }

// standardRoot is the root of the UIDs DICOM issues (PS3.5 9.1).
const standardRoot = "1.2.840.10008."

// keyedSum returns the received checksum of a file at a receiver that
// de-identifies: HMAC-SHA-256 under the tenant's UID key of the SHA-256 of
// its bytes as received, so a file sent twice still has one received
// checksum, and the database holds no checksum an original file could be
// matched against without the key. The prefix keeps it apart from a UID
// remapped under the same key, which never holds a NUL.
func keyedSum(keys engine.Keys, sum tenant.SHA256) tenant.SHA256 {
	m := hmac.New(sha256.New, keys.UID)
	m.Write([]byte("received-file\x00"))
	m.Write(sum[:])
	var out tenant.SHA256
	copy(out[:], m.Sum(nil))
	return out
}

// protect remaps the placing UIDs and keys the checksum of a hold record
// made at a receiver that de-identifies, so a quarantine row holds nothing
// the original could be matched by either; only the bytes it keeps in the
// incoming area are the original's. Its SOP Class UID stays as received,
// as in the received file's row and, under PS3.15 Annex E's basic profile,
// in the archived file: a class names a kind of object, not a patient, and
// a person compares it with a receiver's list of classes.
func protect(q *tenant.QuarantinedFile, keys engine.Keys) {
	q.Deidentified = true
	q.SOPInstanceUID = remapPlacingUID(keys, q.SOPInstanceUID)
	q.StudyUID = remapPlacingUID(keys, q.StudyUID)
	q.SeriesUID = remapPlacingUID(keys, q.SeriesUID)
	if !q.SHA256.IsZero() {
		q.SHA256 = keyedSum(keys, q.SHA256)
	}
}

// The tags the de-identified file's placing UIDs are read from (PS3.6).
var (
	dsSOPClassUID    = tag.New(0x0008, 0x0016)
	dsSOPInstanceUID = tag.New(0x0008, 0x0018)
	dsStudyUID       = tag.New(0x0020, 0x000D)
	dsSeriesUID      = tag.New(0x0020, 0x000E)
)

// dsUID returns a top-level UID of a data set, its padding removed.
func dsUID(ds *dataset.Dataset, t tag.Tag) string {
	elem, ok := ds.Get(t)
	if !ok {
		return ""
	}
	b, ok := elem.Bytes()
	if !ok {
		return ""
	}
	return strings.TrimRight(string(b), "\x00 ")
}

// errWrite is the DICOM library refusing to write a de-identified data
// set: the file is held (unwritable), never archived as it was.
type errWrite struct{ err error }

func (e errWrite) Error() string { return "the de-identified data set cannot be written" }
func (e errWrite) Unwrap() error { return e.err }

// writeDataset writes ds as a Part 10 file in the transfer syntax ts to w,
// its values left in the file copied from source (PS3.10 7.1: a zeroed
// preamble, the DICM prefix and a file meta information of the library's,
// whose Media Storage SOP Class and Instance UIDs are the data set's). The
// writer reads no clock and no random source, so the same data set always
// gives the same bytes.
func writeDataset(w io.WriteSeeker, ds *dataset.Dataset, ts string, source io.ReaderAt, littleEndian bool) error {
	bw := &bufferedSeeker{w: w, b: bufio.NewWriterSize(w, 256<<10)}
	fw := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(bw))
	fw.SetFileMetaInfo(&filewriter.FileMetaInfo{
		MediaStorageSOPClassUID:    dsUID(ds, dsSOPClassUID),
		MediaStorageSOPInstanceUID: dsUID(ds, dsSOPInstanceUID),
		TransferSyntaxUID:          ts,
	})
	fw.SetValueSource(source, littleEndian)
	if err := fw.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		return errWrite{err}
	}
	if err := fw.Write(); err != nil {
		return errWrite{err}
	}
	// Not the writer's Close, which would close w: the caller reads what
	// was written back from it.
	return bw.b.Flush()
}

// bufferedSeeker buffers the writer's many small writes (a tag, a length,
// a value each) into few, and writes what it holds before a seek, which
// the writer makes to fill in a length it learns late.
type bufferedSeeker struct {
	w io.WriteSeeker
	b *bufio.Writer
}

// Write buffers p.
func (s *bufferedSeeker) Write(p []byte) (int, error) { return s.b.Write(p) }

// Seek writes what is buffered, then seeks.
func (s *bufferedSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := s.b.Flush(); err != nil {
		return 0, err
	}
	return s.w.Seek(offset, whence)
}
