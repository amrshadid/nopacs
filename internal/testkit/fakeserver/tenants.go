package fakeserver

import (
	"context"
	"sort"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// tenantHandler implements TenantService.
type tenantHandler struct{ s *Server }

// CreateTenant makes a tenant, refusing a bad name, a database address that
// does not fit the type, and a name already taken.
func (h tenantHandler) CreateTenant(_ context.Context, req *v1.CreateTenantRequest) (*v1.CreateTenantResponse, error) {
	s := h.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if !tenantName.MatchString(req.GetName()) {
		return nil, fault.New(fault.Invalid, "the tenant name must be 1 to 63 lowercase letters, digits and hyphens, starting with a letter")
	}
	switch req.GetDatabaseType() {
	case v1.DatabaseType_DATABASE_TYPE_SQLITE:
		if req.GetDatabaseAddress() != "" {
			return nil, fault.New(fault.Invalid, "a SQLite tenant takes no database address")
		}
	case v1.DatabaseType_DATABASE_TYPE_POSTGRES:
		if req.GetDatabaseAddress() == "" || len(req.GetDatabaseAddress()) > 4096 {
			return nil, fault.New(fault.Invalid, "a Postgres tenant needs a database address of at most 4 KiB")
		}
	default:
		return nil, fault.New(fault.Invalid, "the database type is required")
	}
	if _, dup := s.tenants[req.GetName()]; dup {
		return nil, fault.New(fault.Conflict, "a tenant with that name exists")
	}
	now := timestamppb.New(s.now())
	t := &v1.Tenant{
		Name:         req.GetName(),
		DatabaseType: req.GetDatabaseType(),
		State:        v1.TenantState_TENANT_STATE_READY,
		CreateTime:   now,
		// The real server's view of a new tenant begins when its creation
		// finds it ready.
		StateTime: now,
	}
	s.tenants[t.GetName()] = t
	s.addresses[t.GetName()] = req.GetDatabaseAddress()
	return &v1.CreateTenantResponse{Tenant: t}, nil
}

// GetTenant answers one tenant.
func (h tenantHandler) GetTenant(_ context.Context, req *v1.GetTenantRequest) (*v1.GetTenantResponse, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if req.GetName() == "" {
		return nil, fault.New(fault.Invalid, "the tenant name is required")
	}
	if err := h.s.checkTenant(req.GetName()); err != nil {
		return nil, err
	}
	return &v1.GetTenantResponse{Tenant: h.s.tenants[req.GetName()]}, nil
}

// ListTenants answers a page of tenants ordered by name: every tenant for
// the local operator and for a caller key with a grant on the installation,
// otherwise only the tenants the caller key holds a grant on, as the real
// server answers.
func (h tenantHandler) ListTenants(_ context.Context, req *v1.ListTenantsRequest) (*v1.ListTenantsResponse, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	all := h.s.RequireKey == "" || h.s.grants[h.s.CallerKeyID+"|installation"] != v1.Role_ROLE_UNSPECIFIED
	names := make([]string, 0, len(h.s.tenants))
	for n := range h.s.tenants {
		if all || h.s.grants[h.s.CallerKeyID+"|tenant:"+n] != v1.Role_ROLE_UNSPECIFIED {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	from, to, next, err := pageOf(len(names), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &v1.ListTenantsResponse{NextPageToken: next}
	for _, n := range names[from:to] {
		res.Tenants = append(res.Tenants, h.s.tenants[n])
	}
	return res, nil
}

// RemoveTenant unregisters a tenant and says where its data was left. The name
// is sent twice and the two must agree.
func (h tenantHandler) RemoveTenant(_ context.Context, req *v1.RemoveTenantRequest) (*v1.RemoveTenantResponse, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if req.GetName() == "" || req.GetName() != req.GetConfirmName() {
		return nil, fault.New(fault.Invalid, "name and confirm_name must both be given and be equal")
	}
	t, ok := h.s.tenants[req.GetName()]
	if !ok {
		return nil, fault.New(fault.NotFound, "no tenant has that name")
	}
	delete(h.s.tenants, req.GetName())
	loc := "/var/lib/nopacs/tenants/" + t.GetName() + ".db"
	if t.GetDatabaseType() == v1.DatabaseType_DATABASE_TYPE_POSTGRES {
		loc = "db.example.org:5432/" + t.GetName()
	}
	return &v1.RemoveTenantResponse{DatabaseType: t.GetDatabaseType(), DataLocation: loc}, nil
}
