package tenant

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// ReceiverType is a receiver's type. Its text is what a store keeps.
type ReceiverType string

// The receiver types.
const (
	// ReceiverDIMSE is a DICOM listener on one port (PS3.8, PS3.7).
	ReceiverDIMSE ReceiverType = "dimse"
	// ReceiverSTOW takes instances stored over HTTP into the tenant's
	// DICOMweb service, STOW-RS (PS3.18 10.5), from the keys it lists.
	ReceiverSTOW ReceiverType = "stow"
)

// The bounds of a receiver's configuration, the API contract's.
const (
	MinQuietPeriod   = 5 * time.Second
	MaxQuietPeriod   = time.Hour
	MinMaxOpenTime   = time.Minute
	MaxMaxOpenTime   = 24 * time.Hour
	MinMaxFileBytes  = 1024
	MaxMaxFileBytes  = 1 << 38
	MaxCallerRules   = 256
	MaxSOPClasses    = 512
	MaxTransferSyns  = 64
	MaxTLSFileLength = 255
	MaxAddressRange  = 64
	// MaxSTOWKeys bounds the keys one stow receiver lists.
	MaxSTOWKeys = 256
	// MaxForwardTo is the most senders a receiver forwards to.
	MaxForwardTo = 16
	// MinMaxRequestBytes and MaxMaxRequestBytes bound a stow receiver's
	// largest request: at least its smallest largest file, at most 1 TiB.
	MinMaxRequestBytes = MinMaxFileBytes
	MaxMaxRequestBytes = 1 << 40
)

// The defaults the contract gives a receiver that leaves them out; the
// caller fills them before a receiver reaches a store, which stores what it
// is given.
const (
	DefaultQuietPeriod  = 60 * time.Second
	DefaultMaxOpenTime  = time.Hour
	DefaultMaxFileBytes = 1 << 32
	// DefaultMaxRequestBytes is a stow receiver's largest request when it
	// names none: 16 GiB, a large study in one request.
	DefaultMaxRequestBytes = 1 << 34
)

// Receiver is a way for data to arrive at a tenant, and how what arrives is
// handled.
type Receiver struct {
	// Name is the receiver's name, under the rule of a tenant's name.
	Name string
	// Type is its type; DIMSE, STOW or Import holds that type's
	// configuration, and the others are their zero values.
	Type ReceiverType
	// DIMSE is a DIMSE receiver's configuration.
	DIMSE DIMSEReceiver
	// STOW is a stow receiver's configuration.
	STOW STOWReceiver
	// Import is an import receiver's configuration (receiver_import.go).
	Import ImportReceiver
	// Enabled says it accepts data.
	Enabled bool
	// Storage names the storage what it receives is archived to.
	Storage string
	// Naming is its naming rule, nil for the default (naming.DefaultSpec).
	Naming *naming.Spec
	// QuietPeriod is how long without a file before a reception is
	// complete: whole seconds, 5 seconds to 1 hour.
	QuietPeriod time.Duration
	// MaxOpenTime is the longest a reception stays receiving: whole
	// seconds, 1 minute to 24 hours, not less than QuietPeriod.
	MaxOpenTime time.Duration
	// MaxFileBytes is the largest file it accepts, 1024 to 2^38.
	MaxFileBytes int64
	// ForwardTo names the senders each reception it completes is sent to,
	// one job per sender when the reception reaches done; at most
	// MaxForwardTo, each once, each a sender the tenant has (the store's
	// to check).
	ForwardTo []string
	// Profile is the de-identification profile version every file it
	// receives is de-identified with before any byte of it reaches a
	// storage; the zero ref archives what it receives as received. The
	// version must be one the tenant has (the caller checks, since a
	// version is never removed), and an as-received receiver may exist
	// only where the tenant's setting receiver.allow-as-received allows it
	// (the caller's to check too).
	Profile ProfileRef
	// Routing chooses, for each study, the storage, the profile version
	// and the senders in place of Storage, Profile and ForwardTo; its zero
	// value chooses nothing, and every study goes as those say.
	Routing Routing
	// Created is when it was added; Updated when it last changed. In a
	// change given to ApplyReceiver, an Updated not zero is the time the
	// caller read: the change is made only while the stored receiver
	// still has it.
	Created, Updated time.Time
}

