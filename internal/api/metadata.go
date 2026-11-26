package api

import (
	"context"
	"errors"
	"io"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The bounds of an answer's DICOM JSON (metadata.proto, "Size").
const (
	// MaxInstanceJSON is the longest DICOM JSON answered for one instance,
	// with a BulkDataURI in place of every value that is bulk data; an
	// instance whose JSON would be longer is answered without it.
	MaxInstanceJSON = 1 << 20
	// MaxPageJSON is the most DICOM JSON a page holds in all, so that with
	// the rest of the message it stays under the 4 MB a stock gRPC client
	// accepts.
	MaxPageJSON = 3 << 20
)

// The messages of what the metadata methods find missing.
const (
	messageNoScan     = "the tenant has no scan with these labels"
	messageNoInstance = "the scan has no instance with this id"
	messageNoSidecar  = "the scan's metadata file is missing from the storage"
	messageBadSidecar = "the scan's metadata file is not what was archived"
)

// detailInstances is the audit detail that counts the instances a page
// answered.
const detailInstances = "instances"

// metadataService is MetadataService: a scan's headers, read from the
// metadata file the scan is archived with and, for what that file does not
// hold, from the instances' files.
type metadataService struct{ c *core }

var _ nopacsv1connect.MetadataServiceHandler = metadataService{}

// scanPath returns the path the request names, checked.
func scanPath(subject, session, scan string) (label.Path, error) {
	p := label.Path{Subject: subject, Session: session, Scan: scan}
	if err := tenant.ValidateScanPath(p); err != nil {
		return p, err
	}
	return p, nil
}

// lookupScan reads a scan of the catalog, answering one the tenant does not
// have as NotFound with the contract's message.
func lookupScan(ctx context.Context, p tenant.Pipeline, path label.Path) (tenant.CatalogScan, error) {
	scan, err := p.CatalogScan(ctx, path)
	if fault.KindOf(err) == fault.NotFound {
		return scan, relabeled(ctx, p, path, fault.Wrap(fault.NotFound, err, messageNoScan))
	}
	return scan, err
}

// GetScanMetadata answers the headers of a page of a scan's instances.
func (s metadataService) GetScanMetadata(ctx context.Context, req *v1.GetScanMetadataRequest) (*v1.GetScanMetadataResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.MetadataServiceGetScanMetadataProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	path, err := scanPath(req.GetSubjectLabel(), req.GetSessionLabel(), req.GetScanLabel())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	scan, err := lookupScan(ctx, p, path)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	held, next, err := p.ListHeldInstances(ctx, path, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	for _, in := range held {
		if err := refuseAsReceived(d, in); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	out := &v1.GetScanMetadataResponse{ManifestSha256: scan.ManifestSHA256.String()}
	// The storages are this method's, so its audit entry names those that
	// served it, a replica among them when the primary could not.
	stores := c.newReadStores(p, req.GetTenant())
	defer stores.Close()
	if len(held) > 0 {
		src, _, err := c.metadataSource(ctx, p, req.GetTenant(), scan, path, stores)
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		out.Instances, err = src.page(ctx, held)
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	if len(out.Instances) < len(held) {
		// The page's DICOM JSON reached its bound before its last
		// instance: the catalog's cursor, which resumes after the last row
		// of a page of that size, is asked again for the page that was
		// answered, which is the same rows.
		next, err = resume(ctx, p, path, page.Cursor, held[:len(out.Instances)])
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	out.NextPageToken = next
	c.record(d, st, master.OutcomeDone, "", access.Facts{Details: withDetails(map[string]string{detailInstances: strconv.Itoa(len(out.Instances))}, servedDetails(stores))})
	return out, nil
}

// resume returns the cursor that resumes after the last of answered, the
// first instances of the page that began at cursor, by listing that page
// again with their count.
func resume(ctx context.Context, p tenant.InstanceReads, path label.Path, cursor string, answered []tenant.ScanInstance) (string, error) {
	got, next, err := p.ListHeldInstances(ctx, path, master.Page{Cursor: cursor, Limit: len(answered)})
	if err != nil {
		return "", listError(err, cursor)
	}
	if len(got) != len(answered) || got[len(got)-1].SOPInstanceUID != answered[len(answered)-1].SOPInstanceUID {
		return "", fault.New(fault.Unavailable, "the scan changed while its page was read; ask for it again")
	}
	return next, nil
}

// GetInstanceMetadata answers the header of one instance of a scan.
func (s metadataService) GetInstanceMetadata(ctx context.Context, req *v1.GetInstanceMetadataRequest) (*v1.GetInstanceMetadataResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.MetadataServiceGetInstanceMetadataProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	path, err := scanPath(req.GetSubjectLabel(), req.GetSessionLabel(), req.GetScanLabel())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if req.GetInstanceId() == "" {
		return nil, c.failIn(d, st, invalid("instance_id", "is required"))
	}
	if err := tenant.ValidateInstanceUID(req.GetInstanceId()); err != nil {
		return nil, c.failIn(d, st, fieldOf("instance_id", err))
	}
	scan, err := lookupScan(ctx, p, path)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	held, err := p.HeldInstance(ctx, path, req.GetInstanceId())
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoInstance)
	}
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := refuseAsReceived(d, held); err != nil {
		return nil, c.failIn(d, st, err)
	}
	// The storages are this method's, so its audit entry names those that
	// served it, a replica among them when the primary could not.
	stores := c.newReadStores(p, req.GetTenant())
	defer stores.Close()
	src, _, err := c.metadataSource(ctx, p, req.GetTenant(), scan, path, stores)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := src.readable(ctx, held); err != nil {
		return nil, c.failIn(d, st, instanceFailure(err, path))
	}
	got, err := src.page(ctx, []tenant.ScanInstance{held})
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	// The details given replace the request's, so its own (an instance
	// named, never which) is given again beside the storages.
	c.record(d, st, master.OutcomeDone, "", access.Facts{Details: withDetails(map[string]string{detailInstance: presentValue}, servedDetails(stores))})
	return &v1.GetInstanceMetadataResponse{Instance: got[0]}, nil
}

// metadataSource returns what answers the headers of the scan from the
// storage it is on, or from a replica in step when that storage cannot
// serve the scan's metadata file or an instance's file (checkedread), with
// the function that ends it. The storages are those of stores, which the
// caller closes; nil gives the source storages of its own, closed by the
// function returned. A storage that cannot be opened is the server's or the
// storage's, as the opener says, met at the first read.
func (c *core) metadataSource(ctx context.Context, p tenant.Pipeline, tenantName string, scan tenant.CatalogScan, path label.Path, stores *checkedread.Stores) (*metaSource, func(), error) {
	limit, err := c.values().HeaderLimit(ctx)
	if err != nil {
		return nil, nil, err
	}
	key, err := layout.MetadataKey(path)
	if err != nil {
		return nil, nil, fault.Wrap(fault.Internal, err, "api: the scan's metadata key cannot be made")
	}
	end := func() {}
	if stores == nil {
		stores = c.newReadStores(p, tenantName)
		end = stores.Close
	}
	file := stores.Source(checkedread.Object{Storage: scan.Storage, Key: key, SeriesUID: scan.SeriesUID, Path: path})
	src := &metaSource{store: file.Store(), stores: stores, storage: scan.Storage, series: scan.SeriesUID,
		tenant: tenantName, path: path, key: key, limit: limit}
	return src, end, nil
}

// page answers the headers of held, the instances of a page in order of
// SOP Instance UID, from one read of the scan's metadata file. It stops
// before the instance that would take the page's DICOM JSON past MaxPageJSON
// and returns the instances answered, which are at least one. A metadata
// file that is missing, is not in its form, or does not hold an instance
// the catalog holds, is data loss; so is a page holding an instance a
// scrub marked that no replica in step serves (metaSource.readable),
// refused before any read.
func (s *metaSource) page(ctx context.Context, held []tenant.ScanInstance) ([]*v1.InstanceMetadata, error) {
	for _, h := range held {
		if err := s.readable(ctx, h); err != nil {
			// A scrub found its bytes missing or not what was archived:
			// what its header says is not answered as the archive's, as
			// none of its bytes is, before anything is read.
			return nil, instanceFailure(err, s.path)
		}
	}
	rd, rc, first, err := s.lineOf(ctx, held[0].SOPInstanceUID, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var (
		out   []*v1.InstanceMetadata
		total int
	)
	for i, h := range held {
		in := first
		if i > 0 {
			if in, err = nextInstance(rd, h.SOPInstanceUID); err != nil {
				return nil, err
			}
		}
		m, err := s.instance(ctx, h, in)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 && total+len(m.GetDicomJson()) > MaxPageJSON {
			break
		}
		total += len(m.GetDicomJson())
		out = append(out, m)
	}
	return out, nil
}

// lineOf opens the scan's metadata file at the place its index gives for
// the instance uid and reads forward to that instance's line. It returns
// the reader, which goes on from the line after it, the open body for the
// caller to close, and the line. wrap, when not nil, wraps the body before
// it is read, for a caller that counts what it reads.
//
// The index and the lines are two reads of the file, and the frame pass
// and an archive into the scan replace the file whole between any two
// reads: an offset of the old file's index is then a place in the middle
// of a line, or at another line, of the new one. A read from an index's
// offset that does not find the line where it should be is made once more
// with the index read again; a second failure, or a failure of a read
// from the first line, which no rewrite explains, is data loss.
func (s *metaSource) lineOf(ctx context.Context, uid string, wrap func(io.Reader) io.Reader) (*sidecar.Reader, io.ReadCloser, sidecar.Instance, error) {
	for try := 0; ; try++ {
		start := s.lineStart(ctx, uid)
		rc, err := s.store.Read(ctx, s.key, start, -1)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			return nil, nil, sidecar.Instance{}, fault.Wrap(fault.DataLoss, err, messageNoSidecar)
		case err != nil:
			return nil, nil, sidecar.Instance{}, storageFailure(err, "the scan's metadata file cannot be read")
		}
		var body io.Reader = rc
		if wrap != nil {
			body = wrap(rc)
		}
		rd := sidecar.NewReader(body)
		if start > 0 {
			rd = sidecar.NewReaderFrom(body)
		}
		in, err := nextInstance(rd, uid)
		if err == nil {
			return rd, rc, in, nil
		}
		_ = rc.Close()
		if try > 0 || start == 0 || fault.KindOf(err) != fault.DataLoss {
			return nil, nil, in, err
		}
	}
}

// The tail of the metadata file the index is looked for in, first this many
// bytes and then twice as many until the index line is whole, and the
// smallest file worth a look at its index: one that is read through in less
// time than the lookup takes.
const (
	indexTail      = 16 << 10
	indexTailMax   = 16 << 20
	indexMinimumAt = 256 << 10
)

// lineStart returns where to start reading the metadata file for the
// instance uid: the offset the file's index gives, or 0 for a file with no
// index or one that cannot be read, which is then read from its first line,
// so every failure shows in that read.
func (s *metaSource) lineStart(ctx context.Context, uid string) int64 {
	info, err := s.store.Stat(ctx, s.key)
	if err != nil || info.Size < indexMinimumAt {
		return 0
	}
	for n := int64(indexTail); ; n *= 2 {
		n = min(n, info.Size)
		rc, err := s.store.Read(ctx, s.key, info.Size-n, n)
		if err != nil {
			return 0
		}
		tail, err := io.ReadAll(io.LimitReader(rc, n))
		_ = rc.Close()
		if err != nil || int64(len(tail)) != n {
			return 0
		}
		entries, complete := sidecar.ParseIndex(tail, n == info.Size)
		switch {
		case complete && entries == nil:
			return 0
		case complete:
			return sidecar.Locate(entries, uid)
		case n >= min(info.Size, indexTailMax):
			return 0
		}
	}
}

// nextInstance reads the metadata file forward to the line of the instance
// with the SOP Instance UID uid. The file holds the instances of the scan
// in order, and may hold some the catalog does not (a reception wrote them
// and did not confirm them), which are passed over; one the catalog holds
// and the file does not is data loss.
func nextInstance(rd *sidecar.Reader, uid string) (sidecar.Instance, error) {
	for {
		in, _, err := rd.Next()
		switch {
		case errors.Is(err, io.EOF):
			return in, fault.New(fault.DataLoss, messageBadSidecar)
		case errors.Is(err, sidecar.ErrFormat), errors.Is(err, sidecar.ErrOrder):
			return in, fault.Wrap(fault.DataLoss, err, messageBadSidecar)
		case err != nil:
			return in, storageFailure(err, "the scan's metadata file cannot be read")
		case in.SOPInstanceUID == uid:
			return in, nil
		case in.SOPInstanceUID > uid:
			return in, fault.New(fault.DataLoss, messageBadSidecar)
		}
	}
}

// storageFailure returns a storage's failure with its kind: one the
// storage does not classify is the storage not answering, never an
// internal fault, which only the server's own code makes.
func storageFailure(err error, message string) error {
	if errors.Is(err, blob.ErrNotOnline) {
		return notOnline(err, message)
	}
	k := fault.KindOf(err)
	if k == fault.Internal {
		k = fault.Unavailable
	}
	return fault.Wrap(k, err, message)
}
