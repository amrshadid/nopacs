package api

import (
	"context"
	"slices"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
)

// maxGrantPages bounds how many pages of a key's grants ListTenants reads
// for a caller who may not list every tenant: a thousand a page, so a
// hundred thousand grants, past any installation's tenants. A store that
// keeps answering a cursor fails the call rather than loop.
const maxGrantPages = 100

// tenantService is TenantService. A tenant's registration and state are in
// the master; it is created and removed through the registry, which holds
// its open store.
type tenantService struct{ c *core }

var _ nopacsv1connect.TenantServiceHandler = tenantService{}

// CreateTenant registers a tenant and makes its tables in its database.
func (s tenantService) CreateTenant(ctx context.Context, req *v1.CreateTenantRequest) (*v1.CreateTenantResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.TenantServiceCreateTenantProcedure)
	if err != nil {
		return nil, err
	}
	name, address := req.GetName(), req.GetDatabaseAddress()
	// The requirement is fixed, so the check's entry names no tenant; these
	// are the facts the handler learns from the request, never its address.
	facts := access.Facts{
		Object:     tenantObject(name),
		TenantName: name,
		Details:    map[string]string{registry.DetailDatabaseType: driverOf(req.GetDatabaseType())},
	}
	if err := tenantName("name", name); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	var drv string
	switch req.GetDatabaseType() {
	case v1.DatabaseType_DATABASE_TYPE_SQLITE:
		if address != "" {
			return nil, c.failWith(d, nil, facts, invalid("database_address", "is empty for a SQLite tenant: its file is made in the data directory"))
		}
		drv = master.SQLiteDriver
	case v1.DatabaseType_DATABASE_TYPE_POSTGRES:
		if address == "" {
			return nil, c.failWith(d, nil, facts, invalid("database_address", "is required for a Postgres tenant"))
		}
		if len(address) > MaxDatabaseAddressLength {
			return nil, c.failWith(d, nil, facts, invalid("database_address", "is at most 4 KiB"))
		}
		drv = master.PostgresDriver
	default:
		return nil, c.failWith(d, nil, facts, invalid("database_type", "is required: sqlite or postgres"))
	}
	id := c.newID()
	if master.ValidateID("tenant id", id) != nil {
		// The id source is the server's, so a bad id is the server's fault,
		// not the caller's: it must not come back as INVALID_ARGUMENT.
		return nil, c.failWith(d, nil, facts, fault.New(fault.Internal, "api: the id source made a tenant id that is not valid"))
	}
	// The registry seals the address under "tenant/<id>/database" before it
	// writes the row; the address is in nothing the registry returns, logs or
	// records. The request's entry is handed to the registry, which writes
	// it once with its real outcome: done with the write that makes the
	// tenant ready, failed with the undoing of a creation whose database
	// could not be opened. It says whether it wrote it, and writes nothing
	// for a creation it refuses before writing anything (a name taken, an
	// address its driver refuses, a key that may no longer act, which the
	// master checks as it writes the row): the request is then recorded
	// here.
	entry := d.Entry(master.OutcomeDone, "", facts)
	nt := registry.NewTenant{ID: id, Name: name, Driver: drv, Address: address}
	row, recorded, err := c.reg.Create(ctx, nt, entry, d.Actor(master.Installation, master.RoleAdmin))
	if err != nil {
		if !recorded {
			return nil, c.failWith(d, nil, facts, err)
		}
		return nil, c.toAPI(d, err)
	}
	return &v1.CreateTenantResponse{Tenant: c.tenantView(row)}, nil
}

// GetTenant answers one tenant, whatever its state: its registration from
// the master and its state from the registry (tenantView), without touching
// the tenant's database. The row is read here rather than taken from the
// Decision, which is to carry only the tenant's id and name; a row gone
// since the check is answered as the check would have answered it.
func (s tenantService) GetTenant(ctx context.Context, req *v1.GetTenantRequest) (*v1.GetTenantResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.TenantServiceGetTenantProcedure)
	if err != nil {
		return nil, err
	}
	if err := tenantName("name", req.GetName()); err != nil {
		return nil, c.fail(d, err)
	}
	t, err := c.master.TenantByName(ctx, req.GetName())
	if fault.KindOf(err) == fault.NotFound {
		// Removed since the check: answered as the check would have.
		err = noTenant(d)
	}
	if ref, ok := d.Tenant(); err == nil && ok && t.ID != ref.ID {
		// Removed and created again under the same name since the check: a
		// reader of the old tenant may not read the new one.
		err = noTenant(d)
	}
	if err != nil {
		return nil, c.fail(d, err)
	}
	c.done(d, nil)
	m := c.tenantView(t)
	m.Quota = c.namedTenantQuota(ctx, t.Name)
	return &v1.GetTenantResponse{Tenant: m}, nil
}

