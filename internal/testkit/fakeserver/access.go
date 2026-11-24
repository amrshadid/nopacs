package fakeserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// accessHandler implements AccessService.
type accessHandler struct{ s *Server }

// findKey returns the record of a key id.
func (s *Server) findKey(id string) (*keyRecord, error) {
	if id == "" {
		return nil, fault.New(fault.Invalid, "the key id is required")
	}
	for _, k := range s.keys {
		if k.key.GetId() == id {
			return k, nil
		}
	}
	return nil, fault.New(fault.NotFound, "no key has that id")
}

// CreateKey makes a key whose secret is answered once.
func (h accessHandler) CreateKey(_ context.Context, req *v1.CreateKeyRequest) (*v1.CreateKeyResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.GetHolder() == "" || len(req.GetHolder()) > 256 {
		return nil, fault.New(fault.Invalid, "the holder is required and at most 256 bytes")
	}
	created := s.now()
	// The installation's key.default-lifetime, else its built-in default,
	// as the real server reads it for a key made with no expiry.
	lifetime := 90 * 24 * time.Hour
	if def, ok := s.known().Lookup(settings.KeyDefaultLifetime); ok {
		v := def.Default
		if set, ok := s.settings[""][def.Name]; ok {
			v = set
		}
		if d, err := settings.ParseDuration(v); err == nil {
			lifetime = d
		}
	}
	expire := created.Add(lifetime)
	switch e := req.GetExpiry().(type) {
	case *v1.CreateKeyRequest_ExpireAfter:
		d := e.ExpireAfter.AsDuration()
		if d <= 0 || d > 3650*24*time.Hour {
			return nil, fault.New(fault.Invalid, "expire_after must be positive and at most 3650 days")
		}
		expire = created.Add(d)
	case *v1.CreateKeyRequest_NeverExpire:
		if !e.NeverExpire {
			return nil, fault.New(fault.Invalid, "never_expire must be true when set")
		}
	}
	s.nextKey++
	id := fmt.Sprintf("k%011d", s.nextKey)
	key := &v1.Key{Id: id, Holder: req.GetHolder(), CreateTime: timestamppb.New(created),
		Networks: req.GetNetworks(), ClientCertSha256: req.GetClientCertSha256()}
	if _, never := req.GetExpiry().(*v1.CreateKeyRequest_NeverExpire); !never {
		key.ExpireTime = timestamppb.New(expire)
	}
	secret := "nopacs_" + id + "_" + strings.Repeat("S", 44)
	s.keys = append(s.keys, &keyRecord{key: key, secret: secret})
	return &v1.CreateKeyResponse{Key: key, Secret: secret}, nil
}

// GetKey answers one key.
func (h accessHandler) GetKey(_ context.Context, req *v1.GetKeyRequest) (*v1.GetKeyResponse, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	k, err := h.s.findKey(req.GetId())
	if err != nil {
		return nil, err
	}
	return &v1.GetKeyResponse{Key: k.key}, nil
}

// ListKeys answers a page of keys, oldest first.
func (h accessHandler) ListKeys(_ context.Context, req *v1.ListKeysRequest) (*v1.ListKeysResponse, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	from, to, next, err := pageOf(len(h.s.keys), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &v1.ListKeysResponse{NextPageToken: next}
	for _, k := range h.s.keys[from:to] {
		res.Keys = append(res.Keys, k.key)
	}
	return res, nil
}

// RevokeKey ends a key; revoking a revoked key answers it with its first
// revoke time.
func (h accessHandler) RevokeKey(_ context.Context, req *v1.RevokeKeyRequest) (*v1.RevokeKeyResponse, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	k, err := h.s.findKey(req.GetId())
	if err != nil {
		return nil, err
	}
	if k.key.GetRevokeTime() == nil {
		k.key.RevokeTime = timestamppb.New(h.s.now())
	}
	return &v1.RevokeKeyResponse{Key: k.key}, nil
}

// SetGrant sets a key's role on a scope, replacing the one it had.
func (h accessHandler) SetGrant(_ context.Context, req *v1.SetGrantRequest) (*v1.SetGrantResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	k, err := s.findKey(req.GetKeyId())
	if err != nil {
		return nil, err
	}
	tenant, installation, err := scopeOf(req.GetScope())
	if err != nil {
		return nil, err
	}
	if req.GetRole() == v1.Role_ROLE_UNSPECIFIED {
		return nil, fault.New(fault.Invalid, "the role is required")
	}
	if installation && req.GetRole() == v1.Role_ROLE_WRITER {
		return nil, fault.New(fault.Invalid, "the writer role is not given on the installation")
	}
	if !installation {
		if err := s.checkTenant(tenant); err != nil {
			return nil, err
		}
	}
	if k.key.GetRevokeTime() != nil {
		return nil, fault.New(fault.Conflict, "the key is revoked")
	}
	gk := req.GetKeyId() + "|" + scopeText(req.GetScope())
	prev := s.grants[gk]
	s.grants[gk] = req.GetRole()
	return &v1.SetGrantResponse{
		Grant:        &v1.Grant{KeyId: req.GetKeyId(), Scope: req.GetScope(), Role: req.GetRole()},
		PreviousRole: prev,
	}, nil
}

// allGrants returns every grant ordered by key id and then scope.
func (s *Server) allGrants() []*v1.Grant {
	keys := make([]string, 0, len(s.grants))
	for k := range s.grants {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*v1.Grant, 0, len(keys))
	for _, gk := range keys {
		keyID, scope, _ := strings.Cut(gk, "|")
		g := &v1.Grant{KeyId: keyID, Role: s.grants[gk], Scope: &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}}
		if t, ok := strings.CutPrefix(scope, "tenant:"); ok {
			g.Scope = &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: t}}
		}
		out = append(out, g)
	}
	return out
}

