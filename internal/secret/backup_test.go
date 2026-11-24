package secret

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// smallChunk is the chunk size the tests write with, so a few hundred bytes
// make a file of many chunks.
const smallChunk = 16

// makeBackup encrypts plain under keys with the given chunk size, in writes
// of uneven sizes, and returns the file.
func makeBackup(t testing.TB, keys *Keys, size int, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := newBackupWriter(&buf, keys, size)
	if err != nil {
		t.Fatal(err)
	}
	for step, rest := 1, plain; len(rest) > 0; step = step%7 + 1 {
		n := min(step, len(rest))
		if m, err := w.Write(rest[:n]); err != nil || m != n {
			t.Fatalf("Write = %d, %v", m, err)
		}
		rest = rest[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readBackup decrypts file under keys and returns the plaintext and the first
// error, nil when the reader ended with io.EOF.
func readBackup(keys *Keys, file []byte) ([]byte, error) {
	r, err := NewBackupReader(bytes.NewReader(file), keys)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// pattern returns n bytes that differ from position to position.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*13 + i/251)
	}
	return b
}

// chunkSpan returns where chunk i of a file of small chunks starts and ends.
func chunkSpan(i int) (int, int) {
	start := backupHeaderSize + i*(smallChunk+gcmTagSize)
	return start, start + smallChunk + gcmTagSize
}

// TestBackupRoundTrip guards that what is written reads back the same, for
// an empty stream, one shorter than a chunk, exactly one and several chunks
// (where the last chunk is full), and one byte past a chunk boundary, and
// that the file's length is the header plus one tag per chunk.
func TestBackupRoundTrip(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	for _, n := range []int{0, 1, smallChunk - 1, smallChunk, smallChunk + 1, 3 * smallChunk, 3*smallChunk + 5, 1000} {
		plain := pattern(n)
		file := makeBackup(t, keys, smallChunk, plain)
		chunks := max(1, (n+smallChunk-1)/smallChunk)
		if want := backupHeaderSize + n + chunks*gcmTagSize; len(file) != want {
			t.Errorf("%d bytes: file of %d bytes, want %d", n, len(file), want)
		}
		if binary.BigEndian.Uint32(file[9:13]) != 2 {
			t.Errorf("%d bytes: header names key version %d, want 2", n, binary.BigEndian.Uint32(file[9:13]))
		}
		got, err := readBackup(keys, file)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("%d bytes: read back %d different bytes", n, len(got))
		}
	}
}

// TestBackupDefaultChunkRoundTrip guards the exported writer and its 64 KiB
// chunks over a stream of several chunks written in small pieces, and that
// neither side's buffers grow past one chunk: memory stays bounded whatever
// the stream's length.
func TestBackupDefaultChunkRoundTrip(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	plain := make([]byte, 3*BackupChunkSize+100)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := NewBackupWriter(&buf, keys)
	if err != nil {
		t.Fatal(err)
	}
	for rest := plain; len(rest) > 0; {
		n := min(1000, len(rest))
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	if c := w.s.c; cap(c.plain) != BackupChunkSize || cap(c.sealed) > BackupChunkSize+gcmTagSize {
		t.Errorf("the writer's buffers grew to %d and %d bytes", cap(c.plain), cap(c.sealed))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("a closed writer took more bytes")
	}
	if binary.BigEndian.Uint32(buf.Bytes()[13:17]) != BackupChunkSize {
		t.Errorf("the header names chunk size %d", binary.BigEndian.Uint32(buf.Bytes()[13:17]))
	}
	r, err := NewBackupReader(bytes.NewReader(buf.Bytes()), keys)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("the stream read back differs")
	}
	if c := r.s.c; cap(c.plain) != BackupChunkSize || len(c.sealed) != BackupChunkSize+gcmTagSize {
		t.Errorf("the reader's buffers grew to %d and %d bytes", cap(c.plain), len(c.sealed))
	}
}

// TestBackupEachFileHasItsOwnKey guards that two backups of the same bytes
// under the same key file differ in salt and in every chunk, so the counter
// nonces are never used twice under one key.
func TestBackupEachFileHasItsOwnKey(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	plain := pattern(64)
	a, b := makeBackup(t, keys, smallChunk, plain), makeBackup(t, keys, smallChunk, plain)
	if bytes.Equal(a[17:backupHeaderSize], b[17:backupHeaderSize]) {
		t.Fatal("two files have the same salt")
	}
	for i := range 4 {
		s, e := chunkSpan(i)
		if bytes.Equal(a[s:e], b[s:e]) {
			t.Errorf("chunk %d is the same in two files", i)
		}
	}
}

// wantRefused fails the test unless file is refused with the one fixed
// error, after returning at most a prefix of plain.
func wantRefused(t *testing.T, keys *Keys, file, plain []byte, what string) {
	t.Helper()
	got, err := readBackup(keys, file)
	if err == nil {
		t.Errorf("%s: opened, %d bytes", what, len(got))
		return
	}
	if !errors.Is(err, errBackupNotOpened) && !errors.Is(err, errNotBackup) {
		t.Errorf("%s: err = %v, want the fixed refusal", what, err)
	}
	if !bytes.HasPrefix(plain, got) {
		t.Errorf("%s: returned %d bytes that are not the start of the stream", what, len(got))
	}
}

// TestBackupRefusesTruncation guards that a file cut at any byte does not
// open, including a cut exactly at a chunk boundary, where every chunk left
// is whole and authentic but none is marked last.
func TestBackupRefusesTruncation(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	for _, n := range []int{0, 5, 3 * smallChunk, 3*smallChunk + 7} {
		plain := pattern(n)
		file := makeBackup(t, keys, smallChunk, plain)
		for cut := range len(file) {
			wantRefused(t, keys, file[:cut], plain, "cut")
		}
	}
}

// TestBackupRefusesReorderedChunks guards that two chunks swapped, a chunk
// dropped from the middle, and a chunk repeated do not open, though each
// chunk is authentic where it was written.
func TestBackupRefusesReorderedChunks(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	plain := pattern(4*smallChunk + 3)
	file := makeBackup(t, keys, smallChunk, plain)
	s0, e0 := chunkSpan(0)
	s1, e1 := chunkSpan(1)

	swapped := append([]byte(nil), file...)
	copy(swapped[s0:e0], file[s1:e1])
	copy(swapped[s1:e1], file[s0:e0])
	wantRefused(t, keys, swapped, plain, "chunks 0 and 1 swapped")

	dropped := append(append([]byte(nil), file[:s1]...), file[e1:]...)
	wantRefused(t, keys, dropped, plain, "chunk 1 dropped")

	repeated := append(append(append([]byte(nil), file[:e1]...), file[s1:e1]...), file[e1:]...)
	wantRefused(t, keys, repeated, plain, "chunk 1 repeated")
}

// TestBackupRefusesAppending guards that nothing can be added after the last
// chunk: a byte, a chunk copied from the same file, or another backup's last
// chunk, which is itself marked last.
func TestBackupRefusesAppending(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	plain := pattern(2*smallChunk + 4)
	file := makeBackup(t, keys, smallChunk, plain)
	s0, e0 := chunkSpan(0)
	wantRefused(t, keys, append(append([]byte(nil), file...), 0), plain, "a byte appended")
	wantRefused(t, keys, append(append([]byte(nil), file...), file[s0:e0]...), plain, "chunk 0 appended")
	other := makeBackup(t, keys, smallChunk, pattern(3))
	wantRefused(t, keys, append(append([]byte(nil), file...), other[backupHeaderSize:]...), plain, "another file's last chunk appended")
	// A full plaintext whose last chunk is full: the case where the reader
	// learns that a chunk is last only from the end of the stream.
	full := pattern(2 * smallChunk)
	fullFile := makeBackup(t, keys, smallChunk, full)
	wantRefused(t, keys, append(append([]byte(nil), fullFile...), fullFile[s0:e0]...), full, "a chunk after a full last chunk")
}

// TestBackupRefusesAnyFlippedBit guards that one bit changed anywhere, in
// the header or in any chunk, ciphertext or tag, makes the file not open.
func TestBackupRefusesAnyFlippedBit(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1)+keyLine(2, 2))
	plain := pattern(2*smallChunk + 9)
	file := makeBackup(t, keys, smallChunk, plain)
	for i := range len(file) {
		for _, bit := range []byte{0x01, 0x80} {
			changed := append([]byte(nil), file...)
			changed[i] ^= bit
			got, err := readBackup(keys, changed)
			if err == nil {
				t.Fatalf("byte %d bit %#x: opened, %d bytes", i, bit, len(got))
			}
			if !bytes.HasPrefix(plain, got) {
				t.Fatalf("byte %d bit %#x: returned bytes that are not the stream's", i, bit)
			}
		}
	}
}

