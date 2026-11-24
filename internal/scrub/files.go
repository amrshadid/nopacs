package scrub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// catalogView is what the catalog holds of a scan now: its manifest's
// checksum and its instances by SOP Instance UID, every one held whatever
// the job's bound, since the manifest and the sidecar list them all.
type catalogView struct {
	manifestSHA256 tenant.SHA256
	byUID          map[string]tenant.ScanInstance
	byKey          map[string]tenant.ScanInstance
	studyUID       string
	seriesUID      string
}

// view reads the catalog's view of the scan; false when the catalog no
// longer holds it.
func (s *scanCheck) view() (catalogView, bool, error) {
	u := s.u
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	sc, err := u.p.CatalogScan(ctx, s.scan.Path)
	if fault.KindOf(err) == fault.NotFound {
		return catalogView{}, false, nil
	}
	if err != nil {
		return catalogView{}, false, err
	}
	ses, err := u.p.CatalogSession(ctx, s.scan.Path.Subject, s.scan.Path.Session)
	if err != nil {
		return catalogView{}, false, err
	}
	v := catalogView{manifestSHA256: sc.ManifestSHA256, byUID: map[string]tenant.ScanInstance{}, byKey: map[string]tenant.ScanInstance{},
		studyUID: ses.StudyUID, seriesUID: sc.SeriesUID}
	page := master.Page{Limit: instancePage}
	for range maxListPages {
		rows, next, err := u.p.ListHeldInstances(ctx, s.scan.Path, page)
		if err != nil {
			return catalogView{}, false, err
		}
		for _, in := range rows {
			v.byUID[in.SOPInstanceUID], v.byKey[in.Key] = in, in
		}
		if next == "" {
			return v, true, nil
		}
		page.Cursor = next
	}
	return catalogView{}, false, fault.New(fault.Internal, "scrub: a scan's instances did not end")
}

// files compares the scan's manifest, sidecar and objects with the
// catalog, recording what differs. Each difference is believed only when a
// second look, SettleDelay later and with the catalog read again, finds it
// too: a reception writes a scan's files before it commits their catalog
// rows, so one look can fall between the two.
func (s *scanCheck) files() outcome {
	first, ok, err := s.look()
	if err != nil {
		return s.u.failure(err, reasonStorage)
	}
	if !ok {
		return outcome{} // the scan is no longer in the catalog
	}
	if len(first.findings) > 0 {
		if err := s.u.sup.cfg.seams.sleep(s.u.ctx, s.u.sup.cfg.seams.settle); err != nil {
			return outcome{stopped: true}
		}
		var second lookResult
		if second, ok, err = s.look(); err != nil {
			return s.u.failure(err, reasonStorage)
		}
		if !ok {
			return outcome{}
		}
		first = first.both(second)
	}
	for k := range first.good {
		s.good[k] = true
	}
	for _, f := range first.findings {
		if out := s.record(f); out != (outcome{}) {
			return out
		}
	}
	for k, n := range first.tallies {
		s.tallies[k] += n
	}
	return outcome{}
}

// lookResult is what one look at a scan's files found: the differences,
// the findings it checked and found gone, and the tallies.
type lookResult struct {
	findings []tenant.ScrubFinding
	good     map[findingKey]bool
	tallies  map[string]int64
}

// both keeps the differences r and next both found, as next found them,
// and what next found good.
func (r lookResult) both(next lookResult) lookResult {
	out := lookResult{good: next.good, tallies: next.tallies}
	for _, f := range next.findings {
		if slices.ContainsFunc(r.findings, func(g tenant.ScrubFinding) bool { return g.Key == f.Key && g.Kind == f.Kind }) {
			out.findings = append(out.findings, f)
		}
	}
	return out
}

// look reads the catalog's view of the scan and compares its manifest, its
// sidecar and the objects under its keys with it once.
func (s *scanCheck) look() (lookResult, bool, error) {
	s.exp, s.recs = nil, nil // a correction may have moved since the last look
	v, ok, err := s.view()
	if err != nil || !ok {
		return lookResult{}, ok, err
	}
	r := lookResult{good: map[findingKey]bool{}, tallies: map[string]int64{}}
	listed, err := s.lookManifest(v, &r)
	if err != nil {
		return lookResult{}, false, err
	}
	if err := s.lookSidecar(v, &r); err != nil {
		return lookResult{}, false, err
	}
	if err := s.lookOrphans(v, listed, &r); err != nil {
		return lookResult{}, false, err
	}
	return r, true, nil
}

