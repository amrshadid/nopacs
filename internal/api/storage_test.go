package api_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
)

// testAddStorage guards AddStorage: admin on the tenant and no other role,
// at either scope; a tenant the caller may not know answered as no grant;
// the order of checks; each refusal with its code; the directory made with
// a mode only the server's user can use; and the entry in the tenant's log
// with the storage's name and type, or none there when refused.
func testAddStorage(t *testing.T) {
	f := newPlaceFixture(t)
	f.downTenant("down")
	add := func(tenantName string, s *v1.Storage) func(clients) outcome { return addStorage(tenantName, s) }
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: add("lab1", dirStorage("main", "fast", "mri")), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: add("lab1", dirStorage("main", "fast", "mri")), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "operator on the tenant", as: f.lab1Operator, call: add("lab1", dirStorage("main", "fast", "mri")), code: connect.CodePermissionDenied},
		{name: "admin on the installation is no role inside a tenant", as: f.instAdmin, call: add("lab1", dirStorage("main", "fast", "mri")), code: connect.CodePermissionDenied},
		{name: "admin of another tenant", as: f.lab2Admin, call: add("lab1", dirStorage("main", "fast", "mri")), code: connect.CodePermissionDenied},
		{name: "no such tenant, no right to know", as: f.lab1Admin, call: add("nolab", dirStorage("main", "fast", "mri")), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "no such tenant, may list", as: local, call: add("nolab", dirStorage("main", "fast", "mri")), code: connect.CodeNotFound},
		{name: "a malformed tenant, no role on the installation", as: f.lab1Admin, call: add("Lab 1", dirStorage("main", "fast", "mri")), code: connect.CodePermissionDenied},
		{name: "a malformed tenant, admin on the installation", as: f.instAdmin, call: add("Lab 1", dirStorage("main", "fast", "mri")), code: connect.CodeInvalidArgument, msg: "tenant"},
		{name: "unauthenticated before invalid", as: nobody, call: add("lab1", nil), code: connect.CodeUnauthenticated},
		{name: "denied before invalid", as: f.lab1Reader, call: add("lab1", nil), code: connect.CodePermissionDenied},
		{name: "no storage", as: f.lab1Admin, call: add("lab1", nil), code: connect.CodeInvalidArgument, msg: "storage: is required", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a bad name", as: f.lab1Admin, call: add("lab1", dirStorage("Main", "fast", "")), code: connect.CodeInvalidArgument, msg: "storage.name"},
		{name: "no type", as: f.lab1Admin, call: add("lab1", &v1.Storage{Name: "main"}), code: connect.CodeInvalidArgument, msg: "has a type"},
		{name: "a negative capacity", as: f.lab1Admin, call: add("lab1", &v1.Storage{Name: "main", CapacityBytes: -1, Type: dirStorage("x", "fast", "").Type}), code: connect.CodeInvalidArgument, msg: "capacity_bytes"},
		{name: "a root not listed", as: f.lab1Admin, call: add("lab1", dirStorage("main", "slow", "")), code: connect.CodeFailedPrecondition, msg: "no directory root named slow",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "main", details: map[string]string{"type": "directory"}}},
		{name: "a tenant not ready", as: local, call: add("down", dirStorage("main", "fast", "")), code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
		// A replica names a primary the tenant has; the archive tier is for
		// a replica only.
		{name: "a replica of a primary the tenant does not have", as: f.lab1Admin, call: add("lab1", withStorage(dirStorage("copy", "fast", "copy"), func(s *v1.Storage) { s.ReplicaOf = "main" })),
			code: connect.CodeFailedPrecondition, audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a replica_of that is not a name", as: f.lab1Admin, call: add("lab1", withStorage(dirStorage("copy", "fast", "copy"), func(s *v1.Storage) { s.ReplicaOf = "Main" })),
			code: connect.CodeInvalidArgument, msg: "storage.replica_of"},
		{name: "the archive tier, not a replica", as: f.lab1Admin, call: add("lab1", withStorage(s3Storage("cold", "https://s3.example.com", "x"), func(s *v1.Storage) { s.Tier = v1.StorageTier_STORAGE_TIER_ARCHIVE })),
			code: connect.CodeFailedPrecondition, audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "the archive tier on a directory", as: f.lab1Admin, call: add("lab1", withStorage(dirStorage("cold", "fast", "cold"), func(s *v1.Storage) { s.Tier = v1.StorageTier_STORAGE_TIER_ARCHIVE })),
			code: connect.CodeInvalidArgument, msg: "a directory storage is always online"},
		{name: "a tier this version does not know", as: f.lab1Admin, call: add("lab1", withStorage(dirStorage("odd", "fast", "odd"), func(s *v1.Storage) { s.Tier = 7 })),
			code: connect.CodeInvalidArgument, msg: "storage.tier: is online or archive"},
		{name: "added", as: f.lab1Admin, call: add("lab1", dirStorage("main", "fast", "mri")),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "main", tenant: "lab1", details: map[string]string{"type": "directory"}},
			check: func(t *testing.T, o outcome) {
				s := o.res.(*v1.AddStorageResponse).GetStorage()
				if s.GetName() != "main" || s.GetDirectory().GetRoot() != "fast" || s.GetDirectory().GetPath() != "mri" || s.GetCreateTime() == nil {
					t.Errorf("%v", s)
				}
				info, err := os.Stat(filepath.Join(f.root, "lab1", "mri"))
				// Windows has no owner-only mode bits: a directory's Perm is
				// 0o777 there whatever was asked, and access is by its ACL.
				if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
					t.Errorf("the directory made: %v %v", info, err)
				}
			}},
		{name: "a name taken", as: f.lab1Admin, call: add("lab1", dirStorage("main", "fast", "other")), code: connect.CodeAlreadyExists},
		{name: "the same directory in other case", as: f.lab1Admin, call: add("lab1", dirStorage("copy", "fast", "MRI")), code: connect.CodeFailedPrecondition},
		{name: "inside another storage's directory", as: f.lab1Admin, call: add("lab1", dirStorage("inner", "fast", "mri/inner")), code: connect.CodeFailedPrecondition},
		{name: "around another storage's directory", as: f.lab1Admin, call: add("lab1", dirStorage("outer", "fast", "")), code: connect.CodeFailedPrecondition},
		{name: "the same path in another tenant", as: f.lab2Admin, call: add("lab2", dirStorage("main", "fast", "mri")),
			check: func(t *testing.T, o outcome) {
				if _, err := os.Stat(filepath.Join(f.root, "lab2", "mri")); err != nil {
					t.Errorf("lab2's directory: %v", err)
				}
			}},
		{name: "local operator", as: local, call: add("lab1", dirStorage("ct", "fast", "ct"))},
	})
	o := add("lab1", dirStorage("refused", "fast", "x"))(f.as(f.lab1Reader))
	f.refusedNotInTenant(t, "a refusal", "lab1", o)
}

