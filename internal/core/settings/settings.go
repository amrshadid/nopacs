package settings

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// KeyDefaultLifetime is how long a new API key lives when its maker names
// no expiry. Only the installation sets it: a key belongs to the
// installation, not to a tenant.
const KeyDefaultLifetime = "key.default-lifetime"

// The bounds of a key's default lifetime: at least an hour, so a mistyped
// value cannot make keys that expire before they are handed over, and at most
// the longest expiry a key may be given (access.proto, CreateKeyRequest).
const (
	MinKeyLifetime = time.Hour
	MaxKeyLifetime = 3650 * 24 * time.Hour
)

// The settings the reception pipeline reads, each named where it is used in
// the API contract: storage.proto (the roots and endpoints storages may use),
// receiver.proto (the ports, the bind addresses, the incoming limit and the
// reserve) and monitor.proto (how long events are kept).
const (
	// StorageDirectoryRoots is the roots directory storages may lie under,
	// one "name=path" per line (ParseDirectoryRoots). Installation-only.
	StorageDirectoryRoots = "storage.directory-roots"
	// StorageS3Endpoints is the endpoints S3 storages may use, one per line
	// (ParseS3Endpoints). Installation-only.
	StorageS3Endpoints = "storage.s3-endpoints"
	// StorageReserve is the free space kept on every disk or store data is
	// written to (ParseReserve). Installation-only.
	StorageReserve = "storage.reserve"
	// ReceiverPorts is the ports allotted to a tenant (ParsePorts). Set on
	// a tenant by the installation's admin only; no installation value.
	ReceiverPorts = "receiver.ports"
	// ReceiverBindAddresses is the addresses receivers may listen on
	// (ParseBindAddresses). Installation-only.
	ReceiverBindAddresses = "receiver.bind-addresses"
	// IncomingLimit is the most bytes a tenant may hold in the incoming
	// area, "0" for no limit of its own (ParseSize). Set on a tenant by the
	// installation's admin only; the installation's value is every tenant's
	// default.
	IncomingLimit = "incoming.limit"
	// EventsRetention is how long a tenant's events are kept
	// (ParseRetention). Ordinary: a tenant's admin may override it.
	EventsRetention = "events.retention"
	// ReceiverHeaderLimit is the most bytes of a received file read as its
	// header, everything before its pixel data (ParseHeaderLimit). A file
	// whose header is longer is held with its bytes kept, never lost.
	// Installation-only: it bounds the server's memory, which tenants
	// share.
	ReceiverHeaderLimit = "receiver.header-limit"
	// ReceiverAllowAsReceived is "true" when a receiver of the tenant may
	// archive what it receives as received, without de-identifying it
	// (ParseBool). Set on a tenant by the installation's admin only, and
	// "false" by default, so a tenant holds identified data only where a
	// person wrote down that it may; a tenant that had receivers before
	// receivers de-identified was given "true" when its database was
	// upgraded, so they keep doing what they did.
	ReceiverAllowAsReceived = "receiver.allow-as-received"
	// DeidPixelLimit is the most bytes of an image decoded that
	// de-identification works on (ParsePixelLimit): an image a pixel rule
	// fills, or whose JPEG segments are removed, larger than it is held
	// with its bytes kept, never lost, and released once it is raised.
	// The installation's value is every tenant's default; a tenant's own
	// is set by the installation's admin, since the work is the
	// installation's processors and memory.
	DeidPixelLimit = "deid.pixel-limit"
	// DeidMemory is the most bytes the server's de-identification work
	// holds in memory at once, across every tenant (ParseDeidMemory): the
	// check after's copy of each file and a trial's or a deid job's
	// copies. Work that does not fit beside the rest waits for room; a
	// file whose check alone needs more is held with its bytes kept
	// (deid_failed, check_too_large), and archived by a release once it is
	// raised. Installation-only: the memory is the server's, which tenants
	// share.
	DeidMemory = "deid.memory"
	// DeidListCacheBytes is the most bytes the compiled text lists of
	// stored profile versions are kept in, their automata and the raw ones
	// the check after builds from them (ParseListCacheBytes): every
	// reception, job and correction of a version shares one copy, the
	// least recently used dropped past it and built again when next
	// needed. Installation-only: the memory is the server's.
	DeidListCacheBytes = "deid.list-cache-bytes"
)

