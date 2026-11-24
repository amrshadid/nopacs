package server

import (
	"crypto/subtle"
	"encoding/base64"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
)

// The key check: the installation's id, sealed by init with the
// installation's key file and kept in the master beside the id. serve opens
// it with the key file it was given before it opens any tenant or listens.
// A server running with another key file would fail every tenant and, worse,
// seal a new tenant's address with the wrong key, leaving an installation
// that no one key file can open; so a key file that does not open the check
// stops the start before anything is opened or changed.
//
// The value is not secret: it is a known text sealed, which proves only
// that a key file opens it. A sealed value records the key version that
// sealed it, so the check opens with any key file that still holds that
// version, also after newer versions are added for rotation.
const (
	// KeyCheckSetting is the master's setting that holds the key check,
	// under settings.ReservedPrefix, so the settings service neither shows
	// nor changes it.
	KeyCheckSetting = settings.ReservedPrefix + "key-check"
	// keyCheckContext is the context the check is sealed under: its own,
	// so a sealed tenant address cannot pass for a key check, nor the
	// reverse.
	keyCheckContext = "installation/key-check"
)

// Refusal messages of the key check. They name no key material and no path.
const (
	// MessageWrongKeyFile answers a key file that does not open the check.
	MessageWrongKeyFile = "the key file is not the one this installation was made with; nothing was changed. " +
		"Put the installation's own key file back in the data directory and start again"
	// MessageNoKeyCheck answers a master that holds no key check: an
	// installation made before serve checked its key file.
	MessageNoKeyCheck = "the master database holds no key check: this installation was made before serve checked its key file, " +
		"so serve cannot tell whether the key file is the right one; nothing was changed. Make the installation again with nopacs-server init in a new data directory"
)

// SealKeyCheck returns the key check of the installation with this id,
// sealed with keys, as the text the master's settings store keeps.
func SealKeyCheck(keys *secret.Keys, id string) (string, error) {
	sealed, err := keys.Seal(keyCheckContext, []byte(id))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// CheckKeyFile reports whether keys opens the key check stored as value
// for the installation with this id. Any failure, whatever its cause (a
// wrong key, a damaged or foreign value), is Conflict (exit 1) with
// MessageWrongKeyFile: the remedy is the same, and the cause is not told.
func CheckKeyFile(keys *secret.Keys, id, value string) error {
	sealed, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return fault.Wrap(fault.Conflict, err, MessageWrongKeyFile)
	}
	plain, err := keys.Open(keyCheckContext, sealed)
	if err != nil {
		return fault.Wrap(fault.Conflict, err, MessageWrongKeyFile)
	}
	if subtle.ConstantTimeCompare(plain, []byte(id)) != 1 {
		return fault.New(fault.Conflict, MessageWrongKeyFile)
	}
	return nil
}
