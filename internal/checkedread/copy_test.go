package checkedread

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCopyChecked guards the copy of an instance's bytes: every byte
// passed in order when the count and the SHA-256 agree, whatever the
// buffers' length; and when they do not (fewer bytes, more bytes, other
// bytes), the right finding, with the last piece never passed, so what was
// passed is short of the whole.
func TestCopyChecked(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 1000)
	sum := tenant.SHA256(sha256.Sum256(data))
	other := bytes.Clone(data)
	other[len(other)-1] ^= 1
	for _, size := range []int{1, 7, 1000, len(data), len(data) + 1, 1 << 16} {
		var got bytes.Buffer
		emit := func(b []byte) error { got.Write(b); return nil }
		bufs := [2][]byte{make([]byte, size), make([]byte, size)}
		n, err := CopyChecked(bytes.NewReader(data), int64(len(data)), &sum, bufs, emit)
		if err != nil || n != int64(len(data)) || !bytes.Equal(got.Bytes(), data) {
			t.Errorf("buffers of %d: %d bytes, %v", size, n, err)
		}
		for _, c := range []struct {
			name   string
			src    []byte
			length int64
			want   error
		}{
			{"fewer", data[:len(data)-1], int64(len(data)), ErrShort},
			{"more", data, int64(len(data) - 1), ErrLong},
			{"other", other, int64(len(data)), ErrChecksum},
		} {
			got.Reset()
			n, err := CopyChecked(bytes.NewReader(c.src), c.length, &sum, bufs, emit)
			if !errors.Is(err, c.want) || !Mismatched(err) || n >= c.length || int64(got.Len()) != n {
				t.Errorf("buffers of %d, %s bytes: %d passed, %v, want %v before the last piece", size, c.name, n, err, c.want)
			}
		}
	}
	// No checksum: the count alone; nothing at all for an empty body.
	n, err := CopyChecked(bytes.NewReader(other), int64(len(other)), nil, [2][]byte{make([]byte, 100), make([]byte, 100)}, func([]byte) error { return nil })
	if err != nil || n != int64(len(other)) {
		t.Errorf("without a checksum: %d, %v", n, err)
	}
	empty := tenant.SHA256(sha256.Sum256(nil))
	n, err = CopyChecked(bytes.NewReader(nil), 0, &empty, [2][]byte{make([]byte, 1), make([]byte, 1)}, func([]byte) error {
		t.Error("an empty body passed a piece")
		return nil
	})
	if err != nil || n != 0 {
		t.Errorf("an empty body: %d, %v", n, err)
	}
	// An error of the reader stops the copy, the hash's goroutine ended,
	// with the bytes before it passed but the piece held.
	boom := errors.New("the disk failed")
	got := 0
	n, err = CopyChecked(io.MultiReader(bytes.NewReader(data[:5000]), iotest.ErrReader(boom)), int64(len(data)), &sum,
		[2][]byte{make([]byte, 1000), make([]byte, 1000)}, func(b []byte) error { got += len(b); return nil })
	if !errors.Is(err, boom) || n != 4000 || got != 4000 {
		t.Errorf("an error of the reader: %d passed, %v", n, err)
	}
	// An error of emit stops the copy.
	stop := errors.New("the client went away")
	if _, err := CopyChecked(bytes.NewReader(data), int64(len(data)), &sum, [2][]byte{make([]byte, 10), make([]byte, 10)}, func([]byte) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("an error of emit: %v", err)
	}
}
