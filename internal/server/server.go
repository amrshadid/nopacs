package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/grpchealth/v2"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/obs"
)

// The limits of the server (access.proto, "Limits"). Each is a number the RPC
// library or net/http would otherwise leave unbounded or set for another
// purpose.
const (
	// MaxMessageBytes is the largest message read or sent: 4 MiB, which is
	// what a stock gRPC client accepts by default (grpc-go's
	// defaultClientMaxReceiveMessageSize), so a stock client never gets an
	// answer it refuses, and what bounds the memory one message can cost.
	MaxMessageBytes = 4 << 20
	// DefaultDeadline is applied to a call whose client sent no deadline,
	// so no call holds a handler, a connection and a database slot
	// forever.
	DefaultDeadline = 30 * time.Second
	// MaxDeadline is the longest any call may run, whatever deadline its
	// client asks for: 5 minutes. Without it, a client that sends a long
	// deadline, or a handler that waits, holds a connection, a goroutine and
	// a file descriptor as long as it likes. A client's shorter deadline is
	// kept. The long methods (Config.LongMethods: streams and downloads that
	// may take hours) have no deadline and are bound by StallTimeout
	// instead.
	MaxDeadline = 5 * time.Minute
	// BodyReadTimeout bounds the time to read a request's body until the
	// check has allowed the request: 10 seconds, ample for a message of at
	// most 4 MiB on any working link. A method whose requirement depends on
	// the request reads its first message before the check, and without
	// this a caller with no key could trickle a body and hold the request
	// unchecked; when it runs out the caller is refused (unauthenticated,
	// denied) or told its request was invalid, in the usual order.
	BodyReadTimeout = 10 * time.Second
	// StallTimeout is how long the client of a long method may leave the
	// response unread: a write or flush that cannot finish within it ends the
	// response, so a client that stops reading does not hold the handler and
	// a storage connection for ever, though the call has no deadline.
	StallTimeout = 5 * time.Minute
	// DefaultDrain is how long Stop waits for requests in flight before it
	// cancels them: longer than one call under the default deadline is
	// likely to need, and with the grace and the closes below it keeps the
	// whole stop under the 30 seconds an orchestrator waits before a kill
	// (Kubernetes' default grace period).
	DefaultDrain = 15 * time.Second
	// StopGrace is how long Stop waits, after canceling the requests still
	// running at the drain's end, for their handlers to return. A handler
	// that ignores its context is then given up on, and the close sequence
	// goes on without it.
	StopGrace = 2 * time.Second
	// DefaultClose is how long Stop gives each of the access package (to
	// write the audit entries it holds), the registry and the master to
	// close, and Config.AfterDrain to run. The worst case of a whole stop
	// is DefaultDrain + StopGrace + 4 x DefaultClose = 15 + 2 + 16 = 33
	// seconds.
	DefaultClose = 4 * time.Second

	// maxHeaderBytes bounds a request's headers (HTTP/1 and HTTP/2): 64
	// KiB. A request needs a few hundred bytes (a key is 64 characters);
	// gRPC's own default for metadata is 8 KiB, and net/http's 1 MiB
	// default would let each connection make the server hold a megabyte
	// before any check.
	maxHeaderBytes = 64 << 10
	// maxConcurrentStreams bounds the requests one HTTP/2 connection may
	// have open at once: 100, the number gRPC servers and the HTTP/2
	// specification's advice use, so one client cannot open thousands of
	// handlers over one connection.
	maxConcurrentStreams = 100
	// readHeaderTimeout bounds the time a client takes to send a request's
	// headers, so a client that opens connections and sends slowly
	// (slowloris) holds each for 10 seconds at most.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes a connection that has no request for two minutes.
	idleTimeout = 2 * time.Minute
	// pingTimeout closes an HTTP/2 connection whose peer does not answer a
	// ping within 15 seconds, so a vanished client does not hold a stream.
	pingTimeout = 15 * time.Second
)

// MetricsPattern is the route of the metrics.
const MetricsPattern = "GET /metrics"

