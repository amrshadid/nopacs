package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
)

// pgTenant creates a tenant of the test's Postgres driver, whose store
// hands back an S3 storage's sealed secret.
func pgTenant(t *testing.T, f *placeFixture, name string) {
	t.Helper()
	if o := call(f.as(local).tenants.CreateTenant, &v1.CreateTenantRequest{Name: name, DatabaseType: v1.DatabaseType_DATABASE_TYPE_POSTGRES, DatabaseAddress: pgAddress}); o.err != nil {
		t.Fatal(o.err)
	}
}

// countingS3 is a server that counts the requests it gets and answers each
// as refused.
func countingS3(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// TestSealedSecretBoundToRow guards what a sealed secret is bound to: one
// copied to another storage's row of the same tenant, or to a row of the
// same name in another tenant, does not open there, and the test of that
// storage fails as the server's error, never reaching the store with it.
func TestSealedSecretBoundToRow(t *testing.T) {
	f := newPlaceFixture(t)
	url, requests := countingS3(t)
	pgTenant(t, f, "pg1")
	pgTenant(t, f, "pg2")
	f.setSetting(installation(), settings.StorageS3Endpoints, url+" plain-http private-address")
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("pg1", s3Storage("cold", url, "secret-one"))(f.as(local)))
	warm := s3Storage("warm", url, "secret-two")
	warm.GetS3().Prefix = "other/"
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("pg1", warm)(f.as(local)))
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("pg2", s3Storage("cold", url, "secret-three"))(f.as(local)))
	sealed, _ := f.pg.secrets.Load("pg1/cold")
	f.pg.secrets.Store("pg2/cold", sealed)
	f.pg.secrets.Store("pg1/warm", sealed)
	for _, c := range [][2]string{{"pg2", "cold"}, {"pg1", "warm"}} {
		o := call(f.as(local).storages.TestStorage, &v1.TestStorageRequest{Tenant: c[0], Name: c[1]})
		if o.code() != connect.CodeInternal || o.message() != "internal error" {
			t.Errorf("%s/%s with another row's sealed secret: %v %q", c[0], c[1], o.code(), o.message())
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the store got %d requests with a secret that is not the storage's", n)
	}
}

// TestEndpointChangeNeedsCredentials guards SetStorage moving an S3
// storage to another endpoint: a mask that names the endpoint without the
// credentials is refused as invalid, so the credentials given for one host
// never sign a request to another; with the credentials it is accepted.
func TestEndpointChangeNeedsCredentials(t *testing.T) {
	f := newPlaceFixture(t)
	old := fakeS3(t)
	next, requests := countingS3(t)
	pgTenant(t, f, "pg1")
	f.setSetting(installation(), settings.StorageS3Endpoints, old+" plain-http private-address\n"+next+" plain-http private-address")
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("pg1", s3Storage("cold", old, "secret-for-old-host"))(f.as(local)))
	o := call(f.as(local).storages.SetStorage, &v1.SetStorageRequest{Tenant: "pg1", Storage: s3Storage("cold", next, ""), UpdateMask: mask("s3.endpoint")})
	if o.code() != connect.CodeInvalidArgument || !strings.Contains(o.message(), "update_mask") {
		t.Errorf("the endpoint alone: %v %q", o.code(), o.message())
	}
	call(f.as(local).storages.TestStorage, &v1.TestStorageRequest{Tenant: "pg1", Name: "cold"})
	if n := requests.Load(); n != 0 {
		t.Errorf("the new host got %d requests with the old host's credentials", n)
	}
	got := mustCall[*v1.GetStorageResponse](t, "get", call(f.as(local).storages.GetStorage, &v1.GetStorageRequest{Tenant: "pg1", Name: "cold"}))
	if got.GetStorage().GetS3().GetEndpoint() == strings.ToLower(next) {
		t.Error("the refused change was stored")
	}
	res := mustCall[*v1.SetStorageResponse](t, "with credentials", call(f.as(local).storages.SetStorage, &v1.SetStorageRequest{Tenant: "pg1",
		Storage: s3Storage("cold", next, "secret-for-new-host"), UpdateMask: mask("s3.endpoint", "s3.access_key_id", "s3.secret_access_key")}))
	if res.GetStorage().GetS3().GetEndpoint() != next {
		t.Errorf("moved to %q", res.GetStorage().GetS3().GetEndpoint())
	}
}

