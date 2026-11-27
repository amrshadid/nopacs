package api

import (
	"fmt"
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The keys of the details the correction methods put in audit entries,
// beside those requirement.go names.
const (
	// detailQuarantined is the quarantined file a replace takes.
	detailQuarantined = "quarantined_file"
	// detailOldSHA256 and detailNewSHA256 are a replace's checksums.
	detailOldSHA256 = "old_sha256"
	detailNewSHA256 = "new_sha256"
	// detailOverrideReason is the reason a delete gives for overriding a
	// retention rule.
	detailOverrideReason = "override_reason"
	// detailKinds and detailOrigins are a listing's filters, beside
	// detailStates.
	detailKinds   = "kinds"
	detailOrigins = "origins"
	// detailIncludeCleared says whether a listing of findings asks for
	// those cleared too.
	detailIncludeCleared = "include_cleared"
	// detailValueKind is the kind of the value AllowArrivals lets arrive
	// again; never the value, which may be a UID.
	detailValueKind = "value_kind"
	// detailAttributes are the tags of the attributes an edit names, as
	// gggg,eeee; never a value.
	detailAttributes = "attributes"
	// detailChanges is how many changes an edit names.
	detailChanges = "changes"
)

// editDetails returns an edit's audit details: its target, whether it is a
// preview, how many changes it names and their tags (an attribute that
// names no tag recorded as "?"), and show_values when asked. No value is
// recorded: a value an edit gives can be anything a person types.
func editDetails(r *v1.EditRequest) map[string]string {
	tags := make([]string, 0, min(len(r.GetChanges()), correct.MaxEditChanges+1))
	for i, ch := range r.GetChanges() {
		if i > correct.MaxEditChanges {
			break
		}
		t, err := correct.ParseEditTag(ch.GetAttribute())
		if err != nil {
			tags = append(tags, "?")
			continue
		}
		tags = append(tags, tagWord(t))
	}
	d := map[string]string{detailTarget: targetDetail(r.GetTarget()), detailValidateOnly: strconv.FormatBool(r.GetValidateOnly()),
		detailChanges: strconv.Itoa(len(r.GetChanges())), detailAttributes: clip(strings.Join(tags, " "), 250)}
	return showValuesDetails(r.GetShowValues(), d)
}

// tagWord writes a tag as gggg,eeee, as the contract names an attribute.
func tagWord(t uint32) string {
	return fmt.Sprintf("%04X,%04X", t>>16, t&0xffff)
}

// targetDetail returns a correction's target as its entry records it: its
// path of labels, with an instance recorded only as present, since its id
// holds a SOP Instance UID as received.
func targetDetail(t *v1.CorrectionTarget) string {
	p := labelPath(t.GetSubjectLabel(), t.GetSessionLabel(), t.GetScanLabel())
	if t.GetInstanceId() != "" {
		p = clip(p, 200) + " (one instance)"
	}
	return p
}

// deleteDetails returns a delete's audit details: its target, whether it is
// a preview, whether it overrides retention, and what it covers when
// counts is given.
func deleteDetails(r *v1.DeleteRequest, counts *tenant.CorrectionCounts) map[string]string {
	d := map[string]string{
		detailTarget: targetDetail(r.GetTarget()), detailValidateOnly: strconv.FormatBool(r.GetValidateOnly()),
		detailOverrideRetention: strconv.FormatBool(r.GetOverrideRetention()),
	}
	if counts != nil {
		countDetails(d, *counts)
	}
	return d
}

// relabelDetails returns a relabel's audit details: its target and its new
// label.
func relabelDetails(r *v1.RelabelRequest) map[string]string {
	return map[string]string{detailTarget: targetDetail(r.GetTarget()), detailToLabel: labelPath(r.GetToLabel())}
}

// allowDetails returns AllowArrivals' audit details: the kind of the value
// it names.
func allowDetails(r *v1.AllowArrivalsRequest) map[string]string {
	return map[string]string{detailValueKind: clip(r.GetValue().GetKind().String(), 64)}
}

// enumList writes a listing's filter of enum values as its entry records
// it, each value's name, comma separated; at most 16 are recorded.
func enumList[E interface{ String() string }](values []E) string {
	names := make([]string, 0, min(len(values), 16))
	for i, v := range values {
		if i == 16 {
			break
		}
		names = append(names, v.String())
	}
	return clip(strings.Join(names, ","), 240)
}

// correctionFilterDetails returns the filters of a listing of corrections,
// as its entry's details record them.
func correctionFilterDetails(r *v1.ListCorrectionsRequest) map[string]string {
	d := map[string]string{}
	if len(r.GetKinds()) > 0 {
		d[detailKinds] = enumList(r.GetKinds())
	}
	if len(r.GetStates()) > 0 {
		d[detailStates] = enumList(r.GetStates())
	}
	if len(r.GetOrigins()) > 0 {
		d[detailOrigins] = enumList(r.GetOrigins())
	}
	return d
}

// findingFilterDetails returns the filters of a listing of scrub findings,
// as its entry's details record them.
func findingFilterDetails(r *v1.ListScrubFindingsRequest) map[string]string {
	d := map[string]string{detailIncludeCleared: strconv.FormatBool(r.GetIncludeCleared())}
	if len(r.GetKinds()) > 0 {
		d[detailKinds] = enumList(r.GetKinds())
	}
	if r.GetStorage() != "" {
		d[detailStorage] = receiverName(r.GetStorage())
	}
	return d
}