// testGetStorage guards GetStorage: reader on the tenant and up, a storage
// not there, and an answer that never holds a secret.
func testGetStorage(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	get := func(tenantName, name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.storages.GetStorage, &v1.GetStorageRequest{Tenant: tenantName, Name: name})
		}
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: get("lab1", "main"), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: get("lab1", "main"), code: connect.CodePermissionDenied},
		{name: "reader on the installation is no role inside a tenant", as: f.instReader, call: get("lab1", "main"), code: connect.CodePermissionDenied},
		{name: "no such tenant, no right to know", as: f.lab1Reader, call: get("nolab", "main"), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "reader", as: f.lab1Reader, call: get("lab1", "main"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "main"},
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.GetStorageResponse).GetStorage(); s.GetDirectory().GetPath() != "mri" || s.GetTier() != v1.StorageTier_STORAGE_TIER_ONLINE || s.GetReplicaOf() != "" {
					t.Errorf("%v", s)
				}
			}},
		{name: "admin", as: f.lab1Admin, call: get("lab1", "main")},
		{name: "no name", as: f.lab1Reader, call: get("lab1", ""), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "not there", as: f.lab1Reader, call: get("lab1", "none"), code: connect.CodeNotFound, msg: "no storage with this name", audit: inTenant("lab1", master.OutcomeFailed)},
	})
	a := get("lab1", "main")(f.as(f.lab1Reader))
	b := get("lab1", "main")(f.as(f.lab1Reader))
	if !proto.Equal(a.res, b.res) {
		t.Error("two reads answer differently")
	}
}

