package place

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/blob/s3"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Values answers the installation's settings a storage's place is checked
// against. An error is the server's: a database that does not answer, a
// stored value that does not follow its rule.
type Values interface {
	// DirectoryRoots returns storage.directory-roots.
	DirectoryRoots(ctx context.Context) (settings.DirectoryRoots, error)
	// S3Endpoints returns storage.s3-endpoints.
	S3Endpoints(ctx context.Context) (settings.S3Endpoints, error)
}

// Secrets hands back what a tenant's store keeps of an S3 storage beside
// its configuration, which tenant.Pipeline.Storage never returns.
type Secrets interface {
	// StorageSecret returns the sealed secret access key of the S3 storage
	// named name, or NotFound.
	StorageSecret(ctx context.Context, name string) ([]byte, error)
}

// Sealer seals a secret with the installation key (secret.Keys).
type Sealer interface {
	// Seal returns plaintext sealed, bound to context.
	Seal(context string, plaintext []byte) ([]byte, error)
}

// Keys opens what Sealer sealed (secret.Keys).
type Keys interface {
	// Open returns what sealed holds, or an error unless it was sealed with
	// an installation key bound to context.
	Open(context string, sealed []byte) ([]byte, error)
}

// Tenant is whose storage is opened.
type Tenant struct {
	// ID is the tenant's id, which a sealed secret is bound to.
	ID string
	// Name is the tenant's name, which its directory under a root is named
	// for.
	Name string
	// Secrets is the tenant's store's hand-back of sealed secrets, nil for a
	// store that keeps none; an S3 storage cannot be opened without it.
	Secrets Secrets
}

// ServerError is a failure to open a storage that is the server's, not the
// storage's: a database that does not answer, a store that cannot hand back
// a secret, a seal that does not open. A test of a storage answers it as the
// call's error, never as a step of the test.
type ServerError struct{ Err error }

// Error returns the cause's text, for the log.
func (e *ServerError) Error() string { return e.Err.Error() }

// Unwrap returns the cause, so its kind and message are the error's.
func (e *ServerError) Unwrap() error { return e.Err }

// errNoSecrets is the failure to open an S3 storage of a tenant whose store
// cannot hand its sealed secret back.
var errNoSecrets = errors.New("the tenant's store cannot hand back an S3 storage's sealed secret access key")

// SecretContext is what an S3 storage's sealed secret is bound to: the
// tenant's id and the storage's name, so a sealed secret copied to another
// storage's row, or another tenant's, does not open there (package secret:
// the context is part of what the seal authenticates).
func SecretContext(tenantID, storage string) string {
	return "tenant/" + tenantID + "/storage/" + storage + "/s3-secret"
}

// SealSecret seals an S3 storage's secret access key for its row
// (SecretContext). A sealed secret longer than a store keeps is the
// server's fault.
func SealSecret(k Sealer, tenantID, storage, secret string) ([]byte, error) {
	return sealSecret(k, tenantID, storage, []byte(secret))
}

// sealSecret is SealSecret of a secret held as bytes, which its caller may
// clear after.
func sealSecret(k Sealer, tenantID, storage string, secret []byte) ([]byte, error) {
	sealed, err := k.Seal(SecretContext(tenantID, storage), secret)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "place: the secret access key could not be sealed")
	}
	if len(sealed) > tenant.MaxSealedSecretLength {
		return nil, fault.New(fault.Internal, "place: a sealed secret access key is longer than the store keeps")
	}
	return sealed, nil
}

// KeyRing opens and seals secrets with the installation key (secret.Keys).
type KeyRing interface {
	Keys
	Sealer
}

// SwapSecrets returns the sealed secrets a switch's swap of the places of
// t's storages primary and replica writes (tenant.SwitchSwap): forPrimary
// is the replica's secret access key sealed for the primary's name, which
// comes to name the replica's place, and forReplica the primary's sealed
// for the replica's name; each nil when that place is a directory. A seal
// is bound to its storage's name (SecretContext), so the sealed bytes of a
// row moved to the other name would not open there. The secret is opened
// and sealed again here, where the installation key is; it is never
// written anywhere in the clear. A failure is the server's.
func SwapSecrets(ctx context.Context, k KeyRing, t Tenant, primary, replica tenant.Storage) (forPrimary, forReplica []byte, err error) {
	if forPrimary, err = resealFor(ctx, k, t, replica, primary.Name); err != nil {
		return nil, nil, err
	}
	if forReplica, err = resealFor(ctx, k, t, primary, replica.Name); err != nil {
		return nil, nil, err
	}
	return forPrimary, forReplica, nil
}

