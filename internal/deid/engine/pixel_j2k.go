package engine

import (
	"encoding/binary"

	"github.com/amrshadid/go-dicom/compress"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/encaps"
	"github.com/amrshadid/go-dicom/tag"
)

// A JPEG 2000 codestream (ITU-T T.800 Annex A; High-Throughput JPEG 2000,
// ITU-T T.814, shares its marker syntax) carries free text in its comment
// segments (COM, 0xFF64), in the main header and in any tile-part header,
// which a decoder skips. Each frame is walked marker by marker: every COM
// is removed, and with it the main header's CPF and CRG, whose words are
// free and which no decoder needs for the samples; every other segment
// must be one a decoder needs, at the length its contents define, and
// carry no free words (pixel_j2k_markers.go). A frame holding anything
// else is held as pixel_text_unsearched, as the JPEG walk holds one: text
// there would travel unseen by the check after, which does not search
// pixel data.
//
// A COM in a tile-part header is removed too, and the tile-part's length
// (Psot) and its entry in a TLM segment lowered by the bytes removed, so
// the codestream stays one a decoder walks by those lengths. The packet
// data after SOD is copied byte for byte, never decoded, so the frame
// decodes to the same samples.
//
// Held, besides a marker the walk does not know: a frame that begins with
// a JP2 file-format signature box rather than SOC (PS3.5 A.4.4 carries the
// bare codestream); a segment longer or shorter than its contents define;
// a second SIZ, COD, QCD, CAP, CPF or CRG, or a second COC, QCC or RGN for
// one component; a CAP that names any part but Part 15, or a Ccap15 with a
// reserved bit set (T.814 A.3); quantization values for more or fewer
// subbands than the decomposition levels in force give (T.800 A.6.4); a
// TLM entry, or a PLM or PLT list of packet lengths, that does not match
// the tile-parts as they lie; packed packet headers (PPM, PPT), whose
// bytes only a parse of the packets delimits; fill bytes between markers,
// which T.800 does not allow; and after EOC anything but 0x00 padding and
// the 0xFF fill byte.
//
// What lies inside the packet data is not parsed, as the JPEG walk does
// not parse entropy-coded data. The limit this leaves: bytes after a
// tile-part's last packet, up to the end its Psot gives, are part of its
// data to the walk, and a decoder skips them, so text placed there is
// taken. Only when the tile-part has PLT segments, or the main header PLM,
// are such bytes found: the packet lengths must then sum to the tile-part's
// data exactly, and a frame where they do not is held.

// The JPEG 2000 markers the walk knows, by their second byte (T.800 Table
// A.2; CAP and CPF, T.800 A.5.2 and A.5.3 as amended for T.814).
const (
	j2kSOC = 0x4F
	j2kCAP = 0x50
	j2kSIZ = 0x51
	j2kCOD = 0x52
	j2kCOC = 0x53
	j2kTLM = 0x55
	j2kPLM = 0x57
	j2kPLT = 0x58
	j2kCPF = 0x59
	j2kQCD = 0x5C
	j2kQCC = 0x5D
	j2kRGN = 0x5E
	j2kPOC = 0x5F
	j2kCRG = 0x63
	j2kCOM = 0x64
	j2kSOT = 0x90
	j2kSOD = 0x93
	j2kEOC = 0xD9
)

// j2kSyntax reports whether a transfer syntax carries JPEG 2000 Part 1 or
// High-Throughput JPEG 2000 codestreams, whose frames are walked here
// (PS3.5 A.4.4, A.4.10). The Part 2 multi-component syntaxes are not: their
// codestreams may hold extension markers this walk does not know.
func j2kSyntax(ts string) bool {
	switch ts {
	case compress.JPEG2000Lossless, compress.JPEG2000Lossy,
		compress.HTJ2KLossless, compress.HTJ2KLosslessRPCL, compress.HTJ2K:
		return true
	}
	return false
}

