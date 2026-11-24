package frames

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// TestChecksumsReadEachFrameOnce guards Checksums: each frame's checksum is
// that of its ranges' bytes joined, for ranges that cross the read
// buffer's edge, ranges of no bytes and a frame of several ranges, from a
// reader that hands out one byte at a time as well as one that fills the
// buffer; the file's checksum is of every byte; and a table whose ranges
// pass the end of the file is io.ErrUnexpectedEOF.
func TestChecksumsReadEachFrameOnce(t *testing.T) {
	data := make([]byte, 2*sumBuffer+1000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	table := [][]sidecar.Range{
		{{Offset: 10, Length: 0}, {Offset: 100, Length: 50}},
		{{Offset: sumBuffer - 20, Length: 24}},
		{{Offset: sumBuffer + 5, Length: sumBuffer}, {Offset: 2*sumBuffer + 10, Length: 990}},
		{{Offset: int64(len(data)), Length: 0}},
	}
	for name, read := range map[string]func([]byte) io.Reader{
		"whole buffers": func(b []byte) io.Reader { return bytes.NewReader(b) },
		"one byte":      func(b []byte) io.Reader { return iotest.OneByteReader(bytes.NewReader(b)) },
	} {
		sums, file, err := Checksums(read(data), table)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i, f := range table {
			h := sha256.New()
			for _, r := range f {
				h.Write(data[r.Offset : r.Offset+r.Length])
			}
			if want := hex.EncodeToString(h.Sum(nil)); sums[i] != want {
				t.Errorf("%s: frame %d has the checksum %s, want %s", name, i+1, sums[i], want)
			}
		}
		if whole := sha256.Sum256(data); file != whole {
			t.Errorf("%s: the file's checksum is %x, want %x", name, file, whole)
		}
	}
	past := [][]sidecar.Range{{{Offset: int64(len(data)) - 10, Length: 11}}}
	if _, _, err := Checksums(bytes.NewReader(data), past); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a range past the end: %v, want io.ErrUnexpectedEOF", err)
	}
}
