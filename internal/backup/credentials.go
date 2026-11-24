package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The credentials an installation keeps in its data directory outside its
// databases and its key file: the TLS files its receivers and senders name,
// in each tenant's TLS directory (server.TLSDir). A backup holds the
// databases only, so a restore onto a new data directory brings back the
// configuration that names those files and not the files. The report names
// each one missing so the operator gives it again before the receiver or
// sender is used: without it a receiver does not listen and a sender cannot
// connect. Only file names are read and printed, never what a file holds.
//
// Every other credential is in a database, sealed with the key file, and
// comes back with it: a storage's secret key, a stow sender's credential,
// the naming and date-shift secrets. The network listener's certificate and
// key are named on the server's command line (--tls-cert, --tls-key), not in
// the configuration, and may be anywhere: the report says so once.

// Credential is one receiver's or sender's TLS files that its configuration
// names and the restored data directory lacks.
type Credential struct {
	// Kind is "receiver" or "sender".
	Kind string
	// Name is the receiver's or sender's name.
	Name string
	// Files are the missing files, each its role and its name in the
	// tenant's TLS directory ("TLS private key client.key"), in the order
	// the configuration names them; one that could not be looked at says
	// so after its name, with the system's reason.
	Files []string
	// Dir is the tenant's TLS directory, where the files are put again.
	Dir string
}

// listPage is the size of a page of receivers or senders a restore reads.
const listPage = 1000

// missingCredentials returns the TLS files the tenant's receivers and
// senders name that its TLS directory under dataDir lacks, receivers first,
// each in order of name.
func missingCredentials(ctx context.Context, p tenant.Pipeline, dataDir, tenantName string) ([]Credential, error) {
	dir := server.TLSDir(dataDir, tenantName)
	var out []Credential
	receivers, err := listAll(ctx, p.ListReceivers)
	if err != nil {
		return nil, err
	}
	for _, r := range receivers {
		if r.DIMSE.TLS == nil {
			continue
		}
		files := missing(dir, []tlsFile{{"TLS certificate", r.DIMSE.TLS.CertificateFile}, {"TLS private key", r.DIMSE.TLS.PrivateKeyFile}})
		if len(files) > 0 {
			out = append(out, Credential{Kind: "receiver", Name: r.Name, Files: files, Dir: dir})
		}
	}
	sending, ok := p.(tenant.Sending)
	if !ok {
		return out, nil
	}
	senders, err := listAll(ctx, sending.ListSenders)
	if err != nil {
		return nil, err
	}
	for _, s := range senders {
		var files []string
		for _, t := range []*tenant.SenderTLS{s.DIMSE.TLS, s.STOW.TLS} {
			if t == nil {
				continue
			}
			files = append(files, missing(dir, []tlsFile{{"TLS certificate authorities", t.CAFile}, {"TLS client certificate", t.CertificateFile},
				{"TLS private key", t.PrivateKeyFile}})...)
		}
		if len(files) > 0 {
			out = append(out, Credential{Kind: "sender", Name: s.Name, Files: files, Dir: dir})
		}
	}
	return out, nil
}

// tlsFile is one file a configuration names: its role and its name.
type tlsFile struct{ role, name string }

// missing returns, as "role name", each named file of files that is not in
// dir, following a symbolic link as the receiver or sender that reads it
// would (a link to nothing is missing). A file that cannot be looked at
// (no permission, a path through a file) is named as one that could not be
// checked, with the system's reason and not its path: a credential the
// report cannot vouch for is said so, and never fails the restore. A file
// is looked at, never opened.
func missing(dir string, files []tlsFile) []string {
	var out []string
	for _, f := range files {
		if f.name == "" {
			continue
		}
		_, err := os.Stat(filepath.Join(dir, filepath.Base(f.name)))
		switch {
		case err == nil:
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, f.role+" "+f.name)
		default:
			reason := err.Error()
			var pe *fs.PathError
			if errors.As(err, &pe) {
				reason = pe.Err.Error()
			}
			out = append(out, f.role+" "+f.name+" (could not be checked: "+reason+")")
		}
	}
	return out
}

// listAll reads every page of a list of a tenant's configuration.
func listAll[T any](ctx context.Context, list func(context.Context, master.Page) ([]T, string, error)) ([]T, error) {
	var out []T
	cursor := ""
	for range maxListPages {
		page, next, err := list(ctx, master.Page{Limit: listPage, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return out, nil
}
