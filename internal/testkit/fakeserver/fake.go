package fakeserver

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// RequestIDHeader is the response header the contract has the server set on
// every answer.
const RequestIDHeader = "Nopacs-Request-Id"

// tenantName is the contract's rule for a tenant name: 1 to 63 lowercase
// ASCII letters, digits and hyphens, starting with a letter.
var tenantName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// settingName is the contract's rule for a setting name.
var settingName = regexp.MustCompile(`^[a-z0-9._-]{1,128}$`)

// Server is the fake. Its exported fields are set before it is served; the
// methods that read and change its state are safe to call from the test while
// the server runs.
type Server struct {
	// Now is the clock; the real time when nil.
	Now func() time.Time
	// RequireKey, when not empty, is the one key the server accepts, as
	// "Authorization: Bearer <RequireKey>"; any other request is answered
	// UNAUTHENTICATED. When empty the caller is the local operator, as on the
	// local socket.
	RequireKey string
	// CallerKeyID is the id of the key RequireKey stands for, which WhoAmI
	// reports.
	CallerKeyID string
	// Delay is how long each call waits before it is answered, for a test of
	// the client's deadline.
	Delay time.Duration
	// Settings is the list of settings the fake knows, as the real server's
	// settings service knows package settings' list: nil is the server's own
	// list (settings.MustBuiltin). A test that needs a setting a tenant may
	// override builds a list with one of its own.
	Settings *settings.Table

	mu        sync.Mutex
	tenants   map[string]*v1.Tenant
	addresses map[string]string
	keys      []*keyRecord
	grants    map[string]v1.Role
	settings  map[string]map[string]string
	audit     []*v1.AuditEntry
	fail      map[string]*connect.Error
	failSkip  map[string]int
	auth      []string
	requests  int
	nextKey   int
	seen      []Seen
	certPEM   []byte
	arch      archive
	read      readArchive
	prof      profiles
	deid      deid
	seal      sealState
	corr      corrections
}

// keyRecord is a key and the secret it was made with.
type keyRecord struct {
	key    *v1.Key
	secret string
}

// New returns an empty fake: no tenant, no key, and no setting given a value
// on any scope, so every setting has its built-in default.
func New() *Server {
	return &Server{
		tenants:   map[string]*v1.Tenant{},
		addresses: map[string]string{},
		grants:    map[string]v1.Role{},
		settings:  map[string]map[string]string{"": {}},
		fail:      map[string]*connect.Error{},
		failSkip:  map[string]int{},
	}
}

// known returns the list of settings the fake knows.
func (s *Server) known() *settings.Table {
	if s.Settings != nil {
		return s.Settings
	}
	return settings.MustBuiltin()
}

// now returns the fake's time.
func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// FailWith makes every call of a procedure (such as
// nopacsv1connect.TenantServiceCreateTenantProcedure) answer with an API
// error of the given code and message, after the request id is set.
func (s *Server) FailWith(procedure string, code connect.Code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[procedure] = connect.NewError(code, message)
}

// FailAfter is FailWith for the calls of a procedure after the first n,
// which are answered as usual: a test of a command that makes the same call
// more than once, and fails part way.
func (s *Server) FailAfter(procedure string, n int, code connect.Code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[procedure] = connect.NewError(code, message)
	s.failSkip[procedure] = n
}

// AddTenant puts a tenant in the fake directly, for a test that needs many.
func (s *Server) AddTenant(t *v1.Tenant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenants[t.GetName()] = t
}

// AddKey puts a key in the fake directly.
func (s *Server) AddKey(k *v1.Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, &keyRecord{key: k})
}

// AddGrant gives a key a role on a scope directly, as SetGrant would.
func (s *Server) AddGrant(keyID string, scope *v1.Scope, role v1.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[keyID+"|"+scopeText(scope)] = role
}

// AddAudit puts an entry in the audit log, newest first when called in that
// order.
func (s *Server) AddAudit(e *v1.AuditEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, e)
}

// DatabaseAddress returns the address CreateTenant received for a tenant, so a
// test can show that it reached the server and nowhere else.
func (s *Server) DatabaseAddress(tenant string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addresses[tenant]
}

