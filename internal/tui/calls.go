package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// This file is the only place the program reaches the API. Each function
// returns a command (or, for whoami, what a command runs); a screen only ever
// holds the command, so what a screen does is decided by messages, and no
// screen can wait on the network inside its update function.
//
// The names given to client.Call are the commands' own ("tenant list", "key
// revoke"), so the line at the bottom of the screen for a failure reads as the
// line the command prints for the same failure.

// pageSize is how many rows a list asks for at a time. The contract's default
// is the same; the interactive program never asks for everything.
const pageSize = 100

// answer turns the result of client.Call into the message of a command: the
// failure as a failMsg, or what ok makes of the response.
func answer[T any](res *T, err error, ok func(*T) tea.Msg) tea.Msg {
	if err != nil {
		return failMsg{err}
	}
	return ok(res)
}

// whoami asks who is connected.
func whoami(ctx context.Context, cl *client.Client) tea.Msg {
	res, err := client.Call(ctx, cl, "whoami", func(ctx context.Context) (*v1.WhoAmIResponse, error) {
		return cl.Access.WhoAmI(ctx, &v1.WhoAmIRequest{})
	})
	return answer(res, err, func(r *v1.WhoAmIResponse) tea.Msg { return whoamiMsg{r.GetPrincipal()} })
}

// tenantRow is a tenant as the cells of a row. A tenant that is not ready
// shows why in the state cell, so an unreachable database is never a bare word.
func tenantRow(t *v1.Tenant, loc *time.Location) row {
	state := output.State(t.GetState())
	if t.GetState() != v1.TenantState_TENANT_STATE_READY && t.GetStateReason() != "" {
		state += ": " + t.GetStateReason()
	}
	return row{
		cells: []string{t.GetName(), output.Database(t.GetDatabaseType()), state, listTime(t.GetCreateTime(), loc)},
		ref:   t,
	}
}

// tenantsPage asks for one page of tenants.
func tenantsPage(c *ctx, owner, gen int, token string) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "tenant list", func(ctx context.Context) (*v1.ListTenantsResponse, error) {
			return cl.Tenants.ListTenants(ctx, &v1.ListTenantsRequest{PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListTenantsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetTenants()))
			for _, t := range r.GetTenants() {
				rows = append(rows, tenantRow(t, loc))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// keyRow is a key as the cells of a row.
func keyRow(k *v1.Key, now time.Time, loc *time.Location) row {
	return row{
		cells: []string{k.GetId(), k.GetHolder(), keyStatusWord(k, now), listDate(k.GetCreateTime(), loc, "-"), listDate(k.GetExpireTime(), loc, "never"), listDate(k.GetLastUseTime(), loc, "never")},
		ref:   k,
	}
}

// keysPage asks for one page of keys.
func keysPage(c *ctx, owner, gen int, token string) tea.Cmd {
	cl, loc, now := c.client, c.loc, c.now()
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "key list", func(ctx context.Context) (*v1.ListKeysResponse, error) {
			return cl.Access.ListKeys(ctx, &v1.ListKeysRequest{PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListKeysResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetKeys()))
			for _, k := range r.GetKeys() {
				rows = append(rows, keyRow(k, now, loc))
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// grantFilter says whose grants a list shows: those of one key, or those on
// one scope.
type grantFilter struct {
	keyID string
	scope *v1.Scope
}

// grantsPage asks for one page of grants.
func grantsPage(c *ctx, owner, gen int, token string, f grantFilter) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		req := &v1.ListGrantsRequest{PageSize: pageSize, PageToken: token}
		if f.scope != nil {
			req.Filter = &v1.ListGrantsRequest_Scope{Scope: f.scope}
		} else {
			req.Filter = &v1.ListGrantsRequest_KeyId{KeyId: f.keyID}
		}
		res, err := client.Call(ctx, cl, "grant list", func(ctx context.Context) (*v1.ListGrantsResponse, error) {
			return cl.Access.ListGrants(ctx, req)
		})
		return answer(res, err, func(r *v1.ListGrantsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetGrants()))
			for _, g := range r.GetGrants() {
				rows = append(rows, row{cells: []string{g.GetKeyId(), output.Scope(g.GetScope()), output.Role(g.GetRole())}, ref: g})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// settingsPage asks for one page of the settings set on a scope.
func settingsPage(c *ctx, owner, gen int, token string, scope *v1.Scope) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "setting list", func(ctx context.Context) (*v1.ListSettingsResponse, error) {
			return cl.Settings.ListSettings(ctx, &v1.ListSettingsRequest{Scope: scope, PageSize: pageSize, PageToken: token})
		})
		return answer(res, err, func(r *v1.ListSettingsResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetSettings()))
			for _, s := range r.GetSettings() {
				rows = append(rows, row{cells: []string{s.GetName(), s.GetValue(), output.SettingFrom(s)}, ref: s})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// auditFilter is what an audit list asks for.
type auditFilter struct {
	scope   *v1.Scope
	outcome v1.AuditOutcome
	keyID   string
}

// auditPage asks for one page of audit entries, newest first.
func auditPage(c *ctx, owner, gen int, token string, f auditFilter) tea.Cmd {
	cl, loc := c.client, c.loc
	return c.call(owner, func(ctx context.Context) tea.Msg {
		req := &v1.ListAuditEntriesRequest{Scope: f.scope, KeyId: f.keyID, Outcome: f.outcome, PageSize: pageSize, PageToken: token}
		res, err := client.Call(ctx, cl, "audit list", func(ctx context.Context) (*v1.ListAuditEntriesResponse, error) {
			return cl.Audit.ListAuditEntries(ctx, req)
		})
		return answer(res, err, func(r *v1.ListAuditEntriesResponse) tea.Msg {
			rows := make([]row, 0, len(r.GetEntries()))
			for _, e := range r.GetEntries() {
				rows = append(rows, row{cells: []string{
					listTime(e.GetTime(), loc), output.Outcome(e.GetOutcome()), output.Who(e),
					output.Method(e.GetMethod()), e.GetObject(),
				}, ref: e})
			}
			return pageMsg{gen: gen, rows: rows, next: r.GetNextPageToken()}
		})
	})
}

// getTenant fetches one tenant again.
func getTenant(c *ctx, owner int, name string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "tenant show", func(ctx context.Context) (*v1.GetTenantResponse, error) {
			return cl.Tenants.GetTenant(ctx, &v1.GetTenantRequest{Name: name})
		})
		return answer(res, err, func(r *v1.GetTenantResponse) tea.Msg { return detailMsg{ref: r.GetTenant()} })
	})
}

// getKey fetches one key again.
func getKey(c *ctx, owner int, id string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "key show", func(ctx context.Context) (*v1.GetKeyResponse, error) {
			return cl.Access.GetKey(ctx, &v1.GetKeyRequest{Id: id})
		})
		return answer(res, err, func(r *v1.GetKeyResponse) tea.Msg { return detailMsg{ref: r.GetKey()} })
	})
}

