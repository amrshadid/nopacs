package engine

import "encoding/binary"

// The JPEG 2000 marker segments, each checked against the length its
// contents define (ITU-T T.800 A.5 to A.9; CAP and CPF as amended for
// High-Throughput JPEG 2000, ITU-T T.814 A.3). pixel_j2k.go walks the
// codestream and calls these for each segment it meets.

// siz checks the image and tile size segment (T.800 A.5.1): 36 bytes and
// three a component, tiles that cover the image from an origin inside it.
func (w *j2kWalk) siz(p []byte) bool {
	if len(p) < 38 {
		return false
	}
	u := func(i int) uint64 { return uint64(binary.BigEndian.Uint32(p[i:])) }
	xsiz, ysiz, xo, yo := u(2), u(6), u(10), u(14)
	xt, yt, xto, yto := u(18), u(22), u(26), u(30)
	csiz := int(binary.BigEndian.Uint16(p[34:]))
	if csiz < 1 || csiz > 16384 || len(p) != 36+3*csiz {
		return false
	}
	if xt == 0 || yt == 0 || xsiz <= xo || ysiz <= yo || xto > xo || yto > yo || xto+xt <= xo || yto+yt <= yo {
		return false
	}
	tiles := ((xsiz - xto + xt - 1) / xt) * ((ysiz - yto + yt - 1) / yt)
	if tiles > 65535 {
		// Isot is 16 bits (T.800 A.4.2).
		return false
	}
	for c := range csiz {
		if p[36+3*c+1] == 0 || p[36+3*c+2] == 0 {
			return false
		}
	}
	w.csiz, w.numTiles, w.cb = csiz, int(tiles), 1
	if csiz >= 257 {
		w.cb = 2
	}
	return true
}

// capPart15 is the bit of Pcap that names Part 15, High-Throughput JPEG
// 2000: bit 15 counted from the most significant, 1 (T.800 A.5.2, Table
// A.11).
const capPart15 = 1 << (32 - 15)

// ccap15Reserved are the bits of Ccap15 that T.814 Table A.3 reserves,
// bits 10 to 6, which a codestream sets to zero.
const ccap15Reserved = 0x07C0

// mainSegment checks one segment of the main header, at..next in the
// frame.
//
// CPF and CRG are removed with COM, not checked word by word: their
// payloads are arbitrary words (Pcpf, T.800 A.5.3) and offsets (Xcrg and
// Ycrg, T.800 A.9.1) that could carry text, and no decoder needs either
// to reconstruct the samples (CPF names a profile the codestream follows;
// CRG places components for display). The main header has no length to
// lower, so removing them needs no patch.
func (w *j2kWalk) mainSegment(m byte, p []byte, at, next int) bool {
	switch m {
	case j2kCAP:
		return w.first(m) && capOK(p)
	case j2kCPF:
		return w.first(m) && len(p) >= 2 && len(p)%2 == 0 && w.drop(m, at, next, nil)
	case j2kCRG:
		return w.first(m) && len(p) == 4*w.csiz && w.drop(m, at, next, nil)
	case j2kTLM:
		return w.tlmSegment(p, at+4)
	case j2kPLM:
		return w.plmSegment(p)
	case j2kCOM: // Rcom, then the comment (T.800 A.9.2)
		return len(p) >= 2 && w.drop(m, at, next, nil)
	}
	return w.coding(m, p, w.main)
}

// capOK checks a CAP segment (Pcap, then a Ccap of two bytes a part it
// names: T.800 A.5.2). Only the capabilities of Part 15 are taken, whose
// Ccap15 has every bit defined but the reserved ones (T.814 A.3): any other
// Pcap bit names a part whose Ccap words the walk cannot vouch for, and
// would carry two free bytes each.
func capOK(p []byte) bool {
	if len(p) != 6 || binary.BigEndian.Uint32(p) != capPart15 {
		return false
	}
	ccap := binary.BigEndian.Uint16(p[4:])
	// Bits 15 and 14 of 01 are reserved too (T.814 Table A.3).
	return ccap&ccap15Reserved == 0 && ccap>>14 != 1
}

