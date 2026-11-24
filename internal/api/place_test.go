package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
)

// TestDirectoryGuards guards where a directory storage may be: a planted
// symbolic link anywhere below the root, "..", an absolute path, a path
// that differs from another storage's only by case, a directory that is
// not empty, and a root the installation does not list are each refused
// before anything is stored, and none of the refusals names the root's own
// path.
func TestDirectoryGuards(t *testing.T) {
	f := newPlaceFixture(t)
	outside := filepath.Join(f.dir, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, "lab1", "full"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "lab1", "full", "x.dcm"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "lab1", "link")); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(f.root, "lab1", "real")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(f.root, "lab1", "alias")); err != nil {
		t.Fatal(err)
	}
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	for _, c := range []struct {
		name string
		s    *v1.Storage
		code connect.Code
		msg  string
	}{
		{"a link below the root, to outside it", dirStorage("a", "fast", "link"), connect.CodeFailedPrecondition, "symbolic link"},
		{"a directory under a link", dirStorage("b", "fast", "link/deeper"), connect.CodeFailedPrecondition, "symbolic link"},
		{"a link that stays inside the root", dirStorage("c", "fast", "alias"), connect.CodeFailedPrecondition, "symbolic link"},
		{"..", dirStorage("d", "fast", "../lab2"), connect.CodeInvalidArgument, "path"},
		{"a dot segment", dirStorage("e", "fast", "mri/./x"), connect.CodeInvalidArgument, "path"},
		{"an absolute path", dirStorage("f", "fast", "/etc"), connect.CodeInvalidArgument, "path"},
		{"another storage's path in other case", dirStorage("g", "fast", "Mri"), connect.CodeFailedPrecondition, ""},
		{"a directory that is not empty", dirStorage("h", "fast", "full"), connect.CodeFailedPrecondition, "not empty"},
		{"a root not listed", dirStorage("i", "slow", "x"), connect.CodeFailedPrecondition, "no directory root named slow"},
	} {
		o := addStorage("lab1", c.s)(f.as(f.lab1Admin))
		if o.code() != c.code || !strings.Contains(o.message(), c.msg) {
			t.Errorf("%s: %v %q, want %v holding %q", c.name, o.code(), o.message(), c.code, c.msg)
		}
		if strings.Contains(o.message(), f.dir) {
			t.Errorf("%s: the message names the root's path: %q", c.name, o.message())
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was made through the link: %v", entries)
	}
	list := mustCall[*v1.ListStoragesResponse](t, "list", call(f.as(local).storages.ListStorages, &v1.ListStoragesRequest{Tenant: "lab1"}))
	if len(list.GetStorages()) != 1 {
		t.Errorf("a refused storage was stored: %v", list.GetStorages())
	}
	if strings.Contains(f.log.String(), f.dir) {
		t.Error("the log names the root's path")
	}

	// A link planted after the storage was added is caught when the
	// storage is opened: the test fails its write, and nothing is written
	// where the link points.
	mri := filepath.Join(f.root, "lab1", "mri")
	if err := os.Remove(mri); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, mri); err != nil {
		t.Fatal(err)
	}
	res := mustCall[*v1.TestStorageResponse](t, "test", call(f.as(local).storages.TestStorage, &v1.TestStorageRequest{Tenant: "lab1", Name: "main"}))
	if res.GetPassed() || res.GetResults()[0].GetFault() != v1.StorageFault_STORAGE_FAULT_REFUSED {
		t.Errorf("a link planted later: %v", res)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the test wrote through the link: %v", entries)
	}
}

