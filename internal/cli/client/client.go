package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/fault"
)

// RequestIDHeader is the response header in which the server returns the id
// it made for the request, on success and on error alike; the contract says a
// client cannot set it.
const RequestIDHeader = "Nopacs-Request-Id"

// maxMessage is the contract's limit on a message, 4 MB. Setting it here too
// means an answer larger than the server may send is refused rather than read
// into memory.
const maxMessage = 4 << 20

// Client holds the service clients of the API over one transport.
type Client struct {
	// Tenants is the TenantService.
	Tenants nopacsv1connect.TenantServiceClient
	// Access is the AccessService: keys, grants and WhoAmI.
	Access nopacsv1connect.AccessServiceClient
	// Settings is the SettingsService.
	Settings nopacsv1connect.SettingsServiceClient
	// Audit is the AuditService.
	Audit nopacsv1connect.AuditServiceClient
	// Storages is the StorageService.
	Storages nopacsv1connect.StorageServiceClient
	// Receivers is the ReceiverService.
	Receivers nopacsv1connect.ReceiverServiceClient
	// Receptions is the ReceptionService.
	Receptions nopacsv1connect.ReceptionServiceClient
	// Quarantine is the QuarantineService.
	Quarantine nopacsv1connect.QuarantineServiceClient
	// Monitor is the MonitorService.
	Monitor nopacsv1connect.MonitorServiceClient
	// Catalog is the CatalogService.
	Catalog nopacsv1connect.CatalogServiceClient
	// Metadata is the MetadataService.
	Metadata nopacsv1connect.MetadataServiceClient
	// Data is the DataService.
	Data nopacsv1connect.DataServiceClient
	// Senders is the SenderService.
	Senders nopacsv1connect.SenderServiceClient
	// Jobs is the JobService.
	Jobs nopacsv1connect.JobServiceClient
	// Profiles is the ProfileService.
	Profiles nopacsv1connect.ProfileServiceClient
	// Deid is the DeidService.
	Deid nopacsv1connect.DeidServiceClient
	// AuditSeal is the AuditSealService.
	AuditSeal nopacsv1connect.AuditSealServiceClient
	// Corrections is the CorrectionService.
	Corrections nopacsv1connect.CorrectionServiceClient
	// Replicas is the ReplicaService.
	Replicas nopacsv1connect.ReplicaServiceClient
	// Retention is the RetentionService.
	Retention nopacsv1connect.RetentionServiceClient
	// Routes is the RouteService.
	Routes nopacsv1connect.RouteServiceClient
	// Transfers is the TransferService.
	Transfers nopacsv1connect.TransferServiceClient
	// Backups is the BackupService.
	Backups nopacsv1connect.BackupServiceClient

	timeout time.Duration
	target  string
	trust   string
	http    *http.Client
	base    string
	key     string
}

