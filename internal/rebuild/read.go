package rebuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a rebuild reads from one storage, and nothing else: its correction
// records, the list of its scans under meta/, and each scan's manifest and
// metadata.

// errUnreadableRecord is a correction record that cannot be read: the build
// stops rather than show what it may hide.
var errUnreadableRecord = errors.New("a correction record cannot be read")

// hides is what a storage's correction records keep out of a rebuild:
// whole scans, and instances of scans, each scan by its labels and its
// Series Instance UID together, as a record names it.
type hides struct {
	// scans are the scans hidden whole.
	scans map[string]bool
	// instances are the instances hidden in a scan that stays.
	instances map[string]map[string]bool
	// unresolved counts the deletes applied with no row in the tenant's
	// database to confirm them.
	unresolved int64
	// unauthenticated counts the records in the form whose MAC is missing
	// or does not verify: each is applied not at all.
	unauthenticated int64
}

// recordAuth is what a record is checked against: the tenant's id and the
// installation keys (record.DecodeCorrection).
type recordAuth struct {
	tenantID string
	keys     record.Keyring
}

// hideKey names a scan by its labels and its series.
func hideKey(p label.Path, series string) string {
	return p.Subject + "/" + p.Session + "/" + p.Scan + "\x00" + series
}

// pathOf returns a record's scan's labels.
func pathOf(s record.Scan) label.Path {
	return label.Path{Subject: s.Subject, Session: s.Session, Scan: s.Scan}
}

// relabeled returns p with its label at level changed to to.
func relabeled(p label.Path, level, to string) label.Path {
	switch level {
	case record.LevelSubject:
		p.Subject = to
	case record.LevelSession:
		p.Session = to
	default:
		p.Scan = to
	}
	return p
}

// add applies one record. A delete hides what it names unless it was
// undone or abandoned; one still prepared is taken as committed, since no
// row says otherwise and a delete is fail-closed. A relabel hides the tree
// that is not the archive's: the old labels once it was published, the new
// ones before (its copy may be partial). A move hides as a relabel does,
// its new tree being the old one under the destination subject; undone, it
// hides that new tree too. A replace hides nothing: the manifest names the
// revision it read back before naming it.
//
// With no Config.Decide the record alone is read, and its state decides: the
// fail-closed side, a delete the record names applied whatever a row might
// say. The server's rebuilds and a restore's read the row (ByRow): add is
// handed the record with the row's state, so a delete undone or abandoned,
// or still prepared, hides nothing whatever its record says, or it is kept
// out. Every delete applied is counted unresolved either way; counting
// only those with no row to confirm them is not done yet.
func (h *hides) add(c record.Correction) {
	switch c.Kind {
	case record.KindDelete:
		if c.State == record.StateUndone || c.State == record.StateAbandoned {
			return
		}
		h.unresolved++
		for _, s := range c.Scans {
			k := hideKey(pathOf(s), s.SeriesUID)
			if c.Level != record.LevelInstance {
				h.scans[k] = true
				continue
			}
			if h.instances[k] == nil {
				h.instances[k] = map[string]bool{}
			}
			for _, u := range c.Instances {
				h.instances[k][u] = true
			}
		}
	case record.KindRelabel:
		for _, s := range c.Scans {
			p := pathOf(s)
			if !c.State.Published() {
				p = relabeled(p, c.Relabel.Level, c.Relabel.To)
			}
			h.scans[hideKey(p, s.SeriesUID)] = true
		}
	case record.KindMove:
		// As a relabel: the old tree once it was published, the tree under
		// the destination before (its copy may be partial) and once it was
		// undone (its copies not yet swept). A merge's subject is left with
		// no scan, so no row of it is made.
		for _, s := range c.Scans {
			p := pathOf(s)
			if !c.State.Published() {
				p = tenant.MovedPath(p, c.Move.To)
			}
			h.scans[hideKey(p, s.SeriesUID)] = true
		}
	}
}

// decider answers how a record is applied (Config.Decide); nil applies it
// as it reads.
type decider func(c record.Correction) (record.Correction, bool, error)