// readAll reads the whole object at key on the scan's storage, at most
// limit bytes, under a read slot and paced (readWhole).
func (s *scanCheck) readAll(key string, limit int64) ([]byte, presence, error) {
	return s.u.readWhole(s.store, s.scan.Storage, key, limit)
}

// readWhole reads the whole object at key on store, the storage named
// storage, at most limit bytes, under a read slot and paced; a larger
// object reads as its first limit+1 bytes.
func (u *unitRun) readWhole(store Storage, storage, key string, limit int64) ([]byte, presence, error) {
	release, err := u.sup.cfg.Reads.Take(ReadOwner)
	if err != nil {
		return nil, missing, err
	}
	defer release()
	u.reads[key]++
	if f := u.sup.cfg.seams.read; f != nil {
		f(key, u.reads[key])
	}
	body, err := store.Read(u.ctx, key, 0, -1)
	switch {
	case errors.Is(err, blob.ErrNotOnline):
		return nil, notOnline, nil
	case fault.KindOf(err) == fault.NotFound:
		return nil, missing, nil
	case err != nil:
		return nil, missing, err
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	var b bytes.Buffer
	r := &pacedReader{ctx: u.ctx, r: io.LimitReader(body, limit+1), p: u.t.pacer(storage), rate: u.rate}
	if _, err := io.CopyBuffer(&b, r, u.buf); err != nil {
		if errors.Is(err, blob.ErrNotOnline) {
			return nil, notOnline, nil
		}
		return nil, missing, err
	}
	return b.Bytes(), present, nil
}

// lookManifest compares the scan's manifest with the catalog: its SHA-256
// with the one the catalog holds, then each line with the catalog's row of
// its instance. It returns the keys the manifest lists, for the look for
// orphans.
func (s *scanCheck) lookManifest(v catalogView, r *lookResult) (map[string]bool, error) {
	key, err := layout.ManifestKey(s.scan.Path)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	data, p, err := s.readAll(key, int64(manifest.MaxBytes))
	if err != nil || p == notOnline {
		return nil, err
	}
	damaged := tenant.ScrubFinding{Kind: tenant.FindingManifestDamaged, Storage: s.scan.Storage, Key: key, Path: s.scan.Path,
		ExpectedSHA256: v.manifestSHA256}
	if p == missing {
		r.findings = append(r.findings, damaged)
		return nil, nil
	}
	size, sum := int64(len(data)), tenant.SHA256(sha256.Sum256(data))
	damaged.FoundSize, damaged.FoundSHA256 = &size, sum
	if !v.manifestSHA256.IsZero() && sum != v.manifestSHA256 {
		listed, inFlight, err := s.manifestInFlight(v, data)
		if err != nil {
			return nil, err
		}
		if inFlight {
			// A reception put the scan's next manifest and has not
			// committed its rows: nothing to record, nothing to clear.
			return listed, nil
		}
		if listed, corrected, err := s.manifestCorrected(v, data); err != nil || corrected {
			// A correction not yet published, or a purge not yet ended,
			// wrote it: nothing to record, nothing to clear.
			return listed, err
		}
		r.findings = append(r.findings, damaged)
		return nil, nil
	}
	m, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		r.findings = append(r.findings, damaged)
		return nil, nil
	}
	r.good[findingKey{key, tenant.FindingManifestDamaged}] = true
	listed := make(map[string]bool, len(m.Files))
	lines := make(map[string]manifest.File, len(m.Files))
	var unknown []string
	for _, f := range m.Files {
		listed[f.Key], lines[f.SOPInstanceUID] = true, f
		if _, held := v.byUID[f.SOPInstanceUID]; !held {
			unknown = append(unknown, f.SOPInstanceUID)
		}
	}
	hidden, err := s.hiddenOf(unknown)
	if err != nil {
		return nil, err
	}
	claimed, err := s.claimed(v, unknown)
	if err != nil {
		return nil, err
	}
	for _, uid := range unknown {
		if claimed[uid] || hidden[uid] {
			continue // a reception's file not yet committed, or a delete's not yet purged
		}
		f := lines[uid]
		found := f.Size
		r.findings = append(r.findings, tenant.ScrubFinding{Kind: tenant.FindingDisagreeing, Storage: s.scan.Storage, Key: f.Key,
			Path: s.scan.Path, FoundSize: &found, FoundSHA256: hexSHA256(f.SHA256)})
	}
	for uid, in := range v.byUID {
		k := findingKey{in.Key, tenant.FindingDisagreeing}
		expected := in.Size
		d := tenant.ScrubFinding{Kind: tenant.FindingDisagreeing, Storage: s.scan.Storage, Key: in.Key, Path: s.scan.Path,
			SOPInstanceUID: uid, Revision: revisionOf(in.Key), ExpectedSize: &expected, ExpectedSHA256: in.ArchivedSHA256}
		f, ok := lines[uid]
		switch {
		case !ok:
			r.findings = append(r.findings, d)
		case f.Key != in.Key || f.Offset != in.Offset || f.Length != in.Length || f.Size != in.Size || f.SHA256 != in.ArchivedSHA256.String():
			found := f.Size
			d.FoundSize, d.FoundSHA256 = &found, hexSHA256(f.SHA256)
			r.findings = append(r.findings, d)
		default:
			r.good[k] = true
		}
	}
	return listed, nil
}

