package tenant

import (
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/ascii"
	"github.com/amrshadid/nopacs/internal/core/pathname"
)

// StorageType is a storage's type. Its text is what a store keeps.
type StorageType string

// The storage types.
const (
	// StorageDirectory is a directory on the server, under a root the
	// installation names.
	StorageDirectory StorageType = "directory"
	// StorageS3 is a bucket of an object store that speaks the S3 API.
	StorageS3 StorageType = "s3"
)

// The bounds of a storage's configuration, the API contract's.
const (
	// MaxCapacityBytes is the largest capacity a storage may state: 2^62.
	MaxCapacityBytes = 1 << 62
	// MaxDirectoryPathLength bounds a directory storage's path.
	MaxDirectoryPathLength = 1024
	// MaxDirectorySegments bounds the segments of that path.
	MaxDirectorySegments = 16
	// MaxSegmentLength bounds one segment.
	MaxSegmentLength = 128
	// MaxEndpointLength bounds an S3 endpoint.
	MaxEndpointLength = 2048
	// MaxRegionLength bounds an S3 region.
	MaxRegionLength = 64
	// MaxPrefixLength bounds an S3 prefix.
	MaxPrefixLength = 128
	// MaxAccessKeyIDLength bounds an S3 access key id.
	MaxAccessKeyIDLength = 128
	// MaxSealedSecretLength bounds a sealed secret access key: a secret of
	// at most 128 bytes sealed (package secret) is far shorter.
	MaxSealedSecretLength = 1 << 10
)

// Storage is a place a tenant's archived files are written to. Exactly the
// configuration of its Type is meaningful; the other is zero.
type Storage struct {
	// Name is the storage's name, under the rule of a tenant's name.
	Name string
	// Type is its type.
	Type StorageType
	// Directory is a directory storage's configuration.
	Directory DirectoryStorage
	// S3 is an S3 storage's configuration.
	S3 S3Storage
	// CapacityBytes is the most bytes it may hold, 0 for no limit of its
	// own.
	CapacityBytes int64
	// Created is when it was added; Updated when it last changed. Set by
	// the store from the time a change is given.
	Created, Updated time.Time
}

// DirectoryStorage is where a directory storage is: a root the installation
// names, and a path below the tenant's directory in it.
type DirectoryStorage struct {
	// Root is the name of the root, under the rule of a tenant's name.
	Root string
	// Path is segments joined by "/", empty for the tenant's directory.
	Path string
}

// S3Storage is where an S3 storage is and the credentials it uses.
type S3Storage struct {
	// Endpoint is the store's base URL, as the caller normalized it.
	Endpoint string
	// Region is the bucket's region.
	Region string
	// Bucket is the bucket's name.
	Bucket string
	// Prefix is where every key of the storage starts, empty or ending
	// with "/".
	Prefix string
	// PathStyle says the bucket is addressed in the URL's path.
	PathStyle bool
	// AccessKeyID names the credentials; it is not a secret.
	AccessKeyID string
	// SealedSecret is the secret access key sealed by the caller (package
	// secret), given to a change only; a store never returns it.
	SealedSecret []byte
	// SecretSet says whether a secret is held. Answered by a store; ignored
	// in a change.
	SecretSet bool
	// KeepVersions leaves, on a versioned bucket, the older versions of an
	// object a purge removes, so the bytes are hidden behind a delete
	// marker rather than gone: the contract's purge_versions off. False,
	// the zero value, is the contract's default (purge_versions absent
	// means true), so a storage made without saying deletes them. It says
	// nothing about where the files are, so it may change at any time: it
	// changes only what later purges do.
	KeepVersions bool
}

// StorageField names one field of a storage a partial change sets, as the
// API contract's update mask names it.
type StorageField string

// The fields of a storage a change can name.
const (
	FieldCapacityBytes   StorageField = "capacity_bytes"
	FieldDirectoryRoot   StorageField = "directory.root"
	FieldDirectoryPath   StorageField = "directory.path"
	FieldS3Endpoint      StorageField = "s3.endpoint"
	FieldS3Region        StorageField = "s3.region"
	FieldS3Bucket        StorageField = "s3.bucket"
	FieldS3Prefix        StorageField = "s3.prefix"
	FieldS3PathStyle     StorageField = "s3.path_style"
	FieldS3AccessKeyID   StorageField = "s3.access_key_id"
	FieldS3SecretKey     StorageField = "s3.secret_access_key"
	FieldS3PurgeVersions StorageField = "s3.purge_versions"
	maxStorageFieldCount              = 11
)