// DIMSEReceiver is a DICOM listener's configuration.
type DIMSEReceiver struct {
	// Port is the TCP port, 1 to 65535.
	Port int
	// BindAddress is the server's address it listens on, empty for the
	// installation's default.
	BindAddress string
	// AETitle is the AE title a caller must call.
	AETitle string
	// AnyCaller admits every caller; when false, Callers lists those
	// admitted, and an empty list admits nobody.
	AnyCaller bool
	// Callers are the rules a caller must match one of.
	Callers []CallerRule
	// TLS is where its certificate and key are, nil for no TLS.
	TLS *ReceiverTLS
	// SOPClasses are the SOP Class UIDs it accepts, empty for the server's
	// default set.
	SOPClasses []string
	// TransferSyntaxes are the Transfer Syntax UIDs it accepts, empty for
	// the server's default set.
	TransferSyntaxes []string
}

// STOWReceiver is the configuration of a receiver that takes instances
// stored over HTTP (PS3.18 10.5). It is closed by default: a request is
// taken only from a key it lists, and that key must also hold writer or
// more on the tenant, which the request's check asks before anything else.
type STOWReceiver struct {
	// Keys are the ids of the keys whose requests it takes; an empty list
	// takes none. A key is listed by at most one stow receiver of a
	// tenant, so which receiver a request comes to is never a choice. The
	// route also takes a key rotated from one listed (the master holds the
	// line of rotations, master.FindRotatedFrom); Accepts asks only the
	// list.
	Keys []string
	// MaxRequestBytes is the largest request body it reads, from 1024 to
	// 2^40; each part is also bounded by the receiver's MaxFileBytes.
	MaxRequestBytes int64
}

// Accepts reports whether the receiver takes a request made with the key
// whose id is keyID. An empty id, the local operator's, is never listed.
func (s STOWReceiver) Accepts(keyID string) bool {
	return keyID != "" && slices.Contains(s.Keys, keyID)
}

// isZero reports whether s holds nothing, as a receiver of another type
// keeps it.
func (s STOWReceiver) isZero() bool { return len(s.Keys) == 0 && s.MaxRequestBytes == 0 }

func (s STOWReceiver) validate() error {
	if len(s.Keys) > MaxSTOWKeys {
		return invalid("a stow receiver lists at most 256 keys")
	}
	seen := make(map[string]bool, len(s.Keys))
	for _, k := range s.Keys {
		if err := validateKeyID(k); err != nil {
			return err
		}
		if seen[k] {
			return invalid("a stow receiver lists each key once")
		}
		seen[k] = true
	}
	if s.MaxRequestBytes < MinMaxRequestBytes || s.MaxRequestBytes > MaxMaxRequestBytes {
		return invalid("a largest request is 1024 to 2^40 bytes")
	}
	return nil
}

// validateKeyID refuses what cannot be a key's id (master.ValidateID).
func validateKeyID(id string) error {
	if err := master.ValidateID("key id", id); err != nil {
		return invalid(fault.Message(err))
	}
	return nil
}

// CallerRule admits callers by calling AE title, address range, or both.
type CallerRule struct {
	// CallingAETitle is the AE title a caller must give, empty for any.
	CallingAETitle string
	// AddressRange is a range in CIDR form with its host bits zero, empty
	// for any address.
	AddressRange string
}

// Admits reports whether the receiver admits a caller that gives the
// Calling AE Title aeTitle from addr (receiver.proto, allowed_callers):
// every caller when it says any caller; otherwise a caller that matches at
// least one rule, where a rule matches when each part it names matches (the
// title compared exactly, case and spaces included, since PS3.8 9.3.2 makes
// it a fixed field the caller fills; the address inside the range). An
// empty list admits nobody, so a receiver is closed until a person lists a
// caller, and an address that is not valid matches no range. An IPv4
// address carried as IPv6 (::ffff:a.b.c.d, as a listener on both families
// sees an IPv4 caller) is compared as the IPv4 address it is.
func (d DIMSEReceiver) Admits(aeTitle string, addr netip.Addr) bool {
	if d.AnyCaller {
		return true
	}
	addr = addr.Unmap().WithZone("")
	for _, rule := range d.Callers {
		if rule.CallingAETitle != "" && rule.CallingAETitle != aeTitle {
			continue
		}
		if rule.AddressRange != "" {
			p, err := netip.ParsePrefix(rule.AddressRange)
			if err != nil || !addr.IsValid() || !p.Contains(addr) {
				continue
			}
		}
		return true
	}
	return false
}

