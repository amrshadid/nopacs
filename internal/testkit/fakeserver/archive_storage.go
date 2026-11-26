package fakeserver

import (
	"context"
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// storageHandler implements StorageService.
type storageHandler struct{ s *Server }

// withoutSecret returns the storage as an answer carries it: the secret empty
// and whether one is held set.
func withoutSecret(st *v1.Storage, held bool) *v1.Storage {
	out := proto.Clone(st).(*v1.Storage)
	if s3 := out.GetS3(); s3 != nil {
		s3.SecretAccessKey = ""
		s3.SecretAccessKeySet = held
		if s3.PurgeVersions == nil {
			// Absent means true, and the server always answers it.
			s3.PurgeVersions = proto.Bool(true)
		}
	}
	return out
}

// AddStorage adds a storage and keeps its secret apart.
func (h storageHandler) AddStorage(_ context.Context, req *v1.AddStorageRequest) (*v1.AddStorageResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	st := req.GetStorage()
	if err := itemOK("storage.name", st.GetName()); err != nil {
		return nil, err
	}
	if st.GetType() == nil {
		return nil, fault.New(fault.Invalid, "storage: names neither a directory nor an S3 type")
	}
	if s3 := st.GetS3(); s3 != nil && (s3.GetSecretAccessKey() == "" || s3.GetAccessKeyId() == "") {
		return nil, fault.New(fault.Invalid, "storage.s3: access_key_id and secret_access_key are required")
	}
	if err := s.allowEndpoint(st.GetS3()); err != nil {
		return nil, err
	}
	k := key(req.GetTenant(), st.GetName())
	if _, ok := s.arch.storages[k]; ok {
		return nil, fault.New(fault.Conflict, "the tenant has a storage with that name")
	}
	stored := proto.Clone(st).(*v1.Storage)
	if s3 := stored.GetS3(); s3 != nil {
		s.arch.secrets[k] = s3.GetSecretAccessKey()
	}
	stored.CreateTime, stored.UpdateTime = timestamppb.New(s.now()), timestamppb.New(s.now())
	s.arch.storages[k] = stored
	_, held := s.arch.secrets[k]
	return &v1.AddStorageResponse{Storage: withoutSecret(stored, held)}, nil
}

// allowEndpoint refuses, as a precondition, an S3 storage that addresses its
// bucket in the host name at an endpoint the installation's
// storage.s3-endpoints marks private-address: with the bucket in the host
// name, a tenant's bucket name would choose which host in the server's
// network is reached. A storage with no S3 configuration, and an endpoint the
// setting does not list, pass: the fake keeps no list of its own.
func (s *Server) allowEndpoint(st *v1.S3Storage) error {
	if st == nil || st.GetPathStyle() {
		return nil
	}
	entries, err := settings.ParseS3Endpoints(s.settings[""][settings.StorageS3Endpoints])
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.URL == st.GetEndpoint() && e.PrivateAddress {
			return fault.New(fault.Precondition, "storage.s3.path_style: the installation's entry for this endpoint allows private addresses, so the bucket must be addressed in the path")
		}
	}
	return nil
}

// find returns a tenant's storage or the contract's NOT_FOUND.
func (a *archive) findStorage(tenant, name string) (*v1.Storage, error) {
	st, ok := a.storages[key(tenant, name)]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no storage with this name")
	}
	return st, nil
}

// GetStorage answers one storage.
func (h storageHandler) GetStorage(_ context.Context, req *v1.GetStorageRequest) (*v1.GetStorageResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	st, err := s.arch.findStorage(req.GetTenant(), req.GetName())
	if err != nil {
		return nil, err
	}
	_, held := s.arch.secrets[key(req.GetTenant(), req.GetName())]
	return &v1.GetStorageResponse{Storage: withoutSecret(st, held)}, nil
}

