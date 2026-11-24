package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/compress"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/encaps"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// j2kTestdata holds JPEG 2000 and High-Throughput JPEG 2000 codestreams
// written by OpenJPEG and by an HTJ2K encoder, and three HTJ2K DICOM files
// (testdata/j2k/README.md).
const j2kTestdata = "testdata/j2k"

// heldJ2KCorpus are the JPEG 2000 files of pydicom's corpus whose frames
// the walk holds, and why: neither is a codestream a decoder reads as
// PS3.5 A.4.4 carries one.
var heldJ2KCorpus = map[string]string{
	// The frame is a JP2 file, its signature box first, not a bare
	// codestream.
	"GDCMJ2K_TextGBR.dcm": "a JP2 file",
	// The SIZ's Xsiz holds a sequence delimiter's bytes, giving 14,540,801
	// tiles; OpenJPEG refuses its header too.
	"JPEG2000-embedded-sequence-delimiter.dcm": "a SIZ past 65,535 tiles",
}

// j2kName is the name planted in comment segments.
const j2kName = "DOE^JOHN 19700101"

// j2kCodestreams returns the codestreams of j2kTestdata, by file name.
func j2kCodestreams(t *testing.T) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(j2kTestdata, "*.j2k"))
	if err != nil || len(paths) < 20 {
		t.Fatalf("the codestreams: %d, %v", len(paths), err)
	}
	out := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // G304: the package's own test data.
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(p)] = b
	}
	return out
}

// j2kSeg returns a marker segment of marker m and payload p.
func j2kSeg(m byte, p []byte) []byte {
	return append(binary.BigEndian.AppendUint16([]byte{0xFF, m}, uint16(len(p)+2)), p...) //nolint:gosec // G115: the tests' segments are a few bytes.
}

// j2kComment returns a COM segment holding s as Latin-1 text.
func j2kComment(s string) []byte { return j2kSeg(0x64, append([]byte{0, 1}, s...)) }

// insertAt returns f with b inserted at offset at.
func insertAt(f []byte, at int, b []byte) []byte {
	return slices.Concat(f[:at], b, f[at:])
}

// sizEnd returns where the SIZ segment, which follows SOC, ends.
func sizEnd(f []byte) int { return 4 + int(binary.BigEndian.Uint16(f[4:])) }

// mainSegments returns the offsets of the main header's segments after
// SOC, read by their lengths alone, and where the first SOT begins.
func mainSegments(t *testing.T, f []byte) ([]int, int) {
	t.Helper()
	var at []int
	pos := 2
	for pos+4 <= len(f) && f[pos] == 0xFF && f[pos+1] != 0x90 {
		at = append(at, pos)
		pos += 2 + int(binary.BigEndian.Uint16(f[pos+2:]))
	}
	if pos+2 > len(f) || f[pos+1] != 0x90 {
		t.Fatal("no SOT after the main header")
	}
	return at, pos
}

// tileParts returns the offsets of a codestream's SOT markers, found by
// each tile-part's Psot.
func tileParts(t *testing.T, f []byte) []int {
	t.Helper()
	_, pos := mainSegments(t, f)
	var out []int
	for pos+12 <= len(f) && f[pos] == 0xFF && f[pos+1] == 0x90 {
		out = append(out, pos)
		psot := int(binary.BigEndian.Uint32(f[pos+6:]))
		if psot == 0 {
			break
		}
		pos += psot
	}
	return out
}

// withTileComments returns f with a COM holding s after every SOT segment,
// each tile-part's Psot raised by its length.
func withTileComments(t *testing.T, f []byte, s string) []byte {
	t.Helper()
	out := slices.Clone(f)
	parts := tileParts(t, f)
	c := j2kComment(s)
	for i := len(parts) - 1; i >= 0; i-- {
		at := parts[i]
		if psot := binary.BigEndian.Uint32(out[at+6:]); psot != 0 {
			binary.BigEndian.PutUint32(out[at+6:], psot+uint32(len(c))) //nolint:gosec // G115: a comment of a few bytes.
		}
		out = insertAt(out, at+12, c)
	}
	return out
}

