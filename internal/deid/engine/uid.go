package engine

import (
	"crypto/hmac"
	"crypto/sha256"
	"math/big"
	"strings"

	dicomuid "github.com/amrshadid/go-dicom/uid"
)

// uidRoot is the root of a UID derived from a UUID (PS3.5 B.2): "2.25."
// followed by the UUID as one decimal integer.
const uidRoot = "2.25."

// RemapUID returns the UID that replaces original under a tenant's UID key:
// "2.25." followed by the decimal of a version 8 UUID (RFC 9562 5.8) whose
// 122 free bits are the first bits of HMAC-SHA-256(key, original). The same
// original always gives the same UID under one key, so references between
// files, and a file received twice, still match; another tenant's key gives
// another UID; and nobody without the key can tell the original from the
// result. It is at most 44 characters, a valid UID by PS3.5 9.1 and B.2,
// and needs no table.
//
// original is the value without its padding. RemapUID does not check that
// it is a well-formed UID: a malformed value identifies as well as a good
// one and is replaced the same way. Whether a UID is remapped at all
// (Registered) is the caller's question.
func RemapUID(key []byte, original string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(original))
	sum := m.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	// RFC 9562 5.8: version 8 in the high nibble of octet 6, the variant
	// 10 in the two high bits of octet 8.
	u[6] = u[6]&0x0F | 0x80
	u[8] = u[8]&0x3F | 0x80
	return uidRoot + new(big.Int).SetBytes(u[:]).String()
}

// Registered reports whether a UID is one the standard registers (PS3.6
// Annex A): a SOP class, a transfer syntax, a coding scheme, a well-known
// instance. It identifies a kind of object, never a patient, and replacing
// it would make the file unreadable, so it is never remapped. The library's
// registry does not list every UID the standard defines (Secondary Capture,
// RT Dose, RT Plan and Segmentation among those missing), so the root the
// standard keeps for its own UIDs, 1.2.840.10008 (PS3.5 B.1), counts too:
// no identifier of a patient or a study can be made under it. So do the
// public coding schemes of PS3.16 Table 8-1 (publicCodingSchemeUIDs).
func Registered(u string) bool {
	_, public := publicCodingSchemeUIDs[u]
	return public || dicomuid.GetUIDInfo(u) != nil || strings.HasPrefix(u, standardRoot)
}

// vocabularyUIDTags are the attributes whose UID names a vocabulary
// (PS3.3 8.1-8.6, the Code Sequence Macro and its Enhanced Encoding Mode;
// PS3.16 8): the coding scheme a code is of, the organization that
// extended a context group, the context group, and the mapping resource.
// A public vocabulary's UID is the same for every patient and is kept
// (Registered: the standard's own, and those of PS3.16 Table 8-1,
// publicCodingSchemeUIDs). Any other is local, mostly under the
// institution's own root, so it names the institution whose name the
// profile removed: such a value is remapped whatever the profile decides
// for it but removal (vocabularyRemap). Context Group Extension Creator
// UID always names an organization (PS3.16 8.6), so it is remapped unless
// the standard registers it. PS3.15 Table E.1-1 lists none of these.
var vocabularyUIDTags = map[uint32]string{
	0x0008010C: "Coding Scheme UID",
	0x0008010D: "Context Group Extension Creator UID",
	0x00080117: "Context UID",
	0x00080118: "Mapping Resource UID",
}

// vocabularyRemap returns the UID action in place of keep on an attribute
// of vocabularyUIDTags, and whether it did. The UID action keeps the
// registered UIDs of the value (remapUIDValue).
func vocabularyRemap(t uint32, a Action) (Action, bool) {
	if _, ok := vocabularyUIDTags[t]; !ok || a.Kind != ActKeep {
		return a, false
	}
	return Action{Kind: ActUID}, true
}

// publicCodingSchemeUIDs are the UIDs of the coding schemes PS3.16 Table
// 8-1 (2026d) lists outside the standard's own root, which the root
// already covers (Registered). Each names a public vocabulary, the same in
// every file that uses it, never a patient or an institution; remapped, a
// code would no longer say what it means.
var publicCodingSchemeUIDs = map[string]string{
	"2.16.840.1.113883.6.76":         "ACR Index",
	"1.2.840.10065.1.12":             "ASTM E 2084",
	"2.16.840.1.113883.6.12":         "CPT-4",
	"2.16.840.1.113883.6.82":         "CPT-5",
	"2.16.840.1.113883.3.26.2":       "Cancer Data Standard Repository",
	"2.16.840.1.113883.6.13":         "CDT-2",
	"2.16.840.1.113883.6.6":          "Clinical Terms Version 3",
	"2.16.840.1.113883.6.119":        "FMA",
	"2.16.840.1.113883.6.14":         "HCPCS",
	"2.16.840.1.113883.6.3":          "ICD-10",
	"2.16.840.1.113883.6.90":         "ICD-10-CM",
	"2.16.840.1.113883.6.4":          "ICD-10-PCS",
	"2.16.840.1.113883.6.42":         "ICD-9",
	"2.16.840.1.113883.6.2":          "ICD-9-CM",
	"2.16.840.1.113883.6.43.1":       "ICD-O-3",
	"2.16.840.1.113883.6.99":         "ISO 639-1",
	"2.16.840.1.113883.6.100":        "ISO 639-2",
	"2.16.1":                         "ISO 3166-1",
	"2.16.840.1.113883.6.1":          "LOINC",
	"2.16.840.1.113883.6.24":         "ISO/IEEE 11073 Medical Device Nomenclature",
	"2.16.840.1.113883.6.177":        "MeSH",
	"2.16.840.1.113883.15.2":         "NASPE/BPEG Defibrillator Code",
	"2.16.840.1.113883.15.3":         "NASPE/BPEG Generic Pacemaker Code",
	"2.16.840.1.113883.3.26.1.1":     "NCI Thesaurus",
	"2.16.840.1.113883.6.69":         "National Drug Code Directory",
	"2.16.840.1.113883.6.210":        "NeuroNames",
	"2.16.840.1.113883.2.1.3.2.4.21": "NICIP",
	"1.3.6.1.4.1.19376.1.8.2.1":      "PathLex",
	"2.16.840.1.113883.6.50":         "Place of Service",
	"2.16.840.1.113883.6.256":        "RadLex",
	"2.16.840.1.113883.6.121":        "RFC 3066",
	"2.16.840.1.113883.6.316":        "RFC 5646",
	"2.16.840.1.113883.6.88":         "RxNorm",
	"2.16.840.1.113883.6.53":         "SDM",
	"2.16.840.1.113883.6.51":         "SNOMED V3",
	"2.16.840.1.113883.6.96":         "SNOMED CT",
	"2.16.840.1.113883.6.8":          "UCUM",
	"2.16.840.1.113883.6.86":         "UMLS",
	"2.16.840.1.113883.6.55":         "UPC",
}

// standardRoot is the UID root PS3.5 B.1 keeps for UIDs the standard
// defines.
const standardRoot = "1.2.840.10008."

// remapUIDValue remaps each UID of a value of VR UI, values separated by
// backslashes (PS3.5 6.4), keeping the registered ones and the empty ones.
// It returns the new value and the originals it replaced.
func remapUIDValue(key []byte, value string) (string, []string) {
	parts := strings.Split(value, `\`)
	var replaced []string
	for i, p := range parts {
		p = strings.Trim(p, "\x00 ")
		parts[i] = p
		if p == "" || Registered(p) {
			continue
		}
		parts[i] = RemapUID(key, p)
		replaced = append(replaced, p)
	}
	return strings.Join(parts, `\`), replaced
}