// manifestInFlight reports whether a manifest that is not of the checksum
// the catalog holds is the next one of a reception in flight: a reception
// puts the scan's manifest, listing every instance the catalog holds and
// its own, before it commits its rows with the new checksum. It is when
// the manifest decodes,
// lists every instance the catalog holds, and lists at least one more,
// each of which a reception claims and has not committed. It returns the
// keys the manifest lists then.
func (s *scanCheck) manifestInFlight(v catalogView, data []byte) (map[string]bool, bool, error) {
	m, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false, nil
	}
	listed := make(map[string]bool, len(m.Files))
	lines := make(map[string]bool, len(m.Files))
	var unknown []string
	for _, f := range m.Files {
		listed[f.Key], lines[f.SOPInstanceUID] = true, true
		if _, held := v.byUID[f.SOPInstanceUID]; !held {
			unknown = append(unknown, f.SOPInstanceUID)
		}
	}
	if len(unknown) == 0 {
		return nil, false, nil
	}
	for uid := range v.byUID {
		if !lines[uid] {
			return nil, false, nil
		}
	}
	claimed, err := s.claimed(v, unknown)
	if err != nil {
		return nil, false, err
	}
	for _, uid := range unknown {
		if !claimed[uid] {
			return nil, false, nil
		}
	}
	return listed, true, nil
}

// lookSidecar compares the scan's sidecar with the catalog: each line's
// key, size and SHA-256 with the catalog's row of its instance, and that
// each lists the same instances. A difference is one finding on the
// sidecar's key; a sidecar not there is a tally, since a scan archived
// before sidecars has none.
func (s *scanCheck) lookSidecar(v catalogView, r *lookResult) error {
	key, err := layout.MetadataKey(s.scan.Path)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	u := s.u
	release, err := u.sup.cfg.Reads.Take(ReadOwner)
	if err != nil {
		return err
	}
	defer release()
	body, err := s.store.Read(u.ctx, key, 0, -1)
	switch {
	case errors.Is(err, blob.ErrNotOnline):
		return nil
	case fault.KindOf(err) == fault.NotFound:
		r.tallies[tenant.TallySidecarMissing]++
		return nil
	case err != nil:
		return err
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	rd := sidecar.NewReader(&pacedReader{ctx: u.ctx, r: body, p: u.t.pacer(s.scan.Storage), rate: u.rate})
	seen := map[string]bool{}
	var unknown []string
	agrees := true
	for range maxListPages {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, sidecar.ErrFormat) {
			agrees = false
			break
		}
		if err != nil {
			return err
		}
		seen[in.SOPInstanceUID] = true
		row, held := v.byUID[in.SOPInstanceUID]
		switch {
		case !held:
			unknown = append(unknown, in.SOPInstanceUID)
		case in.Key != row.Key || in.Size != row.Size || in.SHA256 != row.ArchivedSHA256.String():
			if ok, err := s.sidecarLineExpected(in.SOPInstanceUID, in.Key, in.SHA256, in.Size); err != nil {
				return err
			} else if !ok {
				agrees = false
			}
		}
	}
	hidden, err := s.hiddenOf(unknown)
	if err != nil {
		return err
	}
	claimed, err := s.claimed(v, unknown)
	if err != nil {
		return err
	}
	for _, uid := range unknown {
		if !claimed[uid] && !hidden[uid] {
			agrees = false
		}
	}
	for uid := range v.byUID {
		if !seen[uid] {
			agrees = false
		}
	}
	if agrees {
		r.good[findingKey{key, tenant.FindingDisagreeing}] = true
		return nil
	}
	r.findings = append(r.findings, tenant.ScrubFinding{Kind: tenant.FindingDisagreeing, Storage: s.scan.Storage, Key: key, Path: s.scan.Path})
	return nil
}

