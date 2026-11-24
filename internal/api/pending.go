package api

import (
	"context"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
)

// errNotServed is what a method the contract defines and no handler serves
// yet answers, should a call ever reach it. The server does not mount such
// a method (Methods.Pending), so it answers the call itself, as it answers
// any method it does not serve; these bodies exist only because a served
// service's generated interface names every one of its methods.
var errNotServed = connect.NewError(connect.CodeUnimplemented, "no such method")

// CreateBackup is not served yet: BackupService's ExportTenant is.
func (backupService) CreateBackup(context.Context, *v1.CreateBackupRequest, nopacsv1connect.BackupServiceCreateBackupServerStream) error {
	return errNotServed
}