// testListStorages guards ListStorages: reader and up, paging in order of
// name, and a token refused with another tenant.
func testListStorages(t *testing.T) {
	f := newPlaceFixture(t)
	for _, n := range []string{"c", "a", "b"} {
		mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage(n, "fast", n))(f.as(local)))
	}
	list := func(tenantName string, size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.storages.ListStorages, &v1.ListStoragesRequest{Tenant: tenantName, PageSize: size, PageToken: token})
		}
	}
	names := func(o outcome) string {
		var out string
		for _, s := range o.res.(*v1.ListStoragesResponse).GetStorages() {
			out += s.GetName()
		}
		return out
	}
	var token string
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: list("lab1", 0, ""), code: connect.CodePermissionDenied},
		{name: "no such tenant, may list", as: local, call: list("nolab", 0, ""), code: connect.CodeNotFound},
		{name: "a page of two", as: f.lab1Reader, call: list("lab1", 2, ""), audit: inTenant("lab1", master.OutcomeDone),
			check: func(t *testing.T, o outcome) {
				token = o.res.(*v1.ListStoragesResponse).GetNextPageToken()
				if names(o) != "ab" || token == "" {
					t.Errorf("%q %q", names(o), token)
				}
			}},
		{name: "the next page", as: f.lab1Reader, call: func(c clients) outcome { return list("lab1", 2, token)(c) },
			check: func(t *testing.T, o outcome) {
				if names(o) != "c" || o.res.(*v1.ListStoragesResponse).GetNextPageToken() != "" {
					t.Errorf("%q", names(o))
				}
			}},
		{name: "the token with another tenant", as: local, call: func(c clients) outcome { return list("lab2", 2, token)(c) }, code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "page_size 1001", as: f.lab1Reader, call: list("lab1", 1001, ""), code: connect.CodeInvalidArgument, msg: "page_size"},
	})
}

