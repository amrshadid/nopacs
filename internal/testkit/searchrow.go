package testkit

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The values a measured tenant's scans are given: pools of research
// protocols, scanners, sequences and settings, as many distinct values as a
// tenant of a few sites has, so a condition on one attribute matches a
// share of the rows that a real archive's would (a modality, a quarter of
// them; one station, a fiftieth).
var (
	scanModalities  = weighted{{"MR", 50}, {"CT", 30}, {"PT", 10}, {"US", 5}, {"XA", 5}}
	scanBases       = []string{"T1 MPRAGE", "T2 FLAIR", "T2 TSE", "DWI b1000", "ADC", "SWI", "fMRI rest", "fMRI task", "Field map", "ASL perfusion", "DTI 64 directions", "Localizer", "Head 5mm", "Chest 1.25mm", "Bone recon"}
	scanVariants    = []string{"", " ax", " cor", " sag"}
	scanMakers      = weighted{{"SIEMENS", 45}, {"GE MEDICAL SYSTEMS", 30}, {"Philips", 20}, {"Canon", 5}}
	scanBodies      = []string{"HEAD", "BRAIN", "CHEST", "ABDOMEN", "PELVIS", "SPINE", "KNEE", "HEART"}
	scanFields      = weighted{{"1.5", 45}, {"3", 50}, {"7", 5}}
	scanThickness   = []string{"0.8", "0.9", "1", "1.2", "1.5", "2", "2.5", "3", "5"}
	scanRepetition  = []string{"8.2", "500", "1900", "2300", "3000", "4500", "6000", "8000", "9000", "10000"}
	scanEcho        = []string{"2.2", "2.98", "4.6", "12", "30", "60", "90", "100", "120", "140"}
	scanFlip        = []string{"8", "15", "30", "45", "60", "90", "150", "180"}
	scanImageTypes  = []string{"ORIGINAL", "PRIMARY", "DERIVED", "SECONDARY", "M", "NORM", "AXIAL", "LOCALIZER"}
	scanSequences   = []string{"GR", "SE", "IR", "EP", "RM"}
	scanProtocols   = names("protocol-", 40)
	scanStudies     = names("study-", 30)
	scanModels      = names("model-", 12)
	scanStations    = names("MRC", 20)
	scanSequenceIDs = names("seq-", 15)
	scanOperators   = names("Operator^", 30)
)

// weighted is a list of values with weights.
type weighted []struct {
	value  string
	weight int
}

// pick draws one value.
func (w weighted) pick(r *rand.Rand) string {
	total := 0
	for _, x := range w {
		total += x.weight
	}
	n := r.IntN(total)
	for _, x := range w {
		if n < x.weight {
			return x.value
		}
		n -= x.weight
	}
	return w[len(w)-1].value
}

// names returns n values with a prefix and a number.
func names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%02d", prefix, i+1)
	}
	return out
}

// choose draws one of a list.
func choose(r *rand.Rand, list []string) string { return list[r.IntN(len(list))] }

