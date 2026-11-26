package replicate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A copy that did not end puts no more, but what it put stays on the
// replica: a put cannot share a transaction with the catalog, and a scan
// deleted, or an instance purged, while the copy streamed would otherwise
// keep its bytes on the replica, where nothing removes them (a scrub of the
// replica reads only what the catalog holds, and records the rest as an
// orphan at most). So such a copy removes what it put that the catalog no
// longer names. This is a second guard: a purge already waits until every
// copy its start ended can no longer land, and checks every copy after.

// sweeps reports whether a copy that ended with err removes what it put
// that the catalog no longer names: a change of the scan published under
// it, its lease lost (to a purge's start, a delete's publish, or another
// worker), or its scan gone or hidden.
func sweeps(err error) bool {
	var ce *copyError
	return errors.Is(err, tenant.ErrLeaseLost) || errors.Is(err, errScanGone) || (errors.As(err, &ce) && ce.changed)
}

// putting records key as put by this copy, before its put begins.
func (s *scanCopy) putting(key string) {
	s.mu.Lock()
	s.putKeys = append(s.putKeys, key)
	s.mu.Unlock()
}

// sweep removes from the replica, inside its write gate, each key this copy
// put that the catalog no longer names (named), with every version a store
// keeps of it. Nothing is recorded as the replica's damage: the replica is
// not wrong, the copy is undone. A catalog that cannot be read, or a
// committed manifest that cannot be told, leaves everything as it is; a
// removal that fails is logged, and the replica's scrub finds the key.
func (s *scanCopy) sweep(ctx context.Context) {
	s.mu.Lock()
	keys := slices.Clone(s.putKeys)
	s.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	log := s.w.log.With("replica", s.c.Fence.Replica, "scan", scanPath(s.c.Path))
	keep, err := s.named(cctx)
	if err != nil {
		log.Warn("replicas: what a copy that did not end put could not be told from the catalog; it is left to the scrub",
			"kind", fault.KindOf(err).String())
		return
	}
	removed := 0
	for _, key := range keys {
		if keep[key] {
			continue
		}
		if err := s.remove(cctx, key); err != nil {
			log.Warn("replicas: an object a copy that did not end put could not be removed; it is left to the scrub",
				"kind", fault.KindOf(err).String())
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Info("replicas: a copy that did not end removed what it put that the catalog no longer names", "objects", removed)
	}
}

// named answers the keys of the scan the catalog names now: each live
// instance's object, every object the committed manifest names (an
// instance a delete hid is kept until its purge), and the scan's manifest
// and metadata, which the next copy writes again as the catalog says. A
// scan the catalog no longer holds, or hides, names nothing. An error is a
// catalog that cannot be read, or a committed manifest the primary does not
// hold at its digest now (a change in flight): nothing can be told then.
func (s *scanCopy) named(ctx context.Context) (map[string]bool, error) {
	keep := map[string]bool{}
	held, err := s.held(ctx)
	if errors.Is(err, errScanGone) {
		return keep, nil
	}
	if err != nil {
		return nil, err
	}
	scan, err := s.w.cfg.Catalog.CatalogScan(ctx, s.c.Path)
	if fault.KindOf(err) == fault.NotFound {
		return keep, nil
	}
	if err != nil {
		return nil, &databaseError{err: err}
	}
	for _, in := range held {
		keep[in.Key] = true
	}
	mk, err := layout.ManifestKey(s.c.Path)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	sk, err := layout.MetadataKey(s.c.Path)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	keep[mk], keep[sk] = true, true
	if scan.ManifestSHA256.IsZero() {
		return keep, nil
	}
	if s.primary == nil {
		return nil, fault.New(fault.Unavailable, "the primary was never opened, so its manifest cannot be read")
	}
	body, err := s.primary.Read(ctx, mk, 0, -1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	data, err := io.ReadAll(io.LimitReader(body, int64(manifest.MaxBytes)+1))
	if err != nil {
		return nil, err
	}
	if tenant.SHA256(sha256.Sum256(data)) != scan.ManifestSHA256 {
		return nil, fault.New(fault.Unavailable, "the primary's manifest is not the committed one: a change is in flight")
	}
	m, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fault.Wrap(fault.DataLoss, err, "the primary's committed manifest does not decode")
	}
	for _, f := range m.Files {
		keep[f.Key] = true
	}
	return keep, nil
}

// remove purges key, every version of it, from the replica inside its
// write gate, the replica opened again inside the gate: a switch swaps the
// replica's place only once its writers left, so the place opened then is
// the one the replica's name names while the removal runs, never the
// primary's.
func (s *scanCopy) remove(ctx context.Context, key string) error {
	if s.w.cfg.Pause != nil {
		release, err := s.w.cfg.Pause.Enter(ctx, s.c.Fence.Replica)
		if err != nil {
			return err
		}
		defer release()
	}
	st, err := s.w.cfg.Open(ctx, s.c.Fence.Replica)
	if err != nil {
		return err
	}
	if st == nil {
		return fault.New(fault.Internal, "replicate: the opener answered no storage")
	}
	defer func() { _ = st.Close() }() // every removal returned before this
	_, err = st.Purge(ctx, key)
	return err
}
