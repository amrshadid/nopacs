package secret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
)

// keyLinePrefix starts every line of a key file. It names the line's format,
// so a later format can sit beside it in the same file.
const keyLinePrefix = "nopacs-key-v1"

// keySize is the length of a key in bytes: AES-256 takes a 256-bit key.
const keySize = 32

// MaxKeyVersion is the highest key version a key file may hold, 2^31-1, the
// largest a protobuf int32 carries.
const MaxKeyVersion = 1<<31 - 1

// maxKeyFileSize bounds how much of a key file is read. A real one holds a
// line of 63 bytes per key version; a file larger than this is not a key file,
// and reading it whole would let a mistaken path exhaust memory.
const maxKeyFileSize = 64 << 10

// About wiping key bytes: this package clears every buffer it owns that held
// a key or a key file's text once it is done with it, and never copies one
// into a string, which could not be cleared. Go cannot promise that this
// wipes the memory: the garbage collector may have moved or copied a buffer
// before it was cleared, and the AES cipher keeps its own expanded copy of
// each key for as long as the Keys holding it lives. Clearing narrows the
// window in which a memory dump finds a key; it does not close it.

// Keys is a loaded key file: every key version it holds, ready to open, and
// the highest version, which seals. Its zero value, and a nil *Keys, hold no
// key and seal and open nothing.
//
// Formatted directly, or as an exported field of another value, a Keys
// prints a fixed text (Format). As an unexported field fmt cannot call its
// methods and reads its fields instead, so every secret sits two pointers
// deep. One pointer is not enough: under a verb that does not fit a pointer
// (%s, %q, %t, %f and others) fmt prints the pointer again with %v from depth
// 0, and at depth 0 it follows a pointer into what it points to. After that
// one restart every verb is %v, and at any depth past 0 %v prints a pointer
// as an address. So Keys points to a keyring, and inside the keyring each
// secret (the AEAD's expanded key, each pseudorandom key) is behind a
// pointer of its own.
type Keys struct {
	r *keyring
}

// keyring is what Keys points to. Its fields hold no secret by value; see
// Keys.
type keyring struct {
	aeads map[uint32]cipher.AEAD
	// prks holds, per version, the HKDF pseudorandom key extracted from that
	// version's key (RFC 5869 2.2), from which Derive expands every derived
	// key. Keeping the extract rather than the key itself means the only
	// copy of the key that can open a sealed value stays inside its AEAD.
	prks        map[uint32]*[derivedSize]byte
	sealVersion uint32
}

// ring returns k's keyring, or nil for a nil or zero Keys.
func (k *Keys) ring() *keyring {
	if k == nil {
		return nil
	}
	return k.r
}

// text is the fixed text Keys prints as.
func (k Keys) text() string {
	n := 0
	if k.r != nil {
		n = len(k.r.aeads)
	}
	return "secret.Keys{" + strconv.Itoa(n) + " versions}"
}

// Format writes a fixed text in place of the keys, for every verb.
func (k Keys) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, k.text())
}

// LogValue gives log/slog the same fixed text, so a handler never walks into
// the value.
func (k Keys) LogValue() slog.Value {
	return slog.StringValue(k.text())
}

// Versions returns how many key versions k holds.
func (k *Keys) Versions() int {
	if r := k.ring(); r != nil {
		return len(r.aeads)
	}
	return 0
}

// SealVersion returns the key version Seal uses: the highest in the file, or
// 0 when k holds no key.
func (k *Keys) SealVersion() uint32 {
	if r := k.ring(); r != nil {
		return r.sealVersion
	}
	return 0
}

// fileSystem is how the key file is checked and written. The zero value is
// the system itself, which every exported function uses; a test sets a
// field to stand in for a user or a moment it cannot make happen.
type fileSystem struct {
	// uid returns the user a private file and its directory must belong
	// to; nil is the process's effective user. Read on Unix only.
	uid func() int //nolint:unused // read on Unix only, by perm_unix.go.
	// beforeLink runs in createKeyFile between writing the temporary file
	// and linking it to its name, so a test can make a file appear at
	// exactly that moment; nil does nothing.
	beforeLink func()
	// syncDir flushes the key file's directory; nil is the system's
	// syncDir, and a test sets one that fails.
	syncDir func(dir string) error
}

// system is the file system as it is, with no stand-in.
var system fileSystem

// CreateKeyFile writes a new key file at path holding one random key, version
// 1. It never replaces a file that exists: losing the key that sealed the
// secrets loses the secrets, so an existing file is a Conflict. On Linux and
// macOS it refuses a directory that is not private to its owner (see
// checkDir in perm_unix.go).
//
// The file is written in full to a temporary file in the same directory,
// created with the narrowest permission the system has (see perm_unix.go and
// perm_windows.go), and only then given its name, so a crash leaves either
// no key file or a whole one, never a short one that would be loaded later.
func CreateKeyFile(path string) error { return system.createKeyFile(path) }