// withTLMLengths returns f with the lengths of its TLM segments set to
// those of its tile-parts as they lie, in order.
func withTLMLengths(t *testing.T, f []byte) []byte {
	t.Helper()
	out := slices.Clone(f)
	segs, _ := mainSegments(t, f)
	parts := tileParts(t, f)
	k := 0
	for _, at := range segs {
		if out[at+1] != 0x55 {
			continue
		}
		length := int(binary.BigEndian.Uint16(out[at+2:]))
		s := out[at+5]
		st, pw := int(s>>4)&3, 2
		if s&0x40 != 0 {
			pw = 4
		}
		for i := at + 6; i < at+2+length; i += st + pw {
			end := len(f)
			if k+1 < len(parts) {
				end = parts[k+1]
			}
			n := end - parts[k]
			if k+1 == len(parts) {
				n = int(binary.BigEndian.Uint32(out[parts[k]+6:]))
			}
			if pw == 2 {
				binary.BigEndian.PutUint16(out[i+st:], uint16(n)) //nolint:gosec // G115: the test codestreams' tile-parts are under 64 KiB.
			} else {
				binary.BigEndian.PutUint32(out[i+st:], uint32(n)) //nolint:gosec // G115: a tile-part's length, inside a small test codestream.
			}
			k++
		}
	}
	return out
}

// planted returns f with a COM holding s in its main header and in every
// tile-part header, the lengths of the tile-parts and TLM raised to match:
// a codestream a decoder reads as f.
func planted(t *testing.T, f []byte, s string) []byte {
	t.Helper()
	g := withTLMLengths(t, withTileComments(t, f, s))
	return insertAt(g, sizEnd(g), j2kComment(s))
}

// TestJ2KWalkTakesWhatEncodersWrite guards the walk over codestreams as
// encoders write them (OpenJPEG over tiles, tile-parts, precincts, POC,
// RGN, SOP and EPH, PLT and TLM, every code-block style, reversible and
// irreversible; an HTJ2K encoder with CAP, over tiles and refinement
// passes): each is taken, a codestream without a comment returned as it
// is, OpenJPEG's own comment removed; and with a comment holding a name
// planted in the main header and in every tile-part header, each is taken,
// every comment removed, and the result is, byte for byte, the codestream
// before planting, Psot and TLM lowered back.
func TestJ2KWalkTakesWhatEncodersWrite(t *testing.T) {
	for name, f := range j2kCodestreams(t) {
		base, removed, ok := cleanJ2K(f)
		if !ok {
			t.Errorf("%s: held as written", name)
			continue
		}
		comments := 0
		segs, _ := mainSegments(t, f)
		for _, at := range segs {
			if f[at+1] == 0x64 {
				comments++
			}
		}
		if len(removed) != comments || bytes.Contains(base, []byte("OpenJPEG")) || bytes.Contains(base, []byte("made for a test")) {
			t.Errorf("%s: %v removed of %d comments", name, removed, comments)
		}
		if comments == 0 && !bytes.Equal(base, f) {
			t.Errorf("%s: changed with nothing to remove", name)
		}
		in := planted(t, base, j2kName)
		parts := len(tileParts(t, base))
		got, removed, ok := cleanJ2K(in)
		switch {
		case !ok:
			t.Errorf("%s: held with comments planted", name)
		case len(removed) != parts+1:
			t.Errorf("%s: %d comments removed of %d", name, len(removed), parts+1)
		case bytes.Contains(got, []byte("DOE^JOHN")):
			t.Errorf("%s: the name is left", name)
		case !bytes.Equal(got, base):
			t.Errorf("%s: not the codestream before planting", name)
		}
	}
}