// Checker is what the server needs of the access package: the judgment of a
// credential before routing, the check, the recording of an allowed read and
// of a request no check took up, the counts it reports in the metrics, and
// Close, which writes the audit entries it holds. *access.Access is one.
type Checker interface {
	Authenticate(ctx context.Context, cred access.Credential, facts access.Request) (access.Credential, error)
	Check(ctx context.Context, cred access.Credential, req access.Requirement, facts access.Request) (access.Decision, error)
	Record(d access.Decision, outcome master.Outcome, reason string, facts access.Facts)
	RecordUnchecked(cred access.Credential, facts access.Request, reason string)
	RecordRefusal(facts access.Request, keyID, rule string, blocked bool, until time.Time) error
	AuditStats() access.AuditStats
	MarkStats() access.MarkStats
	Close(ctx context.Context) error
}

// Registry is what the server needs of the tenant registry: Close, at stop,
// within the close deadline. *registry.Registry is one; it is an interface
// so the server does not depend on the registry's package.
type Registry interface {
	Close(ctx context.Context) error
}

// Config is what New needs. Access and Register are required, and Lock
// unless the server is started with no local socket (tests only).
type Config struct {
	// DataDir is the installation's data directory; the local socket is
	// <DataDir>/run/nopacs.sock.
	DataDir string
	// Lock is the hold on DataDir taken by TakeLock. The server releases it
	// last, after everything else is closed.
	Lock *Lock
	// Listen is the network address, HOST:PORT; empty for no network
	// listener. Without TLSCert and TLSKey only a loopback address is
	// accepted.
	Listen string
	// TLSCert and TLSKey are the PEM files of the network listener's
	// certificate and private key; both or neither.
	TLSCert, TLSKey string
	// NoSocket leaves out the local socket. Only tests set it.
	NoSocket bool

	// Access is the check; the server closes it at stop.
	Access Checker
	// Registry is the tenant registry, or nil; the server closes it at stop.
	Registry Registry
	// TenantsByState returns how many tenants are in each state, for
	// /metrics; nil leaves the family out.
	TenantsByState func() map[string]int
	// Master is closed after the registry, at stop; nil for none.
	Master io.Closer
	// AfterDrain runs once at stop, within the close deadline, after the
	// requests have drained and the access check has written the audit
	// entries it held, and before the registry and the master close: the
	// audit's last seal, which so seals every entry the server wrote. Nil
	// for none.
	AfterDrain func(ctx context.Context) error
	// Counters and Gauges are values other parts keep, for /metrics, read
	// at each scrape; nil for none.
	Counters func() []obs.Counter
	// Gauges: see Counters.
	Gauges func() []obs.Gauge
	// Log receives the server's lines; nil discards them.
	Log *slog.Logger

	// Register registers the API's services on the server's
	// connect.Server, such as
	// nopacsv1connect.RegisterTenantServiceHandler(s, h). That
	// connect.Server is built with the request path's interceptor, so every
	// method registered on it is checked; it is the only connect.Server the
	// server mounts. Every method registered must be known to Methods.
	Register func(s *connect.Server)
	// Methods is the method-to-requirement table (package internal/api's).
	// Nil knows no method: then only the health service and methods nobody
	// registered are served, and New refuses any registered method.
	Methods Methods
	// WithDecision hands each handler the check's Decision in its context;
	// nil leaves it to CallFrom.
	WithDecision WithDecision
	// HTTPRoutes are plain HTTP routes besides /metrics, each wrapped in the
	// request path.
	HTTPRoutes []HTTPRoute
	// PatternRoutes are plain HTTP routes inside one tenant, whose
	// requirement is computed from each request's path (PatternRoute).
	PatternRoutes []PatternRoute
	// LongMethods names the calls that have no deadline, neither the default
	// nor the maximum, since they stream what may take hours: procedures
	// ("/package.Service/Method") and pattern routes' patterns, as given in
	// PatternRoutes. Each must name a served procedure or route, so a
	// renamed route cannot silently lose its exemption. Their responses are
	// bound by StallTimeout instead, and a long procedure's messages are
	// sent uncompressed (uncompressedLong).
	LongMethods []string
	// CORSOrigins returns the web origins whose pages may call the API
	// from a browser (the installation's api.cors-origins); the server
	// reads it at most every corsRefresh. Nil allows no origin.
	CORSOrigins func(ctx context.Context) (settings.Origins, error)
	// AccessRules returns the installation's access settings (trusted
	// proxies, allowed and denied networks, limits on failed calls); the
	// server reads it at most every rulesRefresh. Nil applies
	// DefaultAccessRules: no proxy trusted, every network allowed.
	AccessRules func(ctx context.Context) (AccessRules, error)

	// DefaultDeadline, MaxDeadline and BodyReadTimeout replace the
	// constants of the same names; zero keeps them. Tests shorten them.
	DefaultDeadline, MaxDeadline, BodyReadTimeout time.Duration
	// Drain, Grace and Close replace DefaultDrain, StopGrace and
	// DefaultClose; zero keeps them.
	Drain, Grace, Close time.Duration
}

