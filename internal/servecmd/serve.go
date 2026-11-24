package servecmd

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/spf13/cobra"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/backup"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
)

// Deps is what an installation's handlers are built from, once serve has
// opened it.
type Deps struct {
	// Access is the check and the management of keys and grants.
	Access *access.Access
	// Master is the master database. The server closes it at stop.
	Master master.Store
	// Keys is the installation key file, which seals and opens secrets.
	Keys *secret.Keys
	// DataDir is the data directory.
	DataDir string
	// Log is the server's logger.
	Log *slog.Logger
}

// Built is what Services returns: how to register the handlers, and the
// parts the server must close at stop.
type Built struct {
	// Register registers the API's services on the server's
	// connect.Server.
	Register func(*connect.Server)
	// Methods is the method-to-requirement table of the services Register
	// registers (package internal/api's).
	Methods server.Methods
	// WithDecision hands each handler the check's Decision in its context.
	WithDecision server.WithDecision
	// Registry is the tenant registry, or nil.
	Registry server.Registry
	// HTTPRoutes are plain HTTP routes besides /metrics.
	HTTPRoutes []server.HTTPRoute
	// PatternRoutes are plain HTTP routes inside one tenant, whose
	// requirement the server computes from each request's path.
	PatternRoutes []server.PatternRoute
	// LongMethods are the procedures and pattern routes that stream what may
	// take hours and so have no deadline (server.Config.LongMethods).
	LongMethods []string
	// TenantsByState counts the tenants in each state for /metrics, or nil
	// to leave the family out.
	TenantsByState func() map[string]int
	// TenantNetworks reads a tenant's access.tenant-networks for the access
	// check, or nil for no tenant's rule (no tenants).
	TenantNetworks access.TenantNetworks
	// Reception runs the receivers and the reception pipeline, or nil for
	// none. It is started once the server listens and stopped before the
	// server stops, while the tenants' databases are still open.
	Reception Runner
	// AfterDrain runs at stop once the server has drained its requests and
	// written the audit entries it held, before the databases close
	// (server.Config.AfterDrain): the audit's last seal. Nil for none.
	AfterDrain func(ctx context.Context) error
	// Counters and Gauges are more values for /metrics
	// (server.Config.Counters), or nil.
	Counters func() []obs.Counter
	// Gauges: see Counters.
	Gauges func() []obs.Gauge
}

// Services builds the API's handlers over an open installation.
type Services func(Deps) (Built, error)

// ReceptionStopTimeout bounds the stop of the receivers and the reception
// pipelines at a stop signal: the listeners' own drain (30 seconds) and a
// pipeline's (30 seconds) with room to spare. A second signal exits at once
// whatever is left.
const ReceptionStopTimeout = 90 * time.Second

// serveFlags are the flags of serve.
type serveFlags struct {
	dataDir                            string
	listen, tlsCert, tlsKey, masterEnv string
	logFormat, logLevel                string
	drain, deadline                    time.Duration
}

// Serve returns the "serve" command.
func Serve(opts Options) *cobra.Command {
	opts = opts.withDefaults()
	var f serveFlags
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the installation's server",
		Long: `Run the server of the installation in the data directory.

It listens on the local socket <data-dir>/run/nopacs.sock, where the caller is
the operating system user that runs it, and on the network only when --listen
is given: with TLS when --tls-cert and --tls-key are given, and without TLS on a
loopback address only. One server runs per data directory.

The first stop signal (Ctrl-C, SIGTERM) refuses new requests, lets those in
flight finish, writes the audit entries held in memory and exits 0. A second
signal exits at once.

The data directory is --data-dir, or NOPACS_DATA_DIR.`,
		Example: `  nopacs-server serve --data-dir /var/lib/nopacs
  nopacs-server serve --data-dir /var/lib/nopacs --listen 0.0.0.0:8443 --tls-cert cert.pem --tls-key key.pem`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir(cmd, opts.Getenv)
			if err != nil {
				return err
			}
			f.dataDir = dir
			return runServe(cmd.Context(), opts, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.listen, "listen", "", "the network address to serve, HOST:PORT; none unless given")
	fl.StringVar(&f.tlsCert, "tls-cert", "", "the PEM file of the network listener's certificate")
	fl.StringVar(&f.tlsKey, "tls-key", "", "the PEM file of the certificate's private key")
	fl.StringVar(&f.masterEnv, "master-env", "", "the name of an environment variable holding the master database's address, when it is not SQLite in the data directory")
	fl.StringVar(&f.logFormat, "log-format", string(obs.JSON), "json or text")
	fl.StringVar(&f.logLevel, "log-level", "info", "debug, info, warn or error")
	fl.DurationVar(&f.drain, "drain", server.DefaultDrain, "how long requests in flight may run after a stop signal")
	fl.DurationVar(&f.deadline, "default-deadline", server.DefaultDeadline, "the deadline of a call whose client sent none")
	// The default deadline is the contract's 30 seconds; changing it is for
	// tests of the deadline, so it is not shown in the help.
	_ = fl.MarkHidden("default-deadline")
	return cmd
}

