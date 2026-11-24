package access

import (
	"strconv"
)

// Credential is what a request presents: a key, the local operator, or
// nothing. The zero Credential is a request with no key.
type Credential struct {
	key       string
	localUser string
	local     bool
	// accepted is the caller Authenticate found good; nil for a credential
	// that has not been through it. Only Authenticate sets it, so no other
	// package can make a credential that skips the reading of its key.
	accepted *Principal
	// reread makes Check read an accepted key again (Reread).
	reread bool
}

// APIKey is a credential of a presented key, nopacs_<id>_<secret>. The server
// takes it from the Authorization header, strips the "Bearer" scheme and the
// space after it (the scheme compared case-insensitively, RFC 6750 2.1), and
// passes only the key: anything else, such as the scheme left in, is a
// malformed key. The empty string is a request with no key.
func APIKey(presented string) Credential {
	return Credential{key: presented}
}

// LocalOperator is the credential of a request on the installation's local
// socket, made by the operating system user with this id (a uid, or a SID on
// Windows). It makes its holder admin on the installation and on every
// tenant, so only the server may call it, in package internal/server, and
// only after the socket has shown the caller is the user that runs the
// server: whoever that user is can read the installation's files anyway. A
// test of this package fails if any other package calls it. An empty user id,
// or one holding a control character or longer than an audit field, is
// refused as unauthenticated.
func LocalOperator(userID string) Credential {
	return Credential{localUser: userID, local: true}
}

// String prints which sort of credential it is and never the key, so a
// credential that reaches a log by mistake carries no secret.
func (c Credential) String() string {
	switch {
	case c.accepted != nil && c.accepted.isLoc:
		return "access.LocalOperator(" + strconv.QuoteToASCII(c.accepted.local) + ", accepted)"
	case c.accepted != nil:
		return "access.Credential(key, accepted)"
	case c.local:
		return "access.LocalOperator(" + strconv.QuoteToASCII(c.localUser) + ")"
	case c.key == "":
		return "access.Credential(none)"
	}
	return "access.Credential(key)"
}

// Reread returns c marked so that Check, given it, reads the key again and
// refuses it if it has been revoked or removed since Authenticate accepted
// it. The server marks a credential so when time it does not bound has
// passed between the two, such as the reading of the request's message,
// which may take as long as the body's read bound: the decision to allow
// is made on the key as it is then. A credential Authenticate did not
// accept is returned unchanged; its key is read by Check anyway.
func (c Credential) Reread() Credential {
	if c.accepted != nil {
		c.reread = true
	}
	return c
}

// GoString is String, for the %#v verb.
func (c Credential) GoString() string { return c.String() }

// KeyID returns the id of the key Authenticate accepted in c, or "" for a
// credential it did not accept or the local operator. The server keys its
// count of a key's refused calls by it.
func (c Credential) KeyID() string {
	if c.accepted == nil || !c.accepted.isKey {
		return ""
	}
	return c.accepted.key.ID
}
