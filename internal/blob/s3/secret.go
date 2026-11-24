package s3

import (
	"encoding/json"
	"fmt"
)

// Secret is a credential: the access key id or the secret access key. It
// withholds its value from every way a program prints or encodes a value,
// and it holds the value behind a pointer, so that printing a struct that
// contains one shows an address and not the text even where fmt cannot call
// methods (an unexported field of another struct is printed by reflection,
// field by field, and a plain string field would show in full). A secret is
// read only through Reveal, which the driver calls where it signs a request.
//
// The access key id is withheld too. It is not secret in the sense the
// secret key is, but with a name it identifies the account to the server,
// and nothing about a credential belongs in a log or an error.
type Secret struct{ p *string }

// NewSecret returns a Secret holding s.
func NewSecret(s string) Secret { return Secret{p: &s} }

// Reveal returns the value. It is for signing a request and for nothing
// else: it must never reach a log, an error or an answer.
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

// IsSet reports whether the Secret holds a non-empty value.
func (s Secret) IsSet() bool { return s.Reveal() != "" }

const withheld = "[withheld]"

// String returns a fixed text, whatever the value.
func (Secret) String() string { return withheld }

// GoString is String, for %#v.
func (Secret) GoString() string { return withheld }

// Format writes a fixed text for every verb, so no verb (%v, %+v, %#v, %s,
// %q, %x, %d) prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(withheld)) }

// MarshalText returns a fixed text, which encoders such as the text and JSON
// handlers of log/slog use.
func (Secret) MarshalText() ([]byte, error) { return []byte(withheld), nil }

// MarshalJSON returns a fixed JSON string.
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal(withheld) }

// UnmarshalText sets the value from text, so a configuration file can hold
// one. Nothing reads it back out but Reveal.
func (s *Secret) UnmarshalText(b []byte) error {
	*s = NewSecret(string(b))
	return nil
}