// runServe opens the installation and serves until a stop signal.
func runServe(ctx context.Context, opts Options, f serveFlags) error {
	level, ok := obs.ParseLevel(f.logLevel)
	if !ok {
		return fault.New(fault.Invalid, "--log-level is debug, info, warn or error")
	}
	if f.logFormat != string(obs.JSON) && f.logFormat != string(obs.Text) {
		return fault.New(fault.Invalid, "--log-format is json or text")
	}
	// Zero would otherwise read as "the default" further down, which is not
	// what a person who typed --drain 0 asked for.
	if f.drain <= 0 {
		return fault.New(fault.Invalid, "--drain is a positive duration, such as 15s")
	}
	if f.deadline <= 0 {
		return fault.New(fault.Invalid, "--default-deadline is a positive duration, such as 30s")
	}
	log := obs.NewLogger(opts.Stderr, obs.LogOptions{Format: obs.Format(f.logFormat), Level: level})

	dir := f.dataDir
	keyPath := filepath.Join(dir, KeyFileName)
	if err := checkMade(dir, f.masterEnv); err != nil {
		return err
	}
	// The lock first: a second server is refused before it opens the
	// master database, so two servers never migrate it at once.
	lock, err := server.TakeLock(dir)
	if err != nil {
		return err
	}
	srv, reception, err := open(ctx, opts, f, dir, keyPath, lock, log)
	if err != nil {
		_ = lock.Release()
		return err
	}
	return serveUntilStopped(srv, reception, opts, log)
}

// exists reports whether path exists.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// checkMade refuses a data directory that is not a whole installation, and
// says what is wrong in plain words: no installation at all, or one whose
// init was interrupted, with what to remove before running init again. A
// master in another database (--master-env) is checked by checkIdentity
// once it is open.
func checkMade(dir, masterEnv string) error {
	// A restore leaves its marker until the installation's id is written. The
	// advice for an interrupted init below says to remove the key file, which
	// after a restore may be the only copy of the key its backup opens with.
	if backup.Interrupted(dir) {
		return fault.New(fault.Invalid, "the data directory "+dir+": "+backup.InterruptedAdvice)
	}
	key := exists(filepath.Join(dir, KeyFileName))
	id := exists(filepath.Join(dir, server.IDFileName))
	db := filepath.Join(dir, MasterFileName)
	sqlite := exists(db)
	switch {
	case !key && !id && !sqlite:
		return fault.New(fault.NotFound, "no installation in "+dir+"; make one with: nopacs-server init --data-dir "+dir)
	case !key:
		return fault.New(fault.Invalid, "the data directory "+dir+" has no key file but holds part of an installation: init was interrupted before it wrote the key file, or the key file was lost "+
			"(and with it every secret). If init was interrupted, remove "+filepath.Join(dir, server.IDFileName)+" and "+db+"* and run init again")
	case !id:
		return fault.New(fault.Invalid, "the data directory "+dir+" has a key file but no installation id: init was interrupted. Remove "+
			filepath.Join(dir, KeyFileName)+" and "+db+"* (or empty the database --master-env names) and run init again")
	case masterEnv == "" && !sqlite:
		return fault.New(fault.NotFound, "no master database at "+db+": if the installation was made with --master-env, give the same flag; "+
			"if init was interrupted, remove "+filepath.Join(dir, KeyFileName)+" and "+filepath.Join(dir, server.IDFileName)+" and run init again")
	}
	return nil
}

// checkNotEmpty refuses a master database with no tables, before anything
// is migrated: serve never fills a master it did not make, so a mistyped
// address to an empty database is refused and the database left as it was.
// It reads only the schema version, which needs no table of the master's.
func checkNotEmpty(ctx context.Context, m master.Store) error {
	v, err := m.Version(ctx)
	if err != nil {
		return err
	}
	if v == 0 {
		return fault.New(fault.Invalid, notOurs+": it is empty. Check the address (--master-env) or the data directory; serve never fills a master it did not make")
	}
	return nil
}

// notOurs begins every refusal of a master that is not this installation's.
const notOurs = "the master database is not the one this installation was made with"