// removeJ2KText removes the comment segments, and the CPF and CRG
// segments, of every frame of the JPEG 2000 Pixel Data of scratch and
// returns the name of each segment it removed ("COM", "CPF", "CRG"), in
// order. The frames are laid out again, one fragment each with a Basic
// Offset Table, only when one was removed. A frame the walk cannot vouch
// for is a Failure, CausePixelTextUnsearched; Pixel Data not divided into
// frames as the image says, CausePixelsUndecodable.
func removeJ2KText(scratch *dataset.Dataset) ([]string, error) {
	hold := &Failure{Cause: CausePixelTextUnsearched, Path: formatTag(tagPixelData)}
	enc, err := scratch.ExtractEncapsulatedFrames()
	if err != nil {
		return nil, &Failure{Cause: CausePixelsUndecodable, Path: formatTag(tagPixelData)}
	}
	n := 0
	if info, err := scratch.GetPixelDataInfo(); err == nil {
		n = info.NumberOfFrames
	}
	frames, err := encaps.Frames(enc, n)
	if err != nil {
		return nil, &Failure{Cause: CausePixelsUndecodable, Path: formatTag(tagPixelData)}
	}
	var removed []string
	cleaned := make([][]byte, len(frames))
	for i, f := range frames {
		out, names, ok := cleanJ2K(f)
		if !ok {
			return nil, hold
		}
		removed = append(removed, names...)
		cleaned[i] = out
	}
	if len(removed) == 0 {
		return nil, nil
	}
	for i := range cleaned {
		if len(cleaned[i])%2 == 1 {
			// The pad of an odd length (PS3.5 A.4), after EOC, where the
			// walk allows zeros.
			cleaned[i] = append(cleaned[i], 0x00)
		}
	}
	value, err := compress.EncapsulateFrames(cleaned, 1, true)
	if err != nil {
		return nil, &Failure{Cause: CausePixelsUndecodable, Path: formatTag(tagPixelData)}
	}
	elem, _ := scratch.Get(tag.Tag(tagPixelData))
	elem.SetVR(dataelem.OB) // encapsulated pixel data is OB (PS3.5 A.4)
	if err := scratch.SetValue(tag.Tag(tagPixelData), value); err != nil {
		return nil, &Failure{Cause: CausePixelsUndecodable, Path: formatTag(tagPixelData)}
	}
	return removed, nil
}

// cleanJ2K walks one frame's codestream and returns it without its COM,
// CPF and CRG segments, the name of each segment it removed, in order, and
// whether every other segment is one a decoder needs at its defined length.
// The frame is returned as it is when nothing was removed.
func cleanJ2K(f []byte) ([]byte, []string, bool) {
	w := &j2kWalk{f: f, main: newJ2KCoding(), tiles: map[int]*j2kCoding{}}
	if !w.walk() {
		return nil, nil, false
	}
	if len(w.drops) == 0 {
		return f, nil, true
	}
	names := make([]string, len(w.drops))
	for i, d := range w.drops {
		names[i] = j2kMarkerNames[d.marker]
	}
	return w.rewrite(), names, true
}

// j2kMarkerNames are the names of the segments the walk removes.
var j2kMarkerNames = map[byte]string{j2kCOM: "COM", j2kCPF: "CPF", j2kCRG: "CRG"}

// j2kDrop is a segment to remove, [start, end) in the frame, and its
// marker's second byte.
type j2kDrop struct {
	start, end int
	marker     byte
}

// j2kWalk is the state of the walk of one codestream.
type j2kWalk struct {
	f []byte
	// csiz is the number of components; cb the bytes of a component
	// index in COC, QCC, RGN and POC: one below 257 components, else two.
	csiz, cb int
	// numTiles is the number of tiles SIZ gives.
	numTiles int
	// eoc is where the EOC marker begins, found from the frame's end.
	eoc int
	// main is the main header's coding; tiles each tile's, from its
	// tile-part headers.
	main  *j2kCoding
	tiles map[int]*j2kCoding
	// once holds the main header's markers that may appear only once.
	once map[byte]bool
	// tlm are the TLM entries in order; tlmZ and plmZ the next index a
	// TLM or PLM segment must carry.
	tlm        []j2kTLMEntry
	tlmZ, plmZ int
	// plm are the packet lengths' sums of the PLM segments, a tile-part
	// each, in order; hasPLM says there was one.
	plm    []int
	hasPLM bool
	// parts are the tile-parts as they lie.
	parts []j2kPart
	// drops are the segments to remove, in order; patches the length
	// fields to lower.
	drops   []j2kDrop
	patches []j2kPatch
}

