// Package client is how the command line reaches the API: it picks the
// transport (the local socket, or the network over TLS), attaches the key,
// bounds every call with a deadline, and turns an error answer into an error
// with a fault kind and the request id the server made for the call.
//
// The key is read from the environment or from a file and from nowhere else:
// an argument is visible in the process list and in shell history, so this
// package has no way to be given a key as a value on the command line.
//
// The key file must be private to its owner, by the one rule the installation's
// own key file follows (secret.OpenPrivate); this package uses internal/secret
// for that check only and never seals or hashes anything. Certificate
// authorities come from a PEM file of public certificates and replace the
// system's; TLS 1.2 is the floor.
//
// This package must never print, log, or put a key, a key file's content or a
// URL's credentials into an error or a message, never send the key anywhere
// but the Authorization header, and never send a key over a connection that is
// neither TLS nor on the loopback address.
package client
