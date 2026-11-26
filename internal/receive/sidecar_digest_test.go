package receive

import (
	"crypto/sha256"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCommitKeepsTheSidecarsDigest guards that the commit that writes a
// scan's manifest digest keeps beside it the SHA-256 of the metadata file
// put with it: the catalog's digest is that of the file on the storage, for
// every scan, and a second reception into the scan keeps the new file's.
func TestCommitKeepsTheSidecarsDigest(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send(studyFiles[:len(studyFiles)-1]...)
	rs := e.settle(1)
	check := func(what string) {
		t.Helper()
		for _, path := range scanPaths(t, e, rs) {
			key, _ := layout.MetadataKey(path)
			want := tenant.SHA256(sha256.Sum256(readObject(t, e.storage, key)))
			sc, err := e.store.CatalogScan(e.ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if sc.SidecarSHA256 != want {
				t.Errorf("%s: scan %s keeps metadata digest %x; want %x, the file's on the storage", what, path.Scan,
					sc.SidecarSHA256[:4], want[:4])
			}
		}
	}
	check("the first reception")
	e.send(studyFiles[len(studyFiles)-1])
	rs = e.settle(2)
	check("a second reception into the scan")
}
