package dimse

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The defaults of a Config.
const (
	// DefaultDrain is how long Stop lets the stores in flight run: long
	// enough for a large instance on a slow link to finish, short enough
	// that a stop or a restart after a change is not held for minutes.
	DefaultDrain = 30 * time.Second
	// DefaultMaxAssociations bounds the associations one receiver serves at
	// once; a caller past it is rejected as transient, local-limit-exceeded
	// (PS3.8 Table 9-21), and tries again.
	DefaultMaxAssociations = 64
	// maxPDUSize is the Maximum Length this side advertises (PS3.8 D.1):
	// 128 KiB, so a large instance crosses in few PDUs, while the two PDUs
	// the library holds per association stay small.
	maxPDUSize = 128 << 10
)

// Refusal says why the archive cannot take files now: a transient failure
// of the association vocabulary (tenant.FailureNearlyFull,
// tenant.FailureIncomingLimit, tenant.FailureTenantNotReady), or "" when it
// can. It is called for every association and every store, so it reads
// memory and never a disk or a network.
type Refusal func() tenant.AssociationFailure

// Report is told of each caller turned away, with why. It is called on the
// library's goroutine for the connection, so it must not block: the
// supervisor groups the failures and writes their events behind.
type Report func(failure tenant.AssociationFailure, caller tenant.GroupCaller)

// Config is what one listener is made from.
type Config struct {
	// Receiver is the receiver, a DIMSE receiver as stored.
	Receiver tenant.Receiver
	// BindAddress is the address to listen on: the receiver's own, or the
	// installation's default when it names none. Empty is every address of
	// both families: one socket on the IPv6 wildcard [::] that also takes
	// IPv4 callers (as IPv4-mapped addresses, which admission compares as
	// IPv4), after a check that no other program holds the port on the
	// IPv4 wildcard 0.0.0.0 (probeIPv4).
	BindAddress string
	// TLSDir is the tenant's TLS directory, where the receiver's TLS files
	// are named. Unused when the receiver has no TLS.
	TLSDir string
	// Sink takes each received file.
	Sink receive.Sink
	// Refusal says whether the archive can take files now; nil always can.
	Refusal Refusal
	// Report is told of each caller turned away; nil tells no one.
	Report Report
	// Drain bounds how long Stop waits for the stores in flight; 0 is
	// DefaultDrain.
	Drain time.Duration
	// MaxAssociations bounds the associations served at once; 0 is
	// DefaultMaxAssociations.
	MaxAssociations int
	// Logger receives the listener's lines: the receiver's name and fixed
	// phrases only. Nil discards them.
	Logger *slog.Logger
}

// Listener is one receiver's DICOM listener. Make it with New, start it
// with Start, end it with Stop; it is not started again after a Stop.
type Listener struct {
	cfg Config
	log *slog.Logger
	// tag tells this listener's association ids from those of the listener
	// before it on the same receiver: the library numbers associations from
	// one in each process.
	tag string

	srv *network.Server
	// leaf is the certificate the listener serves, nil without TLS; Check
	// reads its validity.
	leaf *x509.Certificate
	// mu guards stopping and stores, so Stop's wait cannot pass a store
	// that has just begun.
	mu       sync.Mutex
	stopping bool
	stores   sync.WaitGroup

	seams seams
}

// seams are the points a test changes (export_test.go).
type seams struct {
	// now is the clock a certificate's validity is read against.
	now func() time.Time
	// beforeAccept, when set, runs in a store after its file is handed to
	// the Sink's reader and before Accept, for a test that holds a store in
	// flight.
	beforeAccept func()
	// anyPort listens on a port the system picks instead of the
	// receiver's, so tests in parallel never contend for one.
	anyPort bool
}

// discard is the logger the library's server is given. What its network
// layer logs names AE titles, addresses and SOP Instance UIDs as received,
// which the server's log must not hold; the listener reports what it needs
// as events and in its own lines instead. Given to the server, it holds for
// everything that server does and touches nothing else in the process.
var discard = slog.New(slog.DiscardHandler)

