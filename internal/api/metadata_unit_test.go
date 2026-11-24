package api

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// sidecarLine returns a metadata file's line for an instance with the data
// set text and its pixel data at offset 1000, 4100 bytes long.
func sidecarLine(text string) sidecar.Instance {
	return sidecar.Instance{Dataset: json.RawMessage(text), PixelData: &sidecar.PixelData{Offset: 1000, Length: 4100}}
}

// answer returns the JSON datasetFromSidecar makes, or "" when it says the
// instance's file must be read.
func answer(in sidecar.Instance, ts string) string {
	return string(datasetFromSidecar(in, ts, func(o, l int64) string { return "u?" + strconv.FormatInt(o, 10) + "," + strconv.FormatInt(l, 10) }))
}

// TestDatasetFromSidecarPlacesPixelData guards the answer made from the
// metadata file alone: Pixel Data becomes a reference past the element's
// header, which is 12 bytes in an explicit syntax and 8 in implicit VR
// little endian (PS3.5 7.1), trailing padding is left out, and the other
// members are kept as they are.
func TestDatasetFromSidecarPlacesPixelData(t *testing.T) {
	const set = `{"00100010":{"vr":"PN","Value":[{"Alphabetic":"A^B"}]},"00280010":{"vr":"US","Value":[64]},"7FE00010":{"vr":"OW"},"FFFCFFFC":{"vr":"OB","InlineBinary":"AAAA"}}`
	for _, c := range []struct{ ts, want string }{
		{"1.2.840.10008.1.2.1", `u?1012,4088`},
		{"1.2.840.10008.1.2.4.50", `u?1012,4088`},
		{"1.2.840.10008.1.2", `u?1008,4092`},
	} {
		got := answer(sidecarLine(set), c.ts)
		if !strings.Contains(got, `"BulkDataURI":"`+c.want+`"`) || strings.Contains(got, "FFFCFFFC") || !strings.Contains(got, `"Value":[64]`) ||
			!strings.Contains(got, `{"Alphabetic":"A^B"}`) {
			t.Errorf("%s: %s, want a reference %s and the other members, without padding", c.ts, got, c.want)
		}
	}
}

// TestDatasetFromSidecarSendsWhatItCannotSettleToTheFile guards that an
// answer is made from the metadata file only when it settles it: a binary
// value skipped for its length or empty, text that may pass 64 KiB, a
// Pixel Data inside a sequence, a transfer syntax the standard does not
// register, and a line without a data set each return nothing, so the
// instance's file is read.
func TestDatasetFromSidecarSendsWhatItCannotSettleToTheFile(t *testing.T) {
	long := strings.Repeat("x", 17<<10)
	for name, c := range map[string]struct{ set, ts string }{
		"a skipped binary":    {`{"7FE00010":{"vr":"OW"},"7FDF1001":{"vr":"OB"}}`, "1.2.840.10008.1.2.1"},
		"long text":           {`{"7FE00010":{"vr":"OW"},"00081030":{"vr":"LO","Value":["` + long + `"]}}`, "1.2.840.10008.1.2.1"},
		"nested pixel data":   {`{"7FE00010":{"vr":"OW"},"00880200":{"vr":"SQ","Value":[{"7FE00010":{"vr":"OW"}}]}}`, "1.2.840.10008.1.2.1"},
		"nested skipped":      {`{"7FE00010":{"vr":"OW"},"00081140":{"vr":"SQ","Value":[{"7FDF1001":{"vr":"OB"}}]}}`, "1.2.840.10008.1.2.1"},
		"unregistered syntax": {`{"7FE00010":{"vr":"OW"}}`, "1.2.3"},
		"no pixel data":       {`{"00280010":{"vr":"US","Value":[1]}}`, "1.2.840.10008.1.2.1"},
		"not JSON":            {`[1]`, "1.2.840.10008.1.2.1"},
	} {
		if got := answer(sidecarLine(c.set), c.ts); got != "" {
			t.Errorf("%s: answered %.80s from the metadata file", name, got)
		}
	}
	if got := answer(sidecar.Instance{PixelData: &sidecar.PixelData{Offset: 1000, Length: 4100}, DatasetOmitted: true}, "1.2.840.10008.1.2.1"); got != "" {
		t.Errorf("a line with no data set was answered: %s", got)
	}
}

// reencoded is the answer to a line as encoding/json makes it: the data set
// decoded into values, trailing padding left out, Pixel Data replaced by
// its reference, and written again. datasetFromSidecar must answer what it
// does, byte for byte, without decoding the values.
func reencoded(t *testing.T, in sidecar.Instance, ts string) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(in.Dataset))
	dec.UseNumber()
	var set map[string]any
	if err := dec.Decode(&set); err != nil {
		t.Fatal(err)
	}
	delete(set, keyPaddingMember)
	hdr, _ := headerLength(ts)
	for key := range pixelMembers {
		if el, ok := set[key].(map[string]any); ok {
			set[key] = map[string]any{memberVR: el[memberVR], memberBulkURI: "u?" + strconv.FormatInt(in.PixelData.Offset+hdr, 10) + "," + strconv.FormatInt(in.PixelData.Length-hdr, 10)}
		}
	}
	out, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestDatasetFromSidecarIsWhatEncodingJSONWrites guards the answer made
