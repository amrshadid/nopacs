// Package fakeserver is an in-memory implementation of the four services of
// the API, written against the generated handler interfaces and the comments
// of the contract, and served over the transports the real server will use: a
// Unix socket with HTTP/2 without TLS, and TLS (or plain http) on the loopback
// address. The command line's tests run real commands against it, so a command
// that only works against a mock of the client cannot pass.
//
// It follows the contract where the contract is specific: the order of checks,
// the roles' errors, the limits, the paging with opaque tokens, and the
// Nopacs-Request-Id header on every answer, errors included. It is not the
// server: it keeps its state in maps, checks no grants beyond what a test asks
// for, and is not a place to learn what the real server does under load.
//
// This package must never be imported by code that the nopacs binary links:
// it is test support, and the static builds must not carry it. It must never
// hold a real key or a real address.
package fakeserver