// ListStorages answers a page of a tenant's storages by name.
func (h storageHandler) ListStorages(_ context.Context, req *v1.ListStoragesRequest) (*v1.ListStoragesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	page, next, err := pageOfKeys(forTenant(s.arch.storages, req.GetTenant()), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := &v1.ListStoragesResponse{NextPageToken: next}
	for _, k := range page {
		_, held := s.arch.secrets[k]
		out.Storages = append(out.Storages, withoutSecret(s.arch.storages[k], held))
	}
	return out, nil
}

// SetStorage changes the fields the mask names; it knows the contract's
// paths and the rule that the two credentials change together.
func (h storageHandler) SetStorage(_ context.Context, req *v1.SetStorageRequest) (*v1.SetStorageResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, fault.New(fault.Invalid, "update_mask: is required and not empty")
	}
	k := key(req.GetTenant(), req.GetStorage().GetName())
	cur, err := s.arch.findStorage(req.GetTenant(), req.GetStorage().GetName())
	if err != nil {
		return nil, err
	}
	in := req.GetStorage()
	if slices.Contains(paths, "s3.access_key_id") != slices.Contains(paths, "s3.secret_access_key") {
		return nil, fault.New(fault.Invalid, "update_mask: s3.access_key_id and s3.secret_access_key change together")
	}
	if slices.Contains(paths, "s3.endpoint") && !slices.Contains(paths, "s3.secret_access_key") {
		return nil, fault.New(fault.Invalid, "update_mask: s3.endpoint is named with s3.access_key_id and s3.secret_access_key: credentials given for one store are never kept for another")
	}
	next := proto.Clone(cur).(*v1.Storage)
	for _, p := range paths {
		switch p {
		case "capacity_bytes":
			next.CapacityBytes = in.GetCapacityBytes()
		case "tier":
			if in.GetTier() == v1.StorageTier_STORAGE_TIER_ARCHIVE && next.GetReplicaOf() == "" {
				return nil, fault.New(fault.Precondition, "tier: the archive tier is only for a replica")
			}
			next.Tier = in.GetTier()
		case "s3.recall_days", "s3.recall_tier":
			if next.GetS3() == nil {
				return nil, fault.New(fault.Invalid, "update_mask: "+p+" is a field of the other type")
			}
			if p == "s3.recall_days" {
				next.GetS3().RecallDays = in.GetS3().GetRecallDays()
			} else {
				next.GetS3().RecallTier = in.GetS3().GetRecallTier()
			}
		case "directory.encrypted_at_rest_declared":
			if next.GetDirectory() == nil {
				return nil, fault.New(fault.Invalid, "update_mask: "+p+" is a field of the other type")
			}
			next.GetDirectory().EncryptedAtRestDeclared = in.GetDirectory().GetEncryptedAtRestDeclared()
		case "directory.root", "directory.path":
			if next.GetDirectory() == nil {
				return nil, fault.New(fault.Invalid, "update_mask: "+p+" is a field of the other type")
			}
			if p == "directory.root" {
				next.GetDirectory().Root = in.GetDirectory().GetRoot()
			} else {
				next.GetDirectory().Path = in.GetDirectory().GetPath()
			}
		case "s3.endpoint", "s3.region", "s3.bucket", "s3.prefix", "s3.path_style", "s3.access_key_id", "s3.secret_access_key", "s3.purge_versions":
			d := next.GetS3()
			if d == nil {
				return nil, fault.New(fault.Invalid, "update_mask: "+p+" is a field of the other type")
			}
			switch p {
			case "s3.endpoint":
				d.Endpoint = in.GetS3().GetEndpoint()
			case "s3.region":
				d.Region = in.GetS3().GetRegion()
			case "s3.bucket":
				d.Bucket = in.GetS3().GetBucket()
			case "s3.prefix":
				d.Prefix = in.GetS3().GetPrefix()
			case "s3.path_style":
				d.PathStyle = in.GetS3().GetPathStyle()
			case "s3.access_key_id":
				d.AccessKeyId = in.GetS3().GetAccessKeyId()
			case "s3.purge_versions":
				// Absent means true, as the server reads it.
				d.PurgeVersions = proto.Bool(in.GetS3().PurgeVersions == nil || in.GetS3().GetPurgeVersions())
			case "s3.secret_access_key":
				if in.GetS3().GetSecretAccessKey() == "" {
					return nil, fault.New(fault.Invalid, "storage.s3.secret_access_key: must not be empty")
				}
				s.arch.secrets[k] = in.GetS3().GetSecretAccessKey()
			}
		default:
			return nil, fault.New(fault.Invalid, "update_mask: "+p+" names no field that can change")
		}
	}
	if err := s.allowEndpoint(next.GetS3()); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.now())
	s.arch.storages[k] = next
	_, held := s.arch.secrets[k]
	return &v1.SetStorageResponse{Storage: withoutSecret(next, held)}, nil
}

// RemoveStorage removes a storage's configuration unless it holds files or a
// receiver names it.
func (h storageHandler) RemoveStorage(_ context.Context, req *v1.RemoveStorageRequest) (*v1.RemoveStorageResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	st, err := s.arch.findStorage(req.GetTenant(), req.GetName())
	if err != nil {
		return nil, err
	}
	k := key(req.GetTenant(), req.GetName())
	if s.arch.holdsFiles[k] {
		return nil, fault.New(fault.Precondition, "the storage holds archived files")
	}
	for _, rk := range forTenant(s.arch.receivers, req.GetTenant()) {
		if s.arch.receivers[rk].GetStorage() == req.GetName() {
			return nil, fault.New(fault.Precondition, "a receiver names the storage")
		}
	}
	_, held := s.arch.secrets[k]
	delete(s.arch.storages, k)
	delete(s.arch.secrets, k)
	return &v1.RemoveStorageResponse{Storage: withoutSecret(st, held)}, nil
}

// TestStorage reports four steps, passed unless a test made one fail.
func (h storageHandler) TestStorage(_ context.Context, req *v1.TestStorageRequest) (*v1.TestStorageResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := s.arch.findStorage(req.GetTenant(), req.GetName()); err != nil {
		return nil, err
	}
	failed, bad := s.arch.testFaults[key(req.GetTenant(), req.GetName())]
	out := &v1.TestStorageResponse{Passed: !bad}
	steps := []v1.StorageTestStep{v1.StorageTestStep_STORAGE_TEST_STEP_WRITE, v1.StorageTestStep_STORAGE_TEST_STEP_READ, v1.StorageTestStep_STORAGE_TEST_STEP_COMPARE, v1.StorageTestStep_STORAGE_TEST_STEP_REMOVE}
	broken := false
	for _, step := range steps {
		r := &v1.StorageTestResult{Step: step, Outcome: v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_PASSED, Duration: durationpb.New(12_000_000)}
		switch {
		case bad && step == failed:
			r.Outcome, r.Fault = v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_FAILED, v1.StorageFault_STORAGE_FAULT_UNREACHABLE
			broken = true
		case broken && step != v1.StorageTestStep_STORAGE_TEST_STEP_REMOVE:
			r.Outcome, r.Duration = v1.StorageTestOutcome_STORAGE_TEST_OUTCOME_SKIPPED, nil
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}
