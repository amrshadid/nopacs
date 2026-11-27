package correct

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/config"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"
	"github.com/amrshadid/go-dicom/valuerep"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What an edit may change, and how a change is made: through the DICOM
// library's data set, never by NoPACs' own encoding.

// MaxEditChanges is the most attributes one edit names: a correction of a
// few header values, each judged on every instance it reaches.
const MaxEditChanges = 64

// EditChange is one attribute an edit sets, changes or removes: Tag, and
// Value when it is set (an attribute the instance lacks is added), or
// Remove.
type EditChange struct {
	// Tag is the attribute's tag, (gggg,eeee) as one number.
	Tag uint32
	// Value is the value it is given, its values separated by a backslash
	// as DICOM writes them (PS3.5 6.4); empty gives it an empty value.
	Value string
	// Remove says the attribute is removed instead.
	Remove bool
}

// Attributes an edit never changes, each with why. The catalog is built on
// the three UIDs, so changing one is a relabel or another instance, not an
// edit; the SOP Class names what the instance is and the file meta
// information repeats it; the character set says how every text value is
// read; and the attributes that say how the pixel data's bytes are laid
// out would leave the pixels read as something else (PS3.3 C.7.6.3).
var editRefused = map[uint32]string{
	0x0020000D: "the Study Instance UID names the instance's session in the catalog",
	0x0020000E: "the Series Instance UID names the instance's scan in the catalog",
	0x00080018: "the SOP Instance UID names the instance in the catalog",
	0x00080016: "the SOP Class UID names what the instance is",
	0x00080005: "the Specific Character Set says how every text value of the file is read",
	0x00280002: "it says how the pixel data's bytes are laid out",
	0x00280004: "it says how the pixel data's bytes are laid out",
	0x00280006: "it says how the pixel data's bytes are laid out",
	0x00280008: "it says how the pixel data's bytes are laid out",
	0x00280010: "it says how the pixel data's bytes are laid out",
	0x00280011: "it says how the pixel data's bytes are laid out",
	0x00280100: "it says how the pixel data's bytes are laid out",
	0x00280101: "it says how the pixel data's bytes are laid out",
	0x00280102: "it says how the pixel data's bytes are laid out",
	0x00280103: "it says how the pixel data's bytes are laid out",
	// The study's date is the session's in the catalog and in every scan's
	// metadata of the session; an edit of some of its instances would leave
	// them disagreeing.
	0x00080020: "the Study Date is the session's in the catalog, kept by a reception",
	// What de-identification wrote of itself (PS3.15 E.1, PS3.3 C.12.1): a
	// reader of the archive tells a de-identified file by them.
	0x00120062: "it records that the patient's identity was removed (PS3.15 E.1)",
	0x00120063: "it records how the file was de-identified (PS3.15 E.1)",
	0x00280303: "it records whether dates were modified by de-identification (PS3.15 E.1)",
}

// scanValueTags are the attributes whose value the catalog keeps for a
// whole scan (tenant.ScanValues): an edit of one instance would give the
// scan the value while its other instances keep theirs.
var scanValueTags = map[uint32]bool{0x0008103E: true, 0x00080060: true, 0x00200011: true}

// checkTargetChanges returns an Invalid error naming a change an edit of
// target may not make: a value the catalog keeps for a whole scan, by an
// edit of one instance.
func checkTargetChanges(target tenant.CorrectionTarget, changes []EditChange) error {
	if target.Instance == "" {
		return nil
	}
	for _, c := range changes {
		if scanValueTags[c.Tag] {
			return fault.New(fault.Invalid, tagText(c.Tag)+": the catalog keeps it for the whole scan; edit the scan, not one instance")
		}
	}
	return nil
}

// textVRs are the value representations a value given as text is written
// in as it is (PS3.5 6.2). The others are binary, a sequence, or unknown.
var textVRs = map[string]bool{"AE": true, "AS": true, "CS": true, "DA": true, "DS": true, "DT": true, "IS": true, "LO": true,
	"LT": true, "PN": true, "SH": true, "ST": true, "TM": true, "UC": true, "UI": true, "UR": true, "UT": true}

// singleVRs are the text value representations whose value is one value,
// in which a backslash is a character (PS3.5 6.4).
var singleVRs = map[string]bool{"LT": true, "ST": true, "UR": true, "UT": true}

// ParseEditTag returns the tag an edit names by its keyword
// ("SeriesDescription") or its number ("0008,103E", "(0008,103E)",
// "0008103E"), as the data dictionary of the DICOM library knows it.
func ParseEditTag(s string) (uint32, error) {
	t := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "("), ")")
	hex := strings.ReplaceAll(t, ",", "")
	if len(hex) == 8 {
		if v, err := strconv.ParseUint(hex, 16, 32); err == nil {
			return uint32(v), nil
		}
	}
	if v := tag.GlobalDictionary().GetByKeyword(t); v != 0 {
		return uint32(v), nil
	}
	return 0, fault.New(fault.Invalid, "an attribute is named by its keyword or as gggg,eeee")
}

// tagText writes a tag as (gggg,eeee) with its keyword, as a refusal names it.
func tagText(t uint32) string {
	name := tag.Tag(t).GetKeyword()
	text := fmt.Sprintf("(%04X,%04X)", t>>16, t&0xffff)
	if name == "" {
		return text
	}
	return name + " " + text
}