// testSetStorage guards SetStorage: admin only; the update mask in every
// form the contract refuses; a change of the other type's field; the
// credentials named together; a change repeated answers the same; and the
// entry with the mask's paths.
func testSetStorage(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	set := func(s *v1.Storage, m []string) func(clients) outcome {
		return func(c clients) outcome {
			req := &v1.SetStorageRequest{Tenant: "lab1", Storage: s}
			if m != nil {
				req.UpdateMask = mask(m...)
			}
			return call(c.storages.SetStorage, req)
		}
	}
	capacity := &v1.Storage{Name: "main", CapacityBytes: 1 << 40}
	var first *v1.Storage
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: set(capacity, []string{"capacity_bytes"}), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: set(capacity, []string{"capacity_bytes"}), code: connect.CodePermissionDenied},
		{name: "no mask", as: f.lab1Admin, call: set(capacity, nil), code: connect.CodeInvalidArgument, msg: "update_mask"},
		{name: "an empty mask", as: f.lab1Admin, call: set(capacity, []string{}), code: connect.CodeInvalidArgument, msg: "update_mask"},
		{name: "*", as: f.lab1Admin, call: set(capacity, []string{"*"}), code: connect.CodeInvalidArgument, msg: `may not be "*"`},
		{name: "a whole type", as: f.lab1Admin, call: set(capacity, []string{"directory"}), code: connect.CodeInvalidArgument, msg: "whole message"},
		{name: "the other whole type", as: f.lab1Admin, call: set(capacity, []string{"s3"}), code: connect.CodeInvalidArgument, msg: "whole message"},
		{name: "the name", as: f.lab1Admin, call: set(capacity, []string{"name"}), code: connect.CodeInvalidArgument, msg: "does not change"},
		{name: "a read-only field", as: f.lab1Admin, call: set(capacity, []string{"update_time"}), code: connect.CodeInvalidArgument, msg: "answered by the server"},
		{name: "secret_access_key_set", as: f.lab1Admin, call: set(capacity, []string{"s3.secret_access_key_set"}), code: connect.CodeInvalidArgument, msg: "answered by the server"},
		{name: "an unknown path", as: f.lab1Admin, call: set(capacity, []string{"color"}), code: connect.CodeInvalidArgument, msg: "names no field"},
		{name: "replica_of", as: f.lab1Admin, call: set(capacity, []string{"replica_of"}), code: connect.CodeInvalidArgument, msg: "attaching or detaching a replica"},
		{name: "the tier of a storage that is not a replica, archive", as: f.lab1Admin, call: set(&v1.Storage{Name: "main", Tier: v1.StorageTier_STORAGE_TIER_ARCHIVE}, []string{"tier"}),
			code: connect.CodeInvalidArgument, msg: "a directory storage is always online"},
		{name: "a tier this version does not know", as: f.lab1Admin, call: set(&v1.Storage{Name: "main", Tier: 7}, []string{"tier"}), code: connect.CodeInvalidArgument, msg: "is online or archive"},
		{name: "a recall setting of a directory", as: f.lab1Admin, call: set(capacity, []string{"s3.recall_days"}), code: connect.CodeInvalidArgument},
		{name: "a path twice", as: f.lab1Admin, call: set(capacity, []string{"capacity_bytes", "capacity_bytes"}), code: connect.CodeInvalidArgument, msg: "twice"},
		{name: "one credential alone", as: f.lab1Admin, call: set(&v1.Storage{Name: "main", Type: s3Storage("main", "https://s3.example.com", "x").Type}, []string{"s3.access_key_id"}), code: connect.CodeInvalidArgument},
		{name: "a field of the other type", as: f.lab1Admin, call: set(&v1.Storage{Name: "main", Type: s3Storage("main", "https://s3.example.com", "x").Type}, []string{"s3.bucket"}), code: connect.CodeInvalidArgument, msg: "type cannot change"},
		{name: "no storage", as: f.lab1Admin, call: set(nil, []string{"capacity_bytes"}), code: connect.CodeInvalidArgument, msg: "storage"},
		{name: "not there", as: f.lab1Admin, call: set(&v1.Storage{Name: "none"}, []string{"capacity_bytes"}), code: connect.CodeNotFound},
		{name: "the capacity", as: f.lab1Admin, call: set(capacity, []string{"capacity_bytes"}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "main", details: map[string]string{"update_mask": "capacity_bytes"}},
			check: func(t *testing.T, o outcome) {
				first = o.res.(*v1.SetStorageResponse).GetStorage()
				if first.GetCapacityBytes() != 1<<40 || first.GetDirectory().GetPath() != "mri" {
					t.Errorf("%v", first)
				}
			}},
		{name: "the capacity again", as: f.lab1Admin, call: set(capacity, []string{"capacity_bytes"}),
			check: func(t *testing.T, o outcome) {
				again := proto.Clone(o.res.(*v1.SetStorageResponse).GetStorage()).(*v1.Storage)
				again.UpdateTime = first.GetUpdateTime()
				if !proto.Equal(again, first) {
					t.Errorf("a repeated change answers %v, the first %v", again, first)
				}
			}},
		{name: "the path to a root not listed", as: f.lab1Admin, call: set(dirStorage("main", "slow", ""), []string{"directory.root"}), code: connect.CodeFailedPrecondition},
		{name: "the path moved", as: f.lab1Admin, call: set(dirStorage("main", "fast", "moved"), []string{"directory.path"}),
			check: func(t *testing.T, o outcome) {
				if _, err := os.Stat(filepath.Join(f.root, "lab1", "moved")); err != nil {
					t.Errorf("the new directory: %v", err)
				}
			}},
	})
}

