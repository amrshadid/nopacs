package secret

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
)

// The backup cipher: a stream of any length encrypted with AES-256-GCM
// (NIST SP 800-38D) in chunks, in the way the STREAM construction of Hoang,
// Reyhanitabar, Rogaway and Vizár ("Online Authenticated-Encryption and its
// Nonce-Reuse Misuse-Resistance", CRYPTO 2015) lays out, so it can be written
// and read with bounded memory and yet a file that was cut short, had chunks
// reordered or dropped, or had anything appended does not open. The format
// is in the package documentation; scripts/check/backup-cipher.py implements
// it with Python's cryptography package.

// backupMagic starts every backup file, so a file that is not one is named
// as such instead of failing as "changed".
const backupMagic = "NOPACSBK"

// backupFormatV1 is the format byte of the layout below.
const backupFormatV1 = 0x01

// backupHeaderSize is magic (8), format (1), key version (4), chunk size (4)
// and salt (32).
const backupHeaderSize = 8 + 1 + 4 + 4 + backupSaltSize

// backupSaltSize is the length of each file's random salt. Every file gets
// its own key from the salt, so the counter nonces below never repeat under
// one key across files: 256 random bits make two files with the same salt
// beyond any count of backups.
const backupSaltSize = 32

// backupFileInfo is the HKDF info string that derives a file's key from the
// Backup key and the file's salt.
const backupFileInfo = "nopacs/backup/v1/file"

// BackupChunkSize is the plaintext size of every chunk but the last. 64 KiB
// keeps the reader's and writer's memory small and the 16-byte tag per chunk
// at 0.02 percent of the file.
const BackupChunkSize = 64 << 10

// maxBackupChunkSize bounds the chunk size a header may declare, so a hostile
// header cannot make the reader allocate more than this for one chunk.
const maxBackupChunkSize = 1 << 20

// gcmTagSize is the length of the GCM tag that ends every chunk.
const gcmTagSize = 16

// errBackupNotOpened is the one error for a backup that fails to
// authenticate, whatever the cause: a changed byte, a missing or extra chunk,
// a cut, a header changed, another key file. A reason would tell whoever
// changed the file which part to fix next.
var errBackupNotOpened = fault.New(fault.Invalid,
	"secret: the backup does not open: it was changed, cut short or extended, or it was made with another key file")

// errNotBackup is the error for a file that does not start like a backup.
var errNotBackup = fault.New(fault.Invalid, "secret: this is not a NoPACs backup file")

// chunkNonce returns the 96-bit nonce of chunk index: the index as 8 bytes
// big-endian, three zero bytes, and 1 for the last chunk or 0 for any other.
// Each (index, last) pair occurs once in a file, and each file has its own
// key, so no nonce is used twice under a key (NIST SP 800-38D 8.2.1).
func chunkNonce(index uint64, last bool) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce, index)
	if last {
		nonce[11] = 1
	}
	return nonce
}

// chunkAD returns the additional data of chunk index: the whole header, then
// the index as 8 bytes big-endian, then the last-chunk flag as one byte. The
// nonce already holds both; binding them again here, and the header with
// them, means a chunk moved to another place, another file, or a header with
// another chunk size fails authentication on its own terms.
func chunkAD(header []byte, index uint64, last bool) []byte {
	ad := make([]byte, 0, len(header)+9)
	ad = append(ad, header...)
	ad = binary.BigEndian.AppendUint64(ad, index)
	if last {
		return append(ad, 1)
	}
	return append(ad, 0)
}

// fileAEAD returns AES-256-GCM under the file's key: HKDF-SHA-256 (RFC 5869)
// with the Backup key as input, the file's salt as salt, and
// backupFileInfo as info.
func fileAEAD(d *DerivedKey, salt []byte) (cipher.AEAD, error) {
	dd, err := d.need(Backup)
	if err != nil {
		return nil, err
	}
	key, err := hkdf.Key(sha256.New, dd.key[:], salt, backupFileInfo, 32)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: a backup key cannot be derived")
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: a backup key cannot be used")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: a backup key cannot be used")
	}
	return aead, nil
}

// backupChunk holds what a backup writer or reader keeps of the file's key
// and its plaintext: the cipher, with the file key expanded inside it, and
// the buffers of one chunk. It is reached only through a pointer from
// backupWriter or backupReader, which are themselves behind a pointer, so it
// sits two pointers deep and fmt prints an address for it (see Keys).
type backupChunk struct {
	aead   cipher.AEAD
	plain  []byte
	sealed []byte
}

// backupText is the fixed text a backup writer or reader prints as: its type
// and the key version its header names, which is not secret.
func backupText(name string, header []byte) string {
	if len(header) < 13 {
		return "secret." + name + "{}"
	}
	return "secret." + name + "{key version " + strconv.FormatUint(uint64(binary.BigEndian.Uint32(header[9:13])), 10) + "}"
}

