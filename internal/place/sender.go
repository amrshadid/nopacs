package place

import (
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// CredentialContext is what a stow sender's sealed credential is bound to:
// the tenant's id and the sender's name, so a sealed credential copied to
// another sender's row, or another tenant's, does not open there (package
// secret: the context is part of what the seal authenticates). Whatever
// opens it to send uses the same context.
func CredentialContext(tenantID, sender string) string {
	return "tenant/" + tenantID + "/sender/" + sender + "/credential"
}

// SealCredential seals a stow sender's credential for its row
// (CredentialContext). A sealed credential longer than a store keeps is the
// server's fault.
func SealCredential(k Sealer, tenantID, sender, credential string) ([]byte, error) {
	sealed, err := k.Seal(CredentialContext(tenantID, sender), []byte(credential))
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "place: the sender's credential could not be sealed")
	}
	if len(sealed) > tenant.MaxSealedCredentialLength {
		return nil, fault.New(fault.Internal, "place: a sealed credential is longer than the store keeps")
	}
	return sealed, nil
}