// ListTenants answers the tenants the caller may see, ordered by name: every
// tenant for a caller who may list them all, otherwise the tenants its key
// holds a grant on.
func (s tenantService) ListTenants(ctx context.Context, req *v1.ListTenantsRequest) (*v1.ListTenantsResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.TenantServiceListTenantsProcedure)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.fail(d, err)
	}
	var rows []master.Tenant
	var next string
	if d.Principal().CanListTenants() {
		rows, next, err = c.master.ListTenants(ctx, page)
		err = listError(err, page.Cursor)
	} else {
		rows, next, err = c.grantedTenants(ctx, d.Principal().KeyID(), page)
	}
	if err != nil {
		return nil, c.fail(d, err)
	}
	c.done(d, nil)
	out := &v1.ListTenantsResponse{Tenants: make([]*v1.Tenant, 0, len(rows)), NextPageToken: next}
	for _, t := range rows {
		out.Tenants = append(out.Tenants, c.tenantView(t))
	}
	return out, nil
}

// grantedTenants returns a page of the tenants the key holds a grant on,
// ordered by name, and the token of the next page. It reads the key's grants,
// which name their tenants, and then only the tenants of the page, never
// every tenant of the installation: a key holds grants on few tenants, and
// the installation may have thousands. The token is the last name answered,
// bound to the key, so it is refused with another key's list, and a tenant
// granted or removed between two pages neither repeats nor shifts the rest.
func (c *core) grantedTenants(ctx context.Context, keyID string, page master.Page) ([]master.Tenant, string, error) {
	after := ""
	if page.Cursor != "" {
		var err error
		if after, err = readAfterToken(page.Cursor, tokenGrantedTenants, keyID); err != nil {
			return nil, "", err
		}
		if master.ValidateTenantName(after) != nil {
			return nil, "", invalid("page_token", "is not one this list returned for these arguments")
		}
	}
	if keyID == "" {
		// Only a key holds grants; a caller who is not a key and may not list
		// every tenant sees none.
		return nil, "", nil
	}
	ids := map[string]string{}
	cursor := ""
	for i := 0; ; i++ {
		if i == maxGrantPages {
			return nil, "", fault.New(fault.Internal, "api: a key's grants did not end after "+strconv.Itoa(maxGrantPages)+" pages")
		}
		grants, nextCursor, err := c.master.GrantsByKey(ctx, keyID, master.Page{Cursor: cursor, Limit: master.MaxPageLimit})
		if err != nil {
			return nil, "", err
		}
		for _, g := range grants {
			if !g.Scope.IsInstallation() && g.TenantName > after {
				ids[g.TenantName] = g.Scope.TenantID
			}
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	names := make([]string, 0, len(ids))
	for name := range ids {
		names = append(names, name)
	}
	// Names are ASCII, so byte order is the master's order of names.
	slices.Sort(names)
	next := ""
	if len(names) > page.Limit {
		names = names[:page.Limit]
		next = afterToken(tokenGrantedTenants, keyID, names[len(names)-1])
	}
	rows := make([]master.Tenant, 0, len(names))
	for _, name := range names {
		t, err := c.master.Tenant(ctx, ids[name])
		if fault.KindOf(err) == fault.NotFound {
			continue // Removed since its grant was read; its grants went with it.
		}
		if err != nil {
			return nil, "", err
		}
		rows = append(rows, t)
	}
	return rows, next, nil
}

// RemoveTenant unregisters a tenant and says where its data was left. The
// tenant's name is sent twice; the two must be equal.
func (s tenantService) RemoveTenant(ctx context.Context, req *v1.RemoveTenantRequest) (*v1.RemoveTenantResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.TenantServiceRemoveTenantProcedure)
	if err != nil {
		return nil, err
	}
	name := req.GetName()
	facts := access.Facts{Object: tenantObject(name), TenantName: name}
	if err := tenantName("name", name); err != nil {
		return nil, c.failWith(d, nil, facts, err)
	}
	if req.GetConfirmName() != name {
		return nil, c.failWith(d, nil, facts, invalid("confirm_name", "must equal name exactly"))
	}
	removed, err := c.reg.Remove(ctx, name, d.Entry(master.OutcomeDone, "", facts), d.Actor(master.Installation, master.RoleAdmin))
	if fault.KindOf(err) == fault.NotFound {
		err = noTenant(d)
	}
	if err != nil {
		// The removal and its entry are written together or not at all, so
		// a failure, a refusal of the key included, left the entry unwritten
		// and it is recorded here.
		return nil, c.failWith(d, nil, facts, err)
	}
	return &v1.RemoveTenantResponse{DatabaseType: databaseType(removed.Driver), DataLocation: removed.Location}, nil
}

// tenantView returns t as the contract writes it, with the state, reason and
// state time the registry sees now, which is what a request into the tenant
// meets. The master's row may lag behind it: the registry writes a change
// between ready and unreachable at most once a minute per tenant. A tenant
// the registry does not hold yet (for a moment during start or creation)
// keeps the master's state and has no state time. It reads no database.
func (c *core) tenantView(t master.Tenant) *v1.Tenant {
	st, ok := c.reg.State(t.Name)
	if !ok {
		return tenantMessage(t)
	}
	t.State, t.StateReason = st.State, st.Reason
	m := tenantMessage(t)
	m.StateTime = timestamp(st.Since)
	return m
}

// driverOf returns the driver of a database type a request names, for an
// audit entry, and "unspecified" for a type the contract does not define.
func driverOf(t v1.DatabaseType) string {
	switch t {
	case v1.DatabaseType_DATABASE_TYPE_SQLITE:
		return master.SQLiteDriver
	case v1.DatabaseType_DATABASE_TYPE_POSTGRES:
		return master.PostgresDriver
	}
	return "unspecified"
}