// ReceiverTLS names the files of a receiver's certificate and key in the
// tenant's TLS directory.
type ReceiverTLS struct {
	// CertificateFile is the certificate chain's file name.
	CertificateFile string
	// PrivateKeyFile is the private key's file name.
	PrivateKeyFile string
}

// ReceiverField names one field of a receiver a partial change sets, as the
// API contract's update mask names it.
type ReceiverField string

// The fields of a receiver a change can name.
const (
	FieldEnabled          ReceiverField = "enabled"
	FieldStorage          ReceiverField = "storage"
	FieldNaming           ReceiverField = "naming"
	FieldQuietPeriod      ReceiverField = "quiet_period"
	FieldMaxOpenTime      ReceiverField = "max_open_time"
	FieldMaxFileBytes     ReceiverField = "max_file_bytes"
	FieldDIMSEPort        ReceiverField = "dimse.port"
	FieldDIMSEBindAddress ReceiverField = "dimse.bind_address"
	FieldDIMSEAETitle     ReceiverField = "dimse.ae_title"
	FieldDIMSECallers     ReceiverField = "dimse.allowed_callers"
	FieldDIMSEAnyCaller   ReceiverField = "dimse.any_caller"
	FieldDIMSETLS         ReceiverField = "dimse.tls"
	FieldDIMSETLSCert     ReceiverField = "dimse.tls.certificate_file"
	FieldDIMSETLSKey      ReceiverField = "dimse.tls.private_key_file"
	FieldDIMSESOPClasses  ReceiverField = "dimse.sop_classes"
	FieldDIMSESyntaxes    ReceiverField = "dimse.transfer_syntaxes"
	FieldSTOWKeys         ReceiverField = "stow.key_ids"
	FieldSTOWMaxRequest   ReceiverField = "stow.max_request_bytes"
	FieldForwardTo        ReceiverField = "forward_to"
	// FieldDeidentification sets Profile: the profile version, or the zero
	// ref for as received.
	FieldDeidentification ReceiverField = "deidentification"
	// FieldRouting sets Routing, replaced whole.
	FieldRouting ReceiverField = "routing"
)

// maxReceiverFieldCount is how many fields a receiver has that a change
// can name (FieldImporter among them).
const maxReceiverFieldCount = 22

// Deidentifies reports whether the receiver de-identifies what it
// receives, rather than archiving it as received.
func (r Receiver) Deidentifies() bool { return !r.Profile.IsZero() }

// PlainPlaceholders returns the placeholders of r's naming rule that put a
// received value into a label as it is, not hashed (naming.Rule.Plain);
// none for the default rule. A label is kept in the reception's row when
// the reception is identified, before any file is de-identified, so a
// receiver that de-identifies may have no such placeholder: the caller
// refuses one that does. A rule that does not parse has none here, since
// Validate refuses it.
func (r Receiver) PlainPlaceholders() []string {
	if r.Naming == nil {
		return nil
	}
	rule, err := r.NamingRule()
	if err != nil {
		return nil
	}
	return rule.Plain()
}

