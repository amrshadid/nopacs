package suitecheck_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
	"github.com/amrshadid/nopacs/internal/fault"
)

// flaw names one way an in-memory driver is made wrong on purpose, to show
// the suite catches it. The empty name is the correct driver.
type flaw string

// The flaws, each a promise of blob.Store broken in one place.
const (
	noFlaw            flaw = ""
	partialReplace    flaw = "replace-shows-partial-bytes"
	wrongChecksum     flaw = "wrong-checksum-for-large-objects"
	nextOnFullPage    flaw = "next-on-an-exactly-full-last-page"
	liveRead          flaw = "read-follows-later-puts"
	unstableOrder     flaw = "unstable-page-order"
	commitOnCancel    flaw = "canceled-put-still-commits"
	deleteTakesPrefix flaw = "delete-removes-keys-under-it"
	failDropsOld      flaw = "failed-put-drops-the-old-object"
	anyKey            flaw = "accepts-any-key"
	staleStat         flaw = "stat-sees-deleted-keys"
)

var flaws = []flaw{partialReplace, wrongChecksum, nextOnFullPage, liveRead, unstableOrder, commitOnCancel, deleteTakesPrefix, failDropsOld, anyKey, staleStat}

// flawEnv names the variable that makes this test binary, run again by
// TestSuiteCatchesWrongDrivers, run the suite against one flawed driver.
const flawEnv = "NOPACS_BLOBTEST_FLAW"

// TestSuiteOnMemory guards that a correct in-memory driver, written from
// blob.Store's documentation alone, passes the suite: the suite asks for
// nothing a bucket or a map could not do.
func TestSuiteOnMemory(t *testing.T) {
	blobtest.Run(t, blobtest.Driver{Open: openMem(noFlaw)})
}

// TestSuiteCatchesWrongDrivers guards the suite itself: run against an
// in-memory driver that breaks one promise of blob.Store, it must fail, for
// every promise listed in flaws. Each runs in a child process, since a
// failing suite fails the test that runs it.
func TestSuiteCatchesWrongDrivers(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the suite once per flaw, each in a child process")
	}
	for _, f := range flaws {
		t.Run(string(f), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestFlawedDriverChild$", "-test.count=1") // #nosec G204 G702 -- this test binary itself.
			cmd.Env = append(os.Environ(), flawEnv+"="+string(f))
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "--- FAIL") {
				t.Fatalf("the suite passed a driver whose flaw is %q:\n%s", f, out)
			}
		})
	}
}

// TestFlawedDriverChild runs the suite against the flawed driver flawEnv
// names; it is a child of TestSuiteCatchesWrongDrivers and skips otherwise.
func TestFlawedDriverChild(t *testing.T) {
	f := flaw(os.Getenv(flawEnv))
	if f == noFlaw {
		t.Skip("run by TestSuiteCatchesWrongDrivers")
	}
	blobtest.Run(t, blobtest.Driver{Open: openMem(f)})
}

func openMem(f flaw) func(testing.TB) blob.Store {
	return func(testing.TB) blob.Store { return &mem{m: map[string][]byte{}, flaw: f, deleted: map[string]bool{}} }
}

// mem is an in-memory blob.Store. Objects are byte slices that are never
// changed once stored, so a reader keeps the version it was given.
type mem struct {
	mu      sync.Mutex
	m       map[string][]byte
	deleted map[string]bool
	flaw    flaw
	closed  bool
	calls   int
}

func (s *mem) begin(ctx context.Context) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return fault.New(fault.Unavailable, "closed")
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.Unavailable, err, "canceled")
	}
	return nil
}

func (s *mem) checkKey(key string) error {
	if s.flaw == anyKey {
		return nil
	}
	return blob.CheckKey(key)
}

func (s *mem) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if err := s.begin(ctx); err != nil {
		return blob.Written{}, err
	}
	if err := s.checkKey(key); err != nil {
		return blob.Written{}, err
	}
	s.mu.Lock()
	for k := range s.m {
		if strings.HasPrefix(k, key+"/") || strings.HasPrefix(key, k+"/") {
			s.mu.Unlock()
			return blob.Written{}, fault.New(fault.Conflict, "a key and a key under it")
		}
	}
	if s.flaw == failDropsOld {
		delete(s.m, key)
	}
	s.mu.Unlock()
	var buf bytes.Buffer
	chunk := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil && s.flaw != commitOnCancel {
			return blob.Written{}, fault.Wrap(fault.Unavailable, err, "canceled")
		}
		n, err := r.Read(chunk)
		buf.Write(chunk[:n])
		if s.flaw == partialReplace {
			s.mu.Lock()
			s.m[key] = slices.Clone(buf.Bytes())
			s.mu.Unlock()
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return blob.Written{}, err
		}
	}
	if err := ctx.Err(); err != nil && s.flaw != commitOnCancel {
		return blob.Written{}, fault.Wrap(fault.Unavailable, err, "canceled")
	}
	b := buf.Bytes()
	s.mu.Lock()
	s.m[key] = b
	delete(s.deleted, key)
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return blob.Written{}, fault.Wrap(fault.Unavailable, err, "canceled")
	}
	w := blob.Written{Size: int64(len(b)), SHA256: sha256.Sum256(b)}
	if s.flaw == wrongChecksum && len(b) > 70000 {
		w.SHA256 = sha256.Sum256(b[1:])
	}
	return w, nil
}

