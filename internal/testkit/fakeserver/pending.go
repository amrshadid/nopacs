package fakeserver

import (
	"context"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// errNotServed answers the methods the contract defines and the real
// server does not serve yet, as the real server does.
var errNotServed = connect.NewError(connect.CodeUnimplemented, "no such method")

// ReclaimTenant is not served yet by the real server, so not here either.
func (tenantHandler) ReclaimTenant(context.Context, *v1.ReclaimTenantRequest) (*v1.ReclaimTenantResponse, error) {
	return nil, errNotServed
}