// Validate returns an Invalid error unless r can be stored, by the
// contract's rule of each field, or a precondition failure for a receiver
// that de-identifies whose naming rule puts a value into a label unhashed,
// or for a receiver that archives as received one of whose routes names a
// profile version.
// Whether its storage, and its routes' storages and senders, exist and
// whether its port is free are the store's to check, in its transaction.
func (r Receiver) Validate() error {
	if err := ValidateName("receiver", r.Name); err != nil {
		return err
	}
	if err := ValidateName("storage", r.Storage); err != nil {
		return err
	}
	if r.QuietPeriod < MinQuietPeriod || r.QuietPeriod > MaxQuietPeriod || r.QuietPeriod%time.Second != 0 {
		return invalid("a quiet period is a whole number of seconds from 5 seconds to 1 hour")
	}
	if r.MaxOpenTime < MinMaxOpenTime || r.MaxOpenTime > MaxMaxOpenTime || r.MaxOpenTime%time.Second != 0 {
		return invalid("a longest open time is a whole number of seconds from 1 minute to 24 hours")
	}
	if r.MaxOpenTime < r.QuietPeriod {
		return invalid("a longest open time is not less than the quiet period")
	}
	if r.MaxFileBytes < MinMaxFileBytes || r.MaxFileBytes > MaxMaxFileBytes {
		return invalid("a largest file is 1024 to 2^38 bytes")
	}
	if len(r.ForwardTo) > MaxForwardTo {
		return invalid("a receiver forwards to at most 16 senders")
	}
	for i, f := range r.ForwardTo {
		if err := ValidateSenderName(f); err != nil {
			return err
		}
		if slices.Contains(r.ForwardTo[:i], f) {
			return invalid("a receiver names each sender it forwards to once")
		}
	}
	if !r.Profile.IsZero() {
		if err := r.Profile.Validate(); err != nil {
			return err
		}
	}
	if err := r.Routing.Validate(); err != nil {
		return err
	}
	if !r.Deidentifies() {
		for _, rt := range r.Routing.Routes {
			// The receiver's choice decides how a file lands, its UIDs
			// remapped and its checksum keyed or not, before any route is
			// chosen; a route cannot make a file landed as received into
			// one de-identified. A precondition: the same route is taken
			// once the receiver de-identifies.
			if !rt.Profile.IsZero() {
				return Precondition("receiver.routing: route " + rt.Name + " names a profile version, and the receiver archives as received; " +
					"a route's profile replaces the profile of a receiver that de-identifies, so make the receiver de-identify first")
			}
		}
	}
	if r.Naming != nil {
		// An import receiver's rule may also use the parts its layout
		// and map give (GivenParts); a part neither the rule's pattern nor
		// they give, or one both would fill, is refused here, not on a
		// file.
		rule, err := r.NamingRule()
		if err != nil {
			return fault.Wrap(fault.Invalid, err, "the naming rule is not valid: "+err.Error())
		}
		// A label is kept with its reception before any file of it is
		// de-identified, so a receiver that de-identifies takes no value
		// into a label unhashed. A precondition, not a malformed request:
		// each field is valid, and the same rule is taken by a receiver
		// that archives as received.
		if plain := rule.Plain(); r.Deidentifies() && len(plain) > 0 {
			return Precondition("receiver.naming: a receiver that de-identifies makes its labels from hashed values only, and the naming rule puts " +
				strings.Join(plain, ", ") + " into a label as received; write each as {#...}")
		}
	}
	switch r.Type {
	case ReceiverDIMSE:
		if !r.STOW.isZero() || !r.Import.isZero() {
			return invalid("a dimse receiver has no stow or import configuration")
		}
		return r.DIMSE.validate()
	case ReceiverSTOW:
		if !r.DIMSE.isZero() || !r.Import.isZero() {
			return invalid("a stow receiver has no dimse or import configuration")
		}
		return r.STOW.validate()
	case ReceiverImport:
		if !r.DIMSE.isZero() || !r.STOW.isZero() {
			return invalid("an import receiver has no dimse or stow configuration")
		}
		return r.Import.validate()
	}
	return invalid("a receiver's type is dimse, stow or import")
}

// isZero reports whether d holds nothing, as a receiver of another type
// keeps it.
func (d DIMSEReceiver) isZero() bool {
	return d.Port == 0 && d.BindAddress == "" && d.AETitle == "" && !d.AnyCaller && len(d.Callers) == 0 &&
		d.TLS == nil && len(d.SOPClasses) == 0 && len(d.TransferSyntaxes) == 0
}

