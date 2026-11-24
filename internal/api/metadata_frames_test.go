package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// readsCounted is a storage that counts the reads made of each key.
type readsCounted struct {
	blob.Store
	mu    sync.Mutex
	reads map[string]int
}

func (r *readsCounted) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	r.mu.Lock()
	r.reads[key]++
	r.mu.Unlock()
	return r.Store.Read(ctx, key, offset, length)
}

// frameBytes reads a frame's ranges from the file and joins them.
func frameBytes(file []byte, f api.StoredFrame) []byte {
	var out []byte
	for _, r := range f.Ranges {
		out = append(out, file[r.Offset:r.Offset+r.Length]...)
	}
	return out
}

// TestFrameOfALineWithoutATable guards a line written before frame tables:
// the frame is found in the instance's file, its header and its items read
// by range, and its bytes are the frame's codestream, from the JPEG
// start-of-image marker to the end-of-image marker, its three fragments
// joined without their item headers; a frame number outside 1 to 3 is
// NotFound.
func TestFrameOfALineWithoutATable(t *testing.T) {
	file := corpusFile(t, "jpeg_3frame_nobot.dcm")
	store, path, held, line := storedInstance(t, file)
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	line.PixelData = &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}
	for n := 1; n <= 3; n++ {
		f, err := api.MetaFrameOf(context.Background(), store, path, 1<<20, held, line, n)
		if err != nil {
			t.Fatalf("frame %d: %v", n, err)
		}
		b := frameBytes(file, f)
		if f.Count != 3 || len(f.Ranges) != 3 || f.TransferSyntax != "1.2.840.10008.1.2.4.50" ||
			!bytes.HasPrefix(b, []byte{0xFF, 0xD8}) || !bytes.HasSuffix(bytes.TrimSuffix(b, []byte{0}), []byte{0xFF, 0xD9}) {
			t.Errorf("frame %d: %d of %d ranges, syntax %s, %d bytes", n, len(f.Ranges), f.Count, f.TransferSyntax, len(b))
		}
		if bytes.Count(b, []byte{0xFF, 0xD8, 0xFF}) != 1 {
			t.Errorf("frame %d holds more than one codestream's start", n)
		}
	}
	for _, n := range []int{0, 4} {
		if _, err := api.MetaFrameOf(context.Background(), store, path, 1<<20, held, line, n); fault.KindOf(err) != fault.NotFound {
			t.Errorf("frame %d: %v, want NotFound", n, err)
		}
	}
}

// TestFrameFromTheMetadataFile guards the table in the scan's metadata
// file: a frame is answered from the line alone, the instance's file not
// read at all, and is the frame the file's own table names, with the
// checksum of its bytes.
func TestFrameFromTheMetadataFile(t *testing.T) {
	file := corpusFile(t, "SC_rgb_rle_2frame.dcm")
	stored, path, held, line := storedInstance(t, file)
	store := &readsCounted{Store: stored, reads: map[string]int{}}
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	line.PixelData = &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}
	line.TransferSyntax = h.TransferSyntaxUID
	if line.Frames, err = frames.Table(h, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	if line.FrameSHA256, _, err = frames.Checksums(bytes.NewReader(file), line.Frames); err != nil {
		t.Fatal(err)
	}
	var meta bytes.Buffer
	if _, err := sidecar.Merge(&meta, nil, sidecar.List([]sidecar.Instance{line}), nil); err != nil {
		t.Fatal(err)
	}
	key, _ := layout.MetadataKey(path)
	if _, err := stored.Put(context.Background(), key, bytes.NewReader(meta.Bytes())); err != nil {
		t.Fatal(err)
	}
	f, err := api.MetaFrame(context.Background(), store, path, 1<<20, held, 2)
	if err != nil {
		t.Fatal(err)
	}
	if f.Count != 2 || f.TransferSyntax != "1.2.840.10008.1.2.5" || len(f.Ranges) != 1 || f.Ranges[0] != line.Frames[1][0] {
		t.Errorf("frame 2: %+v, want the table's %+v", f, line.Frames[1])
	}
	if sum := sha256.Sum256(frameBytes(file, f)); f.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("frame 2 carries the checksum %q, want that of its bytes", f.SHA256)
	}
	if store.reads[held.Key] != 0 {
		t.Errorf("the instance's file was read %d times for a frame its line places", store.reads[held.Key])
	}
	if len(frameBytes(file, f)) != int(line.Frames[1][0].Length) {
		t.Error("the frame's bytes are not its range's")
	}
}

// TestFrameNotByRanges guards the instances whose frames have no byte
// ranges: a line with an empty table and a deflated file are a
// precondition failure, for the route to decode instead; a line without
// pixel data is NotFound.
func TestFrameNotByRanges(t *testing.T) {
	file := corpusFile(t, "MR_small.dcm")
	store, path, held, line := storedInstance(t, file)
	line.PixelData = &sidecar.PixelData{Offset: 1000, Length: 8000}
	line.Frames = [][]sidecar.Range{}
	if _, err := api.MetaFrameOf(context.Background(), store, path, 1<<20, held, line, 1); fault.KindOf(err) != fault.Precondition {
		t.Errorf("an empty table: %v, want a precondition failure", err)
	}
	line.PixelData, line.Frames = nil, nil
	if _, err := api.MetaFrameOf(context.Background(), store, path, 1<<20, held, line, 1); fault.KindOf(err) != fault.NotFound {
		t.Errorf("no pixel data: %v, want NotFound", err)
	}
	dfl := deflatedFile(t, corpusFile(t, "MR_small.dcm"), '0')
	store, path, held, line = storedInstance(t, dfl)
	if _, err := api.MetaFrameOf(context.Background(), store, path, 1<<20, held, line, 1); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a deflated file: %v, want a precondition failure", err)
	}
}
