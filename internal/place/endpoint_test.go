package place

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAllowS3 guards the endpoints an S3 storage may use and how: one the
// list holds in any spelling, an http one only with plain-http, and at an
// entry that allows private addresses only with the bucket in the path.
func TestAllowS3(t *testing.T) {
	entries := settings.S3Endpoints{
		{URL: "https://s3.example.com"},
		{URL: "http://minio.lab.example:9000", PlainHTTP: true, PrivateAddress: true},
		{URL: "http://plain.example"},
	}
	for _, c := range []struct {
		endpoint string
		path     bool
		kind     fault.Kind // the refusal's kind
		refused  bool
	}{
		{"HTTPS://S3.example.com:443/", false, 0, false},
		{"http://minio.lab.example:9000", true, 0, false},
		{"http://minio.lab.example:9000", false, fault.Precondition, true},
		{"http://plain.example", true, fault.Precondition, true},
		{"https://other.example", true, fault.Precondition, true},
		{"https://3232235777", true, fault.Invalid, true},
	} {
		ep, err := AllowS3(entries, tenant.S3Storage{Endpoint: c.endpoint, PathStyle: c.path})
		if (err != nil) != c.refused || (c.refused && fault.KindOf(err) != c.kind) {
			t.Errorf("%s, path style %v: %v", c.endpoint, c.path, err)
		}
		if err == nil && ep.URL != strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(c.endpoint, "/"), ":443")) {
			t.Errorf("%s: kept as %q", c.endpoint, ep.URL)
		}
	}
}

// fixedValues is Values with fixed settings.
type fixedValues struct {
	roots     settings.DirectoryRoots
	endpoints settings.S3Endpoints
}

func (v fixedValues) DirectoryRoots(context.Context) (settings.DirectoryRoots, error) {
	return v.roots, nil
}

func (v fixedValues) S3Endpoints(context.Context) (settings.S3Endpoints, error) {
	return v.endpoints, nil
}

// TestOpenRefusesHostNameAtPrivateEndpoint guards a storage stored before
// the rule of path style held: at an endpoint whose entry allows private
// addresses, a storage that addresses its bucket in the host name is refused
// when opened, as the storage's precondition, and the server sends nothing,
// so the bucket's name never picks the host reached.
func TestOpenRefusesHostNameAtPrivateEndpoint(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	o := Opener{Values: fixedValues{endpoints: settings.S3Endpoints{{URL: srv.URL, PlainHTTP: true, PrivateAddress: true}}}, StaleUploadAge: -1}
	s := tenant.Storage{Name: "cold", Type: tenant.StorageS3, S3: tenant.S3Storage{Endpoint: srv.URL, Region: "us-east-1", Bucket: "chosen", AccessKeyID: "a"}}
	_, err := o.Open(context.Background(), Tenant{ID: "t1", Name: "lab"}, s)
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "path_style") {
		t.Errorf("opened, or refused as another kind: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests were sent", n)
	}
}