// TestBackupRefusesAnotherKeyFile guards that a backup does not open under a
// key file with another key of the same version, and that a key version the
// file does not hold is NotFound with the version named.
func TestBackupRefusesAnotherKeyFile(t *testing.T) {
	plain := pattern(40)
	file := makeBackup(t, mustKeys(t, keyLine(1, 1)+keyLine(2, 2)), smallChunk, plain)
	wantRefused(t, mustKeys(t, keyLine(2, 9)), file, plain, "another key")
	_, err := readBackup(mustKeys(t, keyLine(1, 1)), file)
	if fault.KindOf(err) != fault.NotFound || !bytes.Contains([]byte(fault.Message(err)), []byte("key version 2")) {
		t.Errorf("missing version: err = %v, want NotFound naming version 2", err)
	}
}

// TestBackupRefusesBadHeaders guards the header's checks: a file that does
// not start with the magic or is shorter than a header is "not a backup", an
// unknown format is named, and a chunk size of 0 or over 1 MiB is refused
// before any buffer of that size is made.
func TestBackupRefusesBadHeaders(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	file := makeBackup(t, keys, smallChunk, pattern(20))
	for _, bad := range [][]byte{nil, []byte("NOPACSBK"), []byte("PK\x03\x04 not a backup at all, but long enough to hold a header")} {
		if _, err := readBackup(keys, bad); !errors.Is(err, errNotBackup) {
			t.Errorf("%q: err = %v, want not a backup", bad, err)
		}
	}
	format := append([]byte(nil), file...)
	format[8] = 2
	if _, err := readBackup(keys, format); fault.KindOf(err) != fault.Invalid || !bytes.Contains([]byte(fault.Message(err)), []byte("format 2")) {
		t.Errorf("format 2: err = %v", err)
	}
	for _, size := range []uint32{0, maxBackupChunkSize + 1, 1<<32 - 1} {
		h := append([]byte(nil), file...)
		binary.BigEndian.PutUint32(h[13:17], size)
		if _, err := readBackup(keys, h); !errors.Is(err, errBackupNotOpened) {
			t.Errorf("chunk size %d: err = %v", size, err)
		}
	}
	if _, err := newBackupWriter(io.Discard, keys, 0); err == nil {
		t.Error("a writer took chunk size 0")
	}
}

// failingWriter fails every write after the first n.
type failingWriter struct{ n int }

// Write counts writes and fails once n are done.
func (f *failingWriter) Write(p []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("disk full")
	}
	f.n--
	return len(p), nil
}

// TestBackupWriterKeepsAWriteError guards that a failed write of a chunk is
// returned, and returned again by every later Write and by Close, so a
// backup that lost a chunk is never reported as written.
func TestBackupWriterKeepsAWriteError(t *testing.T) {
	keys := mustKeys(t, keyLine(1, 1))
	w, err := newBackupWriter(&failingWriter{n: 1}, keys, smallChunk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(pattern(3 * smallChunk)); err == nil {
		t.Fatal("a failed chunk write was not returned")
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("Write after a failure succeeded")
	}
	if err := w.Close(); err == nil {
		t.Error("Close after a failure succeeded")
	}
}
