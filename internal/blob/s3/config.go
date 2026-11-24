package s3

import (
	"crypto/x509"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Defaults and bounds. The smallest part S3 accepts, other than the last, is
// 5 MiB, and an upload has at most 10,000 parts
// (docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html).
const (
	// MinPartSize is the smallest PartSize.
	MinPartSize = 5 << 20
	// DefaultPartSize is the part size, and so the largest object sent in
	// one request, when Config.PartSize is 0. With the default number of
	// buffers one Put holds at most 32 MiB.
	DefaultPartSize = 8 << 20
	// DefaultConcurrency is the number of part buffers one Put uses when
	// Config.Concurrency is 0.
	DefaultConcurrency = 4
	// MaxConcurrency is the largest Concurrency.
	MaxConcurrency = 32
	// DefaultStaleUploadAge is how old an unfinished upload must be, when
	// Config.StaleUploadAge is 0, for Open to abort it.
	DefaultStaleUploadAge = 24 * time.Hour

	maxParts = 10000 // int32, the type of a part number.

	// MaxKeyBytes is the longest object name S3 accepts, in bytes
	// (docs.aws.amazon.com/AmazonS3/latest/userguide/object-keys.html), prefix
	// included.
	MaxKeyBytes = 1024
	// MaxPrefixLength is the longest storage prefix, "/" included. A key
	// longer than MaxKeyBytes less the prefix is refused as Invalid before a
	// request is made, since the server would refuse it. The port's longest
	// key (blob.MaxKeyLength) is MaxKeyBytes less this, so every key the port
	// accepts fits under every prefix, and the refusal is a second guard.
	MaxPrefixLength = 128

	// DefaultMemoryLimit is the bytes of part buffers all the Puts of a store
	// share when Config.MemoryLimit is 0: sixteen default parts.
	DefaultMemoryLimit = 16 * (DefaultPartSize + 1)
)

// Timeouts. A call that moves no object bytes gets callTimeout in all, its
// retries included; one request that carries a part or an object gets
// dataTimeout (8 MiB in five minutes is 28 KB/s, below which a link is
// not worth waiting for); a stream from Read is cut when it stalls for
// idleTimeout.
const (
	callTimeout = 30 * time.Second
	dataTimeout = 5 * time.Minute
	idleTimeout = 60 * time.Second
	dialTimeout = 10 * time.Second
)

// Config says which bucket a storage is, how to reach it and what it may
// connect to. The zero values of the optional fields are the defaults.
type Config struct {
	// Endpoint is the server's URL, "https://host[:port]", with no path.
	// Empty means Amazon S3 in Region.
	Endpoint string
	// Region is the signing region; a server that has no regions takes any
	// name, usually "us-east-1".
	Region string
	// Bucket is the bucket's name.
	Bucket string
	// Prefix is where the storage lives in the bucket: empty, or segments
	// of the characters a key may hold, with or without a final "/". Every
	// key is under it and nothing outside it is listed, read or deleted.
	Prefix string
	// PathStyle addresses the bucket in the URL's path ("host/bucket/key")
	// instead of in the host name ("bucket.host/key"). Most servers other
	// than Amazon need it.
	PathStyle bool
	// AccessKeyID and SecretAccessKey are the only credentials the driver
	// uses.
	AccessKeyID     Secret
	SecretAccessKey Secret

	// AllowHTTP permits an Endpoint that is plain http. Without it an http
	// endpoint is refused, since the signed requests and the objects would
	// cross the network in the clear.
	AllowHTTP bool
	// CABundle is PEM certificates trusted in addition to the system's, for
	// a server whose certificate a private authority issued.
	CABundle []byte
	// AllowAddress says whether the driver may connect to an address. It is
	// required, and is called for the address each connection is made to,
	// after the endpoint's name is resolved. AllowPublic is a usual choice.
	AllowAddress func(netip.Addr) bool
	// Resolver resolves the endpoint's name; nil means the system's.
	Resolver *net.Resolver

	// PartSize is the size of one part: 5 MiB or more, 0 for
	// DefaultPartSize. An object of this size or less is sent in one
	// request.
	PartSize int64
	// Concurrency is the number of part buffers, and so of parts in flight,
	// of one Put: 1 to MaxConcurrency, 0 for DefaultConcurrency.
	Concurrency int
	// MemoryLimit bounds the bytes of part buffers that all the Puts of the
	// store hold together; a Put that cannot get its first buffer waits, and
	// the wait ends with its context. It must hold at least one part. 0 means
	// DefaultMemoryLimit.
	MemoryLimit int64
	// StaleUploadAge is the age at which Open aborts an unfinished upload
	// under the prefix. 0 means DefaultStaleUploadAge; a negative value
	// turns the cleanup at Open off, and a later call of AbortStaleUploads
	// then uses DefaultStaleUploadAge.
	StaleUploadAge time.Duration
	// ForReading opens the storage for Read, Stat, List and Usage only: Open
	// lists to check the bucket and the credentials, and neither puts its
	// probe object nor aborts old uploads, so a read costs no write and
	// needs no right to write; Put and Delete are refused.
	ForReading bool
	// KeepVersions leaves the older versions of an object a purge removes
	// on a versioned bucket, so the purge only hides it (the storage's
	// purge_versions off). By default a purge on a versioned bucket
	// deletes every version of the key by its version id.
	KeepVersions bool
}

// String returns the configuration without its credentials. The credentials
// are Secrets, which withhold themselves wherever they are printed; this is
// for readability.
func (c Config) String() string {
	return "s3.Config{bucket " + c.Bucket + ", prefix " + c.Prefix + ", credentials withheld}"
}

// GoString is String, for %#v.
func (c Config) GoString() string { return c.String() }

// bucketName is the shape S3 gives a bucket name
// (docs.aws.amazon.com/AmazonS3/latest/userguide/bucketnamingrules.html).
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// settings is a Config that has been checked and had its defaults filled.
type settings struct {
	endpoint    *url.URL // nil for Amazon S3
	region      string
	bucket      string
	prefix      string // empty, or ends in "/"
	pathStyle   bool
	accessKey   Secret
	secretKey   Secret
	roots       *x509.CertPool
	allow       func(netip.Addr) bool
	resolver    *net.Resolver
	partSize    int64
	concurrency int
	staleAge    time.Duration
	forReading  bool
	keepOlder   bool // Config.KeepVersions
	memory      int64
	// The timeouts, which are the constants above except in tests that need
	// a server to be slow without waiting for a minute.
	callTimeout, dataTimeout, idleTimeout time.Duration
	maxParts                              int32
}

// check validates c and returns the settings it means. Every error is
// Invalid and names a field, never its value, since a value may be a secret.
func (c Config) check() (settings, error) {
	bad := func(msg string) (settings, error) {
		return settings{}, fault.New(fault.Invalid, "storage configuration: "+msg)
	}
	s := settings{
		region: c.Region, bucket: c.Bucket, pathStyle: c.PathStyle,
		accessKey: c.AccessKeyID, secretKey: c.SecretAccessKey,
		allow: c.AllowAddress, resolver: c.Resolver,
		partSize: c.PartSize, concurrency: c.Concurrency, staleAge: c.StaleUploadAge, forReading: c.ForReading, keepOlder: c.KeepVersions,
		callTimeout: callTimeout, dataTimeout: dataTimeout, idleTimeout: idleTimeout, maxParts: maxParts,
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		switch {
		case err != nil || u.Host == "" || u.Hostname() == "":
			return bad("the endpoint is not a URL with a host")
		case u.Scheme != "https" && u.Scheme != "http":
			return bad("the endpoint's scheme must be https or http")
		case u.Scheme == "http" && !c.AllowHTTP:
			return bad("the endpoint is plain http, which is refused unless AllowHTTP is set")
		case u.User != nil:
			return bad("the endpoint must not carry credentials")
		case (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "":
			return bad("the endpoint must be a scheme and a host only")
		}
		s.endpoint = &url.URL{Scheme: u.Scheme, Host: u.Host}
	}
	if c.Region == "" {
		return bad("the region is empty")
	}
	if !bucketName.MatchString(c.Bucket) {
		return bad("the bucket name is not valid")
	}
	if strings.HasSuffix(c.Bucket, "--x-s3") {
		// A directory bucket makes the SDK open a session and sign later
		// requests with credentials the server returns (docs.aws.amazon.com/
		// AmazonS3/latest/userguide/s3-express-create-session.html): a second
		// credential path, which this driver does not have.
		return bad("the bucket is an S3 Express directory bucket, which needs session credentials; only general purpose buckets are used")
	}
	if !c.AccessKeyID.IsSet() || !c.SecretAccessKey.IsSet() {
		return bad("the access key id and the secret access key are both required")
	}
	if c.AllowAddress == nil {
		return bad("AllowAddress is required: a storage connects only where the caller allows")
	}
	if p := strings.TrimSuffix(c.Prefix, "/"); p != "" {
		if err := blob.CheckKey(p); err != nil {
			return bad("the prefix is not valid: " + fault.Message(err))
		}
		if len(p)+1 > MaxPrefixLength {
			return bad("the prefix is longer than MaxPrefixLength")
		}
		s.prefix = p + "/"
	}
	switch {
	case c.PartSize == 0:
		s.partSize = DefaultPartSize
	case c.PartSize < MinPartSize:
		return bad("the part size is below 5 MiB")
	case c.PartSize > 1<<30:
		return bad("the part size is above 1 GiB")
	}
	switch {
	case c.Concurrency == 0:
		s.concurrency = DefaultConcurrency
	case c.Concurrency < 0 || c.Concurrency > MaxConcurrency:
		return bad("the concurrency must be from 1 to 32")
	}
	switch {
	case c.MemoryLimit == 0:
		s.memory = DefaultMemoryLimit
		if s.memory < s.partSize+1 {
			s.memory = s.partSize + 1
		}
	case c.MemoryLimit < s.partSize+1:
		return bad("the memory limit does not hold one part")
	default:
		s.memory = c.MemoryLimit
	}
	if c.StaleUploadAge == 0 {
		s.staleAge = DefaultStaleUploadAge
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if len(c.CABundle) > 0 && !pool.AppendCertsFromPEM(c.CABundle) {
		return bad("the CA bundle holds no certificate")
	}
	s.roots = pool
	return s, nil
}