// first reports whether a main header marker that may appear once is seen
// the first time.
func (w *j2kWalk) first(m byte) bool {
	if w.once[m] {
		return false
	}
	w.once[m] = true
	return true
}

// drop marks the segment of marker m at..next for removal, its bytes
// counted against the tile-part it is in, if any. It always succeeds.
func (w *j2kWalk) drop(m byte, at, next int, part *j2kPart) bool {
	w.drops = append(w.drops, j2kDrop{start: at, end: next, marker: m})
	if part != nil {
		// The segment is its marker and its length's count of bytes.
		part.dropped += 2 + uint32(binary.BigEndian.Uint16(w.f[at+2:]))
	}
	return true
}

// coding checks a coding or quantization segment of the main header or a
// tile-part header; any other marker is one the walk does not know there.
func (w *j2kWalk) coding(m byte, p []byte, c *j2kCoding) bool {
	cb := w.cb
	switch m {
	case j2kCOD: // Scod, SGcod (4), SPcod (5), and a precinct size a level when Scod says (T.800 A.6.1)
		if c.nl >= 0 || len(p) < 10 {
			return false
		}
		nl := int(p[5])
		want := 10
		if p[0]&1 != 0 {
			want += nl + 1
		}
		if nl > 32 || len(p) != want {
			return false
		}
		c.nl = nl
		return true
	case j2kCOC: // Ccoc, Scoc, SPcoc (5), and precinct sizes when Scoc says (T.800 A.6.2)
		if len(p) < cb+6 {
			return false
		}
		comp := component(p, cb)
		if _, seen := c.cocNL[comp]; seen || comp >= w.csiz {
			return false
		}
		nl := int(p[cb+1])
		want := cb + 6
		if p[cb]&1 != 0 {
			want += nl + 1
		}
		if nl > 32 || len(p) != want {
			return false
		}
		c.cocNL[comp] = nl
		return true
	case j2kQCD: // Sqcd, SPqcd (T.800 A.6.4)
		subbands, derived, ok := quantValues(p)
		if c.hasQCD || !ok {
			return false
		}
		c.hasQCD = true
		c.quant = append(c.quant, j2kQuant{comp: -1, subbands: subbands, derived: derived})
		return true
	case j2kQCC: // Cqcc, Sqcc, SPqcc (T.800 A.6.5)
		if len(p) < cb {
			return false
		}
		comp := component(p, cb)
		subbands, derived, ok := quantValues(p[cb:])
		if c.qcc[comp] || comp >= w.csiz || !ok {
			return false
		}
		c.qcc[comp] = true
		c.quant = append(c.quant, j2kQuant{comp: comp, subbands: subbands, derived: derived})
		return true
	case j2kRGN: // Crgn, Srgn, SPrgn (T.800 A.6.3)
		if len(p) != cb+2 {
			return false
		}
		comp := component(p, cb)
		if c.rgn[comp] || comp >= w.csiz {
			return false
		}
		c.rgn[comp] = true
		return true
	case j2kPOC: // entries of RSpoc, CSpoc, LYEpoc (2), REpoc, CEpoc, Ppoc (T.800 A.6.6)
		entry := 5 + 2*cb
		return len(p) >= entry && len(p)%entry == 0
	}
	return false
}

// component reads a component index of cb bytes.
func component(p []byte, cb int) int {
	if cb == 1 {
		return int(p[0])
	}
	return int(binary.BigEndian.Uint16(p))
}

// quantValues reads a quantization style and its values (T.800 Table
// A.28): no quantization has one byte a subband; scalar derived one value
// of two bytes; scalar expounded two bytes a subband. It returns the
// subbands the values are for and whether the style is scalar derived.
func quantValues(p []byte) (int, bool, bool) {
	if len(p) < 2 {
		return 0, false, false
	}
	rest := len(p) - 1
	switch p[0] & 0x1F {
	case 0:
		return rest, false, true
	case 1:
		return 1, true, rest == 2
	case 2:
		return rest / 2, false, rest%2 == 0
	}
	return 0, false, false
}

