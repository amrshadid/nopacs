package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/blob/s3"
	"github.com/amrshadid/nopacs/internal/fault"
)

// reworded returns err as it would read had the driver worded its message
// otherwise: the same kind and the same chain under another message.
func reworded(err error) error {
	return fault.Wrap(fault.KindOf(err), err, "a message in other words")
}

// TestDirectoryFaultReadsCauses guards the classification of a directory
// storage's failures: each real failure of the driver (a directory not
// there, one that cannot be examined at all) is the fault the contract
// names, and stays that fault when the driver's message is reworded, since
// the cause is read, not the words.
func TestDirectoryFaultReadsCauses(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Below a file, Unix answers "not a directory", which the driver reports
	// as a directory it cannot reach; Windows answers "the path is not
	// found" (ERROR_PATH_NOT_FOUND), which Go reads as fs.ErrNotExist, so
	// there the storage's directory is not found, as the system says.
	belowAFile := v1.StorageFault_STORAGE_FAULT_UNREACHABLE
	if runtime.GOOS == "windows" {
		belowAFile = v1.StorageFault_STORAGE_FAULT_NOT_FOUND
	}
	for _, c := range []struct {
		name string
		path string
		want v1.StorageFault
	}{
		{"not there", filepath.Join(base, "absent"), v1.StorageFault_STORAGE_FAULT_NOT_FOUND},
		{"below a file", filepath.Join(file, "x"), belowAFile},
	} {
		_, err := dir.Open(c.path)
		if err == nil {
			t.Fatalf("%s: opened", c.name)
		}
		if got := directoryFault(err); got != c.want {
			t.Errorf("%s: %v, want %v (%v)", c.name, got, c.want, err)
		}
		if got := directoryFault(reworded(err)); got != c.want {
			t.Errorf("%s, reworded: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestS3FaultReadsClasses guards the classification of an S3 storage's
// failures: each real failure of the driver against a server that answers
// it (credentials refused, a bucket missing, a redirect, an address not
// allowed, nothing listening) is the fault the contract names, and stays
// that fault when the driver's message is reworded, since the driver's
// class is read, not its words.
func TestS3FaultReadsClasses(t *testing.T) {
	answer := func(status int, code string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			if status/100 == 3 {
				w.Header().Set("Location", "http://elsewhere.example/")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>` + code + `</Code><Message>m</Message></Error>`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String()
	_ = l.Close()
	all := func(netip.Addr) bool { return true }
	none := func(netip.Addr) bool { return false }
	for _, c := range []struct {
		name     string
		endpoint string
		allow    func(netip.Addr) bool
		want     v1.StorageFault
	}{
		{"credentials refused", answer(http.StatusForbidden, "AccessDenied"), all, v1.StorageFault_STORAGE_FAULT_REFUSED},
		{"bucket missing", answer(http.StatusNotFound, "NoSuchBucket"), all, v1.StorageFault_STORAGE_FAULT_NOT_FOUND},
		{"redirect", answer(http.StatusMovedPermanently, "PermanentRedirect"), all, v1.StorageFault_STORAGE_FAULT_WRONG_ANSWER},
		{"address not allowed", answer(http.StatusOK, ""), none, v1.StorageFault_STORAGE_FAULT_ADDRESS_NOT_ALLOWED},
		{"nothing listening", closed, all, v1.StorageFault_STORAGE_FAULT_UNREACHABLE},
	} {
		_, err := s3.Open(context.Background(), s3.Config{
			Endpoint: c.endpoint, Region: "us-east-1", Bucket: "archive", PathStyle: true,
			AccessKeyID: s3.NewSecret("a"), SecretAccessKey: s3.NewSecret("b"), AllowHTTP: true,
			AllowAddress: c.allow, StaleUploadAge: -1,
		})
		if err == nil {
			t.Fatalf("%s: opened", c.name)
		}
		if got := s3Fault(err); got != c.want {
			t.Errorf("%s: %v, want %v (%v)", c.name, got, c.want, err)
		}
		if got := s3Fault(reworded(err)); got != c.want {
			t.Errorf("%s, reworded: %v, want %v", c.name, got, c.want)
		}
	}
}