// testRemoveStorage guards RemoveStorage: admin only, refused while a
// receiver names the storage, the entry with its type, and a repeat
// answered NOT_FOUND.
func testRemoveStorage(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage("spare", "fast", "spare"))(f.as(local)))
	remove := func(name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.storages.RemoveStorage, &v1.RemoveStorageRequest{Tenant: "lab1", Name: name})
		}
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: remove("spare"), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: remove("spare"), code: connect.CodePermissionDenied},
		{name: "no name", as: f.lab1Admin, call: remove(""), code: connect.CodeInvalidArgument},
		{name: "a receiver names it", as: f.lab1Admin, call: remove("main"), code: connect.CodeFailedPrecondition, msg: "receiver",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "main", details: map[string]string{"type": "directory"}}},
		{name: "removed", as: f.lab1Admin, call: remove("spare"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "spare", details: map[string]string{"type": "directory"}},
			check: func(t *testing.T, o outcome) {
				if o.res.(*v1.RemoveStorageResponse).GetStorage().GetName() != "spare" {
					t.Error(o.res)
				}
				if _, err := os.Stat(filepath.Join(f.root, "lab1", "spare")); err != nil {
					t.Errorf("a removal removed the directory: %v", err)
				}
			}},
		{name: "again", as: f.lab1Admin, call: remove("spare"), code: connect.CodeNotFound, msg: "no storage with this name"},
	})
}

// testTestStorage guards TestStorage: admin only, every step passed on a
// directory, the object removed (nothing is listed afterwards), the entry
// with whether it passed, and a storage whose directory is gone answered as
// a result, not an error.
func testTestStorage(t *testing.T) {
	f := newPlaceFixture(t)
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage("gone", "fast", "gone"))(f.as(local)))
	if err := os.Remove(filepath.Join(f.root, "lab1", "gone")); err != nil {
		t.Fatal(err)
	}
	test := func(name string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.storages.TestStorage, &v1.TestStorageRequest{Tenant: "lab1", Name: name})
		}
	}
	steps := func(o outcome) string {
		var out string
		for _, r := range o.res.(*v1.TestStorageResponse).GetResults() {
			out += r.GetStep().String()[len("STORAGE_TEST_STEP_"):] + "=" + r.GetOutcome().String()[len("STORAGE_TEST_OUTCOME_"):]
			if r.GetFault() != v1.StorageFault_STORAGE_FAULT_UNSPECIFIED {
				out += "/" + r.GetFault().String()[len("STORAGE_FAULT_"):]
			}
			out += " "
		}
		return out
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: test("main"), code: connect.CodePermissionDenied},
		{name: "operator", as: f.lab1Operator, call: test("main"), code: connect.CodePermissionDenied},
		{name: "not there", as: f.lab1Admin, call: test("none"), code: connect.CodeNotFound},
		{name: "passed", as: f.lab1Admin, call: test("main"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "main", details: map[string]string{"passed": "true"}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.TestStorageResponse)
				if want := "WRITE=PASSED READ=PASSED COMPARE=PASSED REMOVE=PASSED "; !res.GetPassed() || steps(o) != want {
					t.Errorf("%s", steps(o))
				}
				for _, r := range res.GetResults() {
					if r.GetDuration() == nil {
						t.Errorf("%v has no duration", r.GetStep())
					}
				}
				objects, _ := filepath.Glob(filepath.Join(f.root, "lab1", "mri", "nopacs-test", "*"))
				if len(objects) != 0 {
					t.Errorf("the test left %v", objects)
				}
			}},
		{name: "passed again", as: f.lab1Admin, call: test("main"), check: func(t *testing.T, o outcome) {
			if !o.res.(*v1.TestStorageResponse).GetPassed() {
				t.Error(steps(o))
			}
		}},
		{name: "a directory not there", as: f.lab1Admin, call: test("gone"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"passed": "false"}},
			check: func(t *testing.T, o outcome) {
				if want := "WRITE=FAILED/NOT_FOUND READ=SKIPPED COMPARE=SKIPPED REMOVE=SKIPPED "; o.res.(*v1.TestStorageResponse).GetPassed() || steps(o) != want {
					t.Errorf("%s", steps(o))
				}
				if o.res.(*v1.TestStorageResponse).GetResults()[1].GetDuration() != nil {
					t.Error("a skipped step has a duration")
				}
			}},
	})
}

// withStorage returns s changed by edit, for a case that sets a field the
// builders leave unset.
func withStorage(s *v1.Storage, edit func(*v1.Storage)) *v1.Storage {
	edit(s)
	return s
}