// AuthorizationHeaders returns the Authorization header of every request seen,
// in order, so a test can show what the client sent.
func (s *Server) AuthorizationHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// Requests returns how many API calls the server has answered.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Register adds the services to a Connect server, with the interceptor
// that makes the request id, checks the key and turns kinds into codes.
func (s *Server) Register(srv *connect.Server) {
	nopacsv1connect.RegisterTenantServiceHandler(srv, tenantHandler{s})
	nopacsv1connect.RegisterAccessServiceHandler(srv, accessHandler{s})
	nopacsv1connect.RegisterSettingsServiceHandler(srv, settingsHandler{s})
	nopacsv1connect.RegisterAuditServiceHandler(srv, auditHandler{s})
	nopacsv1connect.RegisterAuditSealServiceHandler(srv, auditSealHandler{s})
	s.registerArchive(srv)
	s.registerDeid(srv)
	s.registerRead(srv)
	s.registerCorrections(srv)
}

// interceptor is the one place every call passes: the request id first, so
// every answer has it; then the key; then the forced failure of a test; and
// last the handler, whose error is turned into a code by the codes package,
// the way the real server will.
func (s *Server) interceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		info, _ := connect.CallInfoForServerContext(ctx)
		s.mu.Lock()
		s.requests++
		id := fmt.Sprintf("req-%06d", s.requests)
		header := info.RequestHeader().Get("Authorization")
		s.auth = append(s.auth, header)
		forced := s.fail[spec.Procedure]
		if forced != nil && s.failSkip[spec.Procedure] > 0 {
			s.failSkip[spec.Procedure]--
			forced = nil
		}
		s.mu.Unlock()
		info.ResponseHeader().Set(RequestIDHeader, id)

		if s.Delay > 0 {
			select {
			case <-time.After(s.Delay):
			case <-ctx.Done():
				// The contract: a call whose deadline passes is answered
				// DEADLINE_EXCEEDED, which has no fault kind, so it is not
				// made by ToConnect.
				return connect.NewError(connect.CodeDeadlineExceeded, "the deadline passed")
			}
		}

		if s.RequireKey != "" && header != "Bearer "+s.RequireKey {
			// One message for every cause, as the contract requires.
			return codes.ToConnect(fault.New(fault.Unauthenticated, "the key is missing, unknown, expired or revoked"))
		}
		if forced != nil {
			return forced
		}
		if err := next(ctx, spec, stream); err != nil {
			if ce, ok := passThrough(err); ok {
				return ce
			}
			return codes.ToConnect(err)
		}
		return nil
	}
}

// pageOf applies the contract's paging to n items: a size of zero is 100, more
// than 1000 or less than zero is invalid, and a token is opaque (here, the
// offset behind a prefix). It returns the range to answer and the next token.
func pageOf(n int, size int32, token string) (from, to int, next string, err error) {
	switch {
	case size < 0 || size > 1000:
		return 0, 0, "", fault.New(fault.Invalid, "page_size must be between 0 and 1000")
	case size == 0:
		size = 100
	}
	if len(token) > 1024 {
		return 0, 0, "", fault.New(fault.Invalid, "page_token is too long")
	}
	if token != "" {
		off, ok := strings.CutPrefix(token, "p")
		v, convErr := strconv.Atoi(off)
		if !ok || convErr != nil || v < 0 || v > n {
			return 0, 0, "", fault.New(fault.Invalid, "page_token is not one this server made")
		}
		from = v
	}
	to = min(from+int(size), n)
	if to < n {
		next = "p" + strconv.Itoa(to)
	}
	return from, to, next, nil
}

// scopeOf reads a Scope into its tenant name, which is empty for the
// installation, or an Invalid error for a scope with neither or a tenant
// name that is not one, as the real server answers a caller who may list
// every tenant.
func scopeOf(sc *v1.Scope) (tenant string, installation bool, err error) {
	switch k := sc.GetKind().(type) {
	case *v1.Scope_Tenant:
		if !tenantName.MatchString(k.Tenant) {
			return "", false, fault.New(fault.Invalid, "scope.tenant: is 1 to 63 of a-z, 0-9 and -, starting with a letter")
		}
		return k.Tenant, false, nil
	case *v1.Scope_Installation:
		return "", true, nil
	}
	return "", false, fault.New(fault.Invalid, "the scope is neither a tenant nor the installation")
}

// scopeText is a scope as a key in the grant table.
func scopeText(sc *v1.Scope) string {
	if t := sc.GetTenant(); t != "" {
		return "tenant:" + t
	}
	return "installation"
}

// checkTenant is the contract's answer for a tenant that must exist; the
// caller here may list every tenant, so a missing one is NOT_FOUND.
func (s *Server) checkTenant(name string) error {
	if _, ok := s.tenants[name]; !ok {
		return fault.New(fault.NotFound, "no tenant has that name")
	}
	return nil
}
