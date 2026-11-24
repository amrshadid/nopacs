package profile

import "slices"

// Handling is how the pipeline treats an element in every file, whatever
// a profile says. The engine acts by it, and the language refuses a rule
// that names such an element and never selects one, both from Pipeline,
// so what the engine does and what a profile may name cannot drift apart.
type Handling int

// The handlings.
const (
	// NotHandled: the profile decides.
	NotHandled Handling = iota
	// HandledRemoved: the element is removed from every file.
	HandledRemoved
	// HandledKept: pixel data and the tables of its encoding, which only
	// pixel rules change.
	HandledKept
	// HandledWritten: Specific Character Set (0008,0005), removed at every
	// depth and written once at the top level for the text the file holds
	// once every rule has acted (PS3.5 6.1.2.3).
	HandledWritten
)

// The groups whose every element the pipeline handles.
const (
	groupCommand    = 0x0000
	groupFileMeta   = 0x0002
	groupDirectory  = 0x0004
	groupPixel      = 0x7FE0
	groupSignatures = 0xFFFA
	groupPadding    = 0xFFFC
	groupItems      = 0xFFFE
)

// tagSpecificCharacterSet is Specific Character Set (0008,0005).
const tagSpecificCharacterSet = 0x00080005

// pixelKept are the elements of group 7FE0 the pipeline keeps as they are:
// Pixel Data, Float Pixel Data and Double Float Pixel Data (PS3.3 C.7.6.3,
// PS3.5 7.6.2.1), and the Extended Offset Table and its lengths, which
// locate the frames of encapsulated Pixel Data (PS3.5 A.4). Every other
// element of the group (the retired Coefficients SDVN, SDHN and SDDN, and
// whatever a file puts there) describes no encoding this pipeline writes,
// and may hold anything, so it is removed.
var pixelKept = []uint32{0x7FE00001, 0x7FE00002, 0x7FE00008, 0x7FE00009, 0x7FE00010}

// pipelineTags are single elements the pipeline removes from every file:
// they hold values as received, or cannot hold over a changed data set
// (PS3.15 E.1.1, C.12.1).
var pipelineTags = map[uint32]string{
	0x04000561: "the Original Attributes Sequence, which holds values as received (PS3.15 E.1.1)",
	0x04000500: "the Encrypted Attributes Sequence, which holds values as received (PS3.15 E.1.1)",
	0x4FFE0001: "the MAC Parameters Sequence, which cannot hold over a changed data set (PS3.15 C.12.1)",
	0x00880200: "the Icon Image Sequence, a thumbnail of the image whose pixels no pixel rule fills and which may carry the same burned-in text (PS3.3 C.7.6.1.1.6)",
}

// NestedPixelReason is why an element of group 7FE0 inside a sequence item
// is removed: pixels there (an icon, a vendor's thumbnail) are an image no
// pixel rule fills and the check after does not search, so only the top
// level's are ever kept.
const NestedPixelReason = "pixel data inside a sequence item, an image no pixel rule fills and no check searches, which only the top level keeps (PS3.3 C.7.6.3)"

// Pipeline returns how the pipeline handles the element t in every file,
// and why, in words that complete "the element is ...". A private creator
// element is not one: it follows its block (PS3.5 7.8.1).
//
// Removed: the file meta information (group 0002), written anew for every
// file (PS3.10 7.1); every group length (element 0000), written with the
// file (PS3.5 7.2); command elements (group 0000, PS3.7 E.1), which a
// stored data set does not hold, Affected SOP Instance UID (0000,1000)
// among them; the directory elements of a file-set (group 0004, PS3.3
// F.3), Referenced File ID (0004,1500) and Referenced SOP Instance UID in
// File (0004,1511) among them; items and delimiters (group FFFE, PS3.5
// 7.5); the Digital Signatures Sequence (group FFFA) and Data Set Trailing
// Padding (group FFFC, PS3.10 7.2); the Original Attributes, Encrypted
// Attributes and MAC Parameters Sequences; and every element of group 7FE0
// but those pixelKept names; the Icon Image Sequence. Kept: pixelKept, at
// the top level only (NestedPixelReason: the engine removes them inside an
// item). Written: Specific Character Set.
func Pipeline(t uint32) (Handling, string) {
	g := group(t)
	switch {
	case g == groupFileMeta:
		return HandledRemoved, "file meta information, which is written anew with the file (PS3.10 7.1)"
	case t&0xFFFF == 0:
		return HandledRemoved, "a group length, which is written with the file (PS3.5 7.2)"
	case g == groupCommand:
		return HandledRemoved, "a command element, which no stored data set holds (PS3.7 E.1)"
	case g == groupDirectory:
		return HandledRemoved, "a directory element of a file-set, which names files and instances as received and which an archived file does not keep (PS3.3 F.3)"
	case g == groupItems:
		return HandledRemoved, "an item or a delimiter, which is written with the file (PS3.5 7.5)"
	case g == groupSignatures:
		return HandledRemoved, "a digital signature, which cannot hold over a changed data set (PS3.15 E.1.1)"
	case g == groupPadding:
		return HandledRemoved, "data set trailing padding, which carries no information and may hold anything (PS3.10 7.2)"
	case g == groupPixel && slices.Contains(pixelKept, t):
		return HandledKept, "pixel data or the table of its frames, which only pixel rules change"
	case g == groupPixel:
		return HandledRemoved, "an element of group 7FE0 that is neither pixel data nor the table of its frames, which describes no encoding this pipeline writes and may hold anything"
	case t == tagSpecificCharacterSet:
		return HandledWritten, "the character set, which is declared anew once every value is known: ISO_IR 192 when any text is outside the default repertoire, none otherwise (PS3.5 6.1.2.3)"
	}
	if why, ok := pipelineTags[t]; ok {
		return HandledRemoved, why
	}
	return NotHandled, ""
}

// pipelineOwned reports whether the pipeline handles the element t in
// every file, so that no selector selects it.
func pipelineOwned(t uint32) bool {
	h, _ := Pipeline(t)
	return h != NotHandled
}