// resealFor returns the secret of t's storage s sealed for the storage named
// to, or nil when s is not an S3 storage.
func resealFor(ctx context.Context, k KeyRing, t Tenant, s tenant.Storage, to string) ([]byte, error) {
	if s.Type != tenant.StorageS3 {
		return nil, nil
	}
	if t.Secrets == nil {
		return nil, &ServerError{fault.Wrap(fault.Internal, errNoSecrets, "place: an S3 storage's secret cannot be sealed for its new name")}
	}
	sealed, err := t.Secrets.StorageSecret(ctx, s.Name)
	if err != nil {
		return nil, &ServerError{err}
	}
	plain, err := k.Open(SecretContext(t.ID, s.Name), sealed)
	if err != nil {
		return nil, &ServerError{fault.Wrap(fault.Internal, err, "place: an S3 storage's secret access key does not open with the installation key")}
	}
	defer clear(plain)
	out, err := sealSecret(k, t.ID, to, plain)
	if err != nil {
		return nil, &ServerError{err}
	}
	return out, nil
}

// Opener opens a tenant's storage through the storage port, where the
// installation's settings allow it, checked again on every open.
type Opener struct {
	// Values answers the installation's settings.
	Values Values
	// Keys opens an S3 storage's sealed secret.
	Keys Keys
	// StaleUploadAge is handed to the S3 driver (s3.Config.StaleUploadAge):
	// 0 for its default cleanup of unfinished uploads at open, a negative
	// value for none, as a test of a storage, which touches nothing but its
	// own object, asks.
	StaleUploadAge time.Duration
	// Cleanups, when set, is the process's record of the storages whose
	// crash leftovers have been removed: Open removes them only at the
	// first open of a storage in the process and opens beside it after
	// that (Cleanups). Nil, every Open removes them, as a process that
	// opens each storage once (a restore, an import) may.
	Cleanups *Cleanups
}

// Open opens the storage s of t: a directory storage at its directory,
// checked again as Resolve checks it; an S3 storage at its endpoint, checked
// again as AllowS3 checks it, with the address check its installation entry
// gives and its secret opened from its row. A failure of the server is a
// *ServerError; anything else is the storage's: its directory not there or
// behind a link, its endpoint no longer listed, or the driver's own failure.
func (o Opener) Open(ctx context.Context, t Tenant, s tenant.Storage) (blob.Store, error) {
	return o.open(ctx, t, s, openWrite)
}

// OpenForReading is Open for a user that only reads (a download, the
// metadata of an instance, the free space): the same rules, and a store
// that writes nothing when it opens and refuses Put and Delete
// (dir.OpenForReading, s3.Config.ForReading). Opening a storage to write
// removes what a crash left in it, which would remove the work in progress
// of a store already writing there; a reader opened per request must never
// do that, nor fail when another open runs beside it.
func (o Opener) OpenForReading(ctx context.Context, t Tenant, s tenant.Storage) (blob.Store, error) {
	return o.open(ctx, t, s, openRead)
}

// OpenBeside is Open for a user that writes a few objects beside the
// stores the server already has open on the storage (an owner record taken
// one generation further while a tenant runs): the same rules, and a store
// that writes, but removes nothing a crash left when it opens
// (dir.OpenBeside, and no cleanup of unfinished S3 uploads), since that
// would remove the work in progress of a store already writing there.
func (o Opener) OpenBeside(ctx context.Context, t Tenant, s tenant.Storage) (blob.Store, error) {
	return o.open(ctx, t, s, openBeside)
}

// openMode is how open opens a store: Open, OpenForReading or OpenBeside.
type openMode uint8

// The modes of open.
const (
	openWrite openMode = iota
	openRead
	openBeside
)

// open is Open, OpenForReading or OpenBeside, as mode says. An Open that
// would remove what a crash left, with Cleanups set, does so only when it is
// the first of the process at the storage's place, and opens beside it
// otherwise.
func (o Opener) open(ctx context.Context, t Tenant, s tenant.Storage, mode openMode) (bs blob.Store, err error) {
	at, err := o.resolve(ctx, t, s)
	if err != nil {
		return nil, err
	}
	cleans := mode == openWrite && (s.Type == tenant.StorageDirectory || o.StaleUploadAge >= 0)
	if !cleans || o.Cleanups == nil {
		return o.openAt(ctx, t, s, at, mode)
	}
	id, err := placeOf(s, at)
	if err != nil {
		return nil, err
	}
	claimed, first, err := o.Cleanups.claim(ctx, id)
	if err != nil {
		return nil, err
	}
	if !first {
		return o.openAt(ctx, t, s, at, openBeside)
	}
	// Deferred, so no way out of the open leaves the place claimed and
	// every later open of it waiting.
	defer func() { o.Cleanups.finish(claimed, err == nil && bs != nil && sweptWhole(bs)) }()
	return o.openAt(ctx, t, s, at, openWrite)
}