// ListGrants answers the grants of one key or on one scope; exactly one filter
// is required.
func (h accessHandler) ListGrants(_ context.Context, req *v1.ListGrantsRequest) (*v1.ListGrantsResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	var match func(*v1.Grant) bool
	switch f := req.GetFilter().(type) {
	case *v1.ListGrantsRequest_KeyId:
		// A key that does not exist has no grants: the real server answers
		// an empty list, not "not found".
		if f.KeyId == "" {
			return nil, fault.New(fault.Invalid, "key_id: is required")
		}
		match = func(g *v1.Grant) bool { return g.GetKeyId() == f.KeyId }
	case *v1.ListGrantsRequest_Scope:
		tenant, installation, err := scopeOf(f.Scope)
		if err != nil {
			return nil, err
		}
		if !installation {
			if err := s.checkTenant(tenant); err != nil {
				return nil, err
			}
		}
		want := scopeText(f.Scope)
		match = func(g *v1.Grant) bool { return scopeText(g.GetScope()) == want }
	default:
		return nil, fault.New(fault.Invalid, "exactly one of key_id and scope is required")
	}
	var grants []*v1.Grant
	for _, g := range s.allGrants() {
		if match(g) {
			grants = append(grants, g)
		}
	}
	from, to, next, err := pageOf(len(grants), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListGrantsResponse{Grants: grants[from:to], NextPageToken: next}, nil
}

// RemoveGrant takes back a grant; removing one that is not held succeeds.
func (h accessHandler) RemoveGrant(_ context.Context, req *v1.RemoveGrantRequest) (*v1.RemoveGrantResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.findKey(req.GetKeyId()); err != nil {
		return nil, err
	}
	tenant, installation, err := scopeOf(req.GetScope())
	if err != nil {
		return nil, err
	}
	if !installation {
		if err := s.checkTenant(tenant); err != nil {
			return nil, err
		}
	}
	gk := req.GetKeyId() + "|" + scopeText(req.GetScope())
	prev := s.grants[gk]
	delete(s.grants, gk)
	return &v1.RemoveGrantResponse{PreviousRole: prev}, nil
}

// WhoAmI answers the key the request carried with its grants, or the local
// operator when the server needs no key.
func (h accessHandler) WhoAmI(context.Context, *v1.WhoAmIRequest) (*v1.WhoAmIResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RequireKey == "" {
		return &v1.WhoAmIResponse{Principal: &v1.Principal{
			Caller: &v1.Principal_LocalOperator{LocalOperator: &v1.LocalOperator{UserId: "501"}},
		}}, nil
	}
	p := &v1.Principal{}
	if k, err := s.findKey(s.CallerKeyID); err == nil {
		p.Caller = &v1.Principal_Key{Key: k.key}
	} else {
		p.Caller = &v1.Principal_Key{Key: &v1.Key{Id: s.CallerKeyID}}
	}
	for _, g := range s.allGrants() {
		if g.GetKeyId() == s.CallerKeyID {
			p.Grants = append(p.Grants, g)
		}
	}
	return &v1.WhoAmIResponse{Principal: p}, nil
}