// storageFields says, for each field, the type it belongs to (empty for
// both) and whether it says where the storage's files are.
var storageFields = map[StorageField]struct {
	typ      StorageType
	location bool
}{
	FieldCapacityBytes: {"", false},
	FieldDirectoryRoot: {StorageDirectory, true},
	FieldDirectoryPath: {StorageDirectory, true},
	FieldS3Endpoint:    {StorageS3, true},
	FieldS3Region:      {StorageS3, false},
	FieldS3Bucket:      {StorageS3, true},
	FieldS3Prefix:      {StorageS3, true},
	FieldS3PathStyle:   {StorageS3, false},
	FieldS3AccessKeyID: {StorageS3, false},
	FieldS3SecretKey:   {StorageS3, false},
	// What a purge does with older versions, not where the files are.
	FieldS3PurgeVersions: {StorageS3, false},
}

// Validate returns an Invalid error unless s can be stored as a new
// storage: every field by the contract's rule, and an S3 storage with a
// secret.
func (s Storage) Validate() error {
	if err := s.validate(); err != nil {
		return err
	}
	if s.Type == StorageS3 && len(s.S3.SealedSecret) == 0 {
		return invalid("an S3 storage is added with its secret access key")
	}
	return nil
}

// validate checks every field but the presence of a secret, which a stored
// storage holds and a change need not repeat.
func (s Storage) validate() error {
	if err := ValidateName("storage", s.Name); err != nil {
		return err
	}
	if s.CapacityBytes < 0 || s.CapacityBytes > MaxCapacityBytes {
		return invalid("a storage's capacity is 0 to 2^62 bytes")
	}
	switch s.Type {
	case StorageDirectory:
		if !s.S3.isZero() {
			return invalid("a directory storage has no S3 configuration")
		}
		if err := ValidateName("root", s.Directory.Root); err != nil {
			return err
		}
		return validateDirectoryPath(s.Directory.Path)
	case StorageS3:
		if s.Directory != (DirectoryStorage{}) {
			return invalid("an S3 storage has no directory configuration")
		}
		return s.S3.validate()
	}
	return invalid("a storage's type is directory or s3")
}

// isZero reports whether c holds nothing; S3Storage holds a slice, so it is
// not comparable with ==.
func (c S3Storage) isZero() bool {
	return c.Endpoint == "" && c.Region == "" && c.Bucket == "" && c.Prefix == "" && !c.PathStyle &&
		c.AccessKeyID == "" && len(c.SealedSecret) == 0 && !c.SecretSet && !c.KeepVersions
}