// createKeyFile is CreateKeyFile on fsys.
func (fsys fileSystem) createKeyFile(path string) error {
	dir := filepath.Dir(path)
	if err := fsys.checkDir(dir, path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return existsError(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be checked")
	}

	key := make([]byte, keySize)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return fault.Wrap(fault.Internal, err, "secret: no random key could be made")
	}
	line := make([]byte, 0, len(keyLinePrefix)+3+base64.StdEncoding.EncodedLen(keySize)+1)
	line = append(line, keyLinePrefix+" 1 "...)
	line = base64.StdEncoding.AppendEncode(line, key)
	line = append(line, '\n')
	defer clear(line)

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fault.Wrap(fault.Internal, err, "secret: no random name could be made")
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(suffix))

	f, err := createPrivate(tmp)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be created")
	}
	// The temporary file is removed on every path; once it has been linked to
	// its final name, removing it removes only the temporary name.
	defer func() { _ = os.Remove(tmp) }()

	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be written")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be written")
	}
	if err := f.Close(); err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be written")
	}

	if fsys.beforeLink != nil {
		fsys.beforeLink()
	}
	// A hard link is a rename that refuses to replace: os.Rename replaces an
	// existing file on every system, so a file made at path since the check
	// above would be lost. Link fails with fs.ErrExist instead, on Linux,
	// macOS and Windows (NTFS) alike, and the file keeps the permission it was
	// created with, because it is the same file under a second name.
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return existsError(path)
		}
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be created")
	}

	// From here the key file exists, whole, under its name: removing it would
	// be worse than keeping it, and saying "cannot be created" would be
	// false. A failed directory sync means only that the name may not survive
	// a power loss. The kind is Internal: the caller did nothing wrong
	// (not Invalid), and trying again would not help (not Unavailable), since
	// the file now exists and a second call is a Conflict. The message says
	// what is true.
	sync := syncDir
	if fsys.syncDir != nil {
		sync = fsys.syncDir
	}
	if err := sync(dir); err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the key file "+path+
			" was created, but its directory could not be synced, so the file may not survive a crash; check the disk and back the file up")
	}
	return nil
}

// existsError is the refusal to replace an existing key file.
func existsError(path string) error {
	return fault.New(fault.Conflict, "secret: the key file "+path+" exists, and a key file is never replaced")
}

// OpenPrivate opens the file at path for reading and returns it only when
// it is private to the current user, by the same checks LoadKeyFile makes on
// a key file: a regular file, owned by the current user, that no other user
// may open (mode and, on macOS, access list entries on Linux and macOS; owner
// and access list on Windows). It exists for a command that reads a
// credential from a file of the person's own, such as the command line's
// --key-file, so that rule is written once. The refusal is a fault of kind
// Invalid whose message says how to fix the file and never quotes its
// content; a missing file is NotFound.
//
// Unlike LoadKeyFile it does not check the file's directory: this file is not
// the installation's key, only a credential the person chose to keep, and a
// directory such as a shared temporary one is not a reason to refuse it. The
// caller reads the file and closes it. The file is opened without blocking
// and checked as opened, so the file checked is the file read.
func OpenPrivate(path string) (*os.File, error) { return system.openPrivate(path) }

// openPrivate is OpenPrivate on fsys.
func (fsys fileSystem) openPrivate(path string) (*os.File, error) {
	f, err := openKeyFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fault.Wrap(fault.NotFound, err, "secret: there is no file at "+path)
		}
		return nil, fault.Wrap(fault.Internal, err, "secret: the file "+path+" cannot be opened")
	}
	if err := fsys.checkPrivate(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// LoadKeyFile reads the key file at path. It refuses a file that is not a
// regular file, that other users may open (with the command that fixes it in
// the message), that is over 64 KiB, or with any line that does not follow
// the format in the package documentation. What counts as "other users may
// open" on each system is in the package documentation.
//
// The file is opened without blocking, so a FIFO or a device at the path is
// refused rather than waited on, and its permission is checked on the open
// file, not on the path, so the file checked is the file read.
func LoadKeyFile(path string) (*Keys, error) { return system.loadKeyFile(path) }

// loadKeyFile is LoadKeyFile on fsys.
func (fsys fileSystem) loadKeyFile(path string) (*Keys, error) {
	if err := fsys.checkDir(filepath.Dir(path), path); err != nil {
		return nil, err
	}
	f, err := openKeyFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fault.Wrap(fault.NotFound, err, "secret: there is no key file at "+path)
		}
		return nil, fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be opened")
	}
	defer func() { _ = f.Close() }()

	if err := fsys.checkPrivate(f, path); err != nil {
		return nil, err
	}
	// One buffer of fixed size, read into directly, so no growing copy of the
	// file's text is left behind uncleared.
	buf := make([]byte, maxKeyFileSize+1)
	defer clear(buf)
	n, err := readFull(f, buf)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: the key file "+path+" cannot be read")
	}
	if n > maxKeyFileSize {
		return nil, fault.New(fault.Invalid, "secret: "+path+" is larger than any key file ("+strconv.Itoa(maxKeyFileSize)+" bytes at most)")
	}
	keys, err := parseKeyFile(buf[:n])
	if err != nil {
		// The parser's message already says which line and why; wrapping
		// the same error as the cause would only repeat it in the log.
		return nil, fault.New(fault.KindOf(err), "secret: the key file "+path+" is not valid: "+fault.Message(err))
	}
	return keys, nil
}

