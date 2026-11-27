package scrub

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// archiveStore is a store of the archive tier: its data/ objects are held in
// a cold class unless set online, it reports for each the SHA-256 and the
// size the test sets, and it counts the reads of data/ objects and the
// recalls asked.
type archiveStore struct {
	blob.Store
	mu     sync.Mutex
	online map[string]bool
	sums   map[string][sha256.Size]byte
	sizes  map[string]int64
	// dataReads counts the reads of data/ objects; recalls the recalls.
	dataReads, recalls atomic.Int64
}

// Stat answers what the test set of the object.
func (c *archiveStore) Stat(ctx context.Context, key string) (blob.Info, error) {
	info, err := c.Store.Stat(ctx, key)
	if err != nil || !strings.HasPrefix(key, layout.DataRoot) {
		return info, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.online[key] {
		info.State = blob.Archived
	}
	if s, ok := c.sums[key]; ok {
		info.SHA256, info.HasSHA256 = s, true
	}
	if n, ok := c.sizes[key]; ok {
		info.Size = n
	}
	return info, nil
}

// Read counts a read of a data/ object, and fails one held cold.
func (c *archiveStore) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if strings.HasPrefix(key, layout.DataRoot) {
		c.dataReads.Add(1)
		c.mu.Lock()
		on := c.online[key]
		c.mu.Unlock()
		if !on {
			return nil, fmt.Errorf("the object is in a cold class: %w", blob.ErrNotOnline)
		}
	}
	return c.Store.Read(ctx, key, offset, length)
}

// Recall counts a recall: a scrub must never ask for one.
func (c *archiveStore) Recall(context.Context, string, blob.RecallRequest) (blob.Recall, error) {
	c.recalls.Add(1)
	return blob.Recall{}, nil
}

// addColdReplica adds an S3 storage of the archive tier as a replica of
// "main", held by a archiveStore, and queues every scan of "main" for it.
func (r *replicaRig) addColdReplica(name string) *archiveStore {
	r.t.Helper()
	root := filepath.Join(r.t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		r.t.Fatal(err)
	}
	d, err := dir.Open(root)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = d.Close() })
	at := time.Now().Add(-47 * time.Hour)
	s := tenant.Storage{Name: name, Type: tenant.StorageS3, Tier: tenant.TierArchive, ReplicaOf: "main", S3: tenant.S3Storage{
		Endpoint: "https://s3.example.org", Region: "us-east-1", Bucket: "lab-cold", Prefix: name + "/", PathStyle: true,
		AccessKeyID: "AKIDEXAMPLE", SealedSecret: []byte("sealed-secret-bytes")}}
	if _, err := r.p.AddStorage(r.ctx, s, at, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	if _, _, err := r.p.BackfillReplica(r.ctx, tenant.ReplicaBackfill{Replica: name, JobID: r.id("job"), Now: at}, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
	c := &archiveStore{Store: d, online: map[string]bool{}, sums: map[string][sha256.Size]byte{}, sizes: map[string]int64{}}
	r.replicas[name] = c
	return c
}

// TestArchiveTierIsCheckedByPresence guards that a scrub reads no object
// of an archive-tier replica held cold and asks for no recall: it checks
// each by presence, its size and the store's SHA-256 where the store keeps
// one, records a difference in either as the replica's damage, and passes
// an object whose store keeps no checksum on its size alone; and that once
// scrub.archive-sample is changed to every object, the next scrub of the
// same supervisor reads the bytes of an object online there.
func TestArchiveTierIsCheckedByPresence(t *testing.T) {
	r := newReplicaRig(t)
	files := synthetic("1.2.826.0.1.3680043.10.504", 4)
	p := r.archiveEntries("s1", "1.2.826.0.1.3680043.10.504.90", "1.2.826.0.1.3680043.10.504.80", files)
	series := "1.2.826.0.1.3680043.10.504.80"
	cold := r.addColdReplica("cold")
	r.copyAll()
	k := func(i int) string { return r.keys[files[i].uid] }
	// The first keeps its checksum, right; the second keeps none and is
	// checked by its size alone; the third keeps a wrong one; the fourth
	// is not of the archived size.
	cold.sums[k(0)] = sha256.Sum256(files[0].b)
	cold.sums[k(2)] = sha256.Sum256([]byte("other bytes"))
	cold.sizes[k(3)] = int64(len(files[3].b)) + 1

	// One supervisor serves both scrubs: scrub.archive-sample is read at
	// each unit, so changing it between them changes the second without a
	// new supervisor.
	sample := &atomic.Value{}
	sample.Store(0.0)
	cfg := r.config()
	cfg.TenantSettings = func(tenant.Store) TenantSettings { return settingsOf{rate: 64 << 30, sample: sample} }
	s := r.start(cfg)
	j := r.waitEnded(s, r.createScrub("").ID)
	if n := cold.dataReads.Load(); n != 0 {
		t.Errorf("the scrub read %d objects of the archive tier held cold; want none", n)
	}
	if n := cold.recalls.Load(); n != 0 {
		t.Errorf("the scrub asked for %d recalls; want none", n)
	}
	tallies := r.tallies(j.ID)
	if tallies[TallyPresenceChecked] != 4 || tallies[TallyReplicaDamaged] != 2 {
		t.Errorf("the scrub tallied %v; want 4 objects checked by presence, 2 of them damaged", tallies)
	}
	if row := r.row("cold", series); row.State != tenant.ReplicaDamaged || row.Damaged != 2 {
		t.Errorf("the archive-tier replica's row is %s with %d damaged; want damaged with 2", row.State, row.Damaged)
	}
	for _, in := range r.held(p) {
		if in.Damaged {
			t.Errorf("the instance %s is marked; a replica's damage marks no instance", in.SOPInstanceUID)
		}
	}

	cold.mu.Lock()
	cold.online[k(0)] = true
	cold.mu.Unlock()
	sample.Store(1.0)
	j = r.waitEnded(s, r.createScrub("").ID)
	if n := cold.dataReads.Load(); n != 1 {
		t.Errorf("with every object sampled the scrub read %d objects of the archive tier; want the one online", n)
	}
	if got := r.tallies(j.ID); got[TallyPresenceSampled] != 1 || got[TallyPresenceChecked] != 3 {
		t.Errorf("the sampled scrub tallied %v; want 1 sampled and 3 checked by presence", got)
	}
	if n := cold.recalls.Load(); n != 0 {
		t.Errorf("the sampled scrub asked for %d recalls; want none", n)
	}
}
