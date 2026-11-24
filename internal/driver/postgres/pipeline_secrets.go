package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TenantSecret implements tenant.Secrets. The sealed bytes are read
// as stored and handed back as they are; nothing here opens or logs them.
func (t *Tenant) TenantSecret(ctx context.Context, name tenant.SecretName) ([]byte, error) {
	if err := tenant.ValidateSecretName(name); err != nil {
		return nil, err
	}
	return readOne(ctx, t, "the tenant has no secret named "+string(name), "the tenant's secret cannot be read",
		`SELECT sealed FROM tenant_secrets WHERE name = $1`, []any{string(name)}, scanSealed)
}

// KeepTenantSecret implements tenant.Secrets: the insert does nothing
// when a secret of that name is there (the primary key decides between two
// at once, the second waiting on the first's commit), and the read in the
// same transaction returns whichever is stored, so the first writer's wins.
func (t *Tenant) KeepTenantSecret(ctx context.Context, name tenant.SecretName, sealed []byte, at time.Time) ([]byte, error) {
	if err := tenant.ValidateSecretName(name); err != nil {
		return nil, err
	}
	if err := tenant.ValidateSealedSecret(sealed); err != nil {
		return nil, err
	}
	if err := master.ValidateTime("time", at); err != nil {
		return nil, err
	}
	var out []byte
	err := t.pipe(ctx, "the tenant's secret cannot be kept", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_secrets (name, sealed, created) VALUES ($1, $2, $3)
			ON CONFLICT (name) DO NOTHING`, string(name), sealed, micros(at)); err != nil {
			return err
		}
		var err error
		out, err = scanSealed(tx.QueryRow(ctx, `SELECT sealed FROM tenant_secrets WHERE name = $1`, string(name)))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanSealed scans one sealed secret.
func scanSealed(row scanner) ([]byte, error) {
	var b []byte
	err := row.Scan(&b)
	return b, err
}
