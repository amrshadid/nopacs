package api

import (
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A profile's text lists hold terms (names of staff, of sites) as
// sensitive as a file's values. A masked key, which may be an outside
// model's, is never answered one, nor anything a guess of one could be
// checked against:
//
//   - a version with lists is answered without its terms, its expanded
//     form and its checksum (an unkeyed SHA-256 of the expanded text,
//     terms included, which confirms guessed terms put back into it);
//   - a list's summary is answered without its checksum;
//   - a trial or an explanation of a stored version with lists is
//     refused: whether a value it sends changes tells a guessed name in
//     the list from one that is not;
//   - storing a version with lists is refused: the answer for a document
//     equal to a stored version would confirm its terms.

// messageMaskedLists is the refusal of a masked key's request that would
// tell it a text list's terms.
const messageMaskedLists = "a masked key is never answered a profile's text list terms, nor what confirms a guess of them: a version with lists may not be stored, tried or explained with it; use a key that is not masked"

// maskedListsError is the refusal of such a request.
func maskedListsError() error { return fault.New(fault.Denied, messageMaskedLists) }

// maskedListsRefusal refuses a masked key a trial or an explanation of a
// stored version that has text lists; nil otherwise. A document the key
// gives is its own: its terms are the key's already.
func maskedListsRefusal(masked bool, t trialTarget) error {
	if masked && t.stored != nil && len(t.expanded.GetTextLists()) > 0 {
		return maskedListsError()
	}
	return nil
}

// versionHasLists reports whether a stored version holds text lists, read
// from its expanded text; a text that does not read holds none it could
// tell.
func versionHasLists(v tenant.ProfileVersion) bool {
	if len(v.Expanded) == 0 {
		return false
	}
	e := &v1.ExpandedProfile{}
	if err := protojson.Unmarshal(v.Expanded, e); err != nil {
		return false
	}
	return len(e.GetTextLists()) > 0
}

// withholdLists answers a version with text lists to a masked key: its
// lists named with their versions and no term, no expanded form and no
// checksum, and says the terms were withheld. A version without lists is
// answered whole.
func withholdLists(m *v1.ProfileVersion, hasLists bool) {
	if m == nil || !hasLists {
		return
	}
	for _, l := range m.GetDocument().GetTextLists() {
		l.Terms = nil
	}
	m.Expanded, m.Checksum, m.TermsWithheld = nil, "", true
}

// withholdChecksums empties the checksums of summaries answered to a
// masked key.
func withholdChecksums(sums ...*v1.TextListSummary) {
	for _, s := range sums {
		if s != nil {
			s.Sha256 = ""
		}
	}
}