// New makes a client for a configuration. It does not connect: the first call
// does, so a command that fails on its own arguments never touches the
// network.
func New(cfg Config) (*Client, error) {
	timeout := cfg.Timeout
	if timeout < 0 {
		return nil, fault.New(fault.Invalid, "the timeout must not be negative")
	}
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	var (
		hc       *http.Client
		base     string
		target   string
		trust    string
		interc   []connect.ClientInterceptor
		bearerK  string
		haveData = cfg.DataDir != ""
		haveSrv  = cfg.Server != ""
	)
	switch {
	case haveData == haveSrv:
		return nil, fault.New(fault.Invalid, "give either a data directory (--data-dir or "+EnvDataDir+") or a server (--server or "+EnvServer+")")
	case haveData:
		target = SocketPath(cfg.DataDir)
		hc = unixClient(target)
		// The host is a placeholder: the dialer ignores it and always opens
		// the socket. HTTP/2 without TLS is what the server speaks there.
		base = "http://nopacs.local"
	default:
		u, err := ParseServer(cfg.Server)
		if err != nil {
			return nil, err
		}
		key, err := ReadKey(cfg.KeyFile, cfg.getenv())
		if err != nil {
			return nil, err
		}
		target = u.Scheme + "://" + u.Host
		var roots *x509.CertPool
		trust = "this system"
		if cfg.CAFile != "" {
			trust = "the CA file " + cfg.CAFile
			if roots, err = LoadCAs(cfg.CAFile); err != nil {
				return nil, err
			}
		}
		var certs []tls.Certificate
		if cfg.ClientCert != "" || cfg.ClientKey != "" {
			if u.Scheme != "https" {
				return nil, fault.New(fault.Invalid, "a client certificate is presented only over https; the server address is "+u.Scheme+"://"+u.Host)
			}
			pair, err := LoadClientCert(cfg.ClientCert, cfg.ClientKey)
			if err != nil {
				return nil, err
			}
			certs = []tls.Certificate{pair}
		}
		hc = networkClient(u.Scheme == "https", roots, certs)
		base = u.String()
		bearerK = key
		interc = append(interc, bearer(key))
	}
	c := connect.NewClient(
		connecthttp.NewTransport(hc, base,
			connecthttp.WithReadMaxBytes(maxMessage),
			connecthttp.WithSendMaxBytes(maxMessage)),
		interc...)
	return &Client{
		Tenants:    nopacsv1connect.NewTenantServiceClient(c),
		Access:     nopacsv1connect.NewAccessServiceClient(c),
		Settings:   nopacsv1connect.NewSettingsServiceClient(c),
		Audit:      nopacsv1connect.NewAuditServiceClient(c),
		Storages:   nopacsv1connect.NewStorageServiceClient(c),
		Receivers:  nopacsv1connect.NewReceiverServiceClient(c),
		Receptions: nopacsv1connect.NewReceptionServiceClient(c),
		Quarantine: nopacsv1connect.NewQuarantineServiceClient(c),
		Monitor:    nopacsv1connect.NewMonitorServiceClient(c),
		Catalog:    nopacsv1connect.NewCatalogServiceClient(c),
		Metadata:   nopacsv1connect.NewMetadataServiceClient(c),
		Data:       nopacsv1connect.NewDataServiceClient(c),
		Senders:    nopacsv1connect.NewSenderServiceClient(c),
		Jobs:       nopacsv1connect.NewJobServiceClient(c),
		Profiles:   nopacsv1connect.NewProfileServiceClient(c),
		Deid:       nopacsv1connect.NewDeidServiceClient(c),
		AuditSeal:  nopacsv1connect.NewAuditSealServiceClient(c),
		// The correction service's client.
		Corrections: nopacsv1connect.NewCorrectionServiceClient(c),
		Backups:     nopacsv1connect.NewBackupServiceClient(c),
		Replicas:    nopacsv1connect.NewReplicaServiceClient(c),
		Retention:   nopacsv1connect.NewRetentionServiceClient(c),
		Routes:      nopacsv1connect.NewRouteServiceClient(c),
		Transfers:   nopacsv1connect.NewTransferServiceClient(c),
		base:        strings.TrimSuffix(base, "/"),
		key:         bearerK,
		timeout:     timeout,
		target:      target,
		trust:       trust,
		http:        hc,
	}, nil
}

// Target says where the client's calls go: the path of the local socket, or the
// scheme and host of the server. It holds no key and no header.
func (c *Client) Target() string { return c.target }

// Close releases the connections the client holds.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// bearer sends the key as RFC 6750 says, in the Authorization header. It is an
// interceptor so that no call site can forget it or write it to a log.
func bearer(key string) connect.ClientInterceptor {
	return func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				info.RequestHeader().Set("Authorization", "Bearer "+key)
			}
			return next(ctx, spec)
		}
	}
}

// unixClient is an HTTP client whose every connection is the unix socket at
// path, speaking HTTP/2 without TLS (prior knowledge), as the server's local
// listener does. AF_UNIX works on Windows too, so the same code serves both.
func unixClient(path string) *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
			Protocols: protocols,
		},
		CheckRedirect: noRedirect,
	}
}

// networkClient is an HTTP client for the network: TLS 1.2 or later with the
// system's authorities, or only the authorities in roots when it is given,
// presenting certs, the client certificate, when the server asks for one,
// and HTTP/2 where the server offers it. useTLS is false only for a loopback
// http address.
//
// The floor is TLS 1.2, not 1.3, on purpose: 1.0 and 1.1 are deprecated
// (RFC 8996) and refused, but servers behind the proxies and appliances that
// institutions still run often speak 1.2 and no later, and a command line
// that cannot reach them is no safer for it. 1.2 with the AEAD suites Go
// offers is sound; the server may raise its own floor.
func networkClient(useTLS bool, roots *x509.CertPool, certs []tls.Certificate) *http.Client {
	tr := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: true,
	}
	if useTLS {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: certs}
	}
	return &http.Client{Transport: tr, CheckRedirect: noRedirect}
}