func (s *mem) Read(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	if err := blob.CheckRange(off, length); err != nil {
		return nil, err
	}
	s.mu.Lock()
	b, ok := s.m[key]
	s.mu.Unlock()
	if !ok {
		return nil, fault.New(fault.NotFound, "no object")
	}
	if off > int64(len(b)) {
		return nil, blob.ErrPastEnd()
	}
	n := int64(len(b)) - off
	if length >= 0 && length < n {
		n = length
	}
	if s.flaw == liveRead {
		return io.NopCloser(&live{s: s, key: key, off: off, left: n}), nil
	}
	return io.NopCloser(bytes.NewReader(b[off : off+n])), nil
}

// live reads whatever the key holds at each call, not what it held when
// Read was called.
type live struct {
	s         *mem
	key       string
	off, left int64
}

func (l *live) Read(p []byte) (int, error) {
	l.s.mu.Lock()
	b := l.s.m[l.key]
	l.s.mu.Unlock()
	if l.left == 0 || l.off >= int64(len(b)) {
		return 0, io.EOF
	}
	k := copy(p[:min(int64(len(p)), l.left)], b[l.off:])
	l.off += int64(k)
	l.left -= int64(k)
	return k, nil
}

func (s *mem) Stat(ctx context.Context, key string) (blob.Info, error) {
	if err := s.begin(ctx); err != nil {
		return blob.Info{}, err
	}
	if err := s.checkKey(key); err != nil {
		return blob.Info{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[key]
	if !ok {
		if s.flaw == staleStat && s.deleted[key] {
			return blob.Info{}, nil
		}
		return blob.Info{}, fault.New(fault.NotFound, "no object")
	}
	return blob.Info{Size: int64(len(b))}, nil
}

func (s *mem) List(ctx context.Context, o blob.ListOptions) (blob.Page, error) {
	if err := s.begin(ctx); err != nil {
		return blob.Page{}, err
	}
	if err := blob.CheckListOptions(o); err != nil {
		return blob.Page{}, err
	}
	if o.Token != "" && (!strings.HasPrefix(o.Token, o.Prefix) || blob.CheckKey(strings.TrimSuffix(o.Token, "/")) != nil) {
		return blob.Page{}, fault.New(fault.Invalid, "not a token of this listing")
	}
	s.mu.Lock()
	seen := map[string]bool{}
	var all []string
	for k := range s.m {
		if !strings.HasPrefix(k, o.Prefix) {
			continue
		}
		e := k
		if o.Delimiter != "" {
			if i := strings.IndexByte(k[len(o.Prefix):], '/'); i >= 0 {
				e = k[:len(o.Prefix)+i+1]
			}
		}
		if !seen[e] && e > o.Token {
			seen[e] = true
			all = append(all, e)
		}
	}
	s.calls++
	calls := s.calls
	s.mu.Unlock()
	slices.Sort(all)
	limit := blob.ListLimit(o)
	var p blob.Page
	page := all
	if len(all) > limit {
		page = all[:limit]
		p.Next = page[limit-1]
	} else if s.flaw == nextOnFullPage && len(all) == limit {
		p.Next = page[limit-1]
	}
	if s.flaw == unstableOrder && len(page) > 1 && calls%2 == 0 {
		page[0], page[1] = page[1], page[0]
	}
	for _, e := range page {
		if strings.HasSuffix(e, "/") {
			p.Prefixes = append(p.Prefixes, e)
		} else {
			p.Keys = append(p.Keys, e)
		}
	}
	return p, nil
}

func (s *mem) Delete(ctx context.Context, key string) error {
	if err := s.begin(ctx); err != nil {
		return err
	}
	if err := s.checkKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; ok {
		s.deleted[key] = true
	}
	delete(s.m, key)
	if s.flaw == deleteTakesPrefix {
		for k := range s.m {
			if strings.HasPrefix(k, key+"/") {
				delete(s.m, k)
			}
		}
	}
	return nil
}

func (s *mem) Purge(ctx context.Context, key string) ([]blob.Removal, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, held := s.m[key]
	s.mu.Unlock()
	if !held {
		return nil, nil
	}
	if err := s.Delete(ctx, key); err != nil {
		return nil, err
	}
	return []blob.Removal{{Disposition: blob.Removed}}, nil
}

func (s *mem) Usage(ctx context.Context) (blob.Usage, error) {
	if err := s.begin(ctx); err != nil {
		return blob.Usage{}, err
	}
	return blob.Usage{}, nil
}

func (s *mem) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