// BackupWriter encrypts what is written to it into a backup file. Close
// writes the last chunk; a file whose writer was not closed does not open.
// Like Keys it prints a fixed text in every form and holds the file's key and
// its buffered plaintext two pointers deep.
type BackupWriter struct {
	s *backupWriter
}

// backupWriter is what BackupWriter points to.
type backupWriter struct {
	w      io.Writer
	c      *backupChunk
	header []byte
	size   int
	index  uint64
	closed bool
	err    error
}

// Format writes a fixed text in place of the writer's state, for every verb.
func (bw BackupWriter) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, bw.text())
}

// LogValue gives log/slog the same fixed text.
func (bw BackupWriter) LogValue() slog.Value {
	return slog.StringValue(bw.text())
}

// text is the fixed text BackupWriter prints as.
func (bw BackupWriter) text() string {
	if bw.s == nil {
		return backupText("BackupWriter", nil)
	}
	return backupText("BackupWriter", bw.s.header)
}

// errNoBackupWriter is the error of a BackupWriter that NewBackupWriter did
// not make.
var errNoBackupWriter = fault.New(fault.Internal, "secret: a backup writer used without NewBackupWriter")

// NewBackupWriter writes a backup header to w and returns a writer that
// encrypts under the Backup key derived from k's highest version, with a
// fresh random salt.
func NewBackupWriter(w io.Writer, k *Keys) (*BackupWriter, error) {
	return newBackupWriter(w, k, BackupChunkSize)
}

// newBackupWriter is NewBackupWriter with a given chunk size, so a test can
// make files of many chunks from little data.
func newBackupWriter(w io.Writer, k *Keys, size int) (*BackupWriter, error) {
	if size < 1 || size > maxBackupChunkSize {
		return nil, fault.New(fault.Internal, "secret: a backup chunk is 1 byte to 1 MiB")
	}
	d, err := k.Derive(Backup)
	if err != nil {
		return nil, err
	}
	header := make([]byte, 0, backupHeaderSize)
	header = append(header, backupMagic...)
	header = append(header, backupFormatV1)
	header = binary.BigEndian.AppendUint32(header, d.Version())
	header = binary.BigEndian.AppendUint32(header, uint32(size)) //nolint:gosec // G115: size is bounded to 1 MiB above.
	salt := header[len(header) : len(header)+backupSaltSize]
	if _, err := rand.Read(salt); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: no random salt could be made")
	}
	header = header[:backupHeaderSize]
	aead, err := fileAEAD(d, salt)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(header); err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: the backup cannot be written")
	}
	return &BackupWriter{s: &backupWriter{
		w: w,
		c: &backupChunk{
			aead:   aead,
			plain:  make([]byte, 0, size),
			sealed: make([]byte, 0, size+gcmTagSize),
		},
		header: header,
		size:   size,
	}}, nil
}

// Write encrypts p. A full chunk is written only once more data follows it,
// because only then is it known not to be the last.
func (bw *BackupWriter) Write(p []byte) (int, error) {
	if bw == nil || bw.s == nil {
		return 0, errNoBackupWriter
	}
	s := bw.s
	if s.err != nil {
		return 0, s.err
	}
	if s.closed {
		return 0, fault.New(fault.Internal, "secret: write to a closed backup writer")
	}
	c := s.c
	written := 0
	for len(p) > 0 {
		if len(c.plain) == s.size {
			if err := s.flush(false); err != nil {
				return written, err
			}
		}
		n := copy(c.plain[len(c.plain):s.size], p)
		c.plain = c.plain[:len(c.plain)+n]
		p = p[n:]
		written += n
	}
	return written, nil
}

// Close writes the last chunk, which may hold from 0 to one chunk of bytes,
// and does not close the underlying writer.
func (bw *BackupWriter) Close() error {
	if bw == nil || bw.s == nil {
		return errNoBackupWriter
	}
	s := bw.s
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return nil
	}
	err := s.flush(true)
	s.closed = true
	clear(s.c.plain[:cap(s.c.plain)])
	return err
}

// flush encrypts the buffered plaintext as chunk s.index and writes it.
func (s *backupWriter) flush(last bool) error {
	c := s.c
	c.sealed = c.aead.Seal(c.sealed[:0], chunkNonce(s.index, last), c.plain, chunkAD(s.header, s.index, last))
	if _, err := s.w.Write(c.sealed); err != nil {
		s.err = fault.Wrap(fault.Internal, err, "secret: the backup cannot be written")
		return s.err
	}
	s.index++
	c.plain = c.plain[:0]
	return nil
}

