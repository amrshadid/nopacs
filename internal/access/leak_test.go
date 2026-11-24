package access_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// sprintf is fmt.Sprintf, named so the tests read as what they search.
func sprintf(format string, a ...any) string { return fmt.Sprintf(format, a...) }

// TestNoSecretAnywhere guards that a key's secret reaches no audit entry, no
// error and no log line: a key is made through the package, used for allowed
// and refused requests, presented with a wrong secret and garbled, and then
// every entry in the SQLite master, every error's text and the whole log are
// searched for the secret.
func TestNoSecretAnywhere(t *testing.T) {
	e := newEnv(t)
	made, err := e.access.CreateKey(e.ctx, e.admin("CreateKey"), "searched", access.NeverExpire(), master.KeyRules{})
	if err != nil {
		t.Fatal(err)
	}
	full := made.Secret.Key()
	id, sec, err := secret.ParseAPIKey(full)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.access.SetGrant(e.ctx, e.admin("SetGrant"), id, access.TenantNamed("lab"), master.RoleReader); err != nil {
		t.Fatal(err)
	}
	other, err := secret.MakeAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	creds := []access.Credential{
		access.APIKey(full),
		access.APIKey(full + "x"),
		access.APIKey("Bearer " + full),
		access.APIKey(strings.ToUpper(full)),
		access.APIKey("nopacs_" + other.ID + "_" + sec),
		access.APIKey(full[:len(full)-1] + "1"),
	}
	reqs := []access.Requirement{
		access.Authenticated(),
		access.OnTenant("lab", master.RoleReader),
		access.OnTenant("clinic", master.RoleAdmin),
		access.OnTenant("nosuch", master.RoleReader),
		access.OnInstallation(master.RoleAdmin),
	}
	// Marks fail from the start, so the key is never marked and every
	// check asks for a mark, and each failure writes a log line to search.
	e.store.failMarks.Store(true)
	var errs []string
	use := func(c access.Credential) {
		for _, r := range reqs {
			if _, err := e.access.Check(e.ctx, c, r, facts("m")); err != nil {
				errs = append(errs, err.Error(), sprintf("%+v", err))
			}
		}
	}
	for _, c := range creds {
		if strings.Contains(sprintf("%v %+v %#v %s", c, c, c, c), sec) {
			t.Fatal("a credential prints its secret")
		}
		use(c)
		if d, err := e.access.Check(e.ctx, c, access.Authenticated(), facts("WhoAmI")); err != nil {
			errs = append(errs, err.Error())
		} else if _, _, err := e.access.WhoAmI(e.ctx, d); err != nil {
			errs = append(errs, err.Error())
		}
	}
	// Once more with the audit log failing, so every refusal's entry
	// writes a log line to search when its batch, then the entry alone, is
	// refused.
	e.flush()
	e.store.failAudit.Store(true)
	for _, c := range creds {
		use(c)
	}
	e.flush()
	e.store.failAudit.Store(false)
	if _, err := e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.access.Check(e.ctx, access.APIKey(full), access.Authenticated(), facts("m")); err != nil {
		errs = append(errs, err.Error())
	}
	if err := e.access.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	entries, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < len(creds)*len(reqs) {
		t.Fatalf("only %d entries", len(entries))
	}
	for _, en := range entries {
		if strings.Contains(sprintf("%+v", en), sec) {
			t.Fatalf("audit entry %s holds the secret", en.ID)
		}
	}
	for _, s := range errs {
		if strings.Contains(s, sec) {
			t.Fatal("an error holds the secret")
		}
	}
	e.logMu.Lock()
	logText := e.log.String()
	e.logMu.Unlock()
	if strings.Contains(logText, sec) {
		t.Fatal("the log holds the secret")
	}
	if !strings.Contains(logText, "did not take a batch") || !strings.Contains(logText, "last use was not recorded") {
		t.Fatalf("the log has not the lines this test meant to search:\n%s", logText)
	}
	t.Logf("searched %d audit entries, %d error texts and %d bytes of log for the secret: not found", len(entries), len(errs), len(logText))
}