// ReservedPrefix starts the names the system keeps for itself in the
// master's settings store, such as the installation's id. No setting of a
// table may use it, so the settings service, which knows only a table's
// names, answers such a name as one that does not exist: it neither lists,
// reads, sets nor unsets it.
const ReservedPrefix = "system."

// The bounds every setting's name and value are held to, the same as the
// databases hold them (master.ValidateSettingName, master.MaxSettingValueLength),
// restated here because the core imports nothing of NoPACs'.
const (
	MaxNameLength  = 128
	MaxValueLength = 64 << 10
)

// Definition is one setting the server knows.
type Definition struct {
	// Name is 1 to 128 of a-z, 0-9, ".", "_" and "-".
	Name string
	// Default is the built-in default: the value that applies when neither
	// the installation nor a tenant has set one. Validate accepts it.
	Default string
	// TenantOverride reports whether a tenant may hold its own value. A
	// setting without it is installation-only: a tenant scope is refused.
	TenantOverride bool
	// TenantSetByInstallation reports that a tenant's value is set and unset
	// by an admin on the installation only, never by the tenant's own admin:
	// it holds what the installation hands out to tenants, such as the ports
	// a tenant's receivers may use. It needs TenantOverride.
	TenantSetByInstallation bool
	// TenantOnly reports that the setting has no installation value: the
	// installation scope is refused, and what applies to a tenant without a
	// value of its own is the built-in default. It needs TenantOverride. A
	// tenant's own admin sets it unless TenantSetByInstallation says
	// otherwise: it holds what only one tenant's database can name, such as
	// a profile version.
	TenantOnly bool
	// Validate returns an error, whose text is for the person who wrote the
	// value, unless the value is one this setting accepts. Whether an empty
	// value is accepted is its own business.
	Validate func(value string) error
}

// Kind is who may set a setting, and where (settings.proto,
// SettingsService).
type Kind int

// The kinds.
const (
	// Ordinary: the installation's admin sets the installation's value and,
	// when the setting allows a tenant override, the tenant's admin sets the
	// tenant's.
	Ordinary Kind = iota + 1
	// InstallationOnly: an installation value and no tenant override.
	InstallationOnly
	// SetByInstallation: a tenant's value is set by an admin on the
	// installation only.
	SetByInstallation
)

// Kind returns the setting's kind.
func (d Definition) Kind() Kind {
	switch {
	case d.TenantSetByInstallation:
		return SetByInstallation
	case !d.TenantOverride:
		return InstallationOnly
	}
	return Ordinary
}

// Table is a list of definitions, looked up by name. It is not changed once
// made, so it is safe for concurrent use.
type Table struct {
	byName map[string]Definition
	names  []string
}

