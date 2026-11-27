package api

import (
	"context"
	"errors"
	"slices"
	"strconv"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// messageNoStorage answers a storage name the tenant does not have.
const messageNoStorage = "the tenant has no storage with this name"

// MaxSecretAccessKeyLength bounds an S3 storage's secret access key, the
// contract's 128 bytes of printable ASCII.
const MaxSecretAccessKeyLength = 128

// storageService is StorageService: a tenant's storages, kept in its store,
// placed where the installation's settings allow.
type storageService struct{ c *core }

var _ nopacsv1connect.StorageServiceHandler = storageService{}

// storageMask is what SetStorage's update_mask may name (storage.proto,
// SetStorageRequest.update_mask).
var storageMask = maskRules{
	fields: []string{
		string(tenant.FieldCapacityBytes), string(tenant.FieldDirectoryRoot), string(tenant.FieldDirectoryPath),
		string(tenant.FieldS3Endpoint), string(tenant.FieldS3Region), string(tenant.FieldS3Bucket),
		string(tenant.FieldS3Prefix), string(tenant.FieldS3PathStyle), string(tenant.FieldS3AccessKeyID),
		string(tenant.FieldS3SecretKey), string(tenant.FieldS3PurgeVersions), string(tenant.FieldTier),
		string(tenant.FieldS3RecallDays), string(tenant.FieldS3RecallTier), string(tenant.FieldEncryptionDeclared),
	},
	refused: map[string]string{
		"directory":                "names a type's whole message: name its fields one by one",
		"s3":                       "names a type's whole message: name its fields one by one, so a secret is never replaced by leaving it out",
		"name":                     "is the storage's name, which does not change",
		"create_time":              "is answered by the server and cannot be set",
		"update_time":              "is answered by the server and cannot be set",
		"s3.secret_access_key_set": "is answered by the server and cannot be set",
		"replica_of":               "is changed by attaching or detaching a replica, never by SetStorage",
		"switch_generation":        "is answered by the server and cannot be set",
	},
}

// storageTier returns an Invalid error naming field unless tier is a value
// the contract knows; and, for a directory storage (directory true),
// unless it is online, since a directory reads every file at once
// (storage.proto, Storage.tier).
func storageTier(field string, tier v1.StorageTier, directory bool) error {
	switch tier {
	case v1.StorageTier_STORAGE_TIER_UNSPECIFIED, v1.StorageTier_STORAGE_TIER_ONLINE:
		return nil
	case v1.StorageTier_STORAGE_TIER_ARCHIVE:
		if directory {
			return invalid(field, "is online for a directory storage: a directory reads every file at once")
		}
		return nil
	}
	return invalid(field, "is online or archive")
}

// refuseNotServed records a request that asks for a part of the contract
// the server does not serve yet, as one that did not complete, and answers
// it UNIMPLEMENTED with msg.
func (c *core) refuseNotServed(d access.Decision, st tenant.Store, msg string) error {
	c.record(d, st, master.OutcomeFailed, msg, access.Facts{})
	return codes.Ensure(connect.NewError(connect.CodeUnimplemented, msg))
}

// recallSpeedOf returns the recall speed an S3 storage's recall_tier names:
// unspecified is the store's standard.
func recallSpeedOf(field string, t v1.S3RecallTier) (tenant.RecallSpeed, error) {
	switch t {
	case v1.S3RecallTier_S3_RECALL_TIER_UNSPECIFIED:
		return "", nil
	case v1.S3RecallTier_S3_RECALL_TIER_STANDARD:
		return tenant.RecallStandard, nil
	case v1.S3RecallTier_S3_RECALL_TIER_BULK:
		return tenant.RecallBulk, nil
	case v1.S3RecallTier_S3_RECALL_TIER_EXPEDITED:
		return tenant.RecallExpedited, nil
	}
	return "", invalid(field, "is standard, bulk or expedited")
}

// recallTierMessage returns a recall speed as the contract writes it.
func recallTierMessage(sp tenant.RecallSpeed) v1.S3RecallTier {
	switch sp {
	case tenant.RecallBulk:
		return v1.S3RecallTier_S3_RECALL_TIER_BULK
	case tenant.RecallExpedited:
		return v1.S3RecallTier_S3_RECALL_TIER_EXPEDITED
	}
	return v1.S3RecallTier_S3_RECALL_TIER_STANDARD
}

// tierOf returns the tier a request names: unspecified is online.
func tierOf(t v1.StorageTier) tenant.StorageTier {
	if t == v1.StorageTier_STORAGE_TIER_ARCHIVE {
		return tenant.TierArchive
	}
	return tenant.TierOnline
}

// storageType returns the type a request's storage names, for an audit
// entry: "directory", "s3", or "none".
func storageType(m *v1.Storage) string {
	switch m.GetType().(type) {
	case *v1.Storage_Directory:
		return string(tenant.StorageDirectory)
	case *v1.Storage_S3:
		return string(tenant.StorageS3)
	}
	return "none"
}

// storageFromRequest returns the storage a request gives, field, and its
// secret access key, checked by every rule of the contract that does not
// depend on what exists. The secret is returned apart, unsealed, and never
// put in a message.
func storageFromRequest(field string, m *v1.Storage) (tenant.Storage, string, error) {
	if m == nil {
		return tenant.Storage{}, "", invalid(field, "is required")
	}
	if err := itemName(field+".name", m.GetName()); err != nil {
		return tenant.Storage{}, "", err
	}
	s := tenant.Storage{Name: m.GetName(), CapacityBytes: m.GetCapacityBytes(), ReplicaOf: m.GetReplicaOf(), Tier: tierOf(m.GetTier())}
	if s.CapacityBytes < 0 || s.CapacityBytes > tenant.MaxCapacityBytes {
		return tenant.Storage{}, "", invalid(field+".capacity_bytes", "is 0 to 2^62")
	}
	if s.ReplicaOf != "" {
		if err := itemName(field+".replica_of", s.ReplicaOf); err != nil {
			return tenant.Storage{}, "", err
		}
	}
	var secret string
	switch t := m.GetType().(type) {
	case *v1.Storage_Directory:
		s.Type = tenant.StorageDirectory
		s.Directory = tenant.DirectoryStorage{Root: t.Directory.GetRoot(), Path: t.Directory.GetPath()}
		s.EncryptedAtRestDeclared = t.Directory.GetEncryptedAtRestDeclared()
		if err := itemName(field+".directory.root", s.Directory.Root); err != nil {
			return tenant.Storage{}, "", err
		}
	case *v1.Storage_S3:
		s.Type = tenant.StorageS3
		x := t.S3
		s.S3 = tenant.S3Storage{Endpoint: x.GetEndpoint(), Region: x.GetRegion(), Bucket: x.GetBucket(), Prefix: x.GetPrefix(), PathStyle: x.GetPathStyle(),
			AccessKeyID: x.GetAccessKeyId(), KeepVersions: keepVersions(x), RecallDays: int(x.GetRecallDays())}
		var err error
		if s.S3.RecallSpeed, err = recallSpeedOf(field+".s3.recall_tier", x.GetRecallTier()); err != nil {
			return tenant.Storage{}, "", err
		}
		if s.Tier != tenant.TierArchive {
			// The contract ignores recall settings on an online storage; the
			// store keeps them empty there.
			s.S3.RecallDays, s.S3.RecallSpeed = 0, ""
		}
		secret = x.GetSecretAccessKey()
		if err := secretAccessKey(field+".s3.secret_access_key", secret); err != nil {
			return tenant.Storage{}, "", err
		}
	default:
		return tenant.Storage{}, "", invalid(field, "has a type: directory or s3")
	}
	// The port's own rules, with a stand-in for the sealed secret, which is
	// made only once the request is known to be valid.
	check := s
	if s.Type == tenant.StorageS3 {
		check.S3.SealedSecret = []byte{0}
	}
	if err := check.Validate(); err != nil {
		return tenant.Storage{}, "", fieldOf(field, err)
	}
	return s, secret, nil
}

// secretAccessKey returns an Invalid error naming field unless secret is a
// secret access key the contract accepts: 1 to 128 bytes of printable
// ASCII. The message never holds the secret.
func secretAccessKey(field, secret string) error {
	if secret == "" {
		return invalid(field, "is required and not empty")
	}
	if len(secret) > MaxSecretAccessKeyLength {
		return invalid(field, "is at most 128 bytes")
	}
	for i := 0; i < len(secret); i++ {
		if secret[i] <= 0x20 || secret[i] >= 0x7f {
			return invalid(field, "is printable ASCII")
		}
	}
	return nil
}

// storageChange returns the values a SetStorage request gives, whatever its
// type, for tenant.ApplyStorage to take the fields its mask names from.
func storageChange(m *v1.Storage) (tenant.Storage, string, error) {
	d, x := m.GetDirectory(), m.GetS3()
	speed, err := recallSpeedOf("storage.s3.recall_tier", x.GetRecallTier())
	if err != nil {
		return tenant.Storage{}, "", err
	}
	return tenant.Storage{
		Name:                    m.GetName(),
		CapacityBytes:           m.GetCapacityBytes(),
		Tier:                    tierOf(m.GetTier()),
		EncryptedAtRestDeclared: d.GetEncryptedAtRestDeclared(),
		Directory:               tenant.DirectoryStorage{Root: d.GetRoot(), Path: d.GetPath()},
		S3: tenant.S3Storage{
			Endpoint: x.GetEndpoint(), Region: x.GetRegion(), Bucket: x.GetBucket(), Prefix: x.GetPrefix(),
			PathStyle: x.GetPathStyle(), AccessKeyID: x.GetAccessKeyId(), KeepVersions: keepVersions(x),
			RecallDays: int(x.GetRecallDays()), RecallSpeed: speed,
		},
	}, x.GetSecretAccessKey(), nil
}

// keepVersions reads S3Storage.purge_versions as the store keeps it: only
// false, given, keeps older versions; absent means true (storage.proto), so
// a request that leaves it out, an AddStorage or a SetStorage whose mask
// names it alike, has a purge delete them. GetPurgeVersions alone would
// read absent as false.
func keepVersions(x *v1.S3Storage) bool {
	return x != nil && x.PurgeVersions != nil && !*x.PurgeVersions
}

// storageMessage returns a storage as the contract writes it: never its
// secret, only whether one is held.
func storageMessage(s tenant.Storage) *v1.Storage {
	m := &v1.Storage{Name: s.Name, CapacityBytes: s.CapacityBytes, CreateTime: timestamp(s.Created), UpdateTime: timestamp(s.Updated),
		Tier: tierMessage(s.Tier), ReplicaOf: s.ReplicaOf, SwitchGeneration: s.SwitchGeneration}
	switch s.Type {
	case tenant.StorageDirectory:
		m.Type = &v1.Storage_Directory{Directory: &v1.DirectoryStorage{Root: s.Directory.Root, Path: s.Directory.Path,
			EncryptedAtRestDeclared: s.EncryptedAtRestDeclared}}
	case tenant.StorageS3:
		m.Type = &v1.Storage_S3{S3: &v1.S3Storage{
			Endpoint: s.S3.Endpoint, Region: s.S3.Region, Bucket: s.S3.Bucket, Prefix: s.S3.Prefix,
			PathStyle: s.S3.PathStyle, AccessKeyId: s.S3.AccessKeyID, SecretAccessKeySet: s.S3.SecretSet,
			// Always answered, true or false, so a reader never has to know
			// that absent means true.
			PurgeVersions: proto.Bool(!s.S3.KeepVersions),
		}}
		if s.Tier == tenant.TierArchive {
			x := m.GetS3()
			x.RecallDays = int32(s.S3.RecallDays) //nolint:gosec // G115: at most 365.
			if x.RecallDays == 0 {
				x.RecallDays = tenant.DefaultRecallDays
			}
			x.RecallTier = recallTierMessage(s.S3.RecallSpeed)
		}
	}
	return m
}

// lookupStorage reads a storage, answering one the tenant does not have as
// NotFound with the contract's message.
func lookupStorage(ctx context.Context, p tenant.Pipeline, name string) (tenant.Storage, error) {
	s, err := p.Storage(ctx, name)
	if fault.KindOf(err) == fault.NotFound {
		return s, fault.Wrap(fault.NotFound, err, messageNoStorage)
	}
	return s, err
}

// AddStorage adds a storage to a tenant, where the installation allows it:
// a directory under a root it lists, made by the server and absent or empty
// before; an S3 bucket at an endpoint it lists, with its secret sealed for
// the storage's row before it reaches the store.
func (s storageService) AddStorage(ctx context.Context, req *v1.AddStorageRequest) (*v1.AddStorageResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.StorageServiceAddStorageProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), map[string]string{detailType: storageType(req.GetStorage())})
	if err != nil {
		return nil, err
	}
	in, secret, err := storageFromRequest("storage", req.GetStorage())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if err := storageTier("storage.tier", req.GetStorage().GetTier(), in.Type == tenant.StorageDirectory); err != nil {
		return nil, c.failIn(d, st, err)
	}
	ref, _ := d.Tenant()
	c.places.RLock()
	defer c.places.RUnlock()
	switch in.Type {
	case tenant.StorageDirectory:
		// A port refusal after this (a name taken, a path another storage
		// holds) leaves the directory made, empty, which a later add on the
		// same path accepts.
		if err := c.makeDirectory(ctx, p, req.GetTenant(), in); err != nil {
			return nil, c.failIn(d, st, err)
		}
	case tenant.StorageS3:
		ep, err := c.allowS3(ctx, in.S3)
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		in.S3.Endpoint = ep.URL
		if in.S3.SealedSecret, err = place.SealSecret(c.keys, ref.ID, in.Name, secret); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	out, err := p.AddStorage(ctx, in, c.now(), changeEntry(d, access.Facts{}), c.adminGuard(d))
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("storage", err))
	}
	return &v1.AddStorageResponse{Storage: storageMessage(out)}, nil
}

