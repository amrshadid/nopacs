package settings

import (
	"errors"
	"strings"
	"time"
)

// The settings of scrubs (job.proto, ScrubJob): how fast a scrub reads a
// storage, and how often the server makes one by itself. Both are ordinary:
// a tenant's admin may override the installation's value for the tenant,
// since the archive's bytes are the tenant's to prove.
const (
	// ScrubRate is the most bytes a second a scrub reads from one storage
	// (ParseScrubRate), so a scrub never takes a storage's whole bandwidth
	// from reception and reads.
	ScrubRate = "scrub.rate"
	// ScrubInterval is how often the server makes a scrub of each of a
	// tenant's storages by itself, or "0" for never (ParseScrubInterval).
	ScrubInterval = "scrub.interval"
	// ScrubArchiveSample is the share of the objects online on a storage of
	// the archive tier whose bytes a scrub reads, the rest checked by their
	// presence only (ParseArchiveSample). It is read at the start of each
	// unit of a scrub.
	ScrubArchiveSample = "scrub.archive-sample"
)

// The bounds of the scrub settings.
const (
	// MinScrubRate and MaxScrubRate bound scrub.rate: at least 1 MiB a
	// second, so a scrub of a large archive ends; at most 64 GiB a second,
	// past any storage.
	MinScrubRate = 1 << 20
	MaxScrubRate = 64 << 30
	// MinScrubInterval and MaxScrubInterval bound scrub.interval when it is
	// not "0": a day to ten years.
	MinScrubInterval = 24 * time.Hour
	MaxScrubInterval = 3650 * 24 * time.Hour
)

// ParseScrubRate reads scrub.rate: a size (ParseSize) from 1MiB to 64GiB, the
// bytes a second.
func ParseScrubRate(v string) (int64, error) {
	n, err := ParseSize(v)
	if err != nil || n < MinScrubRate || n > MaxScrubRate {
		return 0, errors.New("is a size from 1MiB to 64GiB, read each second")
	}
	return n, nil
}

// ParseScrubInterval reads scrub.interval: "0" for never, or a duration
// (ParseDuration) from 1d to 3650d. Zero means never.
func ParseScrubInterval(v string) (time.Duration, error) {
	if v == "0" {
		return 0, nil
	}
	d, err := ParseDuration(v)
	if err != nil || d < MinScrubInterval || d > MaxScrubInterval {
		return 0, errors.New(`is "0" for never, or a duration from 1d to 3650d, such as 30d`)
	}
	return d, nil
}

// ParseArchiveSample reads scrub.archive-sample: a whole percentage from
// "0%" to "100%", answered as the share from 0 to 1.
func ParseArchiveSample(v string) (float64, error) {
	digits, ok := strings.CutSuffix(v, "%")
	n, err := wholeNumber(digits, 100)
	if !ok || err != nil {
		return 0, errors.New("is a whole percentage from 0% to 100%, such as 5%")
	}
	return float64(n) / 100, nil
}

// scrubDefinitions returns the definitions of the scrub settings, for
// Builtin.
func scrubDefinitions() []Definition {
	return []Definition{{
		// 64 MiB a second reads a terabyte in under five hours, a small
		// share of a disk's or a bucket's bandwidth.
		Name:           ScrubRate,
		Default:        "64MiB",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseScrubRate(v); return err },
	}, {
		// Thirty days: a byte changed on a storage is found within a month,
		// and a month's reading of the archive at the default rate is a
		// small share of the storage's time.
		Name:           ScrubInterval,
		Default:        "30d",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseScrubInterval(v); return err },
	}, {
		// None: a read of an archive-tier object may bill the site, and a
		// presence check (size, and the store's own checksum where it keeps
		// one) costs nothing; a site that wants its cold copies read sets a
		// share.
		Name:           ScrubArchiveSample,
		Default:        "0%",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseArchiveSample(v); return err },
	}}
}
