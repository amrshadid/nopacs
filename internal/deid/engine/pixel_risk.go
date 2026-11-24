package engine

import (
	"slices"
	"strings"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/dataset"
)

// The images whose pixels may carry burned-in text, beyond those the DICOM
// library's anonymize.BurnedInRisk names (Burned In Annotation YES, a
// secondary capture, the modalities US, IVUS, ES, GM, XC, SC, OT, XA, RF):
// one list, here, beside the one call that judges by it.
//
// By SOP class (PS3.4 B.5): ultrasound (the retired classes too), the
// visible light family (endoscopy, microscopy, photography, still and
// video), ophthalmic photography and tomography. Their devices commonly
// draw the patient's name and a date into the picture. By modality: OP
// (ophthalmic photography), SM (slide microscopy), PX (panoramic dental
// radiography), IO (intra-oral radiography), whose consoles stamp the
// patient into the image. A whole slide's label and overview images
// (Image Type (0008,0008) LABEL or OVERVIEW, PS3.3 C.8.12.4.1.1) are
// photographs of the slide's label, which carries a name or an accession
// number: at risk whatever Burned In Annotation says.
//
// DX and CR are not: a radiograph's burned-in markers are laterality and
// technique, rarely the person, and holding every radiograph would teach
// people to release without looking. A profile that knows a device stamps
// identity on its radiographs writes a pixel rule for it.
var (
	riskSOPClasses = []string{
		"1.2.840.10008.5.1.4.1.1.6.1",    // Ultrasound Image Storage
		"1.2.840.10008.5.1.4.1.1.3.1",    // Ultrasound Multi-frame Image Storage
		"1.2.840.10008.5.1.4.1.1.6",      // Ultrasound Image Storage (Retired)
		"1.2.840.10008.5.1.4.1.1.3",      // Ultrasound Multi-frame Image Storage (Retired)
		"1.2.840.10008.5.1.4.1.1.77.1.1", // VL Endoscopic Image Storage
		"1.2.840.10008.5.1.4.1.1.77.1.1.1",
		"1.2.840.10008.5.1.4.1.1.77.1.2", // VL Microscopic Image Storage
		"1.2.840.10008.5.1.4.1.1.77.1.2.1",
		"1.2.840.10008.5.1.4.1.1.77.1.3", // VL Slide-Coordinates Microscopic Image Storage
		"1.2.840.10008.5.1.4.1.1.77.1.4", // VL Photographic Image Storage
		"1.2.840.10008.5.1.4.1.1.77.1.4.1",
		"1.2.840.10008.5.1.4.1.1.77.1.5.1", // Ophthalmic Photography 8 Bit Image Storage
		"1.2.840.10008.5.1.4.1.1.77.1.5.2", // Ophthalmic Photography 16 Bit Image Storage
		"1.2.840.10008.5.1.4.1.1.77.1.5.4", // Ophthalmic Tomography Image Storage
	}
	riskModalities = []string{"OP", "SM", "PX", "IO"}
)

// wholeSlide is VL Whole Slide Microscopy Image Storage (PS3.4 B.5).
const wholeSlide = "1.2.840.10008.5.1.4.1.1.77.1.6"

// secondaryCapture is the prefix of the Secondary Capture Image Storage SOP
// classes, single and multi-frame (PS3.4 B.5).
const secondaryCapture = "1.2.840.10008.5.1.4.1.1.7"

// burnedInRisk reports whether an image's pixels may carry burned-in text,
// by its header as received, and why, as a fixed word:
// "slide_label" (a whole slide's label or overview image, whatever Burned
// In Annotation says); "burned_in_annotation", "secondary_capture" or
// "modality" (the DICOM library's judgment); "sop_class" or "modality"
// (the lists above, when Burned In Annotation is not NO).
func burnedInRisk(ds *dataset.Dataset) (bool, string) {
	class, _ := topText(ds, 0x00080016)
	if class == wholeSlide {
		types, _ := topText(ds, 0x00080008)
		for _, v := range strings.Split(types, `\`) {
			if v = strings.ToUpper(strings.TrimSpace(v)); v == "LABEL" || v == "OVERVIEW" {
				return true, "slide_label"
			}
		}
	}
	if risk, _ := anonymize.BurnedInRisk(ds); risk {
		return true, libraryCause(ds, class)
	}
	if v, _ := topText(ds, tagBurnedInAnnotation); strings.EqualFold(strings.TrimSpace(v), "NO") {
		return false, ""
	}
	if slices.Contains(riskSOPClasses, class) {
		return true, "sop_class"
	}
	if m, _ := topText(ds, 0x00080060); slices.Contains(riskModalities, strings.ToUpper(strings.TrimSpace(m))) {
		return true, "modality"
	}
	return false, ""
}

// libraryCause is the fixed word of why the DICOM library judged an image
// at risk, in its own order: Burned In Annotation YES, a secondary
// capture, a modality commonly drawn on.
func libraryCause(ds *dataset.Dataset, class string) string {
	if v, _ := topText(ds, tagBurnedInAnnotation); strings.EqualFold(strings.TrimSpace(v), "YES") {
		return "burned_in_annotation"
	}
	if class == secondaryCapture || strings.HasPrefix(class, secondaryCapture+".") {
		return "secondary_capture"
	}
	return "modality"
}