// editVR returns the VR the data dictionary gives the attribute, or why an
// edit may not name it.
func editVR(t uint32) (string, error) {
	group, element := t>>16, t&0xffff
	refuse := func(why string) (string, error) {
		return "", fault.New(fault.Invalid, tagText(t)+": "+why)
	}
	switch {
	case group == 0x0002:
		return refuse("the file meta information is made by the writer")
	case group == 0x7FE0:
		return refuse("pixel data is never edited")
	case group&1 == 1:
		return refuse("a private attribute's form is its creator's; an edit names public attributes")
	case element == 0:
		return refuse("a group length is the writer's")
	case group >= 0x6000 && group <= 0x601E && element == 0x3000:
		return refuse("overlay data is bulk pixel data")
	}
	if why, ok := editRefused[t]; ok {
		return refuse(why)
	}
	vr := tag.GlobalDictionary().GetVR(tag.Tag(t))
	switch {
	case vr == "":
		return refuse("the data dictionary does not know it")
	case !textVRs[vr]:
		return refuse("its VR " + vr + " is not one a value given as text fits (PS3.5 6.2)")
	}
	return vr, nil
}

// checkValue returns why value does not fit vr (go-dicom's validation, each
// value of a multi-valued attribute alone), or nil. A value is in the
// default repertoire (PS3.5 6.1.2.1): an edit does not change the file's
// character set.
func checkValue(t uint32, vr, value string) error {
	for _, r := range value {
		// A text of one value may hold the format effectors (PS3.5 6.1.3).
		control := r < 0x20 && (!singleVRs[vr] || r != '\r' && r != '\n' && r != '\t' && r != '\f')
		if r > 0x7e || control {
			return fault.New(fault.Invalid, tagText(t)+": a value is in the default repertoire, printable ASCII (PS3.5 6.1.2.1)")
		}
	}
	values := []string{value}
	if !singleVRs[vr] {
		values = strings.Split(value, `\`)
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		if err := valuerep.ValidateValue(vr, v); err != nil {
			return fault.New(fault.Invalid, tagText(t)+": the value does not fit its VR "+vr)
		}
	}
	return nil
}

// checkChanges returns an Invalid error naming the first change an edit
// may not make, or nil: 1 to MaxEditChanges, each attribute once, each one
// an edit may change, each value fitting its VR.
func checkChanges(changes []EditChange) error {
	if len(changes) == 0 || len(changes) > MaxEditChanges {
		return fault.New(fault.Invalid, "an edit names 1 to "+strconv.Itoa(MaxEditChanges)+" attributes")
	}
	seen := map[uint32]bool{}
	for _, c := range changes {
		if seen[c.Tag] {
			return fault.New(fault.Invalid, tagText(c.Tag)+": an edit names each attribute once")
		}
		seen[c.Tag] = true
		vr, err := editVR(c.Tag)
		if err != nil {
			return err
		}
		if c.Remove {
			if c.Value != "" {
				return fault.New(fault.Invalid, tagText(c.Tag)+": a removal gives no value")
			}
			continue
		}
		if err := checkValue(c.Tag, vr, c.Value); err != nil {
			return err
		}
	}
	return nil
}

// quiet is the logger the data set's checks of an added element report
// to: what it says names a value of the file.
var quiet = slog.New(slog.DiscardHandler)

// applyChanges makes the changes on ds: a removal removes the attribute
// where it is, a value replaces the attribute's or adds it, in the VR the
// data dictionary gives it.
func applyChanges(ds *dataset.Dataset, changes []EditChange) error {
	ctx := config.WithLogger(context.Background(), quiet)
	for _, c := range changes {
		t := tag.Tag(c.Tag)
		if c.Remove {
			ds.Remove(t)
			continue
		}
		vr, err := editVR(c.Tag)
		if err != nil {
			return err
		}
		if elem, ok := ds.Get(t); ok {
			elem.SetValue([]byte(c.Value))
			elem.SetVR(dataelem.VR(vr))
			continue
		}
		if err := ds.AddWithContext(ctx, dataelem.NewDataElement(t, dataelem.VR(vr), []byte(c.Value))); err != nil {
			return fault.Wrap(fault.Internal, err, "an attribute cannot be added")
		}
	}
	return nil
}

// editValues returns the catalog values the changes set (tenant.EditValues),
// and whether they name any: each value as a reception takes it from a
// file's header (tenant.ScanValuesOf), so the catalog holds what a rebuild
// would read from the new revision.
func editValues(changes []EditChange) (tenant.EditValues, bool) {
	var v tenant.EditValues
	var modality, description, number string
	named := false
	for _, c := range changes {
		switch c.Tag {
		case 0x0008103E:
			description, v.SetDescription, named = c.Value, true, true
		case 0x00080060:
			modality, v.SetModality, named = c.Value, true, true
		case 0x00200011:
			number, v.SetSeriesNumber, named = c.Value, true, true
		}
	}
	sv := tenant.ScanValuesOf("", modality, description, number)
	v.SeriesDescription, v.Modality = sv.SeriesDescription, sv.Modality
	v.SeriesNumber, v.HasSeriesNumber = sv.SeriesNumber, sv.HasSeriesNumber
	return v, named
}

// setTags returns the tags the changes give a value.
func setTags(changes []EditChange) []uint32 {
	var out []uint32
	for _, c := range changes {
		if !c.Remove {
			out = append(out, c.Tag)
		}
	}
	return out
}

// removedTags returns the tags the changes remove.
func removedTags(changes []EditChange) []uint32 {
	var out []uint32
	for _, c := range changes {
		if c.Remove {
			out = append(out, c.Tag)
		}
	}
	return out
}