// checkIdentity refuses a master database that is not the one this
// installation was made with: one with no installation id, or one with
// another installation's id. A mistyped address would otherwise start a
// server on a foreign master as if nothing were wrong. It reads a setting,
// which needs the master's statements, and a driver may prepare every
// statement on its first read, naming columns only the latest schema has;
// so it runs after the migration, and an installation made by an older
// version starts.
func checkIdentity(ctx context.Context, m master.Store, dir string) error {
	want, err := server.ReadInstallationID(dir)
	if err != nil {
		return err
	}
	got, err := m.Setting(ctx, server.InstallationIDSetting)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return fault.New(fault.Invalid, notOurs+": it holds no installation id. Check the address (--master-env); if init was interrupted, run it again on an empty database")
	case err != nil:
		return err
	case got != want:
		return fault.New(fault.Invalid, notOurs+": it belongs to another installation. Check the address (--master-env) and the data directory")
	}
	return nil
}

// checkKeyFile refuses a key file that is not the installation's: it opens
// the master's key check with keys. A master with no key check (made before
// the check existed) is refused too, since serve cannot tell then.
func checkKeyFile(ctx context.Context, m master.Store, dir string, keys *secret.Keys) error {
	id, err := server.ReadInstallationID(dir)
	if err != nil {
		return err
	}
	check, err := m.Setting(ctx, server.KeyCheckSetting)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return fault.New(fault.Conflict, server.MessageNoKeyCheck)
	case err != nil:
		return err
	}
	return server.CheckKeyFile(keys, id, check)
}

// open loads the key file, opens and migrates the master database, starts
// the access package and builds the server. On failure it closes what it
// opened, except the lock, which its caller releases.
func open(ctx context.Context, opts Options, f serveFlags, dir, keyPath string, lock *server.Lock, log *slog.Logger) (srv *server.Server, reception Runner, err error) {
	keys, err := secret.LoadKeyFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	address, _, err := masterAddress(dir, f.masterEnv, opts.Getenv)
	if err != nil {
		return nil, nil, err
	}
	m, err := driver.OpenMaster(ctx, address)
	if err != nil {
		return nil, nil, err
	}
	var a *access.Access
	defer func() {
		if err != nil {
			if a != nil {
				_ = a.Close(context.Background())
			}
			_ = m.Close()
		}
	}()
	// An empty master is refused before it is migrated, so it is left as it
	// was. The migration comes next, before anything prepares the master's
	// statements: a driver may prepare all of them on the first read, and
	// statements that name a column the installation's older schema lacks
	// would not prepare, so an installation made by an older version could
	// never start. Then the identity and the key check, before any tenant is
	// opened: a server running with another key file would fail every
	// tenant and seal a new tenant's address with the wrong key, leaving an
	// installation that no one key file can open, so it must stop before it
	// opens anything. A migration changes only the schema, never a tenant's
	// state, and is what the right key file would have done.
	if err := checkNotEmpty(ctx, m); err != nil {
		return nil, nil, err
	}
	if err := m.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	if err := checkIdentity(ctx, m, dir); err != nil {
		return nil, nil, err
	}
	if err := checkKeyFile(ctx, m, dir, keys); err != nil {
		return nil, nil, err
	}
	// The tenants' networks are read through the registry, which the
	// services build after the access check exists; the check reads them
	// through this variable, set before the server is made and so before
	// any request is served.
	var tenantNetworks access.TenantNetworks
	values := settingvalue.Reader{Master: m}
	a, err = access.New(access.Config{
		Store: m, Clock: time.Now, NewID: server.NewID, Log: log,
		Events: access.LogEvents(log),
		TenantNetworks: func(ctx context.Context, t access.TenantRef) (settings.Networks, error) {
			if tenantNetworks == nil {
				return nil, nil
			}
			return tenantNetworks(ctx, t)
		},
		ExpiryWarning: values.KeyExpiryWarning,
	})
	if err != nil {
		return nil, nil, err
	}
	built := Built{Register: func(*connect.Server) {}}
	if opts.Services != nil {
		built, err = opts.Services(Deps{Access: a, Master: m, Keys: keys, DataDir: dir, Log: log})
		if err != nil {
			return nil, nil, err
		}
		if built.Register == nil {
			built.Register = func(*connect.Server) {}
		}
		if built.Registry != nil {
			// The server closes the registry at stop; if no server is made,
			// it is closed here, before the master it reads.
			defer func() {
				if err != nil {
					ctx, cancel := context.WithTimeout(context.Background(), server.DefaultClose)
					defer cancel()
					_ = built.Registry.Close(ctx)
				}
			}()
		}
	}
	tenantNetworks = built.TenantNetworks
	srv, err = server.New(server.Config{
		DataDir:         dir,
		Lock:            lock,
		Listen:          f.listen,
		TLSCert:         f.tlsCert,
		TLSKey:          f.tlsKey,
		Access:          a,
		Registry:        built.Registry,
		TenantsByState:  built.TenantsByState,
		Master:          m,
		AfterDrain:      built.AfterDrain,
		Counters:        built.Counters,
		Gauges:          built.Gauges,
		Log:             log,
		Register:        built.Register,
		Methods:         built.Methods,
		WithDecision:    built.WithDecision,
		HTTPRoutes:      built.HTTPRoutes,
		PatternRoutes:   built.PatternRoutes,
		LongMethods:     built.LongMethods,
		CORSOrigins:     values.CORSOrigins,
		AccessRules:     accessRules(values),
		Drain:           f.drain,
		DefaultDeadline: f.deadline,
	})
	if err != nil {
		return nil, nil, err
	}
	return srv, built.Reception, nil
}