// j2kCoding is what a header (the main one, or a tile's) set for coding.
type j2kCoding struct {
	// nl is COD's number of decomposition levels, -1 with no COD; cocNL
	// each COC's, by component.
	nl    int
	cocNL map[int]int
	// quant are the QCD (comp -1) and QCC segments to check against the
	// levels once the header is whole.
	quant []j2kQuant
	// hasQCD, qcc and rgn mark what may appear once.
	hasQCD   bool
	qcc, rgn map[int]bool
}

// newJ2KCoding returns an empty coding.
func newJ2KCoding() *j2kCoding {
	return &j2kCoding{nl: -1, cocNL: map[int]int{}, qcc: map[int]bool{}, rgn: map[int]bool{}}
}

// j2kQuant is one QCD or QCC segment: its component (-1 for QCD), how many
// subbands its values are for, and whether it is scalar derived, which has
// one value whatever the levels.
type j2kQuant struct {
	comp, subbands int
	derived        bool
}

// j2kTLMEntry is one tile-part of a TLM segment: its tile, -1 when the
// segment leaves it implicit, where and how wide its length is in the
// frame, and the length.
type j2kTLMEntry struct {
	tile, at, width int
	length          uint32
}

// j2kPart is one tile-part: where its SOT begins and where it ends, its
// tile, its data's length after SOD, its Psot, the bytes of COM removed
// from its header, and the sum of its PLT packet lengths.
type j2kPart struct {
	sot, end, tile, body int
	psot, dropped        uint32
	pltSum               int
	hasPLT               bool
}

// j2kPatch is a big-endian length field of width bytes at an offset of the
// frame as received, and the value it is rewritten to.
type j2kPatch struct {
	at, width int
	value     uint32
}

// walk walks the codestream and reports whether it is one this package
// vouches for.
func (w *j2kWalk) walk() bool {
	f := w.f
	n := len(f)
	if n < 4 || f[0] != 0xFF || f[1] != j2kSOC {
		// A JP2 file (its signature box first) or anything else.
		return false
	}
	w.eoc = findEOC(f)
	m, p, next, ok := w.segment(2, n)
	if !ok || m != j2kSIZ || !w.siz(p) {
		return false
	}
	w.once = map[byte]bool{j2kSIZ: true}
	pos := next
	for {
		m, p, next, ok = w.segment(pos, n)
		if !ok {
			return false
		}
		if m == j2kSOT {
			break
		}
		if !w.mainSegment(m, p, pos, next) {
			return false
		}
		pos = next
	}
	if w.main.nl < 0 || !w.main.hasQCD || !w.quantOK(w.main, nil) {
		return false
	}
	for {
		m, p, next, ok = w.segment(pos, n)
		if !ok {
			return false
		}
		if m == j2kEOC {
			for _, b := range f[next:] {
				if b != 0x00 && b != 0xFF {
					return false
				}
			}
			break
		}
		if m != j2kSOT {
			return false
		}
		if pos, ok = w.tilePart(pos, p, next); !ok {
			return false
		}
	}
	return len(w.parts) > 0 && w.pointersOK()
}

// findEOC returns where the EOC marker begins, the frame's last two bytes
// once its padding (0x00) and fill (0xFF) are set aside, or -1.
func findEOC(f []byte) int {
	e := len(f)
	for e > 0 && (f[e-1] == 0x00 || f[e-1] == 0xFF) {
		e--
	}
	if e < 2 || f[e-1] != j2kEOC || f[e-2] != 0xFF {
		return -1
	}
	return e - 2
}

// segment reads the marker at pos and, but for SOD and EOC, its segment,
// which must end by end; it returns the marker's second byte, the payload
// after the length, and where the next marker begins. T.800 allows no fill
// bytes between markers.
func (w *j2kWalk) segment(pos, end int) (byte, []byte, int, bool) {
	f := w.f
	if pos+2 > end || f[pos] != 0xFF {
		return 0, nil, 0, false
	}
	m := f[pos+1]
	if m == j2kSOD || m == j2kEOC {
		return m, nil, pos + 2, true
	}
	if pos+4 > end {
		return 0, nil, 0, false
	}
	length := int(binary.BigEndian.Uint16(f[pos+2:]))
	if length < 2 || pos+2+length > end {
		return 0, nil, 0, false
	}
	return m, f[pos+4 : pos+2+length], pos + 2 + length, true
}