func (d DIMSEReceiver) validate() error {
	if d.Port < 1 || d.Port > 65535 {
		return invalid("a port is 1 to 65535")
	}
	if d.BindAddress != "" {
		a, err := netip.ParseAddr(d.BindAddress)
		if err != nil || a.Zone() != "" || a.String() != d.BindAddress {
			return invalid("a bind address is an IPv4 address in dotted form or an IPv6 address in the form of RFC 5952, without a zone")
		}
		if a.Is4In6() {
			// One address, two spellings: kept in its IPv4 form only, so
			// the clash rule sees one address.
			return invalid("an IPv4-mapped IPv6 address is written in its IPv4 form, such as 192.0.2.1")
		}
	}
	if err := ValidateAETitle(d.AETitle); err != nil {
		return err
	}
	if d.AnyCaller && len(d.Callers) > 0 {
		return invalid("a receiver admits any caller or the callers it lists, not both")
	}
	if len(d.Callers) > MaxCallerRules {
		return invalid("a receiver lists at most 256 caller rules")
	}
	for _, c := range d.Callers {
		if c.CallingAETitle == "" && c.AddressRange == "" {
			return invalid("a caller rule names an AE title, an address range, or both")
		}
		if c.CallingAETitle != "" {
			if err := ValidateAETitle(c.CallingAETitle); err != nil {
				return err
			}
		}
		if c.AddressRange != "" {
			p, err := netip.ParsePrefix(c.AddressRange)
			if err != nil || len(c.AddressRange) > MaxAddressRange || p.Masked() != p {
				return invalid("an address range is in CIDR form with its host bits zero")
			}
		}
	}
	if d.TLS != nil {
		for _, f := range []string{d.TLS.CertificateFile, d.TLS.PrivateKeyFile} {
			if f == "" || len(f) > MaxTLSFileLength || f[0] == '.' || !allBytes(f, pathByte) {
				return invalid("a TLS file's name is 1 to 255 of A-Z, a-z, 0-9, ., _ and -, not starting with a dot")
			}
		}
	}
	if len(d.SOPClasses) > MaxSOPClasses {
		return invalid("a receiver accepts at most 512 SOP classes")
	}
	for _, u := range d.SOPClasses {
		if err := validateUID("SOP Class UID", u); err != nil {
			return err
		}
	}
	if len(d.TransferSyntaxes) > MaxTransferSyns {
		return invalid("a receiver accepts at most 64 transfer syntaxes")
	}
	for _, u := range d.TransferSyntaxes {
		if err := validateUID("Transfer Syntax UID", u); err != nil {
			return err
		}
	}
	return nil
}

// ApplyReceiver returns stored with the fields named set to their values in
// change, checked by the contract's rules of a partial change and then as a
// whole. A driver calls it inside its transaction with the row it read.
func ApplyReceiver(stored, change Receiver, fields []ReceiverField) (Receiver, error) {
	if len(fields) == 0 {
		return stored, invalid("a change of a receiver names at least one field")
	}
	if len(fields) > maxReceiverFieldCount {
		return stored, invalid("a change of a receiver names each field once")
	}
	if !change.Updated.IsZero() && !change.Updated.Equal(stored.Updated) {
		return stored, Precondition("the receiver changed after it was read; read it again and make the change on what it holds now")
	}
	out := stored
	out.DIMSE.Callers = slices.Clone(stored.DIMSE.Callers)
	out.DIMSE.SOPClasses = slices.Clone(stored.DIMSE.SOPClasses)
	out.DIMSE.TransferSyntaxes = slices.Clone(stored.DIMSE.TransferSyntaxes)
	out.STOW.Keys = slices.Clone(stored.STOW.Keys)
	out.ForwardTo = slices.Clone(stored.ForwardTo)
	out.Routing = stored.Routing.clone()
	out.Import = cloneImport(stored.Import)
	if stored.DIMSE.TLS != nil {
		t := *stored.DIMSE.TLS
		out.DIMSE.TLS = &t
	}
	if stored.Naming != nil {
		n := *stored.Naming
		out.Naming = &n
	}
	tlsPart := func() *ReceiverTLS {
		if out.DIMSE.TLS == nil {
			out.DIMSE.TLS = &ReceiverTLS{}
		}
		return out.DIMSE.TLS
	}
	for _, f := range fields {
		if typ, _, typed := strings.Cut(string(f), "."); typed && ReceiverType(typ) != stored.Type {
			return stored, invalid("a change of a " + string(stored.Type) + " receiver names a field of another type: " + strconv.Quote(string(f)))
		}
		switch f {
		case FieldEnabled:
			out.Enabled = change.Enabled
		case FieldStorage:
			out.Storage = change.Storage
		case FieldNaming:
			out.Naming = nil
			if change.Naming != nil {
				n := *change.Naming
				out.Naming = &n
			}
		case FieldQuietPeriod:
			out.QuietPeriod = change.QuietPeriod
		case FieldMaxOpenTime:
			out.MaxOpenTime = change.MaxOpenTime
		case FieldMaxFileBytes:
			out.MaxFileBytes = change.MaxFileBytes
		case FieldDIMSEPort:
			out.DIMSE.Port = change.DIMSE.Port
		case FieldDIMSEBindAddress:
			out.DIMSE.BindAddress = change.DIMSE.BindAddress
		case FieldDIMSEAETitle:
			out.DIMSE.AETitle = change.DIMSE.AETitle
		case FieldDIMSECallers:
			out.DIMSE.AnyCaller = false
			out.DIMSE.Callers = slices.Clone(change.DIMSE.Callers)
		case FieldDIMSEAnyCaller:
			if !change.DIMSE.AnyCaller {
				return stored, invalid("any_caller, when named, is true")
			}
			out.DIMSE.AnyCaller, out.DIMSE.Callers = true, nil
		case FieldDIMSETLS:
			out.DIMSE.TLS = nil
			if change.DIMSE.TLS != nil {
				t := *change.DIMSE.TLS
				out.DIMSE.TLS = &t
			}
		case FieldDIMSETLSCert:
			cert := ""
			if change.DIMSE.TLS != nil {
				cert = change.DIMSE.TLS.CertificateFile
			}
			tlsPart().CertificateFile = cert
		case FieldDIMSETLSKey:
			key := ""
			if change.DIMSE.TLS != nil {
				key = change.DIMSE.TLS.PrivateKeyFile
			}
			tlsPart().PrivateKeyFile = key
		case FieldDIMSESOPClasses:
			out.DIMSE.SOPClasses = slices.Clone(change.DIMSE.SOPClasses)
		case FieldDIMSESyntaxes:
			out.DIMSE.TransferSyntaxes = slices.Clone(change.DIMSE.TransferSyntaxes)
		case FieldSTOWKeys:
			out.STOW.Keys = slices.Clone(change.STOW.Keys)
		case FieldSTOWMaxRequest:
			out.STOW.MaxRequestBytes = change.STOW.MaxRequestBytes
		case FieldForwardTo:
			out.ForwardTo = slices.Clone(change.ForwardTo)
		case FieldDeidentification:
			out.Profile = change.Profile
		case FieldRouting:
			out.Routing = change.Routing.clone()
		case FieldImporter:
			if stored.Type != ReceiverImport {
				return stored, invalid("a change of a " + string(stored.Type) + " receiver names a field of another type: " + strconv.Quote(string(f)))
			}
			out.Import = cloneImport(change.Import)
		default:
			return stored, invalid("a change of a receiver names a field it does not have: " + strconv.Quote(string(f)))
		}
	}
	if err := out.Validate(); err != nil {
		return stored, err
	}
	return out, nil
}