// openAt opens s, resolved at at, as mode says.
func (o Opener) openAt(ctx context.Context, t Tenant, s tenant.Storage, at resolved, mode openMode) (blob.Store, error) {
	forReading := mode == openRead
	if s.Type == tenant.StorageDirectory {
		switch mode {
		case openRead:
			return dir.OpenForReading(at.dir.Path())
		case openBeside:
			return dir.OpenBeside(at.dir.Path())
		}
		return dir.Open(at.dir.Path())
	}
	stale := o.StaleUploadAge
	if mode == openBeside {
		stale = -1
	}
	if t.Secrets == nil {
		return nil, &ServerError{fault.Wrap(fault.Internal, errNoSecrets, "place: an S3 storage cannot be opened")}
	}
	sealed, err := t.Secrets.StorageSecret(ctx, s.Name)
	if err != nil {
		return nil, &ServerError{err}
	}
	plain, err := o.Keys.Open(SecretContext(t.ID, s.Name), sealed)
	if err != nil {
		return nil, &ServerError{fault.Wrap(fault.Internal, err, "place: an S3 storage's secret access key does not open with the installation key")}
	}
	ep := at.endpoint
	return s3.Open(ctx, s3.Config{
		Endpoint: ep.URL, Region: s.S3.Region, Bucket: s.S3.Bucket, Prefix: s.S3.Prefix, PathStyle: s.S3.PathStyle,
		AccessKeyID: s3.NewSecret(s.S3.AccessKeyID), SecretAccessKey: s3.NewSecret(string(plain)),
		AllowHTTP: ep.Entry.PlainHTTP, AllowAddress: ep.AllowAddress(),
		StaleUploadAge: stale, ForReading: forReading, KeepVersions: s.S3.KeepVersions,
	})
}

// Check applies every rule Open applies to s, without opening it or
// reading its secret: for a user that keeps a store open across uses (the
// reception pipeline) and must still see a storage that is no longer
// allowed (a link planted below a root, an endpoint taken off the list) at
// its next use, as Open would.
func (o Opener) Check(ctx context.Context, t Tenant, s tenant.Storage) error {
	_, err := o.resolve(ctx, t, s)
	return err
}

// resolved is where a storage is, once its rules are applied.
type resolved struct {
	dir      Directory
	endpoint Endpoint
}

// resolve applies the rules of where s may be, read from the installation's
// settings now, and returns where it is.
func (o Opener) resolve(ctx context.Context, t Tenant, s tenant.Storage) (resolved, error) {
	switch s.Type {
	case tenant.StorageDirectory:
		roots, err := o.Values.DirectoryRoots(ctx)
		if err != nil {
			return resolved{}, &ServerError{err}
		}
		d, err := Resolve(roots, t.Name, s.Directory)
		if err != nil {
			return resolved{}, err
		}
		if !d.Exists() {
			return resolved{}, fault.Wrap(fault.NotFound, fs.ErrNotExist, "the storage's directory is not there")
		}
		return resolved{dir: d}, nil
	case tenant.StorageS3:
		entries, err := o.Values.S3Endpoints(ctx)
		if err != nil {
			return resolved{}, &ServerError{err}
		}
		ep, err := AllowS3(entries, s.S3)
		if err != nil {
			if fault.KindOf(err) == fault.Precondition {
				return resolved{}, err
			}
			return resolved{}, &ServerError{err}
		}
		return resolved{endpoint: ep}, nil
	}
	return resolved{}, &ServerError{fault.New(fault.Internal, "place: a storage of a type this server does not know")}
}

// TenantOf returns the tenant whose id is id and name is name, as Open
// opens its storages: its sealed secrets are handed back by store when the
// store keeps them (Secrets), and an S3 storage cannot be opened otherwise.
func TenantOf(id, name string, store any) Tenant {
	t := Tenant{ID: id, Name: name}
	if s, ok := store.(Secrets); ok {
		t.Secrets = s
	}
	return t
}