// readHides reads every correction record of the storage, each applied as
// decide answers. A record that cannot be listed or read is an error the
// unit fails on. A record whose MAC is missing or does not verify was not
// written by this installation for this tenant, or was changed since:
// anyone who can write to a storage can put one there, and applying it
// would let them hide a live scan for good, so it hides nothing and is
// counted, and the build goes on.
func readHides(ctx context.Context, st Storage, auth recordAuth, decide decider) (*hides, error) {
	h := &hides{scans: map[string]bool{}, instances: map[string]map[string]bool{}}
	token := ""
	for range maxListPages {
		page, err := st.List(ctx, blob.ListOptions{Prefix: layout.CorrectionsPrefix, Token: token})
		if err != nil {
			return nil, err
		}
		for _, key := range page.Keys {
			if _, err := layout.ParseCorrectionKey(key); err != nil {
				return nil, fault.Wrap(fault.Unavailable, errUnreadableRecord, "a key under the correction records is not a record's")
			}
			c, err := readRecord(ctx, st, key, auth)
			if errors.Is(err, record.ErrUnauthenticated) {
				h.unauthenticated++
				continue
			}
			if err != nil {
				return nil, err
			}
			apply := true
			if decide != nil {
				if c, apply, err = decide(c); err != nil {
					return nil, err
				}
			}
			if apply {
				h.add(c)
			}
		}
		if token = page.Next; token == "" {
			return h, nil
		}
	}
	return nil, fault.New(fault.Unavailable, "the listing of the correction records did not end")
}

// readRecord reads, decodes and authenticates one correction record; a
// record that does not verify is an error wrapping
// record.ErrUnauthenticated.
func readRecord(ctx context.Context, st Storage, key string, auth recordAuth) (record.Correction, error) {
	rc, err := st.Read(ctx, key, 0, -1)
	if err != nil {
		return record.Correction{}, err
	}
	defer func() { _ = rc.Close() }()
	c, err := record.DecodeCorrection(rc, auth.tenantID, auth.keys)
	if errors.Is(err, record.ErrUnauthenticated) {
		return record.Correction{}, err
	}
	if err != nil {
		return record.Correction{}, fault.Wrap(fault.Unavailable, errors.Join(errUnreadableRecord, err), "a correction record cannot be read")
	}
	return c, nil
}

// scanKeys is one scan as a listing of meta/ answers it: its labels and the
// first of its keys the listing gave, which orders it.
type scanKeys struct {
	path label.Path
	key  string
}

// listScans calls fn with each scan of the storage whose first key is after
// after, in the listing's order: byte order of keys, in which a scan's two
// files are next to each other, since they share its directory and no
// other key lies between them. The records under meta/ are not a scan's
// and are passed over, as is a key of no scan.
func listScans(ctx context.Context, st Storage, after string, fn func(scanKeys) error) error {
	token := ""
	var cur *scanKeys
	flush := func() error {
		if cur == nil || cur.key <= after {
			return nil
		}
		return fn(*cur)
	}
	for range maxListPages {
		page, err := st.List(ctx, blob.ListOptions{Prefix: layout.MetaRoot, Token: token})
		if err != nil {
			return err
		}
		for _, key := range page.Keys {
			if strings.HasPrefix(key, layout.MetaRoot+".") {
				continue
			}
			k, err := layout.Parse(key)
			if err != nil || (k.Kind != layout.Manifest && k.Kind != layout.Metadata) {
				continue
			}
			if cur != nil && cur.path == k.Path {
				continue
			}
			if err := flush(); err != nil {
				return err
			}
			cur = &scanKeys{path: k.Path, key: key}
		}
		if token = page.Next; token == "" {
			return flush()
		}
	}
	return fault.New(fault.Unavailable, "the listing of meta/ did not end")
}

// scanRead is what a scan's manifest and metadata say, or why the scan is
// left out.
type scanRead struct {
	// scan is its rows; instances its instances, in order of SOP Instance
	// UID.
	scan      tenant.RebuiltScan
	instances []tenant.RebuiltInstance
	// unreadable, when set, says why the scan cannot be cataloged; files
	// and bytes then count what its manifest listed, when it could be
	// read.
	unreadable string
	files      int64
	bytes      int64
}