// BackupReader decrypts a backup file. Each chunk is authenticated before any
// of its bytes is returned, so memory stays bounded, but a stream is
// authenticated whole only when Read returns io.EOF: until then, the bytes
// already read may belong to a file that turns out to be cut short or
// changed further on. A caller that acts on the content, such as a restore,
// must stage what it reads apart from anything live, make it live only after
// io.EOF, and discard all of it on any error. Like Keys it prints a fixed
// text in every form and holds the file's key and the decrypted chunk two
// pointers deep.
type BackupReader struct {
	s *backupReader
}

// backupReader is what BackupReader points to.
type backupReader struct {
	r      *bufio.Reader
	c      *backupChunk
	header []byte
	size   int
	pos    int
	index  uint64
	done   bool
	err    error
}

// Format writes a fixed text in place of the reader's state, for every verb.
func (br BackupReader) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, br.text())
}

// LogValue gives log/slog the same fixed text.
func (br BackupReader) LogValue() slog.Value {
	return slog.StringValue(br.text())
}

// text is the fixed text BackupReader prints as.
func (br BackupReader) text() string {
	if br.s == nil {
		return backupText("BackupReader", nil)
	}
	return backupText("BackupReader", br.s.header)
}

// NewBackupReader reads a backup header from r and returns a reader of its
// plaintext, under the Backup key derived from the key version the header
// names. A file that is not a backup is Invalid; a key version the key file
// does not hold is NotFound.
func NewBackupReader(r io.Reader, k *Keys) (*BackupReader, error) {
	header := make([]byte, backupHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errNotBackup
		}
		return nil, fault.Wrap(fault.Internal, err, "secret: the backup cannot be read")
	}
	if !bytes.Equal(header[:8], []byte(backupMagic)) {
		return nil, errNotBackup
	}
	if header[8] != backupFormatV1 {
		return nil, fault.New(fault.Invalid, "secret: the backup's format "+strconv.Itoa(int(header[8]))+" is not one this version reads")
	}
	version := binary.BigEndian.Uint32(header[9:13])
	size := binary.BigEndian.Uint32(header[13:17])
	if size < 1 || size > maxBackupChunkSize {
		return nil, errBackupNotOpened
	}
	d, err := k.DeriveVersion(Backup, version)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			return nil, fault.New(fault.NotFound, "secret: the backup was made under key version "+
				strconv.FormatUint(uint64(version), 10)+", which the key file does not hold")
		}
		return nil, err
	}
	aead, err := fileAEAD(d, header[17:])
	if err != nil {
		return nil, err
	}
	return &BackupReader{s: &backupReader{
		r: bufio.NewReader(r),
		c: &backupChunk{
			aead:   aead,
			sealed: make([]byte, int(size)+gcmTagSize),
			plain:  make([]byte, 0, int(size)),
		},
		header: header,
		size:   int(size),
	}}, nil
}

// Read returns plaintext. It returns io.EOF only after the last chunk has
// been authenticated as last; any change to the file is the one fixed error.
func (br *BackupReader) Read(p []byte) (int, error) {
	if br == nil || br.s == nil {
		return 0, fault.New(fault.Internal, "secret: a backup reader used without NewBackupReader")
	}
	s := br.s
	for s.pos == len(s.c.plain) {
		if s.err != nil {
			return 0, s.err
		}
		if s.done {
			return 0, io.EOF
		}
		if err := s.next(); err != nil {
			s.err = err
			return 0, err
		}
	}
	n := copy(p, s.c.plain[s.pos:])
	s.pos += n
	return n, nil
}

// next reads, authenticates and decrypts the next chunk. A chunk is the last
// when the stream ends right after it: a short chunk, or a full one followed
// by nothing. Its flag must agree, so a cut at a chunk boundary (a chunk not
// marked last at the end) and anything appended (a chunk marked last that is
// not at the end) both fail.
func (br *backupReader) next() error {
	c := br.c
	n, err := io.ReadFull(br.r, c.sealed)
	last := false
	switch {
	case err == nil:
		if _, perr := br.r.Peek(1); perr != nil {
			if !errors.Is(perr, io.EOF) {
				return fault.Wrap(fault.Internal, perr, "secret: the backup cannot be read")
			}
			last = true
		}
	case errors.Is(err, io.ErrUnexpectedEOF):
		last = true
	case errors.Is(err, io.EOF):
		// The stream ended where a chunk should start: the last chunk is
		// missing.
		return errBackupNotOpened
	default:
		return fault.Wrap(fault.Internal, err, "secret: the backup cannot be read")
	}
	if n < gcmTagSize {
		return errBackupNotOpened
	}
	plain, err := c.aead.Open(c.plain[:0], chunkNonce(br.index, last), c.sealed[:n], chunkAD(br.header, br.index, last))
	if err != nil {
		return errBackupNotOpened
	}
	c.plain = plain
	br.pos = 0
	br.index++
	br.done = last
	return nil
}