// quantOK reports whether every quantization segment of c has values for
// the 3 NL + 1 subbands of the decomposition levels in force for it
// (T.800 A.6.4: Lqcd is 4 + 3 NL, or 5 + 6 NL): a tile's COC, then its
// COD, then the main header's COC, then its COD. tile is c when c is a
// tile's, nil for the main header.
func (w *j2kWalk) quantOK(c, tile *j2kCoding) bool {
	for _, q := range c.quant {
		if q.derived {
			continue
		}
		if q.subbands != 3*w.levels(tile, q.comp)+1 {
			return false
		}
	}
	c.quant = nil
	return true
}

// levels returns the decomposition levels in force for a component (-1
// for all, as QCD is) in a tile (nil for the main header).
func (w *j2kWalk) levels(tile *j2kCoding, comp int) int {
	if tile != nil {
		if nl, ok := tile.cocNL[comp]; ok && comp >= 0 {
			return nl
		}
		if tile.nl >= 0 {
			return tile.nl
		}
	}
	if nl, ok := w.main.cocNL[comp]; ok && comp >= 0 {
		return nl
	}
	return w.main.nl
}

// tlmSegment reads a TLM segment (Ztlm, Stlm, then entries of Ttlm and
// Ptlm: T.800 A.7.1), its payload at offset at of the frame. Its entries
// are checked against the tile-parts once they are walked.
func (w *j2kWalk) tlmSegment(p []byte, at int) bool {
	if len(p) < 2 || int(p[0]) != w.tlmZ {
		return false
	}
	w.tlmZ++
	s := p[1]
	st := int(s>>4) & 3
	if st == 3 || s&0x8F != 0 {
		return false
	}
	pw := 2
	if s&0x40 != 0 {
		pw = 4
	}
	entries := p[2:]
	ew := st + pw
	if len(entries) == 0 || len(entries)%ew != 0 {
		return false
	}
	for i := 0; i < len(entries); i += ew {
		e := j2kTLMEntry{tile: -1, at: at + 2 + i + st, width: pw}
		switch st {
		case 1:
			e.tile = int(entries[i])
		case 2:
			e.tile = int(binary.BigEndian.Uint16(entries[i:]))
		}
		if pw == 2 {
			e.length = uint32(binary.BigEndian.Uint16(entries[i+st:]))
		} else {
			e.length = binary.BigEndian.Uint32(entries[i+st:])
		}
		w.tlm = append(w.tlm, e)
	}
	return true
}

// plmSegment reads a PLM segment (Zplm, then for each tile-part Nplm and
// that many bytes of packet lengths: T.800 A.7.2). Each tile-part's sum is
// checked against its data once the tile-parts are walked.
func (w *j2kWalk) plmSegment(p []byte) bool {
	if len(p) < 2 || int(p[0]) != w.plmZ {
		return false
	}
	w.plmZ++
	w.hasPLM = true
	for i := 1; i < len(p); {
		n := int(p[i])
		i++
		if i+n > len(p) {
			return false
		}
		sum, ok := packetLengths(p[i:i+n], len(w.f))
		if !ok {
			return false
		}
		w.plm = append(w.plm, sum)
		i += n
	}
	return true
}

// packetLengths sums a list of packet lengths, each seven bits a byte with
// the top bit set on every byte but its last (T.800 A.7.3); each is at
// least one byte and the sum at most limit.
func packetLengths(b []byte, limit int) (int, bool) {
	sum, v, open := 0, 0, false
	for _, x := range b {
		if v > limit>>7 {
			// Another seven bits would pass the limit, and could overflow
			// an int of 32 bits.
			return 0, false
		}
		v = v<<7 | int(x&0x7F)
		open = x&0x80 != 0
		if open {
			continue
		}
		if v == 0 || v > limit-sum {
			return 0, false
		}
		sum += v
		v = 0
	}
	return sum, len(b) > 0 && !open
}