// SearchRowOf draws the row a search table holds of a scan: a value for
// each of its attributes but one in ten of the optional ones, in the form
// each column keeps. source is the SOP Instance UID of the file the row is
// read from, studyDate the study's date (the scan's acquisition date is
// that day or the next, none when the study has no date) and number the
// scan's Series Number.
func SearchRowOf(r *rand.Rand, source, studyDate string, number int) search.Row {
	var description string
	if r.IntN(20) != 0 {
		description = choose(r, scanBases) + choose(r, scanVariants)
	}
	given := map[string][]string{
		"Modality": {scanModalities.pick(r)}, "SeriesNumber": {strconv.Itoa(number)}, "SeriesDescription": {description},
		"ProtocolName": {choose(r, scanProtocols)}, "StudyDescription": {choose(r, scanStudies)}, "BodyPartExamined": {choose(r, scanBodies)},
		"Manufacturer": {scanMakers.pick(r)}, "ManufacturerModelName": {choose(r, scanModels)}, "StationName": {choose(r, scanStations)},
		"SequenceName": {choose(r, scanSequenceIDs)}, "PatientSex": {choose(r, []string{"M", "F", "O"})},
		"PatientAge": {fmt.Sprintf("%03dY", 20+r.IntN(60))}, "MagneticFieldStrength": {scanFields.pick(r)},
		"SliceThickness": {choose(r, scanThickness)}, "RepetitionTime": {choose(r, scanRepetition)}, "EchoTime": {choose(r, scanEcho)},
		"FlipAngle": {choose(r, scanFlip)},
	}
	if t, err := time.Parse("20060102", studyDate); err == nil {
		day := t.AddDate(0, 0, r.IntN(2)).Format("20060102")
		given["AcquisitionDate"] = []string{day}
		given["AcquisitionDateTime"] = []string{day + fmt.Sprintf("%02d%02d%02d", r.IntN(24), r.IntN(60), r.IntN(60))}
	}
	for range 2 + r.IntN(3) {
		given["ImageType"] = append(given["ImageType"], choose(r, scanImageTypes))
	}
	for range 1 + r.IntN(2) {
		given["ScanningSequence"] = append(given["ScanningSequence"], choose(r, scanSequences))
	}
	for range r.IntN(3) {
		given["OperatorsName"] = append(given["OperatorsName"], choose(r, scanOperators))
	}
	row := search.Row{Source: source}
	for _, a := range search.Attributes() {
		var c search.Cell
		for _, v := range given[a.Keyword] {
			if v == "" || (a.Keyword != "Modality" && a.Keyword != "SeriesNumber" && r.IntN(10) == 0) {
				continue
			}
			text, num, ok := query.ColumnValue(a.Type, v)
			switch {
			case !ok:
			case a.Type == query.TypeNumber:
				c.Number = &num
			case !slices.Contains(c.Texts, text):
				c.Texts = append(c.Texts, text)
			}
			if !a.Multi {
				break
			}
		}
		slices.Sort(c.Texts)
		row.Cells = append(row.Cells, c)
	}
	return row
}

// searchShape is one search a measurement makes: the condition's text,
// drawn from the pools so each probe asks about other values; the level it
// is made at; and whether it asks for a count or a facet as well.
type searchShape struct {
	what  string
	level query.Level
	count bool
	facet string
	cond  func(r *rand.Rand) string
}

// searchShapes are the searches the measurements make of a tenant, those a
// research archive's users make: a page of an equality, of two equalities
// and of ranges, a contains, the counts of each, a facet over a range, and
// the searches across levels.
var searchShapes = []searchShape{
	{"equality, one attribute: a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `StationName == "` + choose(r, scanStations) + `"`
	}},
	{"equality, folded: a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `ManufacturerModelName = "` + choose(r, scanModels) + `"`
	}},
	{"equality, two attributes: a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `Modality == "MR" and Manufacturer = "` + scanMakers.pick(r) + `"`
	}},
	{"equality, two attributes: the count", query.LevelScan, true, "", func(r *rand.Rand) string {
		return `Modality == "MR" and Manufacturer = "` + scanMakers.pick(r) + `"`
	}},
	{"range, one attribute: a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `SliceThickness < ` + choose(r, scanThickness)
	}},
	{"range, two attributes: a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `SliceThickness between ` + choose(r, scanThickness[:4]) + ` and ` + choose(r, scanThickness[4:]) + ` and EchoTime < ` + choose(r, scanEcho[3:])
	}},
	{"range, two attributes: the count", query.LevelScan, true, "", func(r *rand.Rand) string {
		return `SliceThickness between ` + choose(r, scanThickness[:4]) + ` and ` + choose(r, scanThickness[4:]) + ` and EchoTime < ` + choose(r, scanEcho[3:])
	}},
	{"date range: a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `AcquisitionDate between ` + fmt.Sprintf("%d-%02d-01", 2012+r.IntN(8), 1+r.IntN(12)) + ` and 2019-12-31`
	}},
	{"contains: a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `SeriesDescription contains "` + choose(r, []string{"mprage", "flair", "perfusion", "rest", "directions", "recon"}) + `"`
	}},
	{"contains: the count", query.LevelScan, true, "", func(r *rand.Rand) string {
		return `SeriesDescription contains "` + choose(r, []string{"mprage", "flair", "perfusion", "rest", "directions", "recon"}) + `"`
	}},
	{"multi-valued attribute: a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `ImageType == "` + choose(r, scanImageTypes) + `" and Modality == "CT"`
	}},
	{"facet: model counts over a range", query.LevelScan, false, "ManufacturerModelName", func(r *rand.Rand) string {
		return `SliceThickness >= ` + choose(r, scanThickness[3:])
	}},
	{"facet: modality counts of an equality", query.LevelScan, false, "Modality", func(r *rand.Rand) string {
		return `Manufacturer = "` + scanMakers.pick(r) + `"`
	}},
	{"cross-level: sessions having scan A and scan B, a page of 100", query.LevelSession, false, "", func(r *rand.Rand) string {
		return `has scan (SeriesDescription = "` + choose(r, scanBases) + `") and has scan (SeriesDescription = "` + choose(r, scanBases) + ` ax")`
	}},
	{"cross-level: sessions having scan A and scan B, the count", query.LevelSession, true, "", func(r *rand.Rand) string {
		return `has scan (SeriesDescription = "` + choose(r, scanBases) + `") and has scan (SeriesDescription = "` + choose(r, scanBases) + ` ax")`
	}},
	{"cross-level: sessions whose every scan is of one maker, a page of 100", query.LevelSession, false, "", func(r *rand.Rand) string {
		return `every scan (Manufacturer = "` + scanMakers.pick(r) + `") and session.date >= 2015-01-01`
	}},
	{"cross-level: subjects having a scan of a station, a page of 100", query.LevelSubject, false, "", func(r *rand.Rand) string {
		return `has scan (StationName == "` + choose(r, scanStations) + `" and Modality == "PT")`
	}},
	{"cross-level: scans of a session with a date, a page of 100", query.LevelScan, false, "", func(r *rand.Rand) string {
		return `Modality == "MR" and has session (session.date between 2014-01-01 and 2015-12-31)`
	}},
}

