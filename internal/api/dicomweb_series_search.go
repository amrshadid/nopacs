package api

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A search of series names, besides the study its path gives, keys the
// search tables index (internal/search): each is matched by the same
// condition tree and the same index the search service uses, which a
// scan's one row holds, so a page costs the same at ten thousand series as
// at ten million and no metadata file is read to find the series. A scan's
// row is read from its file with the lowest SOP Instance UID; a value that
// varies inside a series is matched per file at the instance level, which
// still reads the metadata files.

// catalogSeriesKeys are the keys a search of series always matched from the
// catalog's own values, which the search tables also hold.
var catalogSeriesKeys = []tag.Tag{dicomweb.TagModality, dicomweb.TagSeriesDescription, dicomweb.TagSeriesNumber}

// indexedKey reports whether the search tables hold the attribute k names
// for a scan.
func indexedKey(k dicomweb.Key) bool {
	f, err := query.ParseField(k.Keyword)
	if err != nil {
		return false
	}
	col, ok := search.Registry().Column(f)
	return ok && col.Level == query.LevelScan
}

// seriesSearch is the part of a search of series the search tables serve.
type seriesSearch struct {
	// cond is the condition of the keys the tables index, nil for none.
	cond query.Node
	// used says the request is served by the search tables.
	used bool
	// extended says it names an attribute beyond the three the catalog also
	// holds, which no other path matches.
	extended bool
}

// seriesSearchOf returns how a search of series is served by the search
// tables: they serve a request that names at least one key they index, and
// no key outside them and the study and series UIDs (checkKeys refuses
// those). A request that names a Series Instance UID and only the catalog's
// own keys is served by the UID's lookup, as it always was. The keys are
// mapped from the request's parameters (query.FromQIDO), the same text a
// QIDO-RS client sent; a key the mapping refuses is a 400 when no other
// path serves it.
func seriesSearchOf(q dicomweb.Query, rawQuery string) (seriesSearch, *dicomweb.Error) {
	var out seriesSearch
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return out, nil
	}
	var params []query.QIDOParam
	for _, k := range q.Keys {
		switch {
		case k.Tag == dicomweb.TagStudyInstanceUID, k.Tag == dicomweb.TagSeriesInstanceUID:
			continue
		case !indexedKey(k):
			return out, nil
		}
		vs := values[k.Keyword]
		if len(vs) == 0 {
			for name, v := range values {
				if strings.EqualFold(name, k.Tag.Hex()) {
					vs = v
				}
			}
		}
		if len(vs) == 0 {
			return out, nil
		}
		params = append(params, query.QIDOParam{Key: k.Keyword, Value: vs[len(vs)-1]})
		out.extended = out.extended || !slices.Contains(catalogSeriesKeys, k.Tag)
	}
	if len(params) == 0 {
		return out, nil
	}
	if _, byUID := q.Key(dicomweb.TagSeriesInstanceUID); byUID && !out.extended {
		return out, nil
	}
	cond, _, err := query.FromQIDO(params)
	if err != nil {
		if out.extended {
			return out, &dicomweb.Error{Status: http.StatusBadRequest, Message: err.Error()}
		}
		out.extended = false
		return out, nil
	}
	out.cond, out.used = cond, true
	return out, nil
}

// searchedScans iterates the scans the search tables answer for cond, in
// order of their ids, of one study when studyUID is not empty. A key that
// may not learn original values is never answered a scan archived as
// received.
func (x *webRequest) searchedScans(cond query.Node, studyUID string, size int) iterator[tenant.CatalogScan] {
	return pagesOf(func(ctx context.Context, page master.Page) ([]tenant.CatalogScan, string, error) {
		got, err := x.p.Search(ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: cond, StudyUID: studyUID, Page: page,
			Hide: originalsFor(x.d) == tenant.OriginalsHidden, Now: x.c.now()})
		return got.Scans, got.Next, err
	}, size)
}

// searchRefusal returns the 400 of a condition the search tables refuse for
// an attribute no other path matches, and false for any other error: an
// attribute still being filled is the search's own precondition failure.
func searchRefusal(err error) (*dicomweb.Error, bool) {
	switch fault.KindOf(err) {
	case fault.Invalid:
		return &dicomweb.Error{Status: http.StatusBadRequest, Message: fault.Message(err)}, true
	case fault.Precondition:
		// A column still being filled: the request is right and meets a
		// state that ends by itself, so it is retried, not changed.
		return &dicomweb.Error{Status: http.StatusServiceUnavailable, Message: fault.Message(err)}, true
	}
	return nil, false
}

// searchKeysNote ends the message of a key a level does not match: at the
// series level, every attribute the search tables index is matched too.
func searchKeysNote(l dicomweb.Level) string {
	if l == dicomweb.Series {
		return " and the attributes the search tables index (SearchService.ListSearchAttributes)"
	}
	return ""
}
