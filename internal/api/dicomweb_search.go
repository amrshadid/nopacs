package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A search (QIDO-RS, PS3.18 10.6) maps the levels of DICOM onto the
// catalog: a study is a session, a series a scan, an instance an instance
// a scan holds. A key is matched by an index where the catalog has one,
// and every key is matched on each row read, so a key the index does not
// serve narrows what the index served.
//
// PatientID is the subject's label. On an archive that de-identifies, the
// label is the identifier the archive gives the person, and the files'
// own Patient ID was replaced; the label is what the catalog holds and
// indexes, so a search by PatientID is a search by subject, and every
// result answers the label as its PatientID.

// The keys each level matches, beyond the UIDs of its path. A key outside
// these is refused with 400, naming them: the attributes at the instance
// level are matched from the metadata files, whatever they are.
var (
	studyKeys  = []tag.Tag{dicomweb.TagStudyInstanceUID, dicomweb.TagPatientID, dicomweb.TagStudyDate, dicomweb.TagModalitiesInStudy}
	seriesKeys = []tag.Tag{dicomweb.TagStudyInstanceUID, dicomweb.TagSeriesInstanceUID, dicomweb.TagModality, dicomweb.TagSeriesDescription, dicomweb.TagSeriesNumber}
)

// instanceAvailability is Instance Availability (0008,0056) of everything
// the catalog holds: on a storage the server reads now (PS3.4 C.4.23.1.1).
const instanceAvailability = "ONLINE"

// searchAnswer is what a search answers: how many results, whether more match
// than it holds, and the results themselves, each made from the catalog
// and its scan's metadata file only as it is written, so an answer is
// never held whole.
type searchAnswer struct {
	n    int
	more bool
	each func(ctx context.Context, yield func(*dicomweb.Result) error) error
}

// search answers a search: its results as a JSON array streamed one result
// at a time (PS3.18 10.6.3.3), under one of the caller's open reads, which
// it holds from its first read of the catalog and the scans' metadata
// files to its last byte. Everything that decides the answer's head (its
// status, its Warnings) is known before the first result is written: the
// rows are found first, from the catalog and the lines matched, and the
// results made after.
func (x *webRequest) search(ctx context.Context) error {
	media, perr := dicomweb.AcceptJSON(x.r)
	if perr != nil {
		return x.refuse(perr)
	}
	q, perr := dicomweb.ParseQuery(x.res, x.r.URL.RawQuery)
	if perr != nil {
		return x.refuse(perr)
	}
	if kw := q.Keywords(); kw != "" {
		x.details = withDetails(x.details, map[string]string{detailKeys: clip(kw, master.MaxAuditFieldLength-8)})
	}
	if perr := checkKeys(q); perr != nil {
		return x.refuse(perr)
	}
	if err := x.holdRead(); err != nil {
		return x.fail(err)
	}
	ls := x.newLines()
	ls.max = webMaxMetadataRead
	defer ls.closeAll()
	var (
		ans searchAnswer
		err error
	)
	switch q.Level {
	case dicomweb.Study:
		ans, err = x.searchStudies(ctx, q, ls)
	case dicomweb.Series:
		ans, err = x.searchSeries(ctx, q, ls)
	default:
		ans, err = x.searchInstances(ctx, q, ls)
	}
	if ls.exceeded() {
		return x.refuse(errReadTooMuch)
	}
	if perr := (*dicomweb.Error)(nil); errors.As(err, &perr) {
		return x.refuse(perr)
	}
	if fault.KindOf(err) == fault.NotFound {
		// A study, a series or a subject the search names that the tenant
		// does not hold matches nothing.
		ans, err = searchAnswer{}, nil
	}
	if err != nil {
		return x.fail(err)
	}
	h := x.w.Header()
	// The Warnings are of a success: set with its status, so an answer
	// that fails before it begins carries none.
	warn := func() {
		if q.Fuzzy {
			h.Add("Warning", dicomweb.Warning(x.r, dicomweb.WarnFuzzy))
		}
		if q.Capped {
			h.Add("Warning", dicomweb.Warning(x.r, dicomweb.WarnCapped))
		}
		if ans.more {
			h.Add("Warning", dicomweb.Warning(x.r, dicomweb.WarnMore))
		}
	}
	if ans.n == 0 {
		// No match is a success with nothing in it (PS3.18 10.6.3.2).
		warn()
		x.w.WriteHeader(http.StatusNoContent)
		x.done(0)
		return nil
	}
	stream, stop, err := x.openStream(ctx, func() {
		warn()
		h.Set("Content-Type", media)
		x.w.WriteHeader(http.StatusOK)
	})
	if err != nil {
		return x.fail(err)
	}
	defer stop()
	if x.r.Method == http.MethodHead {
		return stream.end(nil, ans.n)
	}
	// Making the results reads again the lines of the rows kept, which the
	// search's reads bounded; they have an allowance of their own, as
	// large, so the bound holds for the whole request.
	ls.max = ls.read + webMaxMetadataRead
	sent := 0
	sep := "["
	err = ans.each(stream.ctx, func(r *dicomweb.Result) error {
		b, err := json.Marshal(r)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "api: a search's result cannot be written as DICOM JSON")
		}
		if _, err := io.WriteString(stream, sep); err != nil {
			return err
		}
		sep = ","
		if _, err := stream.Write(b); err != nil {
			return err
		}
		sent++
		return nil
	})
	if err == nil {
		_, err = io.WriteString(stream, "]")
	}
	if err != nil && !stream.begun && ls.exceeded() {
		stop()
		return x.refuse(errReadTooMuch)
	}
	return stream.end(err, sent)
}