// Builtin returns the definitions of the settings this version of the server
// knows, a new slice each call. Each value form and its rule is in values.go.
func Builtin() []Definition {
	return append([]Definition{{
		Name:           KeyDefaultLifetime,
		Default:        "90d",
		TenantOverride: false,
		Validate:       func(v string) error { _, err := ParseKeyLifetime(v); return err },
	}, {
		// Empty by default, so no directory storage can be added until the
		// installation's admin names a root (storage.proto, StorageService).
		Name:     StorageDirectoryRoots,
		Default:  "",
		Validate: func(v string) error { _, err := ParseDirectoryRoots(v); return err },
	}, {
		// Empty by default, so no S3 storage can be added until the
		// installation's admin lists an endpoint.
		Name:     StorageS3Endpoints,
		Default:  "",
		Validate: func(v string) error { _, err := ParseS3Endpoints(v); return err },
	}, {
		// Installation-only, so no tenant can lower the reserve that keeps
		// a full disk from failing every tenant's receptions.
		Name:     StorageReserve,
		Default:  "10%,10GiB",
		Validate: func(v string) error { _, err := ParseReserve(v); return err },
	}, {
		// Handed out per tenant, so two tenants' receivers never contend for
		// a port; empty by default, so a tenant has no DIMSE receiver until
		// ports are allotted to it. No installation value: a default port
		// range for every tenant would be one range shared by all.
		Name:                    ReceiverPorts,
		Default:                 "",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		TenantOnly:              true,
		Validate:                func(v string) error { _, err := ParsePorts(v); return err },
	}, {
		// Empty by default: every receiver listens on every address, and
		// none may name one.
		Name:     ReceiverBindAddresses,
		Default:  "",
		Validate: func(v string) error { _, err := ParseBindAddresses(v); return err },
	}, {
		// The installation's value is the default for every tenant; a
		// tenant's own is set by the installation's admin, since the
		// incoming area is the installation's disk.
		Name:                    IncomingLimit,
		Default:                 "0",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseSize(v); return err },
	}, {
		// 64 MiB holds the largest honest headers known: the per-frame
		// functional groups of an enhanced multi-frame object (PS3.3
		// C.7.6.16) at about 4 KB a frame, 40 MiB for 10,000 frames. An
		// installation that receives larger ones raises it without a new
		// release.
		Name:     ReceiverHeaderLimit,
		Default:  "64MiB",
		Validate: func(v string) error { _, err := ParseHeaderLimit(v); return err },
	}, {
		// Off by default: a receiver that archives identified data, and
		// so puts identified and de-identified sessions side by side, is
		// something the installation's admin writes down for a tenant.
		// The installation's value is every tenant's default.
		Name:                    ReceiverAllowAsReceived,
		Default:                 "false",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseBool(v); return err },
	}, {
		// 1 GiB decoded holds a 2,000-frame 512 by 512 CT of 16 bits, or
		// an ultrasound cine of 700 color frames of 800 by 600. Filling
		// is done a frame at a time as the file is written, so memory does
		// not follow it, but the de-identified file is checked whole and a
		// JPEG image's compressed pixels are read whole to remove their
		// segments; an installation that receives larger images raises it
		// without a new release.
		Name:                    DeidPixelLimit,
		Default:                 "1GiB",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParsePixelLimit(v); return err },
	}, {
		// 2 GiB holds the check after of eight files of 256 MiB at once,
		// beside the server's other work, on a machine of 8 GiB; an
		// installation that receives larger files, or many at once, on a
		// larger machine raises it without a new release.
		Name:     DeidMemory,
		Default:  "2GiB",
		Validate: func(v string) error { _, err := ParseDeidMemory(v); return err },
	}, {
		// 256 MiB keeps the lists of several versions at their largest (a
		// few tens of megabytes each) beside the server's other work.
		Name:     DeidListCacheBytes,
		Default:  "256MiB",
		Validate: func(v string) error { _, err := ParseListCacheBytes(v); return err },
	}, {
		// Empty by default, so no page in a browser can read an answer of
		// the API until the installation's admin lists its origin.
		Name:     APICORSOrigins,
		Default:  "",
		Validate: func(v string) error { _, err := ParseOrigins(v); return err },
	}, {
		Name:           EventsRetention,
		Default:        "90d",
		TenantOverride: true,
		Validate:       func(v string) error { _, err := ParseRetention(v); return err },
	}}, append(append(append(append(accessDefinitions(), senderDefinitions()...), auditDefinitions()...), readDefinitions()...),
		append(append(append(append(scrubDefinitions(), importDefinitions()...), correctionDefinitions()...), backupDefinitions()...),
			append(append(append(append(replicaDefinitions(), quotaDefinitions()...), searchDefinitions()...), transferDefinitions()...), append(deidDefinitions(), deidTrialDefinitions()...)...)...)...)...)
}