// TestSetStorageS3Refusals guards the kinds of SetStorage's refusals of an
// S3 storage: an endpoint the installation does not list is a
// precondition, as it is for AddStorage; a new secret that is empty, holds
// a space or passes 128 bytes is invalid, and the stored storage is
// unchanged.
func TestSetStorageS3Refusals(t *testing.T) {
	f := newPlaceFixture(t)
	f.setSetting(installation(), settings.StorageS3Endpoints, "https://s3.example.com")
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", s3Storage("cold", "https://s3.example.com", "the-secret"))(f.as(f.lab1Admin)))
	set := func(endpoint, secret string, paths ...string) outcome {
		return call(f.as(f.lab1Admin).storages.SetStorage, &v1.SetStorageRequest{Tenant: "lab1", Storage: s3Storage("cold", endpoint, secret), UpdateMask: mask(paths...)})
	}
	creds := []string{"s3.access_key_id", "s3.secret_access_key"}
	if o := set("https://other.example.com", "new-secret", append([]string{"s3.endpoint"}, creds...)...); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "does not list") {
		t.Errorf("an endpoint not listed: %v %q", o.code(), o.message())
	}
	for _, bad := range []string{"", "has space", strings.Repeat("s", 129)} {
		if o := set("https://s3.example.com", bad, creds...); o.code() != connect.CodeInvalidArgument || !strings.Contains(o.message(), "secret_access_key") {
			t.Errorf("a new secret of %d bytes: %v %q", len(bad), o.code(), o.message())
		}
	}
	got := mustCall[*v1.GetStorageResponse](t, "get", call(f.as(local).storages.GetStorage, &v1.GetStorageRequest{Tenant: "lab1", Name: "cold"}))
	if got.GetStorage().GetS3().GetEndpoint() != "https://s3.example.com" || !got.GetStorage().GetS3().GetSecretAccessKeySet() {
		t.Errorf("a refused change was stored: %v", got.GetStorage())
	}
}

// TestSecretAccessKeyBound guards the bound of a secret access key: 128
// bytes are accepted, 129 are invalid, and the refusal does not hold the
// secret.
func TestSecretAccessKeyBound(t *testing.T) {
	f := newPlaceFixture(t)
	f.setSetting(installation(), settings.StorageS3Endpoints, "https://s3.example.com")
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", s3Storage("a", "https://s3.example.com", strings.Repeat("k", 128)))(f.as(f.lab1Admin)))
	long := s3Storage("b", "https://s3.example.com", strings.Repeat("k", 129))
	long.GetS3().Prefix = "other/"
	o := addStorage("lab1", long)(f.as(f.lab1Admin))
	if o.code() != connect.CodeInvalidArgument || !strings.Contains(o.message(), "128 bytes") || strings.Contains(o.message(), "kkkk") {
		t.Errorf("a secret of 129 bytes: %v %q", o.code(), o.message())
	}
}

// TestPrivateEndpointPathStyle guards the addressing of a bucket at an
// endpoint whose entry allows private addresses: with the bucket in the host
// name, a bucket's name would choose the host inside the server's network
// that is reached, so such a storage must use path style. AddStorage and
// SetStorage refuse it as a precondition, and so does a change of the
// endpoints that would make a storage's endpoint private while it uses the
// host name; at an endpoint without the flag the host name is allowed, as
// AWS S3 needs.
func TestPrivateEndpointPathStyle(t *testing.T) {
	f := newPlaceFixture(t)
	f.setSetting(installation(), settings.StorageS3Endpoints, "https://s3.example.com\nhttps://minio.lab.example private-address")
	vhost := func(name, endpoint, prefix string) *v1.Storage {
		s := s3Storage(name, endpoint, "the-secret")
		s.GetS3().PathStyle, s.GetS3().Prefix = false, prefix
		return s
	}
	if o := addStorage("lab1", vhost("p", "https://minio.lab.example", "p/"))(f.as(f.lab1Admin)); o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "path_style") {
		t.Errorf("the host name at a private endpoint: %v %q", o.code(), o.message())
	}
	mustCall[*v1.AddStorageResponse](t, "path style at a private endpoint", addStorage("lab1", s3Storage("q", "https://minio.lab.example", "the-secret"))(f.as(f.lab1Admin)))
	mustCall[*v1.AddStorageResponse](t, "host name at a public endpoint", addStorage("lab1", vhost("r", "https://s3.example.com", "r/"))(f.as(f.lab1Admin)))
	o := call(f.as(f.lab1Admin).storages.SetStorage, &v1.SetStorageRequest{Tenant: "lab1", Storage: vhost("q", "", ""), UpdateMask: mask("s3.path_style")})
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "path_style") {
		t.Errorf("SetStorage to the host name at a private endpoint: %v %q", o.code(), o.message())
	}
	o = call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.StorageS3Endpoints, Scope: installation(),
		Value: "https://s3.example.com private-address\nhttps://minio.lab.example private-address"}})
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "storage r") {
		t.Errorf("making a public endpoint private under a storage using the host name: %v %q", o.code(), o.message())
	}
}