// TestJ2KCommentInALastTilePartOfNoLength guards a tile-part whose Psot is
// 0, which runs to EOC (T.800 A.4.2): its comment is removed and Psot stays
// 0.
func TestJ2KCommentInALastTilePartOfNoLength(t *testing.T) {
	f, _, _ := cleanJ2K(j2kCodestreams(t)["gradient.j2k"])
	parts := tileParts(t, f)
	if len(parts) != 1 {
		t.Fatalf("%d tile-parts", len(parts))
	}
	zero := slices.Clone(f)
	binary.BigEndian.PutUint32(zero[parts[0]+6:], 0)
	got, removed, ok := cleanJ2K(withTileComments(t, zero, j2kName))
	if !ok || len(removed) != 1 || !bytes.Equal(got, zero) {
		t.Errorf("taken %v, %v removed, as before planting %v", ok, removed, bytes.Equal(got, zero))
	}
}

// TestJ2KWalkHoldsWhatItCannotVouchFor guards the holds: a frame wrapped
// in a JP2 signature box; a marker no part of the syntax defines, in the
// main header or a tile-part header, with a length or none (0xFF30);
// packed packet headers (PPM); a COD and a QCD with values for more
// subbands than its levels, each with text after it; a CPF of odd length;
// a second CPF or CRG; a CAP longer than its parts, a CAP naming every
// part (Pcap 0xFFFFFFFF) whose Ccap words spell a name, a CAP naming Part
// 2, and a CAP of Part 15 with a reserved Ccap15 bit or bit pattern set; a
// second SIZ; a TLM entry or a PLT list that does not match the
// tile-parts; a Psot past the frame; text after EOC; and fill between
// markers. The same codestream unchanged is taken.
func TestJ2KWalkHoldsWhatItCannotVouchFor(t *testing.T) {
	all := j2kCodestreams(t)
	f := all["gradient.j2k"]
	tiled := all["opj_tiles_tlm_plt_comment.j2k"]
	if _, _, ok := cleanJ2K(f); !ok {
		t.Fatal("the codestream is held as it is")
	}
	segs, _ := mainSegments(t, f)
	find := func(f []byte, m byte) int {
		s, _ := mainSegments(t, f)
		for _, at := range s {
			if f[at+1] == m {
				return at
			}
		}
		t.Fatalf("no marker %02X", m)
		return 0
	}
	// grow returns f with text appended to the segment at at, its length
	// raised to cover it.
	grow := func(f []byte, at int, text string) []byte {
		g := slices.Clone(f)
		end := at + 2 + int(binary.BigEndian.Uint16(g[at+2:]))
		binary.BigEndian.PutUint16(g[at+2:], uint16(end-at-2+len(text))) //nolint:gosec // G115: a header segment and a few letters.
		return insertAt(g, end, []byte(text))
	}
	main := sizEnd(f)
	firstPart := tileParts(t, f)[0]
	jp2 := slices.Concat([]byte{0, 0, 0, 0x0C, 'j', 'P', ' ', ' ', 0x0D, 0x0A, 0x87, 0x0A}, f)
	plt := withTileComments(t, f, "")
	pltAt := tileParts(t, plt)[0] + 12
	// The planted empty COM becomes a PLT whose lengths, 68, 79 and 69,
	// spell text and are not the tile-part's data.
	plt[pltAt+1] = 0x58
	plt = slices.Concat(plt[:pltAt+4], []byte{0, 'D', 'O', 'E'}, plt[pltAt+6:])
	binary.BigEndian.PutUint16(plt[pltAt+2:], 6)
	binary.BigEndian.PutUint32(plt[firstPart+6:], binary.BigEndian.Uint32(f[firstPart+6:])+8)
	// The first TLM entry's Ptlm, one more than its tile-part's length.
	tlm := slices.Clone(tiled)
	tlmAt := find(tlm, 0x55)
	st, pw := int(tlm[tlmAt+5]>>4)&3, 2
	if tlm[tlmAt+5]&0x40 != 0 {
		pw = 4
	}
	tlm[tlmAt+6+st+pw-1]++
	past := slices.Clone(f)
	binary.BigEndian.PutUint32(past[firstPart+6:], uint32(len(f))) //nolint:gosec // G115: a test codestream of a few KiB.
	for _, c := range []struct {
		name  string
		frame []byte
	}{
		{"a JP2 file", jp2},
		{"an unknown marker in the main header", insertAt(f, main, j2kSeg(0x65, []byte(j2kName)))},
		{"an unknown marker in a tile-part header", insertAt(f, firstPart+12, j2kSeg(0x65, []byte(j2kName)))},
		{"a reserved marker of no length", insertAt(f, main, []byte{0xFF, 0x30})},
		{"PPM", insertAt(f, main, j2kSeg(0x60, append([]byte{0, 0, 0, 0, 3}, "DOE"...)))},
		{"a COD with text after it", grow(f, find(f, 0x52), "DOE")},
		{"a QCD with values for more subbands", grow(f, find(f, 0x5C), "DOE")},
		{"a CPF of odd length", insertAt(f, main, j2kSeg(0x59, []byte("DOE")))},
		{"a second CPF", insertAt(f, main, slices.Concat(j2kSeg(0x59, []byte{0, 0}), j2kSeg(0x59, []byte("DOE^"))))},
		{"a second CRG", insertAt(f, main, slices.Concat(j2kSeg(0x63, j2kFill(4*j2kComponents(f))), j2kSeg(0x63, j2kFill(4*j2kComponents(f)))))},
		{"a CAP longer than its parts", insertAt(f, main, j2kSeg(0x50, append([]byte{0, 2, 0, 0, 0, 0}, "DOE^"...)))},
		{"a CAP naming every part", insertAt(f, main, j2kSeg(0x50, append([]byte{0xFF, 0xFF, 0xFF, 0xFF}, j2kFill(64)...)))},
		{"a CAP naming Part 2", insertAt(f, main, j2kSeg(0x50, []byte{0x40, 0, 0, 0, 'D', 'O'}))},
		{"a CAP with a reserved Ccap15 bit", insertAt(f, main, j2kSeg(0x50, []byte{0, 2, 0, 0, 0x01, 0x00}))},
		{"a CAP with the reserved Ccap15 set 01", insertAt(f, main, j2kSeg(0x50, []byte{0, 2, 0, 0, 0x40, 0x00}))},
		{"a second SIZ", insertAt(f, main, f[segs[0]:main])},
		{"a TLM entry not its tile-part's length", tlm},
		{"a PLT whose lengths are not the data's", plt},
		{"a Psot past the frame", past},
		{"text after EOC", append(slices.Clone(f), "DOE"...)},
		{"fill between markers", insertAt(f, main, []byte{0xFF})},
	} {
		if _, _, ok := cleanJ2K(c.frame); ok {
			t.Errorf("%s: taken", c.name)
		}
	}
	if _, _, ok := cleanJ2K(append(slices.Clone(f), 0x00, 0xFF)); !ok {
		t.Error("padding and fill after EOC: held")
	}
}