// ReceiversClash reports whether two receivers of one tenant would contend
// for what only one may have. Two stow receivers clash when they list a key
// in common, since a request names no receiver and is taken by the one
// receiver that lists its key. Two DIMSE receivers clash when they would
// contend for one port at the operating system: on the same port, two bind
// addresses clash when they are equal, when either is empty (every
// address), when either is "::" (every IPv6 address, and on a dual-stack
// system every IPv4 address too, so the rule is the strict one), and when
// either is "0.0.0.0" and the other is an IPv4 address (every IPv4
// address). A specific IPv4 and a specific IPv6 address do not clash.
// IPv4-mapped IPv6 addresses are refused by Validate, so each address has
// one spelling here.
func ReceiversClash(a, b Receiver) bool {
	if a.Type == ReceiverSTOW && b.Type == ReceiverSTOW {
		return slices.ContainsFunc(a.STOW.Keys, func(k string) bool { return slices.Contains(b.STOW.Keys, k) })
	}
	if a.Type != ReceiverDIMSE || b.Type != ReceiverDIMSE || a.DIMSE.Port != b.DIMSE.Port {
		return false
	}
	x, y := a.DIMSE.BindAddress, b.DIMSE.BindAddress
	if x == "" || y == "" || x == y || x == "::" || y == "::" {
		return true
	}
	ipv4 := func(s string) bool { ad, err := netip.ParseAddr(s); return err == nil && ad.Is4() }
	return (x == "0.0.0.0" && ipv4(y)) || (y == "0.0.0.0" && ipv4(x))
}

// ClashError is the refusal of a receiver that clashes with other
// (ReceiversClash), in the words of what the two contend for.
func ClashError(other Receiver) error {
	if other.Type == ReceiverSTOW {
		return Precondition("the receiver " + other.Name + " already takes requests from one of these keys; a key is listed by one stow receiver of a tenant")
	}
	return Precondition("the receiver " + other.Name + " listens on the same port and an overlapping address")
}
