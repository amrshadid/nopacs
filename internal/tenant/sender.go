package tenant

import (
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// SenderType is a sender's protocol, the contract's oneof member in
// lowercase. Its text is what a store keeps.
type SenderType string

// The sender types.
const (
	// SenderDIMSE sends each instance by C-STORE over an association it
	// opens with the peer (PS3.4 Annex B, PS3.8).
	SenderDIMSE SenderType = "dimse"
	// SenderSTOW stores instances into the peer's DICOMweb service
	// (STOW-RS, PS3.18 10.5).
	SenderSTOW SenderType = "stow"
)

// StowAuthentication is how a stow sender's requests prove who sends them,
// the contract's StowAuthentication in lowercase without its prefix.
type StowAuthentication string

// The kinds of authentication.
const (
	StowAuthNone   StowAuthentication = "none"
	StowAuthBasic  StowAuthentication = "basic"
	StowAuthBearer StowAuthentication = "bearer"
	StowAuthHeader StowAuthentication = "header"
)

// The bounds and defaults of a sender, the contract's.
const (
	// MaxSenderNameLength is the longest sender name: the rule of a
	// tenant's name, six characters shorter, since the sender's units of
	// work are queued under the kind "send-<name>" (SendKind), which is
	// at most 63 characters.
	MaxSenderNameLength = 57
	// MinSenderAssociations and MaxSenderAssociations bound a sender's own
	// max_associations; DefaultSenderAssociations is what 0 means.
	MinSenderAssociations     = 1
	MaxSenderAssociations     = 16
	DefaultSenderAssociations = 2
	// MinSenderByteRate and MaxSenderByteRate bound a sender's byte rate
	// when it has one; 0 is none.
	MinSenderByteRate = 1 << 16
	MaxSenderByteRate = 1 << 40
	// The bounds and defaults of a sender's timeouts.
	MinConnectTimeout      = time.Second
	MaxConnectTimeout      = 5 * time.Minute
	DefaultConnectTimeout  = 30 * time.Second
	MinResponseTimeout     = 5 * time.Second
	MaxResponseTimeout     = time.Hour
	DefaultResponseTimeout = 5 * time.Minute
	// The bounds and defaults of a stow sender's requests.
	MinStowInstances     = 1
	MaxStowInstances     = 1000
	DefaultStowInstances = 64
	MinStowRequestBytes  = 1 << 20
	MaxStowRequestBytes  = 1 << 36
	DefaultStowRequest   = 1 << 28
	// MaxSenderHostLength is the longest DNS name a sender's host is (RFC
	// 1123); MaxSenderURLLength the longest stow URL.
	MaxSenderHostLength = 253
	MaxSenderURLLength  = 2048
	// MaxStowUsernameLength and MaxStowHeaderNameLength bound what names a
	// stow sender's credential.
	MaxStowUsernameLength   = 256
	MaxStowHeaderNameLength = 64
	// MaxSealedCredentialLength bounds a sender's sealed credential: a
	// credential is at most 4096 bytes before it is sealed, and sealing
	// adds a few dozen.
	MaxSealedCredentialLength = 8 << 10
)

// SendKind is the kind of the units of work of the jobs that send to the
// sender of that name: a worker claims the units of one sender at a time,
// so a sender that is down or disabled holds its own jobs and no other's.
func SendKind(sender string) string { return "send-" + sender }

// Sender is one destination a tenant's archived data may be sent to, and
// how to reach it.
type Sender struct {
	// Name names it in its tenant (ValidateSenderName).
	Name string
	// Type is its protocol; the configuration of the other type is zero.
	Type SenderType
	// DIMSE is a DIMSE sender's configuration.
	DIMSE DIMSESender
	// STOW is a stow sender's configuration.
	STOW STOWSender
	// Enabled says whether it sends; a disabled sender's jobs wait.
	Enabled bool
	// AllowIdentified says it may also send instances SendRefusal refuses
	// by default: archived as received, without profile provenance, or with
	// an accepted pixel risk. Only the installation's admin sets or clears
	// it; false by default.
	AllowIdentified bool
	// MaxAssociations is how many associations or requests it holds open
	// at once.
	MaxAssociations int
	// MaxBytesPerSecond bounds its rate; 0 for no bound of its own.
	MaxBytesPerSecond int64
	// ConnectTimeout bounds a connection, a TLS handshake and an
	// association request.
	ConnectTimeout time.Duration
	// ResponseTimeout bounds the wait for one answer of the peer.
	ResponseTimeout time.Duration
	// Created and Updated are when it was added and last changed.
	Created, Updated time.Time
}

// DIMSESender is a DICOM peer reached by C-STORE.
type DIMSESender struct {
	// Host is the peer's DNS name or address.
	Host string
	// Port is the peer's TCP port.
	Port int
	// CalledAETitle is the peer's AE title; CallingAETitle the one the
	// server calls with (PS3.8 9.3.2).
	CalledAETitle, CallingAETitle string
	// TLS is how the peer is verified; nil for plain TCP.
	TLS *SenderTLS
}

// STOWSender is a DICOMweb service reached by STOW-RS.
type STOWSender struct {
	// URL is the base URL of the service; requests go to <url>/studies.
	URL string
	// Authentication is how a request proves who sends it.
	Authentication StowAuthentication
	// Username is basic authentication's user name; empty otherwise.
	Username string
	// HeaderName is the header field that carries the credential with
	// header authentication; empty otherwise.
	HeaderName string
	// SealedCredential is the credential sealed by the caller with the
	// installation key, given to an add or a change; a read never answers
	// it (CredentialSet says whether one is held).
	SealedCredential []byte
	// CredentialSet says whether the store holds a credential. Answered by
	// a store.
	CredentialSet bool
	// TLS is how an https peer is verified; nil for the system roots.
	TLS *SenderTLS
	// MaxInstancesPerRequest and MaxRequestBytes bound one request.
	MaxInstancesPerRequest int
	MaxRequestBytes        int64
}

// SenderTLS is how a sender verifies its peer and the certificate it shows,
// each a file name in the tenant's TLS directory.
type SenderTLS struct {
	// CAFile holds the authorities the peer's certificate chains to.
	CAFile string
	// CertificateFile and PrivateKeyFile are the server's own certificate
	// chain and key, both or neither.
	CertificateFile, PrivateKeyFile string
	// ServerName is the name the peer's certificate is issued for; empty
	// for the sender's host.
	ServerName string
}

// SenderField names one field of a sender a partial change sets, as the
// API contract's update mask names it.
type SenderField string

// The fields of a sender a change can name.
const (
	FieldSenderEnabled         SenderField = "enabled"
	FieldSenderAllowIdentified SenderField = "allow_identified"
	FieldSenderAssociations    SenderField = "max_associations"
	FieldSenderByteRate        SenderField = "max_bytes_per_second"
	FieldSenderConnectTimeout  SenderField = "connect_timeout"
	FieldSenderResponseTimeout SenderField = "response_timeout"
	FieldDIMSEHost             SenderField = "dimse.host"
	FieldDIMSEPeerPort         SenderField = "dimse.port"
	FieldDIMSECalledAETitle    SenderField = "dimse.called_ae_title"
	FieldDIMSECallingAETitle   SenderField = "dimse.calling_ae_title"
	FieldDIMSESenderTLS        SenderField = "dimse.tls"
	FieldDIMSETLSCAFile        SenderField = "dimse.tls.ca_file"
	FieldDIMSETLSCertFile      SenderField = "dimse.tls.certificate_file"
	FieldDIMSETLSKeyFile       SenderField = "dimse.tls.private_key_file"
	FieldDIMSETLSServerName    SenderField = "dimse.tls.server_name"
	FieldSTOWURL               SenderField = "stow.url"
	FieldSTOWAuthentication    SenderField = "stow.authentication"
	FieldSTOWUsername          SenderField = "stow.username"
	FieldSTOWHeaderName        SenderField = "stow.header_name"
	FieldSTOWCredential        SenderField = "stow.credential" //nolint:gosec // G101: the name of a mask's path, not a credential.
	FieldSTOWTLS               SenderField = "stow.tls"
	FieldSTOWTLSCAFile         SenderField = "stow.tls.ca_file"
	FieldSTOWTLSCertFile       SenderField = "stow.tls.certificate_file"
	FieldSTOWTLSKeyFile        SenderField = "stow.tls.private_key_file"
	FieldSTOWTLSServerName     SenderField = "stow.tls.server_name"
	FieldSTOWMaxInstances      SenderField = "stow.max_instances_per_request"
	FieldSTOWMaxRequestBytes   SenderField = "stow.max_request_bytes"
	maxSenderFieldCount                    = 27
)

// destinationFields are the fields whose change moves where a sender
// connects, so its health is unknown until its next connection.
var destinationFields = []SenderField{FieldDIMSEHost, FieldDIMSEPeerPort, FieldDIMSESenderTLS, FieldDIMSETLSCAFile,
	FieldDIMSETLSCertFile, FieldDIMSETLSKeyFile, FieldDIMSETLSServerName, FieldSTOWURL, FieldSTOWTLS, FieldSTOWTLSCAFile,
	FieldSTOWTLSCertFile, FieldSTOWTLSKeyFile, FieldSTOWTLSServerName}

// RedirectsSender reports whether a change of field moves where a sender's
// bytes go, or to which application or account they are addressed: its
// host, port, AE titles, URL, TLS settings, or a stow sender's account
// (authentication, user name, header name, credential), since on a
// DICOMweb peer with several accounts the account decides where data
// lands, as the called AE title does on a DICOM peer. On a sender marked
// AllowIdentified such a change is the installation's admin's to make, as
// the mark is, so a tenant's admin cannot point a marked sender somewhere
// else.
func RedirectsSender(field SenderField) bool {
	return slices.Contains(destinationFields, field) || slices.Contains(accountFields, field)
}

// accountFields are the fields that say to which application or account a
// sender's bytes are addressed at its destination.
var accountFields = []SenderField{FieldDIMSECalledAETitle, FieldDIMSECallingAETitle, FieldSTOWAuthentication,
	FieldSTOWUsername, FieldSTOWHeaderName, FieldSTOWCredential}

// ValidateSenderName returns an Invalid error unless name follows the rule
// of a tenant's name and is at most MaxSenderNameLength characters.
func ValidateSenderName(name string) error {
	if err := ValidateName("sender", name); err != nil {
		return err
	}
	if len(name) > MaxSenderNameLength {
		return invalid("a sender's name is at most 57 characters, since its work is named send-<name>")
	}
	return nil
}

// Normalize returns s as a store keeps it: each zero bound given its
// default (the contract's "zero in AddSender means the default"), AE
// titles without their leading and trailing spaces, which are not
// significant (PS3.5 6.2), and a host name in lowercase. It does not
// validate.
func (s Sender) Normalize() Sender {
	if s.MaxAssociations == 0 {
		s.MaxAssociations = DefaultSenderAssociations
	}
	if s.ConnectTimeout == 0 {
		s.ConnectTimeout = DefaultConnectTimeout
	}
	if s.ResponseTimeout == 0 {
		s.ResponseTimeout = DefaultResponseTimeout
	}
	switch s.Type {
	case SenderDIMSE:
		s.DIMSE.CalledAETitle = strings.Trim(s.DIMSE.CalledAETitle, " ")
		s.DIMSE.CallingAETitle = strings.Trim(s.DIMSE.CallingAETitle, " ")
		s.DIMSE.Host = strings.ToLower(s.DIMSE.Host)
	case SenderSTOW:
		if s.STOW.MaxInstancesPerRequest == 0 {
			s.STOW.MaxInstancesPerRequest = DefaultStowInstances
		}
		if s.STOW.MaxRequestBytes == 0 {
			s.STOW.MaxRequestBytes = DefaultStowRequest
		}
	}
	return s
}

// Destination returns where s sends, as an audit entry records it: a DIMSE
// peer's host and port, a stow peer's URL host with its port when the URL
// names one. Never a credential: a stow URL holds no user information
// (Validate), and only its host is taken. A host and port longer than an
// audit detail holds (master.MaxAuditDetailValueLength) is given as the
// host alone, which a valid host always fits.
func (s Sender) Destination() string {
	host, port := s.DIMSE.Host, strconv.Itoa(s.DIMSE.Port)
	if s.Type == SenderSTOW {
		u, err := url.Parse(s.STOW.URL)
		if err != nil {
			return ""
		}
		host, port = u.Hostname(), u.Port()
		if n, err := strconv.Atoi(port); port != "" && (err != nil || n < 1 || n > 65535) {
			return ""
		}
	}
	if port == "" {
		return host
	}
	if out := net.JoinHostPort(host, port); len(out) <= master.MaxAuditDetailValueLength {
		return out
	}
	return host
}

// Validate returns an Invalid error unless s can be stored, by the
// contract's rule of each field. A credential is checked only as sealed
// bytes: its plain rules are the caller's, before it seals it. Whether the
// installation allows its destination is the caller's to check too, since
// the installation's settings are not in a tenant's database.
func (s Sender) Validate() error {
	if err := ValidateSenderName(s.Name); err != nil {
		return err
	}
	if s.MaxAssociations < MinSenderAssociations || s.MaxAssociations > MaxSenderAssociations {
		return invalid("a sender holds 1 to 16 associations at once")
	}
	if s.MaxBytesPerSecond != 0 && (s.MaxBytesPerSecond < MinSenderByteRate || s.MaxBytesPerSecond > MaxSenderByteRate) {
		return invalid("a sender's byte rate is 65536 to 2^40 bytes a second, or 0 for none")
	}
	if s.ConnectTimeout < MinConnectTimeout || s.ConnectTimeout > MaxConnectTimeout || s.ConnectTimeout%time.Second != 0 {
		return invalid("a connect timeout is a whole number of seconds from 1 second to 5 minutes")
	}
	if s.ResponseTimeout < MinResponseTimeout || s.ResponseTimeout > MaxResponseTimeout || s.ResponseTimeout%time.Second != 0 {
		return invalid("a response timeout is a whole number of seconds from 5 seconds to 1 hour")
	}
	switch s.Type {
	case SenderDIMSE:
		if !s.STOW.isZero() {
			return invalid("a dimse sender has no stow configuration")
		}
		return s.DIMSE.validate()
	case SenderSTOW:
		if s.DIMSE != (DIMSESender{}) {
			return invalid("a stow sender has no dimse configuration")
		}
		return s.STOW.validate()
	}
	return invalid("a sender's type is dimse or stow")
}

func (d DIMSESender) validate() error {
	if err := validateHost(d.Host); err != nil {
		return err
	}
	if d.Port < 1 || d.Port > 65535 {
		return invalid("a sender's port is 1 to 65535")
	}
	if err := ValidateAETitle(d.CalledAETitle); err != nil {
		return err
	}
	if err := ValidateAETitle(d.CallingAETitle); err != nil {
		return err
	}
	if d.TLS != nil {
		if d.TLS.CAFile == "" {
			return invalid("a dimse sender with TLS names its CA file")
		}
		return d.TLS.validate()
	}
	return nil
}

func (s STOWSender) isZero() bool {
	return s.URL == "" && s.Authentication == "" && s.Username == "" && s.HeaderName == "" &&
		len(s.SealedCredential) == 0 && !s.CredentialSet && s.TLS == nil && s.MaxInstancesPerRequest == 0 &&
		s.MaxRequestBytes == 0
}

// forbiddenHeaders are the header fields a stow sender's credential may not
// be carried in: those the server writes itself or that frame the request.
var forbiddenHeaders = []string{"authorization", "host", "content-type", "content-length", "transfer-encoding",
	"connection", "accept", "cookie"}

func (s STOWSender) validate() error {
	u, err := url.Parse(s.URL)
	if err != nil || s.URL == "" || len(s.URL) > MaxSenderURLLength || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Host == "" || u.Opaque != "" {
		return invalid("a stow sender's URL is an http or https URL of at most 2048 bytes")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s.URL, "#") {
		return invalid("a stow sender's URL has no user information, query or fragment")
	}
	if err := validateHost(u.Hostname()); err != nil {
		return err
	}
	// A port that does not parse would leave a caller checking the scheme's
	// default port against the destinations instead of the one written.
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return invalid("a stow sender's URL port is 1 to 65535")
		}
	}
	if u.Scheme == "http" && s.TLS != nil {
		return invalid("a stow sender with an http URL has no TLS settings")
	}
	switch s.Authentication {
	case StowAuthNone:
		if len(s.SealedCredential) > 0 || s.CredentialSet {
			return invalid("a stow sender without authentication has no credential")
		}
	case StowAuthBasic, StowAuthBearer, StowAuthHeader:
		if len(s.SealedCredential) == 0 && !s.CredentialSet {
			return invalid("a stow sender with authentication has a credential")
		}
	default:
		return invalid("a stow sender's authentication is none, basic, bearer or header")
	}
	if len(s.SealedCredential) > MaxSealedCredentialLength {
		return invalid("a sealed credential is at most " + strconv.Itoa(MaxSealedCredentialLength) + " bytes")
	}
	if (s.Authentication == StowAuthBasic) != (s.Username != "") {
		return invalid("a user name is given with basic authentication, and only with it")
	}
	if s.Username != "" && (len(s.Username) > MaxStowUsernameLength ||
		!allBytes(s.Username, func(b byte) bool { return b > 0x20 && b < 0x7f && b != ':' })) {
		return invalid("a user name is 1 to 256 bytes of printable ASCII without a colon or space")
	}
	if (s.Authentication == StowAuthHeader) != (s.HeaderName != "") {
		return invalid("a header name is given with header authentication, and only with it")
	}
	if s.HeaderName != "" {
		lower := strings.ToLower(s.HeaderName)
		if len(s.HeaderName) > MaxStowHeaderNameLength || !allBytes(s.HeaderName, tokenByte) ||
			slices.Contains(forbiddenHeaders, lower) || strings.HasPrefix(lower, "proxy-") {
			return invalid("a header name is 1 to 64 characters of a field name's token (RFC 9110 5.1), not one the server writes or that frames the request")
		}
	}
	if s.TLS != nil {
		if err := s.TLS.validate(); err != nil {
			return err
		}
	}
	if s.MaxInstancesPerRequest < MinStowInstances || s.MaxInstancesPerRequest > MaxStowInstances {
		return invalid("a stow request holds 1 to 1000 instances")
	}
	if s.MaxRequestBytes < MinStowRequestBytes || s.MaxRequestBytes > MaxStowRequestBytes {
		return invalid("a stow request is 1 MiB to 64 GiB")
	}
	return nil
}