// createKey makes a key. expire is how long it lasts; zero means it never
// expires, and a caller that wants the server's default does not use this form.
func createKey(c *ctx, owner int, holder string, expire time.Duration) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		req := &v1.CreateKeyRequest{Holder: holder}
		if expire == 0 {
			req.Expiry = &v1.CreateKeyRequest_NeverExpire{NeverExpire: true}
		} else {
			req.Expiry = &v1.CreateKeyRequest_ExpireAfter{ExpireAfter: durationpb.New(expire)}
		}
		res, err := client.Call(ctx, cl, "key create", func(ctx context.Context) (*v1.CreateKeyResponse, error) {
			return cl.Access.CreateKey(ctx, req)
		})
		return answer(res, err, func(r *v1.CreateKeyResponse) tea.Msg {
			return keyCreatedMsg{key: r.GetKey(), secret: newSecret(r.GetSecret())}
		})
	})
}

// revokeKey ends a key.
func revokeKey(c *ctx, owner int, id string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "key revoke", func(ctx context.Context) (*v1.RevokeKeyResponse, error) {
			return cl.Access.RevokeKey(ctx, &v1.RevokeKeyRequest{Id: id})
		})
		return answer(res, err, func(r *v1.RevokeKeyResponse) tea.Msg {
			return actionDoneMsg{note: "key " + output.KeyLabel(r.GetKey()) + " revoked"}
		})
	})
}

// setGrant gives a key a role on a scope.
func setGrant(c *ctx, owner int, keyID string, scope *v1.Scope, role v1.Role) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "grant set", func(ctx context.Context) (*v1.SetGrantResponse, error) {
			return cl.Access.SetGrant(ctx, &v1.SetGrantRequest{KeyId: keyID, Scope: scope, Role: role})
		})
		return answer(res, err, func(r *v1.SetGrantResponse) tea.Msg {
			return actionDoneMsg{note: "key " + keyID + " now holds " + output.Role(r.GetGrant().GetRole()) + " on " + output.Scope(r.GetGrant().GetScope())}
		})
	})
}

// removeGrant takes a key's role on a scope back.
func removeGrant(c *ctx, owner int, keyID string, scope *v1.Scope) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		_, err := client.Call(ctx, cl, "grant remove", func(ctx context.Context) (*v1.RemoveGrantResponse, error) {
			return cl.Access.RemoveGrant(ctx, &v1.RemoveGrantRequest{KeyId: keyID, Scope: scope})
		})
		if err != nil {
			return failMsg{err}
		}
		return actionDoneMsg{note: "key " + keyID + " holds no grant on " + output.Scope(scope)}
	})
}

// setSetting sets a setting on a scope.
func setSetting(c *ctx, owner int, name string, scope *v1.Scope, value string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "setting set", func(ctx context.Context) (*v1.SetSettingResponse, error) {
			return cl.Settings.SetSetting(ctx, &v1.SetSettingRequest{Setting: &v1.Setting{Name: name, Scope: scope, Value: value}})
		})
		return answer(res, err, func(r *v1.SetSettingResponse) tea.Msg {
			return actionDoneMsg{note: "set " + r.GetSetting().GetName() + " on " + output.Scope(scope)}
		})
	})
}

// unsetSetting removes a tenant's override of a setting.
func unsetSetting(c *ctx, owner int, name string, scope *v1.Scope) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		_, err := client.Call(ctx, cl, "setting unset", func(ctx context.Context) (*v1.UnsetSettingResponse, error) {
			return cl.Settings.UnsetSetting(ctx, &v1.UnsetSettingRequest{Name: name, Scope: scope})
		})
		if err != nil {
			return failMsg{err}
		}
		return actionDoneMsg{note: name + " unset on " + output.Scope(scope)}
	})
}

// removeTenant unregisters a tenant. The name is sent twice, as the contract
// requires, so a name altered on the way refuses instead of removing another
// tenant.
func removeTenant(c *ctx, owner int, name string) tea.Cmd {
	cl := c.client
	return c.call(owner, func(ctx context.Context) tea.Msg {
		res, err := client.Call(ctx, cl, "tenant remove", func(ctx context.Context) (*v1.RemoveTenantResponse, error) {
			return cl.Tenants.RemoveTenant(ctx, &v1.RemoveTenantRequest{Name: name, ConfirmName: name})
		})
		return answer(res, err, func(r *v1.RemoveTenantResponse) tea.Msg {
			return actionDoneMsg{note: "tenant " + name + " unregistered; no data was deleted, it is at " + r.GetDataLocation()}
		})
	})
}