func (c S3Storage) validate() error {
	if _, err := NormalizeEndpoint(c.Endpoint); err != nil {
		return err
	}
	if c.Region == "" || len(c.Region) > MaxRegionLength ||
		!allBytes(c.Region, func(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' }) {
		return invalid("an S3 region is 1 to 64 of a-z, 0-9 and -")
	}
	if err := validateBucket(c.Bucket); err != nil {
		return err
	}
	if err := validatePrefix(c.Prefix); err != nil {
		return err
	}
	if c.AccessKeyID == "" || len(c.AccessKeyID) > MaxAccessKeyIDLength ||
		!ascii.Printable(c.AccessKeyID) {
		return invalid("an S3 access key id is 1 to 128 bytes of printable ASCII")
	}
	if len(c.SealedSecret) > MaxSealedSecretLength {
		return invalid("a sealed secret access key is at most " + strconv.Itoa(MaxSealedSecretLength) + " bytes")
	}
	return nil
}

// NormalizeEndpoint returns an S3 endpoint in the one spelling a store keeps
// and compares, so the overlap rule cannot be passed by spelling one endpoint
// two ways: the scheme in lowercase, the host in its one form, the port as a
// number without leading zeros and dropped when it is the scheme's default
// (443 for https, 80 for http), and no path. The host is one of two things:
//
//   - an IP literal netip.ParseAddr accepts, kept in its canonical text
//     (IPv6 as RFC 5952 writes it, in brackets); a zone and an IPv4-mapped
//     IPv6 address are refused, so each address has one spelling;
//   - a DNS name of ASCII letters, digits and hyphens, in labels of 1 to 63
//     that neither start nor end with a hyphen, at most 253 in all, in
//     lowercase, a final dot dropped, whose last label is not all digits
//     (so a number such as 3232235777 or 0xc0.0xa8.1.1 or 192.168.001.001
//     is never read as a name standing for an address).
//
// A percent sign anywhere is refused, so no encoded byte becomes part of a
// host. An endpoint that is not an http or https URL of at most 2048 bytes of
// printable ASCII, or that carries user information, a path other than "/",
// a query or a fragment, is Invalid.
func NormalizeEndpoint(endpoint string) (string, error) {
	const rule = "an S3 endpoint is an http or https URL of at most 2048 bytes, with a host name or an IP address, an optional port, and no user information, path, query or fragment"
	if endpoint == "" || len(endpoint) > MaxEndpointLength || strings.Contains(endpoint, "%") ||
		!ascii.Printable(endpoint) {
		return "", invalid(rule)
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || u.Host == "" {
		return "", invalid(rule)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", invalid(rule)
	}
	host, ok := canonicalHost(u.Host, u.Hostname())
	if !ok {
		return "", invalid(rule)
	}
	if p := u.Port(); p != "" {
		if !allBytes(p, func(b byte) bool { return b >= '0' && b <= '9' }) || len(p) > 5 {
			return "", invalid(rule)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", invalid(rule)
		}
		defaultPort := (scheme == "https" && n == 443) || (scheme == "http" && n == 80)
		if !defaultPort {
			host += ":" + strconv.Itoa(n)
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", invalid(rule)
	}
	return scheme + "://" + host, nil
}

// canonicalHost returns the host of an endpoint in its one form, or false.
// raw is the URL's host part as written (with brackets and port), name the
// host without them.
func canonicalHost(raw, name string) (string, bool) {
	if strings.HasPrefix(raw, "[") {
		a, err := netip.ParseAddr(name)
		if err != nil || !a.Is6() || a.Zone() != "" || a.Is4In6() {
			return "", false
		}
		return "[" + a.String() + "]", true
	}
	if a, err := netip.ParseAddr(name); err == nil {
		if !a.Is4() {
			return "", false
		}
		return a.String(), true
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if name == "" || len(name) > 253 {
		return "", false
	}
	labels := strings.Split(name, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' ||
			!allBytes(l, func(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' }) {
			return "", false
		}
	}
	if last := labels[len(labels)-1]; allBytes(last, func(b byte) bool { return b >= '0' && b <= '9' }) {
		return "", false
	}
	return name, true
}

// Normalize returns s with its S3 endpoint in the spelling a store keeps
// (NormalizeEndpoint), after Validate: a driver stores and compares what it
// returns.
func (s Storage) Normalize() (Storage, error) {
	if err := s.Validate(); err != nil {
		return s, err
	}
	return s.normalized()
}

// normalized rewrites the endpoint of an S3 storage, for a storage already
// checked.
func (s Storage) normalized() (Storage, error) {
	if s.Type == StorageS3 {
		e, err := NormalizeEndpoint(s.S3.Endpoint)
		if err != nil {
			return s, err
		}
		s.S3.Endpoint = e
	}
	return s, nil
}

// validateDirectoryPath applies the contract's rule of a directory storage's
// path: at most 16 segments and 1024 bytes, each 1 to 128 of letters,
// digits, ".", "_" and "-", not starting with a dot; empty for the tenant's
// directory. A segment may not end in "." either, nor be a device name
// Windows reserves, with or without an extension (pathname.DeviceName), on
// every system: Windows drops a segment's final dots, so "mri." would be
// the directory of the storage at "mri" there, and opens the device for a
// name such as "nul.txt", so a storage there could never be made; a data
// directory moved to Windows must still hold what it held.
func validateDirectoryPath(p string) error {
	if p == "" {
		return nil
	}
	const rule = "a directory storage's path is at most 16 segments and 1024 bytes, each 1 to 128 of A-Z, a-z, 0-9, ., _ and -, not starting or ending with a dot, and not a device name Windows reserves (con, prn, aux, nul, com0 to com9, lpt0 to lpt9, with or without an extension)"
	segs := strings.Split(p, "/")
	if len(p) > MaxDirectoryPathLength || len(segs) > MaxDirectorySegments {
		return invalid(rule)
	}
	for _, seg := range segs {
		if seg == "" || len(seg) > MaxSegmentLength || seg[0] == '.' || !allBytes(seg, pathByte) ||
			pathname.TrailingDotOrSpace(seg) || pathname.DeviceName(seg) {
			return invalid(rule)
		}
	}
	return nil
}

// validateBucket applies the S3 API's rule of a bucket's name, as the
// contract states it.
func validateBucket(b string) error {
	ok := len(b) >= 3 && len(b) <= 63 && allBytes(b, func(c byte) bool {
		return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-'
	})
	if ok {
		first, last := b[0], b[len(b)-1]
		ok = first != '.' && first != '-' && last != '.' && last != '-'
	}
	if !ok {
		return invalid("an S3 bucket is 3 to 63 of a-z, 0-9, . and -, starting and ending with a letter or a digit")
	}
	return nil
}

// validatePrefix applies the contract's rule of an S3 prefix.
func validatePrefix(p string) error {
	if p == "" {
		return nil
	}
	const rule = "an S3 prefix is at most 128 of A-Z, a-z, 0-9, ., _, - and /, ending with / and with no empty, . or .. part"
	if len(p) > MaxPrefixLength || p[len(p)-1] != '/' || !allBytes(p, func(b byte) bool { return pathByte(b) || b == '/' }) {
		return invalid(rule)
	}
	for _, part := range strings.Split(p[:len(p)-1], "/") {
		if part == "" || part == "." || part == ".." {
			return invalid(rule)
		}
	}
	return nil
}

// ApplyStorage returns stored with the fields named set to their values in
// change, after checking the fields against the storage's type and the
// contract's rules of a partial change, and whether a field that says where
// the files are was named. A driver calls it inside its transaction with
// the row it read, so every driver applies a change the same way.
func ApplyStorage(stored, change Storage, fields []StorageField) (Storage, bool, error) {
	if len(fields) == 0 {
		return stored, false, invalid("a change of a storage names at least one field")
	}
	if len(fields) > maxStorageFieldCount {
		return stored, false, invalid("a change of a storage names each field once")
	}
	out := stored
	out.S3.SealedSecret = nil
	location := false
	for _, f := range fields {
		info, ok := storageFields[f]
		if !ok {
			return stored, false, invalid("a change of a storage names a field it does not have: " + strconv.Quote(string(f)))
		}
		if info.typ != "" && info.typ != stored.Type {
			return stored, false, invalid("a storage's type cannot change: " + string(f) + " is not a field of a " + string(stored.Type) + " storage")
		}
		location = location || info.location
		switch f {
		case FieldCapacityBytes:
			out.CapacityBytes = change.CapacityBytes
		case FieldDirectoryRoot:
			out.Directory.Root = change.Directory.Root
		case FieldDirectoryPath:
			out.Directory.Path = change.Directory.Path
		case FieldS3Endpoint:
			out.S3.Endpoint = change.S3.Endpoint
		case FieldS3Region:
			out.S3.Region = change.S3.Region
		case FieldS3Bucket:
			out.S3.Bucket = change.S3.Bucket
		case FieldS3Prefix:
			out.S3.Prefix = change.S3.Prefix
		case FieldS3PathStyle:
			out.S3.PathStyle = change.S3.PathStyle
		case FieldS3AccessKeyID:
			out.S3.AccessKeyID = change.S3.AccessKeyID
		case FieldS3SecretKey:
			out.S3.SealedSecret = change.S3.SealedSecret
		case FieldS3PurgeVersions:
			out.S3.KeepVersions = change.S3.KeepVersions
		}
	}
	if slices.Contains(fields, FieldS3AccessKeyID) != slices.Contains(fields, FieldS3SecretKey) {
		return stored, false, invalid("an S3 storage's access key id and secret access key change together")
	}
	if slices.Contains(fields, FieldS3SecretKey) && len(change.S3.SealedSecret) == 0 {
		return stored, false, invalid("a secret access key that is changed is not empty")
	}
	if err := out.validate(); err != nil {
		return stored, false, err
	}
	out, err := out.normalized()
	if err != nil {
		return stored, false, err
	}
	return out, location, nil
}

// StoragesOverlap reports whether two storages of one tenant would write
// into each other's files: two directory storages under one root whose
// paths are equal or one lies inside the other, segment by segment with
// case folded (pathname.Nest: a file system that ignores case makes two
// spellings one directory), or two S3 storages on one endpoint and bucket of
// which one prefix starts with the other.
func StoragesOverlap(a, b Storage) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case StorageDirectory:
		if a.Directory.Root != b.Directory.Root {
			return false
		}
		return pathname.Nest(DirectorySegments(a.Directory.Path), DirectorySegments(b.Directory.Path))
	case StorageS3:
		if a.S3.Endpoint != b.S3.Endpoint || a.S3.Bucket != b.S3.Bucket {
			return false
		}
		return strings.HasPrefix(a.S3.Prefix, b.S3.Prefix) || strings.HasPrefix(b.S3.Prefix, a.S3.Prefix)
	}
	return false
}

// DirectorySegments returns the segments of a directory storage's path,
// none for the tenant's directory itself.
func DirectorySegments(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