// j2kFill returns n bytes of the planted name, repeated.
func j2kFill(n int) []byte {
	return []byte(strings.Repeat(j2kName, n/len(j2kName)+1)[:n])
}

// j2kComponents returns the number of components a codestream's SIZ gives
// (Csiz, T.800 A.5.1).
func j2kComponents(f []byte) int { return int(binary.BigEndian.Uint16(f[40:])) }

// withFreeWords returns f with a CPF of even length and a CRG of 4 Csiz
// bytes after its SIZ, the words of each spelling the planted name.
func withFreeWords(f []byte) []byte {
	cpf := j2kSeg(0x59, j2kFill(2*len(j2kName)))
	crg := j2kSeg(0x63, j2kFill(4*j2kComponents(f)))
	return insertAt(f, sizEnd(f), slices.Concat(cpf, crg))
}

// TestJ2KFreeWordsAreRemoved guards the main header's segments whose words
// are free, CPF and CRG, each holding a name, over every codestream of
// j2kTestdata: both are removed, recorded by name, the name is gone, and
// the result is, byte for byte, the codestream before planting.
func TestJ2KFreeWordsAreRemoved(t *testing.T) {
	for name, f := range j2kCodestreams(t) {
		base, _, ok := cleanJ2K(f)
		if !ok {
			t.Fatalf("%s: held as written", name)
		}
		got, removed, ok := cleanJ2K(withFreeWords(base))
		switch {
		case !ok:
			t.Errorf("%s: held", name)
		case strings.Join(removed, " ") != "CPF CRG":
			t.Errorf("%s: removed %v", name, removed)
		case bytes.Contains(got, []byte("DOE^")):
			t.Errorf("%s: the name is left", name)
		case !bytes.Equal(got, base):
			t.Errorf("%s: not the codestream before planting", name)
		}
	}
}