// Server serves the API on the local socket and, when asked, on the
// network. It owns its goroutines: each listener's serve loop, which Stop
// ends and waits for.
type Server struct {
	cfg         Config
	access      Checker
	log         *slog.Logger
	metrics     *obs.Metrics
	deadline    time.Duration
	maxDeadline time.Duration
	// stall is the write stall bound of long methods; longProcs and
	// longPatterns are the long methods by procedure and by pattern.
	stall        time.Duration
	longProcs    map[string]bool
	longPatterns map[string]bool
	bodyRead     time.Duration
	methods      Methods
	withDecision WithDecision
	health       *grpchealth.StaticChecker
	// certs is the network listener's certificate, read again from its
	// files when they change; nil without TLS.
	certs   *certFiles
	mux     *routeMux
	handler http.Handler
	// procs maps each served procedure, "/package.Service/Method", to its
	// handler from the one checked connect.Server; the router dispatches a
	// call by exact match against it and against plain.
	procs map[string]http.Handler
	// plain maps the path of each plain HTTP route to its pattern.
	plain map[string]string
	// patterns are the pattern routes, in the order given, and
	// patternHeads the literal first segments of their paths, so a path
	// under none of them is not split.
	patterns     []*patternRoute
	patternHeads map[string]bool
	// cors is the policy of answers to pages in a browser.
	cors *corsPolicy
	// guard holds the access rules and the counts of failed calls.
	guard *guard
	// hook, set only by tests before Start, is called at the points named
	// by the point constants (Server.at), to stop a request there.
	hook func(ctx context.Context, point string)

	stopMu    sync.RWMutex
	stopping  bool
	inFlight  sync.WaitGroup
	inFlightN atomic.Int64

	// baseCtx is the context of every request; Stop cancels it when the
	// drain's deadline passes, so a handler still running sees its context
	// end.
	baseCtx    context.Context
	baseCancel context.CancelFunc
	servers    []*http.Server
	netAddr    net.Addr
	socketUser string
	serving    sync.WaitGroup
	serveErrs  chan error
	started    bool
	stopOnce   sync.Once
	stopErr    error
}

// New checks cfg and builds the server's routes. It opens nothing: Start
// listens. It refuses a method registered without an entry in the
// method-to-requirement table, so no method can be served unchecked.
func New(cfg Config) (*Server, error) {
	if cfg.Access == nil || cfg.Register == nil {
		return nil, fault.New(fault.Internal, "server: an access check and the API's services are required")
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, fault.New(fault.Invalid, "server: --tls-cert and --tls-key go together; give both or neither")
	}
	if cfg.Listen == "" && cfg.TLSCert != "" {
		return nil, fault.New(fault.Invalid, "server: --tls-cert and --tls-key need --listen")
	}
	if cfg.Listen != "" {
		if err := checkListen(cfg.Listen, cfg.TLSCert != ""); err != nil {
			return nil, err
		}
	}
	if !cfg.NoSocket && (cfg.DataDir == "" || cfg.Lock == nil) {
		return nil, fault.New(fault.Internal, "server: the local socket needs the data directory and its lock")
	}
	s := &Server{
		cfg:          cfg,
		access:       cfg.Access,
		log:          cfg.Log,
		deadline:     orDefault(cfg.DefaultDeadline, DefaultDeadline),
		maxDeadline:  orDefault(cfg.MaxDeadline, MaxDeadline),
		stall:        StallTimeout,
		bodyRead:     orDefault(cfg.BodyReadTimeout, BodyReadTimeout),
		methods:      cfg.Methods,
		withDecision: cfg.WithDecision,
		health:       grpchealth.NewStaticChecker(),
		serveErrs:    make(chan error, 2),
		socketUser:   systemUser(),
		cors:         &corsPolicy{read: cfg.CORSOrigins, log: cfg.Log, now: time.Now},
	}
	if s.methods == nil {
		s.methods = noMethods{}
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
		s.cors.log = s.log
	}
	s.guard = newGuard(cfg.AccessRules, s.log)
	if cfg.TLSCert != "" {
		certs, err := loadCertFiles(cfg.TLSCert, cfg.TLSKey, s.log)
		if err != nil {
			return nil, err
		}
		s.certs = certs
	}
	s.metrics = obs.NewMetrics(obs.Sources{TenantsByState: cfg.TenantsByState, Counters: s.counters, Gauges: cfg.Gauges})
	if err := s.buildRoutes(); err != nil {
		return nil, err
	}
	s.baseCtx, s.baseCancel = context.WithCancel(context.Background())
	return s, nil
}

