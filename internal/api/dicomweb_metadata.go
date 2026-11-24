package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/amrshadid/go-dicom/dataelem"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// metadata answers the metadata of a study, a series or an instance
// (PS3.18 10.4.1.1.2): a JSON array of each instance's DICOM JSON, read as
// the metadata service reads it, from one read of each scan's metadata
// file a page of instances at a time, so a study is never held whole.
// Each BulkDataURI names the bulk data resource of the instance (PS3.18
// F.2.6), which answers the same bytes the plain download's window does.
// An instance whose DICOM JSON is longer than one answer holds
// (MaxInstanceJSON) is answered with its UIDs alone.
func (x *webRequest) metadata(ctx context.Context) error {
	media, perr := dicomweb.AcceptJSON(x.r)
	if perr != nil {
		return x.refuse(perr)
	}
	t, err := x.locate(ctx)
	if err != nil {
		return x.fail(err)
	}
	if err := t.marked(); err != nil {
		// One instance a scrub marked: data loss before the answer's first
		// byte, as its retrieve answers. Of a study or a series, the
		// stream ends at the marked instance (scanMetadata).
		return x.fail(err)
	}
	stream, stop, err := x.openStream(ctx, func() {
		x.w.Header().Set("Content-Type", media)
		x.w.WriteHeader(http.StatusOK)
	})
	if err != nil {
		return x.fail(err)
	}
	defer stop()
	if x.r.Method == http.MethodHead {
		return stream.end(nil, 0)
	}
	ctx = stream.ctx
	sent := 0
	sep := "["
	write := func(js string) error {
		_, err := io.WriteString(stream, sep+js)
		sep = ","
		return err
	}
	scans := t.scans
	for {
		s, ok, err := scans.next(ctx)
		if err != nil {
			return stream.end(err, sent)
		}
		if !ok {
			break
		}
		n, err := x.scanMetadata(ctx, s, t.instance, write)
		sent += n
		if err != nil {
			return stream.end(err, sent)
		}
	}
	if sent == 0 {
		return stream.end(fault.New(fault.NotFound, messageNoStudy), 0)
	}
	if _, err := io.WriteString(stream, "]"); err != nil {
		return stream.end(err, sent)
	}
	return stream.end(nil, sent)
}

// scanMetadata writes the DICOM JSON of the instances of one scan, or of
// the one instance when only is not nil, and returns how many it wrote.
func (x *webRequest) scanMetadata(ctx context.Context, s tenant.CatalogScan, only *tenant.ScanInstance, write func(string) error) (int, error) {
	path := pathOf(s)
	src, closeSrc, err := x.c.metadataSource(ctx, x.p, x.tenant, s.Storage, path)
	if err != nil {
		return 0, err
	}
	defer closeSrc()
	sess, err := x.p.CatalogSession(ctx, s.SubjectLabel, s.SessionLabel)
	if err != nil {
		return 0, err
	}
	var held iterator[tenant.ScanInstance]
	if only != nil {
		held = &rowsOf[tenant.ScanInstance]{rows: []tenant.ScanInstance{*only}}
	} else {
		held = x.heldOf(s, master.MaxPageLimit)
	}
	n := 0
	var batch []tenant.ScanInstance
	flush := func() error {
		for len(batch) > 0 {
			got, err := src.page(ctx, batch)
			if err != nil {
				return err
			}
			for i, m := range got {
				if err := write(x.webJSON(src, sess, s, batch[i], m)); err != nil {
					return err
				}
				n++
			}
			batch = batch[len(got):]
		}
		return nil
	}
	for {
		in, ok, err := held.next(ctx)
		if err != nil {
			return n, err
		}
		if !ok {
			return n, flush()
		}
		batch = append(batch, in)
		if len(batch) == master.MaxPageLimit {
			if err := flush(); err != nil {
				return n, err
			}
		}
	}
}

// webJSON returns an instance's DICOM JSON as the retrieve service answers
// it: the metadata service's, with each BulkDataURI made the absolute URI
// of the instance's bulk data resource, the same place in its query; or,
// for an instance whose JSON is too long, its UIDs.
func (x *webRequest) webJSON(src *metaSource, sess tenant.CatalogSession, s tenant.CatalogScan, held tenant.ScanInstance, m *v1.InstanceMetadata) string {
	instance := x.base() + "/studies/" + sess.StudyUID + "/series/" + s.SeriesUID + "/instances/" + held.SOPInstanceUID
	if m.GetJsonTooLarge() || m.GetDicomJson() == "" {
		r := dicomweb.NewResult()
		r.Set(dicomweb.TagSOPInstanceUID, dataelem.UI, held.SOPInstanceUID)
		r.Set(dicomweb.TagSeriesInstanceUID, dataelem.UI, s.SeriesUID)
		r.Set(dicomweb.TagStudyInstanceUID, dataelem.UI, sess.StudyUID)
		b, err := json.Marshal(r)
		if err != nil {
			return "{}"
		}
		return string(b)
	}
	// The metadata service writes each URI as the plain download's path
	// and a query; every part of that path is a label or a UID, which JSON
	// writes as they are, so the path is found as written.
	from := src.uriFor(held.SOPInstanceUID)(0, 0)
	from = `"` + from[:strings.IndexByte(from, '?')+1]
	return strings.ReplaceAll(m.GetDicomJson(), from, `"`+instance+"/bulkdata?")
}
