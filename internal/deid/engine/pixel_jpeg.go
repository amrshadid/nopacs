package engine

import (
	"bytes"
	"encoding/binary"

	"github.com/amrshadid/go-dicom/compress"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/encaps"
)

// The segments of a JPEG or JPEG-LS frame that can hold text a decoder
// never reads, beyond the comment and application segments the DICOM
// library removes (anonymize.RemoveJPEGText): an application segment it
// keeps for the decoder (JFIF, Adobe, the JPEG-LS color transform) with
// bytes after its defined ones; a reserved JPGn marker (ISO/IEC 10918-1
// Table B.1); a table or header segment declaring more bytes than its
// contents need. Once the library has cleaned a frame, each segment left is
// checked against its defined length, and a frame holding any other is
// held: text there would travel unseen by the check after, which does not
// search pixel data.

// onlyDefinedSegments returns a Failure (CausePixelTextUnsearched) unless
// every frame of the encapsulated Pixel Data of scratch holds only the
// segments a decoder needs, each at its defined length, and nothing after
// its end-of-image marker but padding (0x00 or 0xFF bytes).
func onlyDefinedSegments(scratch *dataset.Dataset) error {
	hold := &Failure{Cause: CausePixelTextUnsearched, Path: formatTag(tagPixelData)}
	enc, err := scratch.ExtractEncapsulatedFrames()
	if err != nil {
		return hold
	}
	n := 0
	if info, err := scratch.GetPixelDataInfo(); err == nil {
		n = info.NumberOfFrames
	}
	frames, err := encaps.Frames(enc, n)
	if err != nil {
		return hold
	}
	syntax := scratch.TransferSyntaxUID()
	ls := syntax == compress.JPEGLSLossless || syntax == compress.JPEGLSNearLossless
	for _, f := range frames {
		if !definedFrame(f, ls) {
			return hold
		}
	}
	return nil
}

// definedFrame walks a frame's markers (ISO/IEC 10918-1 B.1, ISO/IEC
// 14495-1 C.1) and reports whether every segment is one a decoder needs at
// its defined length; ls says the syntax is JPEG-LS. Entropy-coded data runs from a scan header to the
// next marker that is not stuffing (0xFF00 in JPEG; 0xFF and a byte whose
// top bit is 0 in JPEG-LS), a restart or fill.
func definedFrame(f []byte, ls bool) bool {
	n := len(f)
	if n < 4 || f[0] != 0xFF || f[1] != 0xD8 {
		return false
	}
	pos, inScan := 2, false
	for pos < n {
		if inScan {
			for pos < n {
				if f[pos] != 0xFF {
					pos++
					continue
				}
				if pos+1 >= n {
					return false
				}
				next := f[pos+1]
				if next == 0xFF {
					pos++
					continue
				}
				if (ls && next&0x80 == 0) || (!ls && next == 0x00) || (next >= 0xD0 && next <= 0xD7) {
					pos += 2
					continue
				}
				break
			}
			inScan = false
		}
		if pos+1 >= n || f[pos] != 0xFF {
			return false
		}
		for pos+1 < n && f[pos+1] == 0xFF {
			pos++
		}
		if pos+1 >= n {
			return false
		}
		m := f[pos+1]
		switch {
		case m == 0xD9:
			// After the end of the image, padding only: zeros (the pad of
			// an odd length, PS3.5 A.4) or 0xFF, the fill byte (ITU-T T.81
			// B.1.1.2), which some encoders pad an odd frame with. Neither
			// can spell text.
			for _, b := range f[pos+2:] {
				if b != 0x00 && b != 0xFF {
					return false
				}
			}
			return true
		case m == 0x01 || (m >= 0xD0 && m <= 0xD7):
			pos += 2
			continue
		}
		if pos+4 > n {
			return false
		}
		length := int(binary.BigEndian.Uint16(f[pos+2:]))
		if length < 2 || pos+2+length > n {
			return false
		}
		if !definedSegment(m, f[pos+4:pos+2+length], ls) {
			return false
		}
		pos += 2 + length
		inScan = m == 0xDA
	}
	return false
}

// definedSegment reports whether a segment of marker m, its payload p (the
// bytes after its length), is one a decoder needs, at its defined length.
func definedSegment(m byte, p []byte, ls bool) bool {
	switch {
	case m == 0xE0: // JFIF, as the DICOM library keeps it: no thumbnail
		return len(p) == 14 && bytes.HasPrefix(p, []byte("JFIF\x00")) && p[12] == 0 && p[13] == 0
	case m == 0xEE: // Adobe, its 12 defined bytes
		return len(p) == 12 && bytes.HasPrefix(p, []byte("Adobe"))
	case m == 0xE8: // the JPEG-LS color transform: "mrfx" and its transform
		return ls && len(p) == 5 && bytes.HasPrefix(p, []byte("mrfx"))
	case m == 0xDB: // DQT: tables of 64 one-byte or two-byte entries
		for i := 0; i < len(p); {
			pq := p[i] >> 4
			if pq > 1 {
				return false
			}
			i += 1 + 64*int(pq+1)
			if i > len(p) {
				return false
			}
			if i == len(p) {
				return true
			}
		}
		return false
	case m == 0xC4: // DHT: class and id, 16 counts, the values counted
		for i := 0; i < len(p); {
			if i+17 > len(p) {
				return false
			}
			sum := 0
			for _, c := range p[i+1 : i+17] {
				sum += int(c)
			}
			i += 17 + sum
			if i > len(p) {
				return false
			}
			if i == len(p) {
				return true
			}
		}
		return false
	case m == 0xDD || m == 0xDC: // DRI, DNL: two bytes
		return len(p) == 2
	case m == 0xCC: // DAC: two bytes a table
		return len(p) > 0 && len(p)%2 == 0
	case m == 0xDA: // SOS: components, then three bytes
		return len(p) >= 1 && len(p) == 4+2*int(p[0])
	case (m >= 0xC0 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC) || (m == 0xF7 && ls):
		// SOFn and the JPEG-LS SOF55: six bytes, three a component
		return len(p) >= 6 && len(p) == 6+3*int(p[5])
	case m == 0xF8 && ls: // LSE: only its preset parameters, 11 bytes
		return len(p) == 11 && p[0] == 1
	}
	// Any other application segment or comment, a reserved JPGn marker,
	// hierarchical segments, or a marker no syntax defines.
	return false
}