// serveUntilStopped starts the server and waits for a stop signal or a
// listener's failure; then it stops the server and returns Stop's result,
// so a clean stop exits 0. A second signal while stopping exits at once
// with status 1.
func serveUntilStopped(srv *server.Server, reception Runner, opts Options, log *slog.Logger) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	if err := srv.Start(); err != nil {
		// Nothing is served; Stop closes what open made and the lock.
		return errors.Join(err, srv.Stop())
	}
	if reception != nil {
		if err := reception.Start(context.Background()); err != nil {
			return errors.Join(err, srv.Stop())
		}
	}
	var cause error
	select {
	case sig := <-signals:
		log.Info("server: stopping", "signal", sig.String(), "note", "a second signal exits at once")
	case cause = <-srv.Errors():
		log.Error("server: a listener failed; stopping", "error", cause)
	}
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			log.Warn("server: second signal; exiting at once", "signal", sig.String())
			opts.Exit(1)
		case <-done:
		}
	}()
	// The receivers first, finishing the stores in flight, then the
	// pipelines, while the tenants' databases are open; then the server,
	// which drains its requests, writes the audit entries it holds, runs
	// Built.AfterDrain (the corrections' engines stopped, then the audit's
	// last seal) and closes the databases.
	var rerr error
	if reception != nil {
		ctx, cancel := context.WithTimeout(context.Background(), ReceptionStopTimeout)
		rerr = reception.Stop(ctx)
		cancel()
	}
	err := srv.Stop()
	close(done)
	return errors.Join(cause, rerr, err)
}

// startOnly starts a runner with the others and leaves its stop to a later
// step of the server's stop (stopThen).
type startOnly struct{ r Runner }

// Start starts it.
func (s startOnly) Start(ctx context.Context) error { return s.r.Start(ctx) }

// Stop does nothing: stopThen stops it.
func (startOnly) Stop(context.Context) error { return nil }

// afterDrainStopShare is the part of the time after the server's drain
// (server.DefaultClose) that stopThen gives r's stop: at most half, so what
// runs after it (the audit's last seal) keeps the other half even when a
// correction a request started is still running (its steps outlive their
// request) and r's stop waits for it in vain.
const afterDrainStopShare = server.DefaultClose / 2

// stopThen returns the step the server runs after its drain: r stopped,
// within afterDrainStopShare, then next. The corrections' engines stop
// here, not with the reception: a request in flight writes through their
// storages, which are closed only once the requests have drained.
func stopThen(r Runner, next func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		sctx, cancel := context.WithTimeout(ctx, afterDrainStopShare)
		err := r.Stop(sctx)
		cancel()
		return errors.Join(err, next(ctx))
	}
}

// tenantNetworksRefresh is how long a tenant's networks, once read, are used
// before they are read again: a change applies within it.
const tenantNetworksRefresh = 5 * time.Second

// accessRules returns the reader of the installation's access settings the
// server's front step applies (server.AccessRules), each through its typed
// reader, so a stored value its rule refuses is a failure, never no rule.
func accessRules(values settingvalue.Reader) func(ctx context.Context) (server.AccessRules, error) {
	return func(ctx context.Context) (server.AccessRules, error) {
		var r server.AccessRules
		var err error
		if r.TrustedProxies, err = values.Networks(ctx, settings.AccessTrustedProxies); err != nil {
			return r, err
		}
		if r.ForwardedHeader, err = values.ForwardedHeader(ctx); err != nil {
			return r, err
		}
		if r.Allowed, err = values.Networks(ctx, settings.AccessAllowedNetworks); err != nil {
			return r, err
		}
		if r.Denied, err = values.Networks(ctx, settings.AccessDeniedNetworks); err != nil {
			return r, err
		}
		if r.FailureLimit, err = values.FailureLimit(ctx); err != nil {
			return r, err
		}
		if r.FailureWindow, err = values.FailureWindow(ctx); err != nil {
			return r, err
		}
		r.BlockTime, err = values.BlockTime(ctx)
		return r, err
	}
}