// New checks cfg and returns a listener that is not yet listening.
func New(cfg Config) (*Listener, error) {
	r := cfg.Receiver
	switch {
	case cfg.Sink == nil:
		return nil, fault.New(fault.Internal, "a DICOM listener is given no sink")
	case r.Type != tenant.ReceiverDIMSE:
		return nil, fault.New(fault.Internal, "a DICOM listener is given a receiver of another type")
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if cfg.Drain <= 0 {
		cfg.Drain = DefaultDrain
	}
	if cfg.MaxAssociations <= 0 {
		cfg.MaxAssociations = DefaultMaxAssociations
	}
	if cfg.Refusal == nil {
		cfg.Refusal = func() tenant.AssociationFailure { return "" }
	}
	if cfg.Report == nil {
		cfg.Report = func(tenant.AssociationFailure, tenant.GroupCaller) {}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var b [4]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24 and later).
	return &Listener{
		cfg: cfg, log: log.With("receiver", r.Name), tag: hex.EncodeToString(b[:]),
		seams: seams{now: time.Now},
	}, nil
}

// Start listens on the receiver's port and serves associations until Stop.
// A port it cannot have, or TLS files it cannot use, is a *DownError saying
// why in the terms of the receiver's health; nothing is left listening then.
func (l *Listener) Start(ctx context.Context) error {
	r := l.cfg.Receiver
	var tlsCfg *network.TLSConfig
	if r.DIMSE.TLS != nil {
		c, leaf, err := loadTLS(l.cfg.TLSDir, *r.DIMSE.TLS, l.seams.now())
		if err != nil {
			return err
		}
		tlsCfg = &network.TLSConfig{Config: c}
		l.leaf = leaf
	}
	events := network.NewEventManager()
	l.observe(events)
	scfg := network.SCPConfig{
		AETitle:     r.DIMSE.AETitle,
		Port:        r.DIMSE.Port,
		BindAddress: bindHost(l.cfg.BindAddress),
		Network: network.NetworkConfig{
			MaxPDUSize: maxPDUSize,
			// The library aborts an association whose data set passes this,
			// counted as its bytes arrive. Twice the receiver's largest file,
			// so a file somewhat past the bound is answered out of resources,
			// which tells the sender to keep it (receiver.proto,
			// max_file_bytes), while the bytes the library reads and drops
			// for a refused store stay bounded.
			MaxDataSetSize: 2 * r.MaxFileBytes,
		},
		MaxAssociations: l.cfg.MaxAssociations,
		Admit:           l.admit,
		// The called title is decided in admit, after the caller, so a
		// stranger is told the same whatever title it called, and each
		// refusal is reported with its own reason.
		AcceptAnyCalledAE: true,
		Events:            events,
		// Set before the server listens, so no association is ever
		// negotiated with the library's defaults, which include query and
		// retrieve.
		AbstractSyntaxes: abstractSyntaxes(r.DIMSE.SOPClasses),
		TransferSyntaxes: transferSyntaxes(r.DIMSE.TransferSyntaxes),
		Logger:           discard,
		TLS:              tlsCfg,
	}
	if l.seams.anyPort {
		scfg.Port = 0
	}
	if l.cfg.BindAddress == "" && scfg.Port != 0 {
		if err := probeIPv4(scfg.Port); err != nil {
			return listenError(err)
		}
	}
	h := &handler{l: l}
	srv, err := network.StartServer(context.WithoutCancel(ctx), scfg, h)
	if err != nil {
		return listenError(err)
	}
	l.srv = srv
	l.log.Info("receiver: listening", "port", r.DIMSE.Port, "tls", tlsCfg != nil)
	return nil
}

// Check says whether the listener may go on serving at now: a certificate
// that has expired since it started (or is not yet valid) is a *DownError
// with the health's phrase, so the supervisor, which calls it
// periodically, stops the listener and shows the receiver down. A caller
// would refuse that certificate anyway; showing it down says why. Nil
// without TLS.
func (l *Listener) Check(now time.Time) error {
	if l.leaf == nil {
		return nil
	}
	return validAt(l.leaf, now)
}

// Addr returns the address the listener listens on, "" before Start.
func (l *Listener) Addr() string {
	if l.srv == nil {
		return ""
	}
	return l.srv.Addr()
}

// Stop stops admitting callers and lets the stores in flight finish, within
// the drain bound or until ctx ends, whichever comes first, then closes the
// port. The library's Shutdown does the waiting: it closes the port to new
// connections, rejects as transient an association asked for on one
// already open, lets an operation in progress send its answer, and at the
// bound aborts what is left. A store still running then is cut short and
// not kept: its sender is told nothing and sends it again.
func (l *Listener) Stop(ctx context.Context) error {
	l.mu.Lock()
	already := l.stopping
	l.stopping = true
	l.mu.Unlock()
	if already || l.srv == nil {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, l.cfg.Drain)
	defer cancel()
	err := l.srv.Shutdown(bounded)
	var cut *network.ShutdownError
	switch {
	case err == nil:
	case errors.As(err, &cut):
	default:
		return fmt.Errorf("receiver %s: stopping: %w", l.cfg.Receiver.Name, err)
	}
	// Stop waits for every association's goroutine, those cut short at the
	// bound included; the stores' own count then has nothing left to wait
	// for but a Sink that is still writing.
	l.srv.Stop()
	l.stores.Wait()
	l.log.Info("receiver: stopped", "stores_cut_short", cut != nil)
	return nil
}

// enter counts a store in flight, or reports that the listener is stopping
// and the store is to be refused. Checked under mu, where Stop sets
// stopping, so no store is counted after Stop starts waiting.
func (l *Listener) enter() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping {
		return false
	}
	l.stores.Add(1)
	return true
}