// checkKeys refuses a key the level does not match.
func checkKeys(q dicomweb.Query) *dicomweb.Error {
	var allowed []tag.Tag
	switch q.Level {
	case dicomweb.Study:
		allowed = studyKeys
	case dicomweb.Series:
		allowed = seriesKeys
	default:
		return nil
	}
	for _, k := range q.Keys {
		ok := false
		for _, t := range allowed {
			ok = ok || t == k.Tag
		}
		if !ok {
			names := make([]string, len(allowed))
			for i, t := range allowed {
				names[i] = t.GetKeyword()
			}
			return &dicomweb.Error{Status: http.StatusBadRequest,
				Message: "matching on " + k.Keyword + " is not served at the " + q.Level.String() + " level; it matches " + strings.Join(names, ", ")}
		}
	}
	return nil
}

// dateBounds returns the study dates of a StudyDate key as the catalog's
// range takes them, and false when the key names no date that exists, so
// it matches nothing.
func dateBounds(k dicomweb.Key) (string, string, bool) {
	low, high := k.Value, k.Value
	if k.Match == dicomweb.MatchRange {
		low, high = k.Low, k.High
	}
	for _, d := range []string{low, high} {
		if d != "" && !tenant.ValidStudyDate(d) {
			return "", "", false
		}
	}
	if low != "" && high != "" && high < low {
		return "", "", false
	}
	return low, high, true
}

