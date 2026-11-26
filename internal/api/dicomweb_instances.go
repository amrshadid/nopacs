package api

import (
	"context"
	"net/http"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// instanceRow is an instance a search reads: the scan that holds it and
// its row of the catalog.
type instanceRow struct {
	scan tenant.CatalogScan
	held tenant.ScanInstance
}

// scanInstances iterates the instances of the scans another iterator
// yields, scan after scan, each in order of SOP Instance UID.
type scanInstances struct {
	x     *webRequest
	scans iterator[tenant.CatalogScan]
	size  int
	scan  tenant.CatalogScan
	cur   iterator[tenant.ScanInstance]
}

// next returns the next instance.
func (it *scanInstances) next(ctx context.Context) (instanceRow, bool, error) {
	for {
		if it.cur != nil {
			in, ok, err := it.cur.next(ctx)
			if err != nil {
				return instanceRow{}, false, err
			}
			if ok {
				return instanceRow{scan: it.scan, held: in}, true, nil
			}
		}
		s, ok, err := it.scans.next(ctx)
		if err != nil || !ok {
			return instanceRow{}, false, err
		}
		it.scan, it.cur = s, it.x.heldOf(s, it.size)
	}
}

// heldOf iterates the instances a scan holds. A page that holds an
// instance archived as received ends a masked key's request, before
// anything of it is read (refuseAsReceived).
func (x *webRequest) heldOf(s tenant.CatalogScan, size int) iterator[tenant.ScanInstance] {
	path := pathOf(s)
	return pagesOf(func(ctx context.Context, page master.Page) ([]tenant.ScanInstance, string, error) {
		held, next, err := x.p.ListHeldInstances(ctx, path, page)
		if err != nil {
			return nil, "", err
		}
		for _, in := range held {
			if err := refuseAsReceived(x.d, in); err != nil {
				return nil, "", err
			}
		}
		return held, next, nil
	}, size)
}

// heldInstance reads one instance a scan holds, as HeldInstance does,
// refused to a masked key when it was archived as received.
func (x *webRequest) heldInstance(ctx context.Context, s tenant.CatalogScan, uid string) (tenant.ScanInstance, error) {
	in, err := x.p.HeldInstance(ctx, pathOf(s), uid)
	if err != nil {
		return in, err
	}
	return in, refuseAsReceived(x.d, in)
}

// searchInstances answers a search of instances: of one series, of one
// study, or of the tenant, the scans read by label and each scan's
// instances in order of SOP Instance UID. A key other than the UIDs is
// matched on the instance's line of its scan's metadata file, which is
// read forward with the instances, so a scan's file is read once.
func (x *webRequest) searchInstances(ctx context.Context, q dicomweb.Query, ls *lines) (searchAnswer, error) {
	studyKey, byStudy := q.Key(dicomweb.TagStudyInstanceUID)
	seriesKey, bySeries := q.Key(dicomweb.TagSeriesInstanceUID)
	sopKey, bySOP := q.Key(dicomweb.TagSOPInstanceUID)
	if (byStudy && studyKey.Match != dicomweb.MatchSingle) || (bySeries && seriesKey.Match != dicomweb.MatchSingle) {
		return searchAnswer{}, &dicomweb.Error{Status: http.StatusBadRequest, Message: "a search of instances names at most one study and one series"}
	}
	var it iterator[instanceRow]
	switch {
	case bySeries:
		var (
			s   tenant.CatalogScan
			err error
		)
		if byStudy {
			s, err = x.scan(ctx, studyKey.Value, seriesKey.Value)
		} else {
			var ok bool
			if s, ok, err = x.scanBySeries(ctx, seriesKey.Value); err == nil && !ok {
				err = fault.New(fault.NotFound, messageNoSeries)
			}
		}
		if err != nil {
			return searchAnswer{}, err
		}
		if bySOP {
			var rows []instanceRow
			for _, u := range uidsOf(sopKey) {
				in, err := x.heldInstance(ctx, s, u)
				if fault.KindOf(err) == fault.NotFound {
					continue
				}
				if err != nil {
					return searchAnswer{}, err
				}
				rows = append(rows, instanceRow{scan: s, held: in})
			}
			it = &rowsOf[instanceRow]{rows: rows}
			break
		}
		it = &scanInstances{x: x, scans: &rowsOf[tenant.CatalogScan]{rows: []tenant.CatalogScan{s}}, size: pageSize(q)}
	case byStudy:
		sess, err := x.session(ctx, studyKey.Value)
		if err != nil {
			return searchAnswer{}, err
		}
		it = &scanInstances{x: x, size: pageSize(q),
			scans: x.scans(tenant.ScanFilter{SubjectLabel: sess.SubjectLabel, SessionLabel: sess.Label, Order: tenant.OrderLabel}, master.MaxPageLimit)}
	default:
		it = &scanInstances{x: x, size: pageSize(q), scans: x.scans(tenant.ScanFilter{Order: tenant.OrderLabel}, master.MaxPageLimit)}
	}
	// The length of each kept row's data set is what its result may take:
	// the result holds the data set's members and what the catalog says,
	// never more. A row whose line a key read has it from that read; the
	// others are read once the rows are found, so the rows a search passes
	// over by its offset are never read for it.
	cost := map[string]int64{}
	lineOf := func(row instanceRow) (sidecar.Instance, error) {
		sl, err := ls.of(ctx, row.scan)
		if err != nil {
			return sidecar.Instance{}, err
		}
		line, err := sl.line(ctx, row.held.SOPInstanceUID)
		if err != nil {
			return line, err
		}
		cost[row.held.Key] = int64(len(line.Dataset)) + webResultOverhead
		return line, nil
	}
	keep := func(row instanceRow) (bool, error) {
		var line *sidecar.Instance
		for _, k := range q.Keys {
			var vs []string
			switch k.Tag {
			case dicomweb.TagStudyInstanceUID:
				// The study's scans are the only ones read.
				continue
			case dicomweb.TagSeriesInstanceUID:
				vs = []string{row.scan.SeriesUID}
			case dicomweb.TagSOPInstanceUID:
				vs = []string{row.held.SOPInstanceUID}
			default:
				if line == nil {
					in, err := lineOf(row)
					if err != nil {
						return false, err
					}
					line = &in
				}
				vs = dicomweb.ElementValues(datasetMembers(line.Dataset)[k.Tag.Hex()])
			}
			if !k.Matches(vs...) {
				return false, nil
			}
		}
		return true, nil
	}
	rows, more, err := walk(ctx, it, keep, q.Offset, q.Limit)
	if err != nil {
		return searchAnswer{}, err
	}
	costs := make([]int64, len(rows))
	for i, row := range rows {
		c, ok := cost[row.held.Key]
		if !ok {
			if _, err := lineOf(row); err != nil {
				return searchAnswer{}, err
			}
			c = cost[row.held.Key]
		}
		costs[i] = c
	}
	if n := answerFits(costs, webMaxAnswer); n < len(rows) {
		rows, more = rows[:n], true
	}
	want := q.Returned()
	all := q.AllAttributes()
	return searchAnswer{n: len(rows), more: more, each: func(ctx context.Context, yield func(*dicomweb.Result) error) error {
		sessions := sessionsOf(x)
		for _, row := range rows {
			s := row.scan
			sess, err := sessions(ctx, s)
			if err != nil {
				return err
			}
			sl, err := ls.of(ctx, s)
			if err != nil {
				return err
			}
			line, err := sl.line(ctx, row.held.SOPInstanceUID)
			if err != nil {
				return err
			}
			r := dicomweb.NewResult()
			x.instanceAttributes(r, sess, s, row.held, line, want)
			x.seriesAttributes(r, sess, s, want)
			x.studyAttributes(r, sess, want)
			r.FromDataset(datasetMembers(line.Dataset), func(key string) bool {
				if pixelMembers[key] {
					return false
				}
				return all || wantKey(want, key)
			})
			if err := yield(r); err != nil {
				return err
			}
		}
		return nil
	}}, nil
}

// The bound of a search's answer of instances, whose results may each hold
// a data set of up to sidecar.MaxDataset (includefield=all).
const (
	// webMaxAnswer is the most bytes of results one answer holds: the
	// rows past it are left for the next page, with the Warning that more
	// match (PS3.18 8.3.4.4), as rows past the limit are.
	webMaxAnswer = 64 << 20
	// webResultOverhead is what a result may take beyond its data set's
	// members: the attributes the catalog answers and the Retrieve URL.
	webResultOverhead = 4 << 10
)

// answerFits returns how many of the results whose sizes are costs, in
// order, fit in max bytes together: always the first, so an answer of one
// large result is answered, and then as many as stay within max.
func answerFits(costs []int64, max int64) int {
	total := int64(0)
	for i, c := range costs {
		if i > 0 && total+c > max {
			return i
		}
		total += c
	}
	return len(costs)
}

// instanceAttributes puts what the catalog and the instance's line hold of
// it beyond its data set.
func (x *webRequest) instanceAttributes(r *dicomweb.Result, sess tenant.CatalogSession, s tenant.CatalogScan, held tenant.ScanInstance, line sidecar.Instance, want map[tag.Tag]bool) {
	set := func(t tag.Tag, vr dataelem.VR, v any) {
		if want[t] {
			r.Set(t, vr, v)
		}
	}
	set(dicomweb.TagSOPInstanceUID, dataelem.UI, held.SOPInstanceUID)
	set(dicomweb.TagInstanceAvailability, dataelem.CS, instanceAvailability)
	if line.TransferSyntax != "" {
		set(dicomweb.TagAvailableTransferSyntaxUID, dataelem.UI, line.TransferSyntax)
	}
	set(dicomweb.TagRetrieveURL, dataelem.UR, x.base()+"/studies/"+sess.StudyUID+"/series/"+s.SeriesUID+"/instances/"+held.SOPInstanceUID)
}
