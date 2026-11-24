package secret

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/amrshadid/nopacs/internal/fault"
)

// A tenant moves to another installation under a transfer key: 32 random
// bytes made for one export. The two installations' key files never meet on
// one machine, so the export opens every sealed value with its own key file
// and seals it again under the transfer key, and encrypts the whole file
// under it; the import opens both with the transfer key and seals each value
// again with its own key file.
//
// The transfer key is used as a key file of one version, 1: Seal and Open
// work under it as under any key file, with the same contexts, and the
// backup cipher (NewBackupWriter, NewBackupReader) derives each file's key
// from it with the same labels. So the export needs no cipher of its own,
// and a file or a value made under a transfer key opens with that key alone.

// TransferKeySize is the length of a transfer key in bytes: AES-256's key
// length, the same as a key file's.
const TransferKeySize = keySize

// transferVersion is the one version a transfer key is used as.
const transferVersion = 1

// NewTransferKey returns a new transfer key, from the system's random
// source, and the Keys that seal and open under it. The caller hands the
// bytes on once, to whoever imports, and clears them.
func NewTransferKey() ([]byte, *Keys, error) {
	key := make([]byte, TransferKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, nil, fault.Wrap(fault.Internal, err, "secret: no transfer key could be made")
	}
	k, err := TransferKeys(key)
	if err != nil {
		clear(key)
		return nil, nil, err
	}
	return key, k, nil
}

// A transfer key file holds the key in standard base64 with padding (RFC
// 4648 section 4) and a line feed: one line a person can copy whole, and
// that a reader takes in exactly one form.

// WriteTransferKeyFile writes key to a new file at path that only the
// current user can open (CreatePrivateTemp beside it, then a hard link, so a
// file already at path, or one put there meanwhile, is never replaced; a
// rename after a check where links cannot be made). A file at path is a
// Precondition error.
func WriteTransferKeyFile(path string, key []byte) error {
	if len(key) != TransferKeySize {
		return fault.New(fault.Internal, "secret: a transfer key is 32 bytes")
	}
	if _, err := os.Lstat(path); err == nil {
		return fault.New(fault.Precondition, "secret: "+path+" exists; a transfer key file never replaces a file")
	}
	dir := filepath.Dir(path)
	f, err := CreatePrivateTemp(dir, "."+filepath.Base(path)+".*.part")
	if err != nil {
		return err
	}
	part := f.Name()
	defer func() { _ = os.Remove(part) }()
	line := make([]byte, base64.StdEncoding.EncodedLen(len(key))+1)
	base64.StdEncoding.Encode(line, key)
	line[len(line)-1] = '\n'
	_, werr := f.Write(line)
	clear(line)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fault.Wrap(fault.Internal, werr, "secret: the transfer key file cannot be written")
	}
	if err := os.Link(part, path); err == nil {
		return nil
	} else if errors.Is(err, fs.ErrExist) {
		return fault.New(fault.Precondition, "secret: "+path+" appeared while the transfer key file was written; it is not replaced")
	}
	if _, err := os.Lstat(path); err == nil {
		return fault.New(fault.Precondition, "secret: "+path+" appeared while the transfer key file was written; it is not replaced")
	}
	if err := os.Rename(part, path); err != nil {
		return fault.Wrap(fault.Internal, err, "secret: the transfer key file cannot be written")
	}
	return nil
}

// ReadTransferKeyFile reads the transfer key in the file at path, which
// must be private to the current user (OpenPrivate) and hold exactly the
// form WriteTransferKeyFile writes. Its errors never quote the file.
func ReadTransferKeyFile(path string) ([]byte, error) {
	f, err := OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, base64.StdEncoding.EncodedLen(TransferKeySize)+2)
	defer clear(buf)
	n, err := readFull(f, buf)
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "secret: the transfer key file cannot be read")
	}
	text := bytes.TrimSuffix(bytes.TrimSuffix(buf[:n], []byte("\n")), []byte("\r"))
	bad := fault.New(fault.Invalid, "secret: the transfer key file does not hold one key of 32 bytes in standard base64 with padding")
	if len(text) != base64.StdEncoding.EncodedLen(TransferKeySize) {
		return nil, bad
	}
	key := make([]byte, TransferKeySize+2)
	m, err := base64.StdEncoding.Strict().Decode(key, text)
	if err != nil || m != TransferKeySize {
		clear(key)
		return nil, bad
	}
	return key[:TransferKeySize], nil
}

// TransferKeys returns the Keys that seal and open under a transfer key,
// TransferKeySize bytes; any other length is Invalid. The key's bytes are
// not kept: only its AEAD and its HKDF extract are (see Keys).
func TransferKeys(key []byte) (*Keys, error) {
	if len(key) != TransferKeySize {
		return nil, fault.New(fault.Invalid, "secret: a transfer key is 32 bytes")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: the transfer key cannot be used")
	}
	out, err := extract(key)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "secret: the transfer key cannot be used")
	}
	prk := new([derivedSize]byte)
	copy(prk[:], out)
	clear(out)
	return &Keys{r: &keyring{
		aeads:       map[uint32]cipher.AEAD{transferVersion: aead},
		prks:        map[uint32]*[derivedSize]byte{transferVersion: prk},
		sealVersion: transferVersion,
	}}, nil
}