// lookOrphans lists the objects under the scan's keys, in data/ and meta/,
// and records each that neither the catalog, the manifest, a correction
// not yet purged nor a claim of a reception in flight names, and that is
// not the scan's manifest or sidecar. An open orphan finding of the scan whose object is gone, or now
// named, is found good.
func (s *scanCheck) lookOrphans(v catalogView, listed map[string]bool, r *lookResult) error {
	data, err := layout.InstancePrefix(s.scan.Path)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	manifestKey, _ := layout.ManifestKey(s.scan.Path)
	metadataKey, _ := layout.MetadataKey(s.scan.Path)
	meta := manifestKey[:len(manifestKey)-len(layout.ManifestName)]
	var orphans []string
	uidOf := map[string]string{}
	for _, prefix := range []string{data, meta} {
		keys, err := s.list(prefix)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if _, named := v.byKey[k]; named || listed[k] || k == manifestKey || k == metadataKey {
				continue
			}
			if kept, err := s.expectedObject(k); err != nil {
				return err
			} else if kept {
				continue // a replace's revision a correction keeps until its publish or purge
			}
			orphans = append(orphans, k)
			if lk, err := layout.Parse(k); err == nil && lk.Kind == layout.Instance {
				uidOf[k] = lk.SOPInstanceUID
			}
		}
	}
	var uids []string
	for _, u := range uidOf {
		uids = append(uids, u)
	}
	claimed, err := s.claimed(v, uids)
	if err != nil {
		return err
	}
	isOrphan := map[string]bool{}
	for _, k := range orphans {
		if uid, ok := uidOf[k]; ok && claimed[uid] {
			continue // a reception's file not yet committed
		}
		isOrphan[k] = true
		r.findings = append(r.findings, tenant.ScrubFinding{Kind: tenant.FindingOrphan, Storage: s.scan.Storage, Key: k, Path: s.scan.Path})
	}
	for k := range s.open {
		if k.kind == tenant.FindingOrphan && !isOrphan[k.key] {
			r.good[k] = true
		}
	}
	return nil
}

// list returns every key under prefix on the scan's storage.
func (s *scanCheck) list(prefix string) ([]string, error) {
	return listAll(s.u.ctx, s.store, prefix, "")
}

// listAll returns every key, and with the delimiter every prefix, one level
// under prefix.
func listAll(ctx context.Context, store Storage, prefix, delimiter string) ([]string, error) {
	var out []string
	opts := blob.ListOptions{Prefix: prefix, Delimiter: delimiter}
	for range maxListPages {
		page, err := store.List(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Keys...)
		out = append(out, page.Prefixes...)
		if page.Next == "" {
			return out, nil
		}
		opts.Token = page.Next
	}
	return nil, fault.New(fault.Internal, "scrub: a storage's listing did not end")
}

// claimed returns which of uids a reception claims now and has not
// committed: their objects and lines are written before their catalog rows,
// so they are in flight, not differences.
func (s *scanCheck) claimed(v catalogView, uids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for start := 0; start < len(uids); start += tenant.MaxDuplicateBatch {
		part := uids[start:min(start+tenant.MaxDuplicateBatch, len(uids))]
		qs := make([]tenant.DuplicateQuery, len(part))
		for i, u := range part {
			// The answer Claimed is given whatever the study, series and
			// checksum asked with, so the scan's own and any checksum do.
			qs[i] = tenant.DuplicateQuery{SOPInstanceUID: u, StudyUID: v.studyUID, SeriesUID: v.seriesUID, SHA256: anySum}
		}
		ctx, cancel := context.WithTimeout(s.u.ctx, opTimeout)
		ds, err := s.u.p.CheckDuplicates(ctx, qs)
		cancel()
		if err != nil {
			return nil, err
		}
		for i, d := range ds {
			if i < len(part) && d.Kind == tenant.Claimed {
				out[part[i]] = true
			}
		}
	}
	return out, nil
}

// anySum is a checksum a duplicate check is asked with when only whether a
// UID is claimed matters.
var anySum = tenant.SHA256{0: 1}

// hexSHA256 reads a checksum written in hexadecimal, zero when it is not
// one.
func hexSHA256(s string) tenant.SHA256 {
	var out tenant.SHA256
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(out) {
		return tenant.SHA256{}
	}
	copy(out[:], b)
	return out
}
