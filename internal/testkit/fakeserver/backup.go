package fakeserver

import (
	"context"
	"crypto/sha256"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
)

// backupScript is what the fake's CreateBackup streams: the bytes, then the
// summary unless it is to end without one.
type backupScript struct {
	data    []byte
	summary *v1.BackupSummary
	noEnd   bool
}

// SetBackup makes CreateBackup stream data in messages of at most 1 MiB and
// then a summary that describes it: its size and SHA-256 as data has them,
// unless sizeBytes or sha256 are given, which the summary states instead,
// as a summary that does not describe the bytes sent would. With withoutEnd
// the stream ends after the data, without its summary.
func (s *Server) SetBackup(data []byte, withoutEnd bool, sizeBytes int64, sha []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := sha256.Sum256(data)
	summary := &v1.BackupSummary{Format: "nopacs-backup", FormatVersion: 1, InstallationId: "inst-fake-1", SizeBytes: int64(len(data)),
		Sha256: sum[:], KeyVersion: 1, Sections: []*v1.BackupSection{{SchemaVersion: 1, Tables: 7, Rows: 10}}}
	if sizeBytes != 0 {
		summary.SizeBytes = sizeBytes
	}
	if sha != nil {
		summary.Sha256 = sha
	}
	s.backup = backupScript{data: data, summary: summary, noEnd: withoutEnd}
}

// backupHandler is BackupService.
type backupHandler struct{ s *Server }

var _ nopacsv1connect.BackupServiceHandler = backupHandler{}

// CreateBackup streams what SetBackup set.
func (h backupHandler) CreateBackup(_ context.Context, _ *v1.CreateBackupRequest, stream nopacsv1connect.BackupServiceCreateBackupServerStream) error {
	h.s.mu.Lock()
	script := h.s.backup
	h.s.mu.Unlock()
	for data := script.data; len(data) > 0; {
		n := min(len(data), 1<<20)
		if err := stream.Send(&v1.CreateBackupResponse{Kind: &v1.CreateBackupResponse_Data{Data: data[:n]}}); err != nil {
			return err
		}
		data = data[n:]
	}
	if script.noEnd || script.summary == nil {
		return nil
	}
	return stream.Send(&v1.CreateBackupResponse{Kind: &v1.CreateBackupResponse_Summary{Summary: script.summary}})
}

// ExportTenant is not served by the fake.
func (backupHandler) ExportTenant(context.Context, *v1.ExportTenantRequest, nopacsv1connect.BackupServiceExportTenantServerStream) error {
	return connect.NewError(connect.CodeUnimplemented, "no such method")
}