// New returns a table of defs. It refuses a name that is not valid or given
// twice, a definition without a Validate function, and a built-in default
// that its own Validate refuses: a table that cannot answer every name it
// holds is a mistake of the code, found when the server starts.
func New(defs ...Definition) (*Table, error) {
	t := &Table{byName: make(map[string]Definition, len(defs))}
	for _, d := range defs {
		switch {
		case !validName(d.Name):
			return nil, errors.New("settings: the name " + strconv.Quote(d.Name) + " is not 1 to 128 of a-z, 0-9, ., _ and -")
		case strings.HasPrefix(d.Name, ReservedPrefix):
			return nil, errors.New("settings: the name " + d.Name + " is under " + ReservedPrefix + ", which the system keeps for itself")
		case d.Validate == nil:
			return nil, errors.New("settings: " + d.Name + " has no Validate function")
		case d.TenantSetByInstallation && !d.TenantOverride:
			return nil, errors.New("settings: " + d.Name + " is set on a tenant by the installation, so it takes a tenant's value")
		case d.TenantOnly && !d.TenantOverride:
			return nil, errors.New("settings: " + d.Name + " has no installation value, so it takes a tenant's value")
		case len(d.Default) > MaxValueLength:
			return nil, errors.New("settings: the default of " + d.Name + " is longer than 64 KiB")
		}
		if _, dup := t.byName[d.Name]; dup {
			return nil, errors.New("settings: " + d.Name + " is defined twice")
		}
		if err := d.Validate(d.Default); err != nil {
			return nil, errors.New("settings: the default of " + d.Name + " is refused by its own rule: " + err.Error())
		}
		t.byName[d.Name] = d
		t.names = append(t.names, d.Name)
	}
	slices.Sort(t.names)
	return t, nil
}

// MustBuiltin returns the table of Builtin. It panics if Builtin is not a
// valid table, which its test rules out.
func MustBuiltin() *Table {
	t, err := New(Builtin()...)
	if err != nil {
		panic(err)
	}
	return t
}

// Lookup returns the definition named name, and whether the table holds it.
func (t *Table) Lookup(name string) (Definition, bool) {
	d, ok := t.byName[name]
	return d, ok
}

// Names returns every name of the table in byte order, which is the order
// the databases list settings in. The caller must not change the slice.
func (t *Table) Names() []string { return t.names }

// ParseDuration reads a duration written as a positive whole number followed
// by one unit: "s" for seconds, "m" for minutes, "h" for hours or "d" for
// days of 24 hours: "90d", "2160h", "15m", "30s". It is the one form a
// duration setting takes, so a value reads the same in every place it is
// shown, and it has no fractions, no signs and no mixed units to misread.
// Each setting holds the result to its own bounds.
func ParseDuration(s string) (time.Duration, error) {
	bad := errors.New("is a whole number of seconds, minutes, hours or days, such as 30s, 15m, 2160h or 90d")
	if len(s) < 2 {
		return 0, bad
	}
	var unit time.Duration
	switch s[len(s)-1] {
	case 's':
		unit = time.Second
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return 0, bad
	}
	digits := s[:len(s)-1]
	if strings.Trim(digits, "0123456789") != "" || digits[0] == '0' {
		return 0, bad
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	// Past a hundred thousand years, which no bound here allows, a count
	// would overflow the duration; it is refused rather than wrapped.
	if err != nil || n > int64(time.Duration(1<<62)/unit) {
		return 0, bad
	}
	return time.Duration(n) * unit, nil
}

// validName reports whether name is 1 to 128 of a-z, 0-9, ".", "_" and "-".
func validName(name string) bool {
	if name == "" || len(name) > MaxNameLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '.' && b != '_' && b != '-' {
			return false
		}
	}
	return true
}
