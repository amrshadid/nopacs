package server

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
)

// The identity of an installation: a random id that init writes in two
// places, the data directory and the master database, so that serve can tell
// that the master it was pointed at is the one this installation was made
// with. Without it, serve given another master's address (a typo in
// --master-env, an empty database) would migrate it and serve an installation
// with no keys and no tenants, silently.
const (
	// IDFileName is the file in the data directory that holds the id,
	// beside the key file, private to the user.
	IDFileName = "installation"
	// InstallationIDSetting is the master's copy of the id, kept in the
	// settings store because it is the master's one key-value store, and
	// under settings.ReservedPrefix, which no setting of the server's list
	// may use, so the settings service does not know it and answers it as
	// a name that does not exist.
	InstallationIDSetting = settings.ReservedPrefix + "installation-id"
)

// maxIDFile bounds what is read of the id file: an id is 26 characters.
const maxIDFile = 128

// WriteInstallationID writes id to the id file of dataDir, mode 0600, and
// refuses a file that exists.
func WriteInstallationID(dataDir, id string) error {
	path := filepath.Join(dataDir, IDFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the installation's own file.
	if err != nil {
		return fault.Wrap(fault.Internal, err, "cannot write the installation's id to "+path+": "+err.Error())
	}
	_, werr := f.WriteString(id + "\n")
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return fault.Wrap(fault.Internal, err, "cannot write the installation's id to "+path+": "+err.Error())
	}
	return nil
}

// ReadInstallationID reads the id file of dataDir, refusing one that other
// users may open (as the key file is refused), and returns the id. A missing
// file is NotFound.
func ReadInstallationID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, IDFileName)
	f, err := secret.OpenPrivate(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || fault.KindOf(err) == fault.NotFound {
			return "", fault.Wrap(fault.NotFound, err, "the data directory has no installation id ("+path+")")
		}
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxIDFile))
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "cannot read "+path)
	}
	id := string(bytes.TrimSpace(b))
	if id == "" {
		return "", fault.New(fault.Invalid, "the installation id file "+path+" is empty")
	}
	return id, nil
}