// searchStudies answers a search of studies: the sessions of one or more
// Study Instance UIDs, of a subject, in a range of study dates, with a
// scan of a modality, or all of them newest first.
func (x *webRequest) searchStudies(ctx context.Context, q dicomweb.Query, ls *lines) (searchAnswer, error) {
	studyKey, byStudy := q.Key(dicomweb.TagStudyInstanceUID)
	patient, byPatient := q.Key(dicomweb.TagPatientID)
	date, byDate := q.Key(dicomweb.TagStudyDate)
	modality, byModality := q.Key(dicomweb.TagModalitiesInStudy)
	var it iterator[tenant.CatalogSession]
	switch {
	case byStudy:
		var rows []tenant.CatalogSession
		for _, u := range uidsOf(studyKey) {
			s, err := x.session(ctx, u)
			if fault.KindOf(err) == fault.NotFound {
				continue
			}
			if err != nil {
				return searchAnswer{}, err
			}
			rows = append(rows, s)
		}
		it = &rowsOf[tenant.CatalogSession]{rows: rows}
	case byPatient && patient.Match == dicomweb.MatchSingle:
		if tenant.ValidateLabel("subject", patient.Value) != nil {
			it = none[tenant.CatalogSession]{}
			break
		}
		f := tenant.SessionFilter{SubjectLabel: patient.Value, Order: tenant.OrderLabel}
		if byDate {
			low, high, ok := dateBounds(date)
			if !ok {
				return searchAnswer{}, nil
			}
			f = tenant.SessionFilter{SubjectLabel: patient.Value, StudyDateStart: low, StudyDateEnd: high, Order: tenant.OrderStudyDate}
		}
		it = x.sessions(f, pageSize(q))
	case byDate:
		low, high, ok := dateBounds(date)
		if !ok {
			return searchAnswer{}, nil
		}
		it = x.sessions(tenant.SessionFilter{StudyDateStart: low, StudyDateEnd: high, Order: tenant.OrderStudyDate}, pageSize(q))
	case byModality && modality.Match == dicomweb.MatchSingle:
		if tenant.ValidateModality(modality.Value) != nil {
			return searchAnswer{}, nil
		}
		it = x.sessions(tenant.SessionFilter{Modality: modality.Value}, pageSize(q))
	default:
		it = x.sessions(tenant.SessionFilter{}, pageSize(q))
	}
	keep := func(s tenant.CatalogSession) (bool, error) {
		if x.d.Masked() && namesTag(q.Keys, dicomweb.TagStudyInstanceUID, dicomweb.TagStudyDate, dicomweb.TagModalitiesInStudy) {
			// A study archived as received matches no key on a value of its
			// files: whether it did would tell a masked key that value.
			if as, err := sessionAsReceived(ctx, x.p, s.SubjectLabel, s.Label); err != nil || as {
				return false, err
			}
		}
		for _, k := range q.Keys {
			var vs []string
			switch k.Tag {
			case dicomweb.TagStudyInstanceUID:
				vs = []string{s.StudyUID}
			case dicomweb.TagPatientID:
				vs = []string{s.SubjectLabel}
			case dicomweb.TagStudyDate:
				vs = []string{s.StudyDate}
			case dicomweb.TagModalitiesInStudy:
				vs = s.Modalities
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
	want := q.Returned()
	return searchAnswer{n: len(rows), more: more, each: func(ctx context.Context, yield func(*dicomweb.Result) error) error {
		for _, s := range rows {
			r := dicomweb.NewResult()
			x.studyAttributes(r, s, want)
			if err := x.fillStudy(ctx, r, s, want, ls); err != nil {
				return err
			}
			if err := yield(r); err != nil {
				return err
			}
		}
		return nil
	}}, nil
}

// sessions iterates a list of sessions.
func (x *webRequest) sessions(f tenant.SessionFilter, size int) iterator[tenant.CatalogSession] {
	return pagesOf(func(ctx context.Context, page master.Page) ([]tenant.CatalogSession, string, error) {
		f.Originals = originalsFor(x.d)
		got, err := x.p.ListSessions(ctx, f, page)
		return got.Items, got.Next, err
	}, size)
}

// scans iterates a list of scans.
func (x *webRequest) scans(f tenant.ScanFilter, size int) iterator[tenant.CatalogScan] {
	return pagesOf(func(ctx context.Context, page master.Page) ([]tenant.CatalogScan, string, error) {
		f.Originals = originalsFor(x.d)
		got, err := x.p.ListScans(ctx, f, page)
		return got.Items, got.Next, err
	}, size)
}

// uidsOf returns the UIDs a UID key names.
func uidsOf(k dicomweb.Key) []string {
	if k.Match == dicomweb.MatchList {
		return k.UIDs
	}
	return []string{k.Value}
}

// studyAttributes puts what the catalog holds of a session's study.
func (x *webRequest) studyAttributes(r *dicomweb.Result, s tenant.CatalogSession, want map[tag.Tag]bool) {
	set := func(t tag.Tag, vr dataelem.VR, v any) {
		if want[t] {
			r.Set(t, vr, v)
		}
	}
	set(dicomweb.TagStudyInstanceUID, dataelem.UI, s.StudyUID)
	set(dicomweb.TagPatientID, dataelem.LO, s.SubjectLabel)
	if s.StudyDate != "" {
		set(dicomweb.TagStudyDate, dataelem.DA, s.StudyDate)
	}
	if len(s.Modalities) > 0 {
		set(dicomweb.TagModalitiesInStudy, dataelem.CS, s.Modalities)
	}
	set(dicomweb.TagNumberOfStudyRelatedSeries, dataelem.IS, strconv.FormatInt(s.Scans, 10))
	set(dicomweb.TagNumberOfStudyRelatedInstances, dataelem.IS, strconv.FormatInt(s.Instances, 10))
	set(dicomweb.TagInstanceAvailability, dataelem.CS, instanceAvailability)
	set(dicomweb.TagRetrieveURL, dataelem.UR, x.base()+"/studies/"+s.StudyUID)
}

// fillStudy puts the attributes a search returns that the catalog does
// not hold, from the first line of the metadata file of the session's
// first scan by label: attributes of the study and the patient, the same
// in each of its files.
func (x *webRequest) fillStudy(ctx context.Context, r *dicomweb.Result, s tenant.CatalogSession, want map[tag.Tag]bool, ls *lines) error {
	got, err := x.p.ListScans(ctx, tenant.ScanFilter{SubjectLabel: s.SubjectLabel, SessionLabel: s.Label, Order: tenant.OrderLabel}, master.Page{Limit: 1})
	if err != nil || len(got.Items) == 0 {
		return err
	}
	return x.fillFrom(ctx, r, got.Items[0], want, ls)
}

// fillFrom puts the attributes want names that r does not hold, from the
// first line of the scan's metadata file.
func (x *webRequest) fillFrom(ctx context.Context, r *dicomweb.Result, s tenant.CatalogScan, want map[tag.Tag]bool, ls *lines) error {
	if x.d.Masked() {
		// The first line is the first instance's, in order of UID, as the
		// catalog lists them: one archived as received holds the sender's
		// values, which a masked key is refused. Read only for such a key.
		held, _, err := x.p.ListHeldInstances(ctx, pathOf(s), master.Page{Limit: 1})
		if err != nil {
			return err
		}
		for _, in := range held {
			if err := refuseAsReceived(x.d, in); err != nil {
				return err
			}
		}
	}
	sl, err := ls.of(ctx, s)
	if err != nil {
		return err
	}
	set, err := sl.first(ctx)
	if err != nil {
		return err
	}
	r.FromDataset(set, func(key string) bool { return wantKey(want, key) })
	return nil
}

// wantKey reports whether want names the attribute of a member key.
func wantKey(want map[tag.Tag]bool, key string) bool {
	v, err := strconv.ParseUint(key, 16, 32)
	return err == nil && len(key) == 8 && want[tag.Tag(v)]
}

// searchSeries answers a search of series: within one study, by Series
// Instance UID, of a modality, by series description, or all newest first.
func (x *webRequest) searchSeries(ctx context.Context, q dicomweb.Query, ls *lines) (searchAnswer, error) {
	studyKey, byStudy := q.Key(dicomweb.TagStudyInstanceUID)
	seriesKey, bySeries := q.Key(dicomweb.TagSeriesInstanceUID)
	modality, byModality := q.Key(dicomweb.TagModality)
	desc, byDesc := q.Key(dicomweb.TagSeriesDescription)
	if byStudy && studyKey.Match != dicomweb.MatchSingle {
		return searchAnswer{}, &dicomweb.Error{Status: http.StatusBadRequest, Message: "a search of series names one study"}
	}
	var it iterator[tenant.CatalogScan]
	switch {
	case byStudy:
		sess, err := x.session(ctx, studyKey.Value)
		if err != nil {
			return searchAnswer{}, err
		}
		if bySeries {
			rows, err := x.scansOfStudy(ctx, studyKey.Value, uidsOf(seriesKey))
			if err != nil {
				return searchAnswer{}, err
			}
			it = &rowsOf[tenant.CatalogScan]{rows: rows}
			break
		}
		it = x.scans(tenant.ScanFilter{SubjectLabel: sess.SubjectLabel, SessionLabel: sess.Label, Order: tenant.OrderLabel}, pageSize(q))
	case bySeries:
		var rows []tenant.CatalogScan
		for _, u := range uidsOf(seriesKey) {
			s, ok, err := x.scanBySeries(ctx, u)
			if err != nil {
				return searchAnswer{}, err
			}
			if ok {
				rows = append(rows, s)
			}
		}
		it = &rowsOf[tenant.CatalogScan]{rows: rows}
	case byModality && modality.Match == dicomweb.MatchSingle:
		if tenant.ValidateModality(modality.Value) != nil {
			return searchAnswer{}, nil
		}
		it = x.scans(tenant.ScanFilter{Modality: modality.Value}, pageSize(q))
	case byDesc && (desc.Match == dicomweb.MatchSingle || dicomweb.LiteralPrefix(desc.Value) != ""):
		prefix := desc.Value
		if desc.Match == dicomweb.MatchWildcard {
			prefix = dicomweb.LiteralPrefix(desc.Value)
		}
		if len([]rune(prefix)) > tenant.MaxSeriesDescriptionLength {
			return searchAnswer{}, nil
		}
		it = x.scans(tenant.ScanFilter{DescriptionPrefix: prefix, Order: tenant.OrderSeriesDescription}, pageSize(q))
	default:
		it = x.scans(tenant.ScanFilter{}, pageSize(q))
	}
	keep := func(s tenant.CatalogScan) (bool, error) {
		if x.d.Masked() && namesTag(q.Keys, dicomweb.TagSeriesInstanceUID, dicomweb.TagModality, dicomweb.TagSeriesDescription, dicomweb.TagSeriesNumber) {
			// As for a study: a series archived as received matches no key
			// on a value of its files.
			if as, err := scanAsReceived(ctx, x.p, pathOf(s)); err != nil || as {
				return false, err
			}
		}
		for _, k := range q.Keys {
			var vs []string
			switch k.Tag {
			case dicomweb.TagStudyInstanceUID:
				// Matched by the study's session, which every row of a
				// study's search is in.
				continue
			case dicomweb.TagSeriesInstanceUID:
				vs = []string{s.SeriesUID}
			case dicomweb.TagModality:
				vs = []string{s.Values.Modality}
			case dicomweb.TagSeriesDescription:
				vs = []string{s.Values.SeriesDescription}
			case dicomweb.TagSeriesNumber:
				if s.Values.HasSeriesNumber {
					vs = []string{strconv.Itoa(int(s.Values.SeriesNumber))}
				}
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
	want := q.Returned()
	return searchAnswer{n: len(rows), more: more, each: func(ctx context.Context, yield func(*dicomweb.Result) error) error {
		sessions := sessionsOf(x)
		for _, s := range rows {
			sess, err := sessions(ctx, s)
			if err != nil {
				return err
			}
			r := dicomweb.NewResult()
			x.seriesAttributes(r, sess, s, want)
			x.studyAttributes(r, sess, want)
			if err := x.fillFrom(ctx, r, s, want, ls); err != nil {
				return err
			}
			if err := yield(r); err != nil {
				return err
			}
		}
		return nil
	}}, nil
}

// sessionsOf returns what finds the session of a scan, each session read
// from the catalog once for a request's results.
func sessionsOf(x *webRequest) func(ctx context.Context, s tenant.CatalogScan) (tenant.CatalogSession, error) {
	held := map[[2]string]tenant.CatalogSession{}
	return func(ctx context.Context, s tenant.CatalogScan) (tenant.CatalogSession, error) {
		key := [2]string{s.SubjectLabel, s.SessionLabel}
		if sess, ok := held[key]; ok {
			return sess, nil
		}
		sess, err := x.p.CatalogSession(ctx, s.SubjectLabel, s.SessionLabel)
		if err != nil {
			return sess, err
		}
		held[key] = sess
		return sess, nil
	}
}

// scansOfStudy returns the scans of the series of a study the tenant
// holds, in the order of the UIDs.
func (x *webRequest) scansOfStudy(ctx context.Context, study string, series []string) ([]tenant.CatalogScan, error) {
	labels, err := x.p.CatalogLabels(ctx, study, series)
	if err != nil || labels.Session == nil {
		return nil, err
	}
	var out []tenant.CatalogScan
	for _, u := range series {
		l, ok := labels.Scans[u]
		if !ok {
			continue
		}
		s, err := x.p.CatalogScan(ctx, label.Path{Subject: labels.Session.SubjectLabel, Session: labels.Session.SessionLabel, Scan: l})
		if fault.KindOf(err) == fault.NotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// scanBySeries returns the scan of a Series Instance UID, found by the
// catalog's list of a series' instances: the key of its first instance
// names the scan's labels (package layout).
func (x *webRequest) scanBySeries(ctx context.Context, series string) (tenant.CatalogScan, bool, error) {
	held, _, err := x.p.ScanInstances(ctx, series, master.Page{Limit: 1})
	if err != nil || len(held) == 0 {
		return tenant.CatalogScan{}, false, err
	}
	k, err := layout.Parse(held[0].Key)
	if err != nil {
		return tenant.CatalogScan{}, false, fault.Wrap(fault.Internal, err, "api: an instance's key is not a key of the layout")
	}
	s, err := x.p.CatalogScan(ctx, k.Path)
	if fault.KindOf(err) == fault.NotFound {
		return s, false, nil
	}
	return s, err == nil, err
}

// seriesAttributes puts what the catalog holds of a scan's series.
func (x *webRequest) seriesAttributes(r *dicomweb.Result, sess tenant.CatalogSession, s tenant.CatalogScan, want map[tag.Tag]bool) {
	set := func(t tag.Tag, vr dataelem.VR, v any) {
		if want[t] {
			r.Set(t, vr, v)
		}
	}
	set(dicomweb.TagSeriesInstanceUID, dataelem.UI, s.SeriesUID)
	if s.Values.Modality != "" {
		set(dicomweb.TagModality, dataelem.CS, s.Values.Modality)
	}
	if s.Values.SeriesDescription != "" {
		set(dicomweb.TagSeriesDescription, dataelem.LO, s.Values.SeriesDescription)
	}
	if s.Values.HasSeriesNumber {
		set(dicomweb.TagSeriesNumber, dataelem.IS, strconv.Itoa(int(s.Values.SeriesNumber)))
	}
	set(dicomweb.TagNumberOfSeriesRelatedInstances, dataelem.IS, strconv.FormatInt(s.Instances, 10))
	set(dicomweb.TagRetrieveURL, dataelem.UR, x.base()+"/studies/"+sess.StudyUID+"/series/"+s.SeriesUID)
}