// GetStorage answers one storage of a tenant, without its secret.
func (s storageService) GetStorage(ctx context.Context, req *v1.GetStorageRequest) (*v1.GetStorageResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.StorageServiceGetStorageProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	out, err := lookupStorage(ctx, p, req.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	c.done(d, st)
	return &v1.GetStorageResponse{Storage: storageMessage(out)}, nil
}

// ListStorages answers a page of a tenant's storages, ordered by name.
func (s storageService) ListStorages(ctx context.Context, req *v1.ListStoragesRequest) (*v1.ListStoragesResponse, error) {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.StorageServiceListStoragesProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	page, err := pageOf(req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	items, next, err := p.ListStorages(ctx, page)
	if err != nil {
		return nil, c.failIn(d, st, listError(err, page.Cursor))
	}
	c.done(d, st)
	out := &v1.ListStoragesResponse{Storages: make([]*v1.Storage, 0, len(items)), NextPageToken: next}
	for _, it := range items {
		out.Storages = append(out.Storages, storageMessage(it))
	}
	return out, nil
}

// SetStorage changes the fields of a storage its update_mask names, under
// every rule AddStorage applies to them.
func (s storageService) SetStorage(ctx context.Context, req *v1.SetStorageRequest) (*v1.SetStorageResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.StorageServiceSetStorageProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), maskDetails(req.GetUpdateMask().GetPaths()))
	if err != nil {
		return nil, err
	}
	in := req.GetStorage()
	if in == nil {
		return nil, c.failIn(d, st, invalid("storage", "is required: its name names the storage to change"))
	}
	if err := itemName("storage.name", in.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p == "tier" {
			if err := storageTier("storage.tier", in.GetTier(), false); err != nil {
				return nil, c.failIn(d, st, err)
			}
		}
	}
	paths, err := storageMask.paths(req.GetUpdateMask())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	fields := make([]tenant.StorageField, len(paths))
	for i, p := range paths {
		fields[i] = tenant.StorageField(p)
	}
	change, secret, err := storageChange(in)
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	newSecret := slices.Contains(fields, tenant.FieldS3SecretKey)
	if slices.Contains(fields, tenant.FieldS3Endpoint) && !newSecret {
		// Credentials are given for one store; sent to another host they
		// would sign its requests, and so hand them to whoever runs it.
		return nil, c.failIn(d, st, invalid("update_mask", "names s3.endpoint without s3.access_key_id and s3.secret_access_key: an S3 storage moved to another endpoint is given the credentials for it, never keeps those given for the endpoint it leaves"))
	}
	if newSecret {
		if err := secretAccessKey("storage.s3.secret_access_key", secret); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	ref, _ := d.Tenant()
	c.places.RLock()
	defer c.places.RUnlock()
	stored, err := lookupStorage(ctx, p, in.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	if newSecret {
		if change.S3.SealedSecret, err = place.SealSecret(c.keys, ref.ID, in.GetName(), secret); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	result, location, err := tenant.ApplyStorage(stored, change, fields)
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("storage", err))
	}
	switch {
	case result.Type == tenant.StorageS3:
		ep, err := c.allowS3(ctx, result.S3)
		if err != nil {
			return nil, c.failIn(d, st, err)
		}
		change.S3.Endpoint = ep.URL
	case result.Type == tenant.StorageDirectory && location:
		if err := c.makeDirectory(ctx, p, req.GetTenant(), result); err != nil {
			return nil, c.failIn(d, st, err)
		}
	}
	out, err := p.SetStorage(ctx, change, fields, c.now(), changeEntry(d, access.Facts{}), c.adminGuard(d))
	if err != nil {
		return nil, c.failIn(d, st, fieldOf("storage", err))
	}
	return &v1.SetStorageResponse{Storage: storageMessage(out)}, nil
}

// maskDetails returns the audit details of a partial change: its mask's
// paths, as given, cut to what a detail holds.
func maskDetails(paths []string) map[string]string {
	return map[string]string{detailMask: clip(joinDetail(paths), master.MaxAuditFieldLength-8)}
}

// RemoveStorage removes a storage's configuration, never a file; the store
// refuses it while the storage holds data or a receiver names it.
func (s storageService) RemoveStorage(ctx context.Context, req *v1.RemoveStorageRequest) (*v1.RemoveStorageResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.StorageServiceRemoveStorageProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	// Read first for its type, which the entry records; a type never
	// changes, so the read and the removal agree on it.
	stored, err := lookupStorage(ctx, p, req.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	facts := access.Facts{Details: map[string]string{detailType: string(stored.Type)}}
	out, err := p.RemoveStorage(ctx, req.GetName(), changeEntry(d, facts), c.adminGuard(d))
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoStorage)
	}
	if err != nil {
		return nil, c.failWith(d, st, facts, err)
	}
	return &v1.RemoveStorageResponse{Storage: storageMessage(out)}, nil
}