// readScan reads the scan at path on the storage named storage.
func readScan(ctx context.Context, st Storage, storage string, path label.Path) (scanRead, error) {
	out := scanRead{scan: tenant.RebuiltScan{Storage: storage, Path: path}}
	mk, err := layout.ManifestKey(path)
	if err != nil {
		return out, err
	}
	raw, err := readAll(ctx, st, mk, int64(manifest.MaxBytes))
	if fault.KindOf(err) == fault.NotFound {
		out.unreadable = "no manifest"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	m, err := manifest.Decode(bytes.NewReader(raw))
	if err != nil {
		out.unreadable = "the manifest is not in its form"
		return out, nil
	}
	out.files = int64(len(m.Files))
	files := make(map[string]manifest.File, len(m.Files))
	for _, f := range m.Files {
		files[f.SOPInstanceUID] = f
		out.bytes += f.Length
	}
	out.scan.ManifestSHA256 = sha256.Sum256(raw)
	sk, err := layout.MetadataKey(path)
	if err != nil {
		return out, err
	}
	rc, err := st.Read(ctx, sk, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		out.unreadable = "no metadata"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer func() { _ = rc.Close() }()
	rd := sidecar.NewReader(rc)
	for {
		line, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, sidecar.ErrFormat) {
			out.unreadable = "the metadata is not in its form"
			return out, nil
		}
		if err != nil {
			return out, err
		}
		if rd.Version() != sidecar.Version || rd.Scan() == nil {
			out.unreadable = "the metadata is of a form older than version " + strconv.Itoa(sidecar.Version)
			return out, nil
		}
		in, why := instanceOf(line, files)
		if why != "" {
			out.unreadable = why
			return out, nil
		}
		if len(out.instances) == 0 {
			// The lines are in order of SOP Instance UID: the first is the
			// file the scan's search values are read from.
			out.scan.Search = search.ExtractJSON(line.Dataset, line.SOPInstanceUID)
		}
		out.instances = append(out.instances, in)
	}
	if len(out.instances) != len(files) {
		out.unreadable = "the manifest and the metadata list other instances"
		return out, nil
	}
	h := rd.Scan()
	out.scan.StudyUID, out.scan.SeriesUID = h.StudyUID, h.SeriesUID
	out.scan.SessionReceiver, out.scan.Receiver = h.SessionReceiver, h.Receiver
	out.scan.Route = h.Route.Tenant()
	out.scan.Values = h.Values.Tenant()
	out.scan.Created, out.scan.Updated = h.Created, h.Updated
	return out, nil
}

// instanceOf returns the instance a metadata line and its manifest line
// say, or why they cannot be cataloged: no manifest line, or one that names
// another key, size or checksum.
func instanceOf(line sidecar.Instance, files map[string]manifest.File) (tenant.RebuiltInstance, string) {
	f, ok := files[line.SOPInstanceUID]
	if !ok || f.Key != line.Key || f.Length != line.Size || f.SHA256 != line.SHA256 {
		return tenant.RebuiltInstance{}, "the manifest and the metadata say other things of an instance"
	}
	archived, err := sha256Of(line.SHA256)
	if err != nil {
		return tenant.RebuiltInstance{}, "an instance's checksum is not one"
	}
	received := archived
	if line.ReceivedSHA256 != "" {
		if received, err = sha256Of(line.ReceivedSHA256); err != nil {
			return tenant.RebuiltInstance{}, "an instance's checksum is not one"
		}
	}
	in := tenant.RebuiltInstance{SOPInstanceUID: line.SOPInstanceUID, Key: line.Key, Offset: f.Offset, Length: f.Length, Size: line.Size,
		ReceivedSHA256: received, ArchivedSHA256: archived, Reception: line.Reception, HeldAt: line.HeldAt}
	if line.Profile != "" {
		name, version, _ := strings.Cut(line.Profile, "@")
		n, err := strconv.ParseInt(version, 10, 32)
		if err != nil {
			return tenant.RebuiltInstance{}, "an instance's profile version is not name@version"
		}
		in.Profile = tenant.ProfileRef{Name: name, Version: int32(n)}
		in.PixelRiskAccepted = line.PixelRiskAccepted
	}
	return in, ""
}

// sha256Of parses 64 lowercase hexadecimal digits.
func sha256Of(s string) (tenant.SHA256, error) {
	var out tenant.SHA256
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(out) || hex.EncodeToString(b) != s {
		return out, fmt.Errorf("not a SHA-256")
	}
	copy(out[:], b)
	return out, nil
}

// readAll reads a whole object of at most limit bytes.
func readAll(ctx context.Context, st Storage, key string, limit int64) ([]byte, error) {
	rc, err := st.Read(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fault.New(fault.Unavailable, "an object is larger than its form allows")
	}
	return b, nil
}