// readFull reads r into buf until buf is full or r ends, and returns how many
// bytes it read.
func readFull(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// parseKeyFile reads the text of a key file. It works on the bytes it is
// given, without copying them into strings. Its errors name the line and the
// field at fault, and never quote a line, since a line holds a key.
func parseKeyFile(data []byte) (*Keys, error) {
	if len(data) == 0 {
		return nil, fault.New(fault.Invalid, "it is empty")
	}
	lines := bytes.Split(data, []byte("\n"))
	// A final line feed ends the last line; it does not start an empty one.
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	ring := &keyring{
		aeads: make(map[uint32]cipher.AEAD, len(lines)),
		prks:  make(map[uint32]*[derivedSize]byte, len(lines)),
	}
	var key [keySize]byte
	defer clear(key[:])
	for i, line := range lines {
		version, err := parseKeyLine(bytes.TrimSuffix(line, []byte("\r")), &key)
		if err != nil {
			return nil, fault.New(fault.Invalid, "line "+strconv.Itoa(i+1)+": "+fault.Message(err))
		}
		if _, ok := ring.aeads[version]; ok {
			return nil, fault.New(fault.Invalid, "line "+strconv.Itoa(i+1)+": version "+strconv.FormatUint(uint64(version), 10)+" appears twice")
		}
		aead, err := newAEAD(key[:])
		if err != nil {
			clear(key[:])
			return nil, fault.Wrap(fault.Internal, err, "line "+strconv.Itoa(i+1)+": the key cannot be used")
		}
		out, err := extract(key[:])
		clear(key[:])
		if err != nil {
			return nil, fault.Wrap(fault.Internal, err, "line "+strconv.Itoa(i+1)+": the key cannot be used")
		}
		prk := new([derivedSize]byte)
		copy(prk[:], out)
		clear(out)
		ring.aeads[version] = aead
		ring.prks[version] = prk
		ring.sealVersion = max(ring.sealVersion, version)
	}
	return &Keys{r: ring}, nil
}

// encodedKeySize is the length of a key in standard base64 with padding.
var encodedKeySize = base64.StdEncoding.EncodedLen(keySize)

// parseKeyLine reads one line, the prefix, the version and the key, and puts
// the key in *key. Every field must be written in exactly one way: base64
// decoding skips line breaks and integer parsing accepts leading zeros, and a
// file read two ways by two programs is a file whose meaning is not fixed.
// So the version is compared with its decimal spelling, and the key with its
// own encoding, in constant time since it is secret.
func parseKeyLine(line []byte, key *[keySize]byte) (uint32, error) {
	fields := bytes.Split(line, []byte(" "))
	if len(fields) != 3 {
		return 0, fault.New(fault.Invalid, "a line must be \""+keyLinePrefix+" <version> <key>\", with one space between fields")
	}
	if !bytes.Equal(fields[0], []byte(keyLinePrefix)) {
		return 0, fault.New(fault.Invalid, "a line must start with \""+keyLinePrefix+"\"")
	}
	// The version is not secret, so a string copy of it is harmless. It is
	// bounded to 2^31-1, not 2^32-1: the API carries a key version as a
	// protobuf int32, and a version above that would be stored, signed and
	// named in a backup's header, yet could not be asked about.
	version := string(fields[1])
	v, err := strconv.ParseUint(version, 10, 31)
	if err != nil || v == 0 || strconv.FormatUint(v, 10) != version {
		return 0, fault.New(fault.Invalid, "the version must be a whole number from 1 to "+strconv.Itoa(MaxKeyVersion)+", without leading zeros")
	}
	badKey := fault.New(fault.Invalid, "the key must be "+strconv.Itoa(keySize)+" bytes in standard base64 with padding")
	field := fields[2]
	if len(field) != encodedKeySize {
		return 0, badKey
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(field)))
	defer clear(decoded)
	n, err := base64.StdEncoding.Strict().Decode(decoded, field)
	if err != nil || n != keySize {
		return 0, badKey
	}
	reencoded := make([]byte, encodedKeySize)
	defer clear(reencoded)
	base64.StdEncoding.Encode(reencoded, decoded[:keySize])
	if subtle.ConstantTimeCompare(reencoded, field) != 1 {
		return 0, badKey
	}
	copy(key[:], decoded[:keySize])
	return uint32(v), nil
}

// newAEAD returns AES-256-GCM under key, with a random 96-bit nonce made by
// the standard library for every seal and carried in front of the
// ciphertext. The standard library makes the nonce, rather than this
// package, so no code here can reuse one.
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}