func (t SenderTLS) validate() error {
	for _, f := range []string{t.CAFile, t.CertificateFile, t.PrivateKeyFile} {
		if f != "" && (len(f) > MaxTLSFileLength || f[0] == '.' || !allBytes(f, pathByte)) {
			return invalid("a TLS file's name is 1 to 255 of A-Z, a-z, 0-9, ., _ and -, not starting with a dot")
		}
	}
	if (t.CertificateFile == "") != (t.PrivateKeyFile == "") {
		return invalid("a sender's certificate file and private key file are given together or not at all")
	}
	if t.ServerName != "" && !dnsName(t.ServerName) {
		return invalid("a TLS server name is a DNS name of at most 253 bytes")
	}
	return nil
}

// validateHost returns an Invalid error unless h is a DNS name (RFC 1123)
// or an address: IPv4 in dotted form, IPv6 in the form of RFC 5952, with
// no zone or brackets.
func validateHost(h string) error {
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" || a.String() != h {
			return invalid("a sender's host address is IPv4 in dotted form or IPv6 in the form of RFC 5952, without a zone")
		}
		return nil
	}
	if !dnsName(h) {
		return invalid("a sender's host is a DNS name of 1 to 253 bytes or an IP address")
	}
	return nil
}

// dnsName reports whether s is a DNS name of RFC 1123: labels of 1 to 63
// letters, digits and hyphens, not starting or ending with a hyphen, at
// most 253 bytes in all.
func dnsName(s string) bool {
	if s == "" || len(s) > MaxSenderHostLength {
		return false
	}
	for l := range strings.SplitSeq(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' ||
			!allBytes(l, func(b byte) bool { return isAlnum(b) || b == '-' }) {
			return false
		}
	}
	return true
}