// TestEndpointGuards guards the S3 endpoint a storage may use: it must be
// one the installation lists, compared in the one spelling the port keeps,
// so a spelling the list does not have word for word is still matched and
// stored as the list's endpoint, a spelling of an address as a number is
// refused, and an endpoint not listed is a precondition.
func TestEndpointGuards(t *testing.T) {
	f := newPlaceFixture(t)
	f.setSetting(installation(), settings.StorageS3Endpoints, "https://s3.example.com\nhttp://minio.lab.example:9000 plain-http private-address")
	for i, c := range []struct {
		endpoint string
		code     connect.Code
		stored   string
	}{
		{"https://s3.example.com", 0, "https://s3.example.com"},
		{"HTTPS://S3.EXAMPLE.COM:443/", 0, "https://s3.example.com"},
		{"https://s3.example.com.", 0, "https://s3.example.com"},
		{"https://s3.example.com:0443", 0, "https://s3.example.com"},
		{"http://MINIO.lab.example:9000/", 0, "http://minio.lab.example:9000"},
		{"https://minio.lab.example:9000", connect.CodeFailedPrecondition, ""},
		{"http://s3.example.com", connect.CodeFailedPrecondition, ""},
		{"https://s3.example.com:8443", connect.CodeFailedPrecondition, ""},
		{"https://other.example.com", connect.CodeFailedPrecondition, ""},
		{"https://3232235777", connect.CodeInvalidArgument, ""},
		{"https://0xc0.0xa8.1.1", connect.CodeInvalidArgument, ""},
		{"https://user@s3.example.com", connect.CodeInvalidArgument, ""},
		{"https://s3.example.com/bucket", connect.CodeInvalidArgument, ""},
		{"https://s3%2eexample.com", connect.CodeInvalidArgument, ""},
	} {
		s := s3Storage(fmt.Sprintf("s%d", i), c.endpoint, "secret-"+fmt.Sprint(i))
		s.GetS3().Prefix = fmt.Sprintf("p%d/", i)
		o := addStorage("lab1", s)(f.as(f.lab1Admin))
		if o.code() != c.code {
			t.Errorf("%s: %v %q, want %v", c.endpoint, o.code(), o.message(), c.code)
			continue
		}
		if c.code == 0 {
			if got := o.res.(*v1.AddStorageResponse).GetStorage().GetS3().GetEndpoint(); got != c.stored {
				t.Errorf("%s: stored as %q, want %q", c.endpoint, got, c.stored)
			}
		}
	}
	// The installation's list is checked the same way: two spellings of
	// one endpoint are one endpoint, and an endpoint a storage could not use
	// is refused.
	for _, v := range []string{"https://s3.example.com\nHTTPS://s3.example.com:443", "https://3232235777", "http://minio:9000"} {
		o := call(f.as(local).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: settings.StorageS3Endpoints, Scope: installation(), Value: v}})
		if o.code() != connect.CodeInvalidArgument {
			t.Errorf("%q: %v %q", v, o.code(), o.message())
		}
	}
}

// fakeS3 is a server that answers every S3 request it gets as refused, as
// a store answers credentials it does not accept.
func fakeS3(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>the store's own words, never answered</Message></Error>`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestS3StorageTest guards TestStorage on an S3 storage, whose store hands
// back its sealed secret on every driver (SQLite and Postgres alike): a
// store that refuses the credentials is the write step's REFUSED, and an
// endpoint whose
// address the installation's entry does not allow is ADDRESS_NOT_ALLOWED,
// with nothing sent; the store's own words are never answered.
func TestS3StorageTest(t *testing.T) {
	f := newPlaceFixture(t)
	url := fakeS3(t)
	o := call(f.as(local).tenants.CreateTenant, &v1.CreateTenantRequest{Name: "pg1", DatabaseType: v1.DatabaseType_DATABASE_TYPE_POSTGRES, DatabaseAddress: pgAddress})
	if o.err != nil {
		t.Fatal(o.err)
	}
	host := strings.TrimPrefix(url, "http://")
	f.setSetting(installation(), settings.StorageS3Endpoints, url+" plain-http private-address\nhttp://localhost:"+strings.Split(host, ":")[1]+" plain-http")
	for _, tn := range []string{"lab1", "pg1"} {
		mustCall[*v1.AddStorageResponse](t, "add", addStorage(tn, s3Storage("cold", url, "the-secret"))(f.as(local)))
	}
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("pg1", s3Storage("local", "http://localhost:"+strings.Split(host, ":")[1], "the-secret"))(f.as(local)))
	test := func(tn, name string) outcome {
		return call(f.as(local).storages.TestStorage, &v1.TestStorageRequest{Tenant: tn, Name: name})
	}
	// The store hands the sealed secret back on every driver, so a SQLite
	// tenant's S3 storage is tested like a Postgres one's: the credentials
	// reach the store, which refuses them.
	for _, tn := range []string{"lab1", "pg1"} {
		res := mustCall[*v1.TestStorageResponse](t, "test", test(tn, "cold"))
		if res.GetPassed() || res.GetResults()[0].GetFault() != v1.StorageFault_STORAGE_FAULT_REFUSED {
			t.Errorf("%s: refused credentials: %v", tn, res)
		}
	}
	o = test("pg1", "local")
	res := mustCall[*v1.TestStorageResponse](t, "test", o)
	if res.GetPassed() || res.GetResults()[0].GetFault() != v1.StorageFault_STORAGE_FAULT_ADDRESS_NOT_ALLOWED {
		t.Errorf("an address not allowed: %v", res)
	}
	text, _ := protojson.Marshal(res)
	if strings.Contains(string(text), "own words") || strings.Contains(f.log.String(), "own words") {
		t.Error("the store's own words were answered or logged")
	}
}