// noRedirect refuses to follow a redirect: the Authorization header would go
// with it to wherever it points, and the API does not redirect.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// CallError is the error of a failed call. It is a fault error (so
// fault.KindOf and the exit code work through it) that also knows which call
// failed and the request id the server made for it.
type CallError struct {
	// What names the call, such as "tenant create", for the message.
	What string
	// RequestID is the server's id for the request; empty when no answer came.
	RequestID string
	err       error
}

// Error returns the message and, when there is one, the request id.
func (e *CallError) Error() string {
	msg := e.What + ": " + fault.Message(e.err)
	if e.RequestID != "" {
		msg += " (request id " + e.RequestID + ")"
	}
	return msg
}

// Unwrap returns the fault error, so fault.KindOf sees its kind.
func (e *CallError) Unwrap() error { return e.err }

// Call runs one API call with the client's deadline and returns its answer. On
// failure it returns a *CallError holding the kind from the table in the
// codes package, the server's message and the request id from the response
// header. The id is read from the response whether the call succeeded or not,
// because the contract has the server send it either way.
func Call[T any](ctx context.Context, c *Client, what string, fn func(context.Context) (*T, error)) (*T, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ctx, info := connect.NewClientContext(ctx)
	res, err := fn(ctx)
	if err == nil {
		return res, nil
	}
	return nil, &CallError{
		What:      what,
		RequestID: info.ResponseHeader().Get(RequestIDHeader),
		err:       c.classify(ctx, err),
	}
}

// Watch opens a stream of events with req and calls each with every message
// until it returns false, the stream ends or ctx does. A stream has no
// deadline of its own: it lasts as long as ctx. Its error, if it ended with
// one, is a *CallError as a call's is; a stream ended by each, or by ctx, is
// nil.
func (c *Client) Watch(ctx context.Context, req *v1.WatchRequest, each func(*v1.WatchResponse) bool) error {
	ctx, info := connect.NewClientContext(ctx)
	fail := func(err error) error {
		if ctx.Err() != nil {
			return nil
		}
		return &CallError{What: "watch", RequestID: info.ResponseHeader().Get(RequestIDHeader), err: c.classify(ctx, err)}
	}
	s, err := c.Monitor.Watch(ctx, req)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = s.Close() }()
	for {
		m, err := s.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fail(err)
		}
		if !each(m) {
			return nil
		}
	}
}

// noAnswerPrefix starts the message of a call the client's own deadline ended.
const noAnswerPrefix = "no answer from "

// NoAnswer reports whether err is a call that got no answer because the
// client's deadline ran out, as opposed to an answer the server sent: a
// server's refusal, even one of the unavailable kind, is not this.
func NoAnswer(err error) bool {
	return fault.KindOf(err) == fault.Unavailable && strings.HasPrefix(fault.Message(err), noAnswerPrefix)
}

// classify turns the error of a call into a fault error. An API error maps
// through the codes table. Anything else never reached an answer: a refused
// connection, a certificate the system does not trust, a deadline. Each is
// said in words, with the target but never with a key or a header.
//
// A deadline is told before anything else. The server learns the deadline from
// the request and may answer first, with a message of its own about an
// abandoned call; the person's own --timeout is what ran out, so that is what
// the line says, whichever side noticed first.
func (c *Client) classify(ctx context.Context, err error) error {
	var ce *connect.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &ce) && ce.Code() == connect.CodeDeadlineExceeded) {
		return fault.Wrap(fault.Unavailable, err, noAnswerPrefix+c.target+" within "+c.timeout.String()+" (see --timeout)")
	}
	if errors.As(err, &ce) && ce.IsRemote() {
		return remoteError(ce)
	}
	var (
		unknown x509.UnknownAuthorityError
		invalid x509.CertificateInvalidError
		host    x509.HostnameError
		op      *net.OpError
	)
	switch {
	case errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &host):
		return fault.Wrap(fault.Unavailable, err, "refusing to connect to "+c.target+": its certificate is not trusted by "+c.trust+" ("+tlsReason(err)+")")
	case errors.As(err, &op):
		return fault.Wrap(fault.Unavailable, err, "cannot reach the server at "+c.target+": "+op.Err.Error())
	}
	if ce != nil {
		return remoteError(ce)
	}
	return fault.Wrap(fault.Unavailable, err, "the call to "+c.target+" failed: "+err.Error())
}

