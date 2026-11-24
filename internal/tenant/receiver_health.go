package tenant

import (
	"slices"
	"time"
)

// ReceiverState is whether a receiver accepts data, the contract's
// ReceiverState in lowercase without its prefix.
type ReceiverState string

// The receiver states.
const (
	ReceiverStarting ReceiverState = "starting"
	ReceiverUp       ReceiverState = "up"
	ReceiverDown     ReceiverState = "down"
)

// ReceiverDownReason is why a receiver is down, the contract's
// ReceiverDownReason in lowercase without its prefix.
type ReceiverDownReason string

// The reasons a receiver is down, the contract's ReceiverDownReason values
// (receiver.proto). The listener gives those of its port and TLS files, the
// supervisor the others; the API maps each to the contract's enum.
const (
	DownDisabled            ReceiverDownReason = "disabled"
	DownAddressInUse        ReceiverDownReason = "address_in_use"
	DownPermissionDenied    ReceiverDownReason = "permission_denied"
	DownAddressNotAvailable ReceiverDownReason = "address_not_available"
	DownTLS                 ReceiverDownReason = "tls"
	DownNearlyFull          ReceiverDownReason = "nearly_full"
	DownTenantNotReady      ReceiverDownReason = "tenant_not_ready"
	DownStopping            ReceiverDownReason = "stopping"
	DownOther               ReceiverDownReason = "other"
	DownIncomingLimit       ReceiverDownReason = "incoming_limit"
)

// The phrases a down receiver's health detail holds, the server's fixed set
// (healthPhrases), named so the listener, the supervisor and the tests
// spell each one way.
const (
	PhraseDisabled               = "disabled"
	PhraseAddressInUse           = "the port is taken by another program"
	PhrasePermissionDenied       = "the port needs a privilege the server does not have"
	PhraseAddressNotAvailable    = "the bind address is not an address of the server"
	PhraseCertificateExpired     = "certificate expired"
	PhraseCertificateNotYet      = "certificate not yet valid"
	PhraseCertificateKeyMismatch = "certificate and key do not match"
	PhraseCertificateUnreadable  = "certificate file cannot be read"
	PhraseKeyUnreadable          = "key file cannot be read"
	PhraseIncomingFull           = "the incoming area is nearly full"
	PhraseStorageFull            = "the storage is nearly full"
	PhraseTenantNotReady         = "the tenant's database does not answer"
	PhraseTenantUpgrading        = "the tenant is being upgraded"
	PhraseStopping               = "the server is stopping"
	PhraseCouldNotStart          = "the receiver could not start"
	PhraseIncomingLimit          = "the tenant's incoming limit is reached"
)

// healthPhrases are the reasons a receiver is down, the contract's
// ReceiverDownReason values, each with the phrases its health's detail may
// hold: the server's fixed set, so a detail is never an error's text, a
// path or anything from outside. Empty is always allowed.
var healthPhrases = map[ReceiverDownReason][]string{
	DownDisabled:            {PhraseDisabled},
	DownAddressInUse:        {PhraseAddressInUse},
	DownPermissionDenied:    {PhrasePermissionDenied},
	DownAddressNotAvailable: {PhraseAddressNotAvailable},
	DownTLS: {PhraseCertificateExpired, PhraseCertificateNotYet, PhraseCertificateKeyMismatch,
		PhraseCertificateUnreadable, PhraseKeyUnreadable},
	DownNearlyFull:     {PhraseIncomingFull, PhraseStorageFull},
	DownTenantNotReady: {PhraseTenantNotReady, PhraseTenantUpgrading},
	DownStopping:       {PhraseStopping},
	DownOther:          {PhraseCouldNotStart},
	DownIncomingLimit:  {PhraseIncomingLimit},
}

// HealthPhrases returns the phrases a down receiver's health detail may
// hold for reason, the server's fixed set; nil for a reason not known.
func HealthPhrases(reason ReceiverDownReason) []string {
	return slices.Clone(healthPhrases[reason])
}

// ReceiverHealthChanged is a receiver's state changing, the contract's
// message of that name.
type ReceiverHealthChanged struct {
	// Receiver names the receiver.
	Receiver string `json:"receiver"`
	// State is its new state.
	State ReceiverState `json:"state"`
	// DownReason is why it is down; empty unless State is down.
	DownReason ReceiverDownReason `json:"down_reason"`
	// Detail is the server's phrase for the state: empty, or for a down
	// receiver one of HealthPhrases(DownReason).
	Detail string `json:"detail"`
	// Since is when it entered the state.
	Since time.Time `json:"since"`
}

// EventType implements EventPayload.
func (*ReceiverHealthChanged) EventType() EventType { return EventReceiverHealth }