// counters are the counts the access package keeps, as metrics: what
// became of the audit entries of refusals and reads, and of the keys'
// last-use marks.
func (s *Server) counters() []obs.Counter {
	a, m := s.access.AuditStats(), s.access.MarkStats()
	c := func(name, help string, v uint64) obs.Counter {
		return obs.Counter{Name: "nopacs_" + name + "_total", Help: help, Value: v}
	}
	var more []obs.Counter
	if s.cfg.Counters != nil {
		more = s.cfg.Counters()
	}
	return append([]obs.Counter{
		c("audit_entries_written", "Audit entries of refusals and reads that a log took.", a.Written),
		c("audit_entries_failed", "Audit entries a log refused one by one, or that were not valid.", a.Failed),
		c("audit_entries_dropped_refusals", "Audit entries of refusals dropped because their queue was full or the server was closing.", a.DroppedRefusals),
		c("audit_entries_dropped_records", "Audit entries of allowed requests dropped because their queue was full or the server was closing.", a.DroppedRecords),
		c("audit_entries_dropped_writes", "Audit entries the writer gave up on: the database did not answer after the retries, or the close deadline passed.", a.DroppedWrites),
		c("audit_entries_rerouted", "Audit entries for a tenant's log written to the master's because the tenant's did not answer.", a.Rerouted),
		c("audit_entries_notices", "Audit entries the writer wrote to say entries were dropped.", a.Notices),
		c("key_last_use_marks_written", "Last-use marks of keys the database took.", m.Written),
		c("key_last_use_marks_dropped", "Last-use marks of keys dropped because their queue was full or the server was closing.", m.Dropped),
		c("key_last_use_marks_failed", "Last-use marks of keys the database refused or failed.", m.Failed),
	}, more...)
}