// remoteError turns an API error into a fault error whose message says what
// the server's details add: the reason (STORAGE_SWITCHING, NOT_ONLINE, HELD,
// RETENTION_MINIMUM, ...) when the message does not start with it, and how long
// to wait before trying again when the server gave a delay. A person who reads
// only the line then knows whether to wait, to change something, or to stop.
// A code's general reason (NOT_FOUND, ...) is not written: it says no more
// than the code, which the exit status already tells.
func remoteError(ce *connect.Error) error {
	err := codes.FromConnect(ce)
	msg := fault.Message(err)
	reason, retry := ReasonOf(ce)
	if reason != "" && !codes.IsGeneral(reason) && !strings.HasPrefix(msg, reason) {
		msg = reason + ": " + msg
	}
	if retry > 0 {
		msg += " (try again in " + retry.Round(time.Second).String() + ")"
	}
	if msg == fault.Message(err) {
		return err
	}
	return fault.Wrap(fault.KindOf(err), ce, msg)
}

// IsNotServed says whether an error is the server's answer that it does not
// serve the method called yet (UNIMPLEMENTED), as opposed to a refusal of what
// was asked.
func IsNotServed(err error) bool {
	var ce *connect.Error
	return errors.As(err, &ce) && ce.Code() == connect.CodeUnimplemented
}

// OrNotServed turns the server's answer that it does not serve a method yet
// (UNIMPLEMENTED) into one plain error that names what was asked and says that
// nothing was changed. Any other error passes as it is. Only code for methods
// the contract defines before a server serves them uses it, so a served method
// that answers UNIMPLEMENTED by mistake is still shown as the server worded it.
func OrNotServed(what string, err error) error {
	if err == nil || !IsNotServed(err) {
		return err
	}
	return fault.New(fault.Internal, what+": not served: this server does not serve it yet; nothing was changed")
}

// ReasonOf returns the reason and the retry delay an API error carries in its
// google.rpc.ErrorInfo and google.rpc.RetryInfo details, found anywhere in the
// chain of err: empty and zero for an error that carries none, or one from
// another domain than the server's.
func ReasonOf(err error) (reason string, retry time.Duration) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return "", 0
	}
	if r, ok := codes.ReasonOfError(ce); ok && r.Domain == codes.Domain {
		reason = r.Reason
	}
	if d, ok := codes.RetryOfError(ce); ok {
		retry = d
	}
	return reason, retry
}

// Detail is what an API error found in the chain of an error says beyond
// its message: the code it was answered with, its google.rpc.ErrorInfo and
// its retry delay.
type Detail struct {
	// Code is the API code the server answered.
	Code connect.Code
	// Reason and Metadata are the ErrorInfo's, when it is of the server's
	// domain; Reason is empty and Metadata nil otherwise.
	Reason   string
	Metadata map[string]string
	// Retry is the RetryInfo's delay, zero for none.
	Retry time.Duration
}

// DetailOf returns the Detail of the API error found anywhere in the chain
// of err, and false when err holds no API error (a failure before an
// answer came: a connection, a certificate, a deadline of this program).
func DetailOf(err error) (Detail, bool) {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return Detail{}, false
	}
	d := Detail{Code: ce.Code()}
	if r, ok := codes.ReasonOfError(ce); ok && r.Domain == codes.Domain {
		d.Reason, d.Metadata = r.Reason, r.Metadata
	}
	if retry, ok := codes.RetryOfError(ce); ok {
		d.Retry = retry
	}
	return d, true
}

// tlsReason is the short text of a certificate failure, without the long
// prefix the standard library puts in front of it.
func tlsReason(err error) string {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	switch {
	case errors.As(err, &unknown):
		return "signed by an unknown authority"
	case errors.As(err, &host):
		return "not valid for the address used"
	case errors.As(err, &invalid):
		return invalid.Error()
	}
	return err.Error()
}