// tokenByte reports whether b may be in a header field's name (RFC 9110
// 5.6.2, tchar).
func tokenByte(b byte) bool { return isAlnum(b) || strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0 }

// ApplySender returns stored with the fields named set to their values in
// change, checked by the contract's rules of a partial change and then as a
// whole, and whether its destination changed (so its health is unknown
// until its next connection). The sealed credential of the result is the
// one change gives when its field is named, and none otherwise: the store
// keeps the one it holds unless the change names it. cleared says the held
// credential is to be removed, which a change that makes the
// authentication none does. A driver calls it inside its transaction with
// the row it read.
func ApplySender(stored, change Sender, fields []SenderField) (out Sender, destination, cleared bool, err error) {
	if len(fields) == 0 {
		return stored, false, false, invalid("a change of a sender names at least one field")
	}
	if len(fields) > maxSenderFieldCount {
		return stored, false, false, invalid("a change of a sender names each field once")
	}
	out = stored
	out.STOW.SealedCredential = nil
	if stored.DIMSE.TLS != nil {
		t := *stored.DIMSE.TLS
		out.DIMSE.TLS = &t
	}
	if stored.STOW.TLS != nil {
		t := *stored.STOW.TLS
		out.STOW.TLS = &t
	}
	dimseTLS := func() *SenderTLS {
		if out.DIMSE.TLS == nil {
			out.DIMSE.TLS = &SenderTLS{}
		}
		return out.DIMSE.TLS
	}
	stowTLS := func() *SenderTLS {
		if out.STOW.TLS == nil {
			out.STOW.TLS = &SenderTLS{}
		}
		return out.STOW.TLS
	}
	ctls := func(t *SenderTLS) SenderTLS {
		if t == nil {
			return SenderTLS{}
		}
		return *t
	}
	for _, f := range fields {
		if typ, _, typed := strings.Cut(string(f), "."); typed && SenderType(typ) != stored.Type {
			return stored, false, false, invalid("a change of a " + string(stored.Type) + " sender names a field of another type: " + strconv.Quote(string(f)))
		}
		destination = destination || slices.Contains(destinationFields, f)
		switch f {
		case FieldSenderEnabled:
			out.Enabled = change.Enabled
		case FieldSenderAllowIdentified:
			out.AllowIdentified = change.AllowIdentified
		case FieldSenderAssociations:
			out.MaxAssociations = change.MaxAssociations
		case FieldSenderByteRate:
			out.MaxBytesPerSecond = change.MaxBytesPerSecond
		case FieldSenderConnectTimeout:
			out.ConnectTimeout = change.ConnectTimeout
		case FieldSenderResponseTimeout:
			out.ResponseTimeout = change.ResponseTimeout
		case FieldDIMSEHost:
			out.DIMSE.Host = strings.ToLower(change.DIMSE.Host)
		case FieldDIMSEPeerPort:
			out.DIMSE.Port = change.DIMSE.Port
		case FieldDIMSECalledAETitle:
			out.DIMSE.CalledAETitle = strings.Trim(change.DIMSE.CalledAETitle, " ")
		case FieldDIMSECallingAETitle:
			out.DIMSE.CallingAETitle = strings.Trim(change.DIMSE.CallingAETitle, " ")
		case FieldDIMSESenderTLS:
			out.DIMSE.TLS = nil
			if change.DIMSE.TLS != nil {
				t := *change.DIMSE.TLS
				out.DIMSE.TLS = &t
			}
		case FieldDIMSETLSCAFile:
			dimseTLS().CAFile = ctls(change.DIMSE.TLS).CAFile
		case FieldDIMSETLSCertFile:
			dimseTLS().CertificateFile = ctls(change.DIMSE.TLS).CertificateFile
		case FieldDIMSETLSKeyFile:
			dimseTLS().PrivateKeyFile = ctls(change.DIMSE.TLS).PrivateKeyFile
		case FieldDIMSETLSServerName:
			dimseTLS().ServerName = ctls(change.DIMSE.TLS).ServerName
		case FieldSTOWURL:
			out.STOW.URL = change.STOW.URL
		case FieldSTOWAuthentication:
			out.STOW.Authentication = change.STOW.Authentication
		case FieldSTOWUsername:
			out.STOW.Username = change.STOW.Username
		case FieldSTOWHeaderName:
			out.STOW.HeaderName = change.STOW.HeaderName
		case FieldSTOWCredential:
			out.STOW.SealedCredential = change.STOW.SealedCredential
		case FieldSTOWTLS:
			out.STOW.TLS = nil
			if change.STOW.TLS != nil {
				t := *change.STOW.TLS
				out.STOW.TLS = &t
			}
		case FieldSTOWTLSCAFile:
			stowTLS().CAFile = ctls(change.STOW.TLS).CAFile
		case FieldSTOWTLSCertFile:
			stowTLS().CertificateFile = ctls(change.STOW.TLS).CertificateFile
		case FieldSTOWTLSKeyFile:
			stowTLS().PrivateKeyFile = ctls(change.STOW.TLS).PrivateKeyFile
		case FieldSTOWTLSServerName:
			stowTLS().ServerName = ctls(change.STOW.TLS).ServerName
		case FieldSTOWMaxInstances:
			out.STOW.MaxInstancesPerRequest = change.STOW.MaxInstancesPerRequest
		case FieldSTOWMaxRequestBytes:
			out.STOW.MaxRequestBytes = change.STOW.MaxRequestBytes
		default:
			return stored, false, false, invalid("a change of a sender names a field it does not have: " + strconv.Quote(string(f)))
		}
	}
	if stored.Type == SenderSTOW {
		credential := slices.Contains(fields, FieldSTOWCredential)
		moved := slices.Contains(fields, FieldSTOWURL) || slices.Contains(fields, FieldSTOWAuthentication)
		if out.STOW.Authentication == StowAuthNone {
			// No credential with none: a held one is removed, and one
			// given is refused by Validate.
			cleared = stored.STOW.CredentialSet
			out.STOW.CredentialSet = false
		} else {
			if moved && !credential {
				return stored, false, false, invalid("a change of a stow sender's URL or authentication names its credential too, so a credential is never sent where it was not given for")
			}
			if credential && len(change.STOW.SealedCredential) == 0 {
				return stored, false, false, invalid("a credential that is changed is not empty while the authentication needs one")
			}
		}
	}
	if err := out.Validate(); err != nil {
		return stored, false, false, err
	}
	return out, destination, cleared, nil
}
