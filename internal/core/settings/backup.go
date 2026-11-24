package settings

import (
	"errors"
	"time"
)

// BackupMaxDuration is the longest a backup over the API may run
// (backup.proto, BackupService.CreateBackup): a duration from 1m to 7d
// (ParseBackupMaxDuration), read at the start of each backup. It is
// installation-only, since a backup is the installation's. A backup holds,
// for as long as it runs, a read transaction on the database it is reading
// (on Postgres that holds back the removal of old row versions across the
// whole cluster), the pause of the audit's sealing pass and the server's
// one place for a backup or an export; the server's write stall bound cuts
// only a client that reads nothing, so a client that reads slowly but
// steadily would otherwise keep all three for as long as it likes.
const BackupMaxDuration = "backup.max-duration"

// The bounds of backup.max-duration: a minute, the least in which a small
// installation is backed up over a slow link; seven days, past any backup a
// server should hold open.
const (
	MinBackupMaxDuration = time.Minute
	MaxBackupMaxDuration = 7 * 24 * time.Hour
)

// backupDefinitions returns the definitions of the backup's settings, for
// Builtin.
func backupDefinitions() []Definition {
	return []Definition{{
		// Six hours: room for a large installation's databases over an
		// ordinary link, and short enough that a stuck client gives the
		// databases, the sealing pass and exports back the same day.
		Name: BackupMaxDuration, Default: "6h",
		Validate: func(v string) error { _, err := ParseBackupMaxDuration(v); return err },
	}}
}

// ParseBackupMaxDuration reads backup.max-duration: a duration
// (ParseDuration) from 1m to 7d.
func ParseBackupMaxDuration(v string) (time.Duration, error) {
	d, err := ParseDuration(v)
	if err != nil || d < MinBackupMaxDuration || d > MaxBackupMaxDuration {
		return 0, errors.New("is a whole number of minutes, hours or days from 1m to 7d, such as 6h")
	}
	return d, nil
}