// tilePart walks a tile-part from its SOT at sot, p its payload (Isot,
// Psot, TPsot, TNsot: T.800 A.4.2) and next the first byte after it, and
// returns where the next marker begins: its header to SOD, then its data,
// which runs to Psot bytes from SOT or, when Psot is 0, to EOC.
func (w *j2kWalk) tilePart(sot int, p []byte, next int) (int, bool) {
	if len(p) != 8 {
		return 0, false
	}
	part := j2kPart{sot: sot, tile: int(binary.BigEndian.Uint16(p)), psot: binary.BigEndian.Uint32(p[2:])}
	if part.tile >= w.numTiles {
		return 0, false
	}
	switch {
	case part.psot == 0:
		part.end = w.eoc
	case part.psot < 14 || int64(part.psot) > int64(len(w.f)-sot):
		return 0, false
	default:
		part.end = sot + int(part.psot)
	}
	if part.end < next+2 {
		return 0, false
	}
	c := w.tiles[part.tile]
	if c == nil {
		c = newJ2KCoding()
		w.tiles[part.tile] = c
	}
	pos := next
	for {
		m, pl, nx, ok := w.segment(pos, part.end)
		if !ok {
			return 0, false
		}
		if m == j2kSOD {
			pos = nx
			break
		}
		switch m {
		case j2kCOM: // Rcom, then the comment (T.800 A.9.2)
			ok = len(pl) >= 2 && w.drop(m, pos, nx, &part)
		case j2kPLT: // Zplt, then packet lengths
			var sum int
			sum, ok = packetLengths(pl[min(1, len(pl)):], len(w.f))
			ok = ok && len(pl) >= 2
			part.pltSum += sum
			part.hasPLT = true
		default:
			ok = w.coding(m, pl, c)
		}
		if !ok {
			return 0, false
		}
		pos = nx
	}
	if !w.quantOK(c, c) {
		return 0, false
	}
	part.body = part.end - pos
	if part.hasPLT && part.pltSum != part.body {
		return 0, false
	}
	if part.dropped > 0 && part.psot != 0 {
		// The COM bytes lie inside the tile-part, so fewer than Psot.
		w.patches = append(w.patches, j2kPatch{at: sot + 6, width: 4, value: part.psot - part.dropped})
	}
	w.parts = append(w.parts, part)
	return part.end, true
}

// pointersOK checks the TLM entries and the PLM packet lengths against the
// tile-parts as they lie, and lowers each TLM length by the COM bytes
// removed from its tile-part.
func (w *j2kWalk) pointersOK() bool {
	if len(w.tlm) > 0 {
		if len(w.tlm) != len(w.parts) {
			return false
		}
		for i, e := range w.tlm {
			part := w.parts[i]
			// With no tile index, the tile-parts are the tiles, in order
			// (T.800 Table A.35).
			if (e.tile >= 0 && e.tile != part.tile) || (e.tile < 0 && part.tile != i) || int64(e.length) != int64(part.end-part.sot) {
				return false
			}
			if part.dropped > 0 {
				w.patches = append(w.patches, j2kPatch{at: e.at, width: e.width, value: e.length - part.dropped})
			}
		}
	}
	if w.hasPLM {
		if len(w.plm) != len(w.parts) {
			return false
		}
		for i, sum := range w.plm {
			if sum != w.parts[i].body {
				return false
			}
		}
	}
	return true
}

// rewrite returns the frame without the dropped segments and with the
// length fields patched.
func (w *j2kWalk) rewrite() []byte {
	out := make([]byte, 0, len(w.f))
	at := 0
	for _, d := range w.drops {
		out = append(out, w.f[at:d.start]...)
		at = d.end
	}
	out = append(out, w.f[at:]...)
	for _, p := range w.patches {
		shift := 0
		for _, d := range w.drops {
			if d.end <= p.at {
				shift += d.end - d.start
			}
		}
		o := p.at - shift
		if p.width == 2 {
			binary.BigEndian.PutUint16(out[o:], uint16(p.value)) //nolint:gosec // G115: lowered from the 16-bit length it replaces.
		} else {
			binary.BigEndian.PutUint32(out[o:], p.value)
		}
	}
	return out
}