// TestJ2KBytesAfterTheLastPacket guards text after a tile-part's last
// packet, inside the length its Psot gives (raised to cover it, and TLM
// with it): held when the tile-part has PLT, whose packet lengths then no
// longer sum to its data; taken without PLT or PLM, since the packet data
// is not parsed, the limit the walk states.
func TestJ2KBytesAfterTheLastPacket(t *testing.T) {
	all := j2kCodestreams(t)
	trailing := func(f []byte) []byte {
		parts := tileParts(t, f)
		end := len(f) - 2 // EOC
		if len(parts) > 1 {
			end = parts[1]
		}
		g := slices.Clone(f)
		binary.BigEndian.PutUint32(g[parts[0]+6:], binary.BigEndian.Uint32(g[parts[0]+6:])+3)
		return withTLMLengths(t, insertAt(g, end, []byte("DOE")))
	}
	if _, _, ok := cleanJ2K(trailing(all["opj_tiles_tlm_plt_comment.j2k"])); ok {
		t.Error("with PLT: taken")
	}
	if _, _, ok := cleanJ2K(trailing(all["gradient.j2k"])); !ok {
		t.Error("without PLT: held, the limit is no longer the walk's; say so where it is stated")
	}
}

// TestJ2KCommentsAreRemovedFromTheImage guards the engine over a JPEG 2000
// image the DICOM library wrote, a name planted in a comment of its main
// header and of its tile-part header: the profile says nothing of
// unsearched segments, the image is archived, both comments are recorded
// removed, the name is nowhere in the written file, the frame is the
// library's codestream again, and it decodes to the original samples.
func TestJ2KCommentsAreRemovedFromTheImage(t *testing.T) {
	en := pixelEngine(t, nil)
	clean := jpegImage(t, compress.JPEG2000Lossless, nil)
	want := oneFrame(t, clean)
	ds := jpegImage(t, compress.JPEG2000Lossless, func(f []byte) []byte { return planted(t, f, j2kName) })
	rec := apply(t, en, ds)
	if rec.Pixels == nil || strings.Join(rec.Pixels.SegmentsRemoved, " ") != "COM COM" || rec.Pixels.Decoded {
		t.Fatalf("pixel work %+v", rec.Pixels)
	}
	if b := writeFile(t, ds); bytes.Contains(b, []byte("DOE^JOHN")) {
		t.Error("the name is in the written file")
	}
	if got := oneFrame(t, ds); !bytes.Equal(got, want) {
		t.Error("the frame is not the library's codestream")
	}
	samples, err := decoded(t, ds)
	if err != nil {
		t.Fatal(err)
	}
	if orig := pixels(t, image(t, "MR", "1.2.840.10008.5.1.4.1.1.4", "", 8, 8, 1)); !bytes.Equal(samples, orig) {
		t.Error("the frame does not decode to the original samples")
	}
	if ds.TransferSyntaxUID() != compress.JPEG2000Lossless {
		t.Errorf("transfer syntax %s", ds.TransferSyntaxUID())
	}
}

// TestJ2KHeldWhateverTheProfileSays guards a JPEG 2000 frame the walk
// cannot vouch for: held as pixel_text_unsearched under a profile that
// keeps unsearched segments too, as a JPEG frame is, with the data set
// unchanged; the syntax's segments are now searched, so keep no longer
// lets them through.
func TestJ2KHeldWhateverTheProfileSays(t *testing.T) {
	unknown := func(f []byte) []byte { return insertAt(f, sizEnd(f), j2kSeg(0x65, []byte(j2kName))) }
	for _, say := range []v1.UnsearchedSegments{v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_UNSPECIFIED, v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_KEEP} {
		en := newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, UnsearchedSegments: say})
		ds := jpegImage(t, compress.JPEG2000Lossless, unknown)
		before := writeFile(t, ds)
		_, err := en.Apply(context.Background(), ds, Reception{})
		if cause(err) != CausePixelTextUnsearched {
			t.Errorf("%s: %v", say, err)
		}
		if !bytes.Equal(before, writeFile(t, ds)) {
			t.Errorf("%s: a held file was changed", say)
		}
	}
}