// isStopping reports whether Stop has begun.
func (l *Listener) isStopping() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopping
}

// associationID returns the id a file records for the association with
// the library's number n: this listener's tag and n, unique while ids of a
// restart are told apart by the tag.
func (l *Listener) associationID(n uint64) string {
	return "a" + l.tag + "-" + strconv.FormatUint(n, 10)
}

// probeIPv4 binds the port on the IPv4 wildcard for a moment and lets it go,
// so a receiver of every address is refused, address in use, when another
// program holds the port for IPv4. A dual-stack bind on [::] fails then on
// Linux and Windows, but on macOS (and the BSDs) it succeeds beside the
// other program, which then takes every IPv4 caller while the receiver
// shows up: senders would reach the wrong program unseen. A program that
// takes the port between this check and the bind is not seen; the window
// is one system call.
func probeIPv4(port int) error {
	ln, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return ln.Close()
}

// bindHost returns the host part the library joins with the port: an IPv6
// address in brackets, so "::1" and port 104 make "[::1]:104", not
// "::1:104".
func bindHost(addr string) string {
	if addr == "" {
		return ""
	}
	if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
		return "[" + addr + "]"
	}
	return addr
}

// abstractSyntaxes returns the SOP classes a receiver negotiates: its own
// list, or the server's default set (every Storage SOP Class the library
// knows, PS3.4 Table B.5-1), and Verification always (receiver.proto,
// sop_classes).
func abstractSyntaxes(list []string) []string {
	if len(list) == 0 {
		list = network.AllStorageSOPClassUIDs()
	}
	return withFirst(network.VerificationSOPClassUID, list)
}

// transferSyntaxes returns the transfer syntaxes a receiver negotiates: its
// own list, or every one the library knows, and Implicit VR Little Endian
// always, which every DICOM application accepts (PS3.5 10.1).
func transferSyntaxes(list []string) []string {
	if len(list) == 0 {
		list = network.AllTransferSyntaxUIDs()
	}
	return withFirst(network.ImplicitVRLittleEndianUID, list)
}

// withFirst returns list with first in it, once.
func withFirst(first string, list []string) []string {
	out := make([]string, 0, len(list)+1)
	out = append(out, first)
	for _, s := range list {
		if s != first {
			out = append(out, s)
		}
	}
	return out
}