// searchCountRows is the bound the measurements give a count or a facet:
// the tenant's setting search.facet-rows at the value its default would
// have for a tenant this size.
const searchCountRows = 1_000_000

// TimeSearches runs each search shape of the measurements rounds times
// against p, each with its values drawn anew, and adds to sum the spread of
// the times (median, 99th percentile, longest) and the median number of
// rows a counted search matched. It fails tb for a search that fails, or
// that matches nothing in most of its rounds.
func TimeSearches(tb testing.TB, p tenant.Searching, rounds int, sum *Summary) {
	tb.Helper()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(3, 9)) // #nosec G404 -- a seeded generator, so a run probes the same values as the last.
	for _, sh := range searchShapes {
		var times []time.Duration
		var counts []int64
		empty := 0
		for range rounds {
			node, err := query.Parse(sh.cond(r))
			if err != nil {
				tb.Fatalf("%s: %v", sh.what, err)
			}
			q := tenant.SearchQuery{Level: sh.level, Condition: node, Page: master.Page{Limit: 100}, Count: sh.count, CountRows: searchCountRows,
				Now: time.Date(2020, 1, 6, 8, 0, 0, 0, time.UTC)}
			if sh.facet != "" {
				f, err := query.ParseField(sh.facet)
				if err != nil {
					tb.Fatal(err)
				}
				q.Facet = &f
			}
			began := time.Now()
			got, err := p.Search(ctx, q)
			took := time.Since(began)
			if err != nil {
				tb.Fatalf("%s: %v", sh.what, err)
			}
			times = append(times, took)
			if len(got.Scans)+len(got.Sessions)+len(got.Subjects) == 0 && len(got.Facet) == 0 {
				empty++
			}
			if got.Counted {
				counts = append(counts, got.Total)
			}
		}
		if empty > rounds/2 {
			tb.Fatalf("%s matched nothing in %d of %d rounds: the tenant does not hold what the search asks", sh.what, empty, rounds)
		}
		note := SpreadOf(times).String()
		if len(counts) > 0 {
			slices.Sort(counts)
			note += fmt.Sprintf(" (median %d rows matched)", counts[len(counts)/2])
		}
		sum.Add(sh.what, note)
	}
}