// TestStorage writes, reads back, compares and removes one small object on
// a storage, and answers each step. A storage that fails a step is a result,
// not an error; a failure of the server (a store that cannot hand back an S3
// storage's secret) is an error.
func (s storageService) TestStorage(ctx context.Context, req *v1.TestStorageRequest) (*v1.TestStorageResponse, error) {
	c := s.c
	d, err := c.changing(ctx, nopacsv1connect.StorageServiceTestStorageProcedure)
	if err != nil {
		return nil, err
	}
	p, st, err := c.inTenant(ctx, d, req.GetTenant(), nil)
	if err != nil {
		return nil, err
	}
	if err := itemName("name", req.GetName()); err != nil {
		return nil, c.failIn(d, st, err)
	}
	stored, err := lookupStorage(ctx, p, req.GetName())
	if err != nil {
		return nil, c.failIn(d, st, err)
	}
	ref, _ := d.Tenant()
	var serverFault error
	results := probe(ctx, faultOfType(stored.Type), func(ctx context.Context) (blob.Store, error) {
		bs, err := c.openToTest(ctx, placeTenant(ref.ID, req.GetTenant(), p), stored)
		var se *place.ServerError
		if errors.As(err, &se) {
			serverFault = se.Err
		}
		return bs, err
	})
	if serverFault != nil {
		return nil, c.failIn(d, st, serverFault)
	}
	passed := passedAll(results)
	c.record(d, st, master.OutcomeDone, "", access.Facts{Details: map[string]string{detailPassed: strconv.FormatBool(passed)}})
	out := &v1.TestStorageResponse{Passed: passed, Results: make([]*v1.StorageTestResult, 0, len(results))}
	for _, r := range results {
		out.Results = append(out.Results, r.message())
	}
	return out, nil
}

// makeDirectory checks the directory a directory storage s of the tenant
// named tenantName, whose pipeline is p, is to be at (place.CheckDirectory,
// against the tenant's other storages) and makes it.
func (c *core) makeDirectory(ctx context.Context, p tenant.Pipeline, tenantName string, s tenant.Storage) error {
	roots, err := installationPlaces{c}.DirectoryRoots(ctx)
	if err != nil {
		return err
	}
	others, err := tenantStorages(ctx, p)
	if err != nil {
		return err
	}
	dir, err := place.CheckDirectory(roots, tenantName, s, others)
	if err != nil {
		return err
	}
	return dir.Make()
}

// allowS3 returns the endpoint of an S3 storage's configuration as the
// installation allows it (place.AllowS3).
func (c *core) allowS3(ctx context.Context, s tenant.S3Storage) (place.Endpoint, error) {
	entries, err := installationPlaces{c}.S3Endpoints(ctx)
	if err != nil {
		return place.Endpoint{}, err
	}
	return place.AllowS3(entries, s)
}
