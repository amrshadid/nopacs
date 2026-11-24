// Package obs is what lets a person watch a running server: the logger every
// part of it writes through, and the metrics it answers on /metrics.
//
// # Logging
//
// NewLogger returns a log/slog logger writing JSON (the default) or text.
// Its handler rewrites two kinds of attribute before anything is written,
// whatever the level and whoever logs:
//
//   - a value that is a protobuf message is replaced by its redacted JSON
//     form (package redact), so a field the contract marks debug_redact is
//     never written, even by code that logs a request or a response
//     carelessly;
//   - an attribute named "authorization", in any case and in any group, is
//     replaced by a fixed text, so the bearer key of a request cannot reach
//     the log by that name.
//
// These are guards behind the rule, not the rule: the server never logs a
// header, and logs a message only through redact.
//
// # Metrics
//
// Metrics keeps request counts and durations and writes them, with gauges
// read from their owners at the moment of a scrape, in the Prometheus text
// exposition format (version 0.0.4), written by hand: the format is a few
// lines of text, and a client library would add a dependency for it.
//
// What it must never do: write a secret, a key, a request's content or any
// label taken unchecked from a request (a label's values come from fixed
// sets: known method names, API codes, tenant states). It holds no business
// logic and imports no other package of NoPACs' but redact.
package obs
