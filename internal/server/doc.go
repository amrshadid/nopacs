// Package server is the process that serves the API: it listens, puts every
// request through one checked path, hides secrets in what it logs, bounds
// what a request may cost, and stops cleanly. It holds no business logic:
// the handlers of the API's services are given to it (Config.Register).
//
// # The request path
//
// Every request takes the same steps, in this order:
//
//  1. The envelope (below): the request id, the stop, the deadlines, the
//     body's read bound.
//  2. The front step, before any route is chosen. The health service's
//     Check and Watch, matched exactly, are the only public paths and go on
//     unauthenticated. For anything else the credential is read from the
//     headers alone (the Authorization bearer key; without one, the local
//     operator on the local socket) and access.Authenticate judges it,
//     reading the key once. A caller who is not authenticated is answered
//     there and then, by one writer, with a refusal chosen only by the
//     protocol the request's Content-Type names: never by the path, the
//     HTTP method, whether anything serves the path, or the body, which is
//     not read. So such a caller learns nothing from the server, not even
//     which methods it serves, and cannot hold one path longer than another.
//  3. The router, for an authenticated caller (or a public path). A served
//     procedure goes to its handler from the one connect.Server built with
//     the interceptor, and a plain route's path to its route, by exact
//     match only. A path with the shape and the method of a pattern route
//     (a route inside one tenant, whose path names the tenant) goes to that
//     route; its path is matched as sent, never decoded or cleaned. Anything
//     else, a path with any percent-encoding, a double or trailing slash
//     included, reaches no handler: the server answers it UNIMPLEMENTED in
//     the caller's RPC protocol (NOT_FOUND to plain HTTP), over any HTTP
//     version, and audits it as failed, served=false.
//  4. pass, the one function the Connect interceptor calls for every RPC and
//     the HTTP middleware for every plain route: access.Check with the
//     method's requirement, using the credential the front step accepted;
//     a refusal answered with the access package's fixed message and code;
//     then the handler, with a Call in its context (CallFrom) and any panic
//     recovered and answered INTERNAL. For an authenticated caller the order
//     of checks is the contract's: the role, then the handler validates the
//     request.
//
// Every request writes exactly one audit entry, but for those deliberately
// not audited: a request to a public path (the health service), a request
// refused because the server is stopping, and an allowed scrape of
// /metrics. The entry is the front step's refusal; the router's for a path
// nothing serves; the check's refusal; the handler's for an allowed request,
// or, when an allowed request ends without one (its message could not be
// read or came too late, the handler failed, panicked, was canceled or ran
// past its deadline, or succeeded without recording), the one pass writes
// with a fixed reason; and, for a call the RPC library answered before the
// check (a wrong HTTP method, content type, codec, compression or timeout
// header), a failed one the front step writes. A refused scrape of /metrics
// is audited under its pattern. A caller who goes away during the check is
// recorded as having ended its request, never as a failure of the master.
//
// For a method whose requirement reads the request, the check that follows
// the message reads the key again: the message may take as long as the
// body's read bound to arrive, and the decision to allow is made on the key
// as it is then, revoked, expired or removed. Every other request reads its
// key once.
//
// The requirement of each method comes from one table, keyed by the full
// procedure name, which package internal/api owns and tests against the
// contract; the server takes it as Config.Methods. Where the requirement
// depends on the request (a role on the tenant the request names) the table
// says so, and only then is the request's message read before the check,
// and only for an authenticated caller. New refuses a method registered
// without an entry. Config.WithDecision hands each handler the check's
// Decision; the access operations take it, so a request is checked once.
// Package servertest holds a stand-in table (taken from the "Role:" lines,
// tested against the descriptors both ways) and stand-in handlers for tests.
//
// There is one way to add a route: the one connect.Server the server builds
// with its interceptor (Config.Register is handed it), Config.HTTPRoutes,
// each one exact path, wrapped in the middleware, and Config.PatternRoutes.
// A pattern route's requirement is computed for each request from its path
// (the tenant's name, given to the requirement only when it follows the
// rule of a tenant's name, so a malformed one is held to the role on the
// installation) and checked by pass like any other; only then are the
// path's other values read, and a path any of which is not a plain segment
// (percent-encoding, a dot segment, an empty or over-long segment) is
// answered 404, never redirected and never handed to its handler. No two
// routes may match one path, but for one path served under two methods. The router serves only the
// paths recorded at New; a handler registered any other way is never
// reached. The route test sends every pattern registered a request without
// a key, which must be refused, and one from the local operator, which must
// be served through the request path.
//
// # The envelope
//
// Around everything, for every request, the envelope deletes a client's own
// Nopacs-Request-Id header, makes the request's id and sets it on the
// response (so every answer carries it: success, refusal, error, panic, and
// the RPC library's own errors); refuses new requests once Stop has begun;
// counts requests in flight for the drain; applies the deadlines and the
// body's read bound; and logs and counts every answer the request path did
// not, so each request id on the wire is in the log.
//
// It also answers pages in a browser (cors.go). The server is closed to
// them by default: only an origin the installation's api.cors-origins
// lists exactly, or every origin when it is "*", gets
// Access-Control-Allow-Origin and the exposed headers, on every answer, a
// refusal included; its preflight is answered 204 with the methods and
// headers the clients send, before the front step and unaudited, since it
// carries no key. Any other origin gets no CORS header, and its preflight
// is refused like any request without a key. Every answer carries Vary:
// Origin; none carries Access-Control-Allow-Credentials.
//
// # Listeners
//
// The local socket, <data-dir>/run/nopacs.sock, speaks HTTP/2 without TLS
// (h2c) and HTTP/1.1. Its directory is private to the server's user, and on
// Linux and macOS each connection's peer user is read from the kernel
// (SO_PEERCRED, LOCAL_PEERCRED) and must be the server's own effective
// user, or the connection is closed before a byte is read. That caller is
// the local operator, admin everywhere. A socket file left by a process
// that is gone is removed only after a dial shows nothing answers on it.
//
// On Windows the peer's user is not read: Windows offers no peer
// credentials for AF_UNIX sockets, only the peer's process id
// (SIO_AF_UNIX_GETPEERPID), which can be reused by another process before it
// is looked up and so does not reliably name the caller. The guard on
// Windows is the directory alone: <data-dir>\run is given, at every start, a
// protected access list that grants the server's user and nobody else, and
// the socket inherits it. Any process of that user can use the socket. Also
// on Windows: the directory's owner is not checked (the access list is set,
// not read); the lock is an exclusive open of the lock file, so another
// program that holds the file open (an antivirus, a backup) can make a
// start look like a second server; and closing the console gives a process
// about five seconds, which may cut the drain short.
//
// The network listener exists only when Config.Listen is given. With a
// certificate it speaks TLS 1.2 or later with HTTP/2 and HTTP/1.1. Without
// one, only a loopback address written as an address is accepted, and it
// speaks h2c and HTTP/1.1. The certificate is read at New, where a pair that
// does not load stops the server, and read again on a handshake at most
// every five seconds: a certificate renewed in its files is served from
// then on, with no restart; files that change into a pair that does not
// load are logged and the last good pair is still served.
//
// # Limits
//
// A message over 4 MiB is refused, read or sent (RESOURCE_EXHAUSTED), sent
// counted in the request's encoding; a call without a deadline gets 30
// seconds, and no call runs longer than 5 minutes, whatever its client
// asks, except the long methods (Config.LongMethods), which stream what may
// take hours and have no deadline, a client that reads nothing of their
// response for 5 minutes (StallTimeout) being cut instead; the body of a
// request whose requirement reads it must arrive within
// 10 seconds, before the check (only an authenticated caller's body is
// read before the check); headers are bounded to 64 KiB, HTTP/2
// streams to 100 per connection, the time to send headers to 10 seconds, an
// idle connection to 2 minutes. The health service serves Check only:
// Watch, a public stream that never ends, answers UNIMPLEMENTED. Each number
// has its reason at its constant in server.go. An HTTP/1.1 connection whose
// request was never authenticated is closed after its answer, and the
// network listener holds at most the process's file limit less a reserve
// of 64 connections at once (connlimit.go), so the local socket and the
// databases always have file descriptors left. So a caller without a key
// holds a request no longer than it takes to send its headers, at most 10
// seconds: it is refused before its body is read, on every path alike, and
// never reaches the local operator's way in.
//
// Known and accepted in this version: a flood of requests with bad keys is
// answered and audited at about 16,000 refusals a second, and makes the
// master's audit log grow by about 6 MB a second, until the audit writer's
// bounded queue drops entries, which it counts. Both numbers were measured on
// one development laptop and are an order of magnitude, not a promise. The
// limits on failed calls below hold such a flood from one address: past the
// installation's access.failure-limit its refusals are held a second each,
// and at twice the limit it is blocked.
//
// # Where a request comes from
//
// The front step judges once, before any key is read, where a request comes
// from (caller.go): the connection's peer, or, when the peer is one of the
// installation's access.trusted-proxies, the rightmost address that is not
// a trusted proxy itself in the one header access.forwarded-header names
// (X-Forwarded-For by default, or Forwarded, RFC 7239); the other header is
// never read, since a proxy that appends to one passes a client's copy of
// the other through. That address is what the audit entry, the log line and a key's
// last use record, and what every network rule judges. A request from one
// of access.denied-networks, or from outside access.allowed-networks when
// it lists any, is refused there and recorded with the rule's name (the
// access package judges a key's and a tenant's networks later, in the
// check). Every refusal answers as a key that is no good does.
//
// The limits on failed calls (failures.go, guard.go) count only refusals:
// per address (an IPv6 address by its /64) the requests refused for their
// credential, per key the requests of a good key refused for its grants.
// Past access.failure-limit in access.failure-window a refusal is held a
// second before it is answered; at twice the limit the address or the key is
// blocked for access.block-time, and every request from it is refused and
// audited, the block's start raised as an event. A request that succeeds is
// never counted nor held. The counts live in this process's memory, bounded
// in size; a restart forgets them. The local socket is judged by none of
// this, and does not read the rules at all.
//
// # One server per data directory
//
// TakeLock takes an operating system file lock (flock on Linux and macOS,
// an exclusive open on Windows) on <data-dir>/run/serve.lock and refuses a
// second server, naming the first one's process. The system drops the lock
// when the process ends however it ends, so a kill with signal 9 leaves
// nothing stale.
//
// # The installation's identity
//
// Init writes a random id into the data directory (IDFileName) and into the
// master's settings store (InstallationIDSetting), and serve refuses a master
// that does not hold the same id, before migrating anything, so a mistyped
// master address cannot start a server on an empty or foreign master. The
// id's name is under settings.ReservedPrefix, which no setting of the
// server's list may use, so the settings service neither lists, reads nor
// changes it.
//
// Init also writes a key check beside the id (KeyCheckSetting): the id
// sealed with the installation's key file. Serve opens it with the key file
// it was given, after the identity and before it migrates, opens a tenant
// or listens, and refuses to start when it does not open
// (MessageWrongKeyFile) or is missing (MessageNoKeyCheck). A server running
// with another key file would seal a new tenant's address with the wrong
// key, leaving an installation that no one key file can open. The check
// opens with any key file that still holds the version that sealed it, so
// adding a newer version for rotation does not refuse a start.
//
// # Stopping
//
// Stop refuses new requests (the health service's Check still answers,
// NOT_SERVING), lets those in flight finish within the drain deadline, then
// cancels them and gives their handlers a grace period; a handler still
// running after that is abandoned and counted in Stop's error. Then it
// closes, each within its own deadline, the access package (which writes
// the audit entries it holds), the tenant registry and the master database,
// and releases the lock. The worst case of a whole Stop is drain + grace +
// 3 x close, 29 seconds with the defaults.
//
// # What it must never do
//
// It must never serve a route outside the request path, log anything
// derived from a request or a response that has not passed through package
// redact (nor anything from a request before the check has allowed it), log
// the Authorization header at any level, show a caller the cause of an
// internal fault, or leave a goroutine running after Stop returns, except a
// handler that ignored its context and was abandoned.
package server