// TestJ2KFilesAreArchivedWithoutKeep guards real JPEG 2000 and HTJ2K files
// under a profile that says nothing of unsearched segments: the receiver's
// JPEG 2000 file, three HTJ2K files (lossless, lossless RPCL, irreversible)
// and, with NOPACS_PYDICOM_CORPUS set, every JPEG 2000 file of pydicom's
// corpus are archived, but those of heldJ2KCorpus, which are held as
// pixel_text_unsearched; a frame changed decodes, by the DICOM library, to
// the samples of the frame received.
func TestJ2KFilesAreArchivedWithoutKeep(t *testing.T) {
	en := newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{
		Bases:      []string{profile.AnnexE("basic")},
		PixelRules: []*v1.PixelRule{{Name: "accept-all", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "the segments are judged here"}},
	})
	paths, _ := filepath.Glob(filepath.Join(j2kTestdata, "*.dcm"))
	paths = append(paths, filepath.Join(receiveTestdata, "MR_small_jp2klossless.dcm"))
	if dir := os.Getenv(corpusEnv); dir != "" {
		paths = append(paths, corpusFiles(t, dir)...)
	}
	archived := 0
	for _, path := range paths {
		ds, err := readPath(t, path)
		if err != nil || !j2kSyntax(ds.TransferSyntaxUID()) {
			continue
		}
		before, _ := readPath(t, path)
		rec, err := en.Apply(context.Background(), ds, Reception{})
		if why, held := heldJ2KCorpus[filepath.Base(path)]; held {
			if cause(err) != CausePixelTextUnsearched {
				t.Errorf("%s (%s): %v, want pixel_text_unsearched", filepath.Base(path), why, err)
			}
			continue
		}
		if err != nil {
			if c := cause(err); c == CausePixelTextUnsearched || c == CausePixelsUndecodable || c == "" {
				t.Errorf("%s: %v", filepath.Base(path), err)
			}
			continue
		}
		archived++
		if rec.Pixels != nil && len(rec.Pixels.SegmentsRemoved) > 0 {
			a, errA := decoded(t, before)
			b, errB := decoded(t, ds)
			if errA == nil && (errB != nil || !bytes.Equal(a, b)) {
				t.Errorf("%s: the cleaned frames decode otherwise: %v", filepath.Base(path), errB)
			}
		}
	}
	if archived < 4 {
		t.Errorf("%d files archived", archived)
	}
}

// decoded returns the samples of every frame of a data set's encapsulated
// Pixel Data, decoded by the DICOM library, or an error.
func decoded(t *testing.T, ds *dataset.Dataset) ([]byte, error) {
	t.Helper()
	enc, err := ds.ExtractEncapsulatedFrames()
	if err != nil {
		return nil, err
	}
	n := 0
	if info, err := ds.GetPixelDataInfo(); err == nil {
		n = info.NumberOfFrames
	}
	frames, err := encaps.Frames(enc, n)
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, f := range frames {
		s, err := ds.DecodeFrame(f)
		if err != nil {
			return nil, err
		}
		out = append(out, s...)
	}
	return out, nil
}

// oneFrame returns the one frame of a data set's encapsulated Pixel Data,
// without the pad of an even length.
func oneFrame(t *testing.T, ds *dataset.Dataset) []byte {
	t.Helper()
	enc, err := ds.ExtractEncapsulatedFrames()
	if err != nil {
		t.Fatal(err)
	}
	frames, err := encaps.Frames(enc, 1)
	if err != nil || len(frames) != 1 {
		t.Fatalf("frames: %d, %v", len(frames), err)
	}
	f := frames[0]
	if e := findEOC(f); e >= 0 {
		f = f[:e+2]
	}
	return f
}