// from a line against encoding/json: for each file of the corpus whose
// header the line settles, go-dicom's DICOM JSON as the line holds it is
// answered exactly as decoding it and writing it again would; and a line
// in another form (members out of order, written twice, names escaped,
// white space, a second value after the first) is answered with the same
// members and values, or not at all when encoding/json would not.
func TestDatasetFromSidecarIsWhatEncodingJSONWrites(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("..", "receive", "testdata", "*.dcm"))
	if err != nil || len(names) < 10 {
		t.Fatalf("the corpus: %d files, %v", len(names), err)
	}
	settled := 0
	for _, name := range names {
		file, err := os.ReadFile(name) // #nosec G304 -- the corpus's own names.
		if err != nil {
			t.Fatal(err)
		}
		h, err := header.Read(bytes.NewReader(file), int64(len(file)))
		if err != nil {
			continue // A file the corpus holds to be refused.
		}
		js, err := h.DICOMJSON()
		if err != nil {
			t.Fatal(err)
		}
		in := sidecar.Instance{Dataset: js}
		if h.HasPixelData {
			in.PixelData = &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}
		}
		got := answer(in, h.TransferSyntaxUID)
		if got == "" {
			continue // The file is read for this one.
		}
		settled++
		if want := reencoded(t, in, h.TransferSyntaxUID); got != want {
			t.Errorf("%s: the answer differs from encoding/json's:\n%.300s\n%.300s", filepath.Base(name), got, want)
		}
	}
	if settled < 5 {
		t.Errorf("only %d files of the corpus were answered from a line", settled)
	}

	const pixel = `"7FE00010":{"vr":"OW"}`
	for name, set := range map[string]string{
		"out of order":      `{"00280010":{"vr":"US","Value":[64]},"00100010":{"Value":[{"Alphabetic":"A^B"}],"vr":"PN"},` + pixel + `}`,
		"written twice":     `{"00280010":{"vr":"US","Value":[1]},"00280010":{"vr":"US","Value":[64]},` + pixel + `}`,
		"escaped names":     `{"\u0030\u00300280010":{"\u0076r":"US","Value":[64]},` + pixel + `}`,
		"white space":       " {\n \"00280010\" : { \"vr\" : \"US\" , \"Value\" : [ 64 ] } ,\t" + pixel + " }\n",
		"a value after":     `{"00280010":{"vr":"US","Value":[64]},` + pixel + `} {"x":1}`,
		"text with escapes": `{"00081030":{"vr":"LO","Value":["a\"b\\c<&>\u00e9"]},` + pixel + `}`,
		"a sequence":        `{"00081140":{"vr":"SQ","Value":[{"00081150":{"vr":"UI","Value":["1.2"]}},{}]},` + pixel + `}`,
		"an empty sequence": `{"00081140":{"vr":"SQ"},"00081141":{"vr":"SQ","Value":null},` + pixel + `}`,
		"an empty text":     `{"00081030":{"vr":"LO"},` + pixel + `}`,
	} {
		in := sidecarLine(set)
		got := answer(in, "1.2.840.10008.1.2.1")
		if got == "" {
			t.Errorf("%s: not answered", name)
			continue
		}
		var a, b any
		if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal([]byte(reencoded(t, in, "1.2.840.10008.1.2.1")), &b) != nil || !reflect.DeepEqual(a, b) {
			t.Errorf("%s: %s, want the members of %s", name, got, reencoded(t, in, "1.2.840.10008.1.2.1"))
		}
	}
	for name, set := range map[string]string{
		"not an object":        `[1]`,
		"not JSON":             `{"00280010":`,
		"an element not a set": `{"00280010":1,` + pixel + `}`,
		"an item not a set":    `{"00081140":{"vr":"SQ","Value":[1]},` + pixel + `}`,
		"a long value":         `{"00081030":{"vr":"LO","Value":["` + strings.Repeat("x", 17<<10) + `"]},` + pixel + `}`,
		"a long inline":        `{"00091010":{"vr":"OB","InlineBinary":"` + strings.Repeat("A", 17<<10) + `"},` + pixel + `}`,
		"a reference":          `{"00091010":{"vr":"OB","BulkDataURI":"x"},` + pixel + `}`,
		"two pixel data":       `{"7FE00008":{"vr":"OF"},` + pixel + `}`,
	} {
		if got := answer(sidecarLine(set), "1.2.840.10008.1.2.1"); got != "" {
			t.Errorf("%s: answered %.80s", name, got)
		}
	}
}