// TestSecretNeverLeaves guards the secret access key of an S3 storage: after
// a run through every method of StorageService, refusals and failures
// included, the secret is in no answer (in every text form), no error, no
// line of the log and no audit entry of either log, and is not in the
// tenant's database as it was sent: it is sealed there.
func TestSecretNeverLeaves(t *testing.T) {
	const secret = "s3cr3t-Wh1ch-Must-N0t-Leak"
	f := newPlaceFixture(t)
	f.setSetting(installation(), settings.StorageS3Endpoints, "https://s3.example.com")
	var answers []string
	keep := func(o outcome) {
		if o.res != nil {
			b, _ := protojson.Marshal(o.res)
			answers = append(answers, string(b), prototext.Format(o.res), fmt.Sprint(o.res))
		}
		if o.err != nil {
			answers = append(answers, o.err.Error())
		}
	}
	c := f.as(f.lab1Admin)
	keep(addStorage("lab1", s3Storage("cold", "https://s3.example.com", secret))(c))
	keep(addStorage("lab1", s3Storage("cold", "https://s3.example.com", secret))(c))
	keep(addStorage("lab1", s3Storage("bad", "https://other.example.com", secret))(c))
	keep(addStorage("lab1", s3Storage("cold", "https://s3.example.com", secret))(f.as(f.lab1Reader)))
	keep(call(c.storages.GetStorage, &v1.GetStorageRequest{Tenant: "lab1", Name: "cold"}))
	keep(call(c.storages.ListStorages, &v1.ListStoragesRequest{Tenant: "lab1"}))
	rotate := s3Storage("cold", "https://s3.example.com", secret+"-2")
	keep(call(c.storages.SetStorage, &v1.SetStorageRequest{Tenant: "lab1", Storage: rotate, UpdateMask: mask("s3.access_key_id", "s3.secret_access_key")}))
	keep(call(c.storages.SetStorage, &v1.SetStorageRequest{Tenant: "lab1", Storage: rotate, UpdateMask: mask("s3.secret_access_key")}))
	keep(call(c.storages.SetStorage, &v1.SetStorageRequest{Tenant: "lab1", Storage: &v1.Storage{Name: "cold", CapacityBytes: 5}, UpdateMask: mask("capacity_bytes")}))
	keep(call(c.storages.TestStorage, &v1.TestStorageRequest{Tenant: "lab1", Name: "cold"}))
	keep(call(c.storages.RemoveStorage, &v1.RemoveStorageRequest{Tenant: "lab1", Name: "cold"}))

	f.flush()
	var seen []string
	seen = append(seen, answers...)
	seen = append(seen, f.log.String())
	for _, e := range append(f.masterLog(), f.tenantLog("lab1")...) {
		seen = append(seen, fmt.Sprintf("%+v", e))
	}
	for _, s := range seen {
		if strings.Contains(s, "s3cr3t") {
			t.Fatalf("the secret is in %q", s)
		}
	}
	files, _ := filepath.Glob(filepath.Join(f.data, "tenants", "lab1*"))
	if len(files) == 0 {
		t.Fatal("the tenant's database files are not where the test looks")
	}
	for _, name := range files {
		b, err := os.ReadFile(name) //nolint:gosec // G304: the test's own temporary directory.
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "s3cr3t") {
			t.Errorf("the secret is in %s as it was sent", filepath.Base(name))
		}
	}
}