// buildRoutes makes the one connect.Server, registers the API's services
// and the health service on it, and records each of its procedures, and
// each plain HTTP route (the metrics and Config.HTTPRoutes, each in the
// middleware), as an exact path the router serves, and each pattern route
// (Config.PatternRoutes), which the router matches on the path as sent. The handler of every
// listener is then the envelope, around the front step, around the router:
// no route is chosen before the caller is known.
func (s *Server) buildRoutes() error {
	rpc := connect.NewServer(s.interceptor)
	// Check only: grpchealth serves Watch when its checker is also a
	// Watcher, and the wrapper hides that, so Watch answers UNIMPLEMENTED.
	// Watch is a stream that never ends, and it is public: any caller could
	// hold as many open as the system's file limit allows.
	grpchealth.Register(rpc, checkOnly{s.health})
	s.cfg.Register(rpc)
	var missing []string
	for spec := range rpc.Specs() {
		if _, ok := s.ruleFor(spec.Procedure); !ok && !s.pending(spec.Procedure) {
			missing = append(missing, spec.Procedure)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fault.New(fault.Internal, "server: methods registered without a requirement in the table: "+joinComma(missing))
	}

	s.mux = newRouteMux()
	s.procs = make(map[string]http.Handler)
	s.plain = make(map[string]string)
	connecthttp.Mount(procedureMux{s}, rpc,
		connecthttp.WithReadMaxBytes(MaxMessageBytes),
		connecthttp.WithSendMaxBytes(MaxMessageBytes),
		connecthttp.WithConditionalOptions(s.uncompressedLong),
	)
	routes := append([]HTTPRoute{{Pattern: MetricsPattern, Need: access.OnInstallation(master.RoleReader), Handler: s.serveMetrics}}, s.cfg.HTTPRoutes...)
	for _, r := range routes {
		if err := s.handleHTTP(r); err != nil {
			return err
		}
	}
	s.patternHeads = make(map[string]bool)
	for _, r := range s.cfg.PatternRoutes {
		if err := s.handlePattern(r); err != nil {
			return err
		}
	}
	if err := s.setLongMethods(); err != nil {
		return err
	}
	s.handler = s.envelope(s.front(http.HandlerFunc(s.route)))
	return nil
}

// uncompressedLong returns, for a long procedure, the option that sends
// every message of its answer uncompressed, whatever the client accepts. A
// long procedure streams archived bytes for as long as a subject takes,
// and gzip (RFC 1952) runs at some 80 MB/s per stream on DICOM files, a
// tenth of a gigabit link and a twentieth of what the stream otherwise
// sends; DICOM's compressed transfer syntaxes do not shrink at all. A
// compressed request is still read, and every other procedure compresses
// as the client asks.
func (s *Server) uncompressedLong(spec connect.Spec) []connecthttp.Option {
	if !slices.Contains(s.cfg.LongMethods, spec.Procedure) {
		return nil
	}
	return []connecthttp.Option{connecthttp.WithCompressMinBytes(MaxMessageBytes + 1)}
}

// procedureMux is what connecthttp.Mount registers on. It keeps each
// procedure's handler for the router, wrapped in withErrorInfo so the
// answers the library writes itself carry the google.rpc.ErrorInfo every
// answer does, and records its pattern; it drops the handler Mount adds for
// each service's subtree ("/package.Service/"): that
// is the RPC library's handler of unknown procedures, which would dispatch a
// path that is not exactly a procedure (a percent-encoded one, say) to a
// served handler with the request's stream shape instead of the method's,
// and refuse calls to unknown names on HTTP/1.1 before any check. A call to
// a procedure nothing serves is answered by the server itself (notServed).
type procedureMux struct{ s *Server }

// Handle keeps h as the handler of the procedure pattern, or drops it when
// pattern is a subtree or a method the table says is not served yet
// (PendingMethods), whose calls notServed answers.
func (m procedureMux) Handle(pattern string, h http.Handler) {
	if strings.HasSuffix(pattern, "/") || m.s.pending(pattern) {
		return
	}
	h = withErrorInfo(h)
	m.s.procs[pattern] = h
	m.s.mux.Handle(pattern, h)
}

// handleHTTP adds a plain HTTP route, wrapped in the request path. It is
// the only way a plain route is served: the router reaches the mux only for
// a path recorded here. The pattern must be one exact path (plainPattern),
// not a procedure's and not one already taken.
func (s *Server) handleHTTP(r HTTPRoute) (err error) {
	if r.Handler == nil || r.Pattern == "" {
		return fault.New(fault.Internal, "server: a route needs a pattern and a handler")
	}
	path, ok := plainPattern(r.Pattern)
	if !ok {
		return fault.New(fault.Internal, "server: the route "+r.Pattern+" is not one exact path")
	}
	if _, taken := s.procs[path]; taken {
		return fault.New(fault.Internal, "server: the route "+r.Pattern+" is a procedure's path")
	}
	if _, taken := s.plain[path]; taken {
		return fault.New(fault.Internal, "server: the route "+r.Pattern+" has a path already taken")
	}
	defer func() {
		// http.ServeMux panics on a pattern that is malformed or registered
		// twice; at setup that is a bug to report, not a crash.
		if p := recover(); p != nil {
			err = fault.New(fault.Internal, "server: the route "+r.Pattern+" cannot be registered")
		}
	}()
	s.mux.Handle(r.Pattern, s.middleware(r))
	s.plain[path] = r.Pattern
	return nil
}

// serveMetrics writes the metrics. An allowed scrape writes no audit entry:
// the metrics are counts with no tenant's content, and a scraper reads them
// every few seconds. A refused scrape is audited by the check, like any
// refusal.
func (s *Server) serveMetrics(_ context.Context, w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", obs.ContentType)
	return s.metrics.WriteText(w)
}

// checkOnly is a health checker that is not a grpchealth.Watcher, so the
// health service serves Check and answers Watch UNIMPLEMENTED.
type checkOnly struct{ c grpchealth.Checker }

// Check answers the health of the server, or of a service by name.
func (h checkOnly) Check(ctx context.Context, req *grpchealth.CheckRequest) (*grpchealth.CheckResponse, error) {
	return h.c.Check(ctx, req)
}

// Handler returns the handler every listener serves: the envelope around
// the mux. Tests serve it with httptest.
func (s *Server) Handler() http.Handler { return s.handler }

// Metrics returns the server's metrics.
func (s *Server) Metrics() *obs.Metrics { return s.metrics }

// orDefault returns d, or def when d is not positive.
func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// joinComma joins names with commas.
func joinComma(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
