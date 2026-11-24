package frames

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// heldPage is how many held instances one read of a scan's rows lists.
const heldPage = 1000

// UpgradeCatalog is what the rewrite of older sidecars asks of the tenant's
// database: its scans and sessions, a session's routing, each scan's held
// instances, and the checksums of profile versions.
type UpgradeCatalog interface {
	// ListScans is tenant.ScanReads.ListScans.
	ListScans(ctx context.Context, filter tenant.ScanFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogScan], error)
	// CatalogScan is tenant.ScanReads.CatalogScan.
	CatalogScan(ctx context.Context, path label.Path) (tenant.CatalogScan, error)
	// CatalogSession is tenant.SessionReads.CatalogSession.
	CatalogSession(ctx context.Context, subjectLabel, sessionLabel string) (tenant.CatalogSession, error)
	// CatalogLabels is tenant.Pipeline.CatalogLabels, which answers the
	// routing a session keeps.
	CatalogLabels(ctx context.Context, studyUID string, seriesUIDs []string) (tenant.CatalogLabels, error)
	// ListHeldInstances is tenant.InstanceReads.ListHeldInstances.
	ListHeldInstances(ctx context.Context, path label.Path, page master.Page) ([]tenant.ScanInstance, string, error)
	// ListProfileVersions is tenant.Profiles.ListProfileVersions.
	ListProfileVersions(ctx context.Context, name string, page master.Page) ([]tenant.ProfileVersion, string, error)
}

// UpgradeConfig is what a rewrite is handed. Every port is given; none is
// opened here.
type UpgradeConfig struct {
	// Catalog is the tenant's database.
	Catalog UpgradeCatalog
	// OpenStorage opens one of the tenant's storages by name. The store is
	// the caller's: the pass never closes it.
	OpenStorage func(ctx context.Context, name string) (blob.Store, error)
	// Lock takes the lock the archive step holds while it writes a scan's
	// metadata and manifest on a storage, and returns its release.
	Lock func(storage string, path label.Path) (unlock func())
	// Busy says the pipeline has a step in flight or files arriving; the
	// pass then slows to a fraction of its pace. Nil is never busy.
	Busy func() bool
	// Clock is the pass's clock.
	Clock Clock
	// Logger receives the pass's log lines: labels, counts and fault
	// messages only. Nil discards them.
	Logger *slog.Logger
}

// UpgradeReport is what a rewrite did.
type UpgradeReport struct {
	// ScansChecked counts the scans whose metadata version was read.
	ScansChecked int64
	// ScansDone counts the scans whose metadata was rewritten.
	ScansDone int64
	// InstancesDone counts the lines of those scans.
	InstancesDone int64
	// ScansLeft counts the scans left as they are because the catalog and
	// the sidecar do not agree on them (a line the catalog has no row for, a
	// row with no line, another key, size or checksum), or the sidecar is
	// not in a form this version reads. A scrub reports such a scan; the next
	// pass tries it again.
	ScansLeft int64
	// ScansWaiting counts the scans whose sidecar still lacks frame tables,
	// which the pass of frame tables gives before this one can.
	ScansWaiting int64
}

// errDisagree is a scan whose catalog rows and sidecar lines are not the
// same instances with the same bytes.
var errDisagree = errors.New("the catalog and the sidecar disagree on the scan")

// Upgrade makes one pass over every scan of the tenant, in label order, and
// rewrites each sidecar of version 2 as version 3 from the catalog's rows
// (sidecar.Upgrade): the scan's header, and for each line its reception,
// when it was held, its received checksum, its profile version's checksum,
// and, for a file archived as received, the syntax it arrived in. Version 3
// sidecars are written so by the archive step from the start, and the pass
// reads only the first bytes of each, so a pass that ends leaves nothing to
// do, and a pass run again changes nothing it did.
//
// It returns when every scan was checked, or at the first failure that is
// not a fact of a scan (a storage or the database not answering, ctx
// ending), with what it did so far; a pass run again goes on from the first
// scan not done, each scan being replaced whole or not at all.
func Upgrade(ctx context.Context, cfg UpgradeConfig) (UpgradeReport, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	u := &upgrader{
		runner:   &runner{cfg: Config{OpenStorage: cfg.OpenStorage, Lock: cfg.Lock, Busy: cfg.Busy, Clock: cfg.Clock, Logger: cfg.Logger}, stores: map[string]blob.Store{}},
		cat:      cfg.Catalog,
		profiles: map[string]map[int32]string{},
	}
	cursor := ""
	for {
		page, err := cfg.Catalog.ListScans(ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: scanPage, Cursor: cursor})
		if err != nil {
			return u.rep, err
		}
		for _, sc := range page.Items {
			if err := u.scan(ctx, sc); err != nil {
				return u.rep, err
			}
		}
		if page.Next == "" {
			return u.rep, nil
		}
		cursor = page.Next
	}
}

// upgrader is one rewrite in progress. It borrows the frame pass's runner
// for its pace, its storages and its logger.
type upgrader struct {
	*runner
	cat UpgradeCatalog
	rep UpgradeReport
	// profiles are the checksums of the profile versions read so far, by
	// name and version; a name with no versions is an empty map.
	profiles map[string]map[int32]string
}

// scan checks one scan and, when its sidecar is of version 2, rewrites it.
func (u *upgrader) scan(ctx context.Context, sc tenant.CatalogScan) error {
	path := label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
	store, err := u.storage(ctx, sc.Storage)
	if err != nil {
		return err
	}
	key, err := metadataKey(path)
	if err != nil {
		return err
	}
	at := u.cfg.Clock.Now()
	head, ok, err := readHead(ctx, store, key, sidecar.HeadLength)
	if err != nil {
		return err
	}
	u.rep.ScansChecked++
	if err := u.pause(ctx, u.cfg.Clock.Now().Sub(at)); err != nil {
		return err
	}
	if !ok {
		u.cfg.Logger.Warn("upgrade: a scan has no metadata on its storage", "subject", path.Subject, "session", path.Session, "scan", path.Scan)
		return nil
	}
	switch sidecar.VersionOf(head) {
	case sidecar.Version:
		return nil
	case sidecar.VersionFrames:
	case 1:
		u.rep.ScansWaiting++
		return nil
	default:
		u.rep.ScansLeft++
		u.cfg.Logger.Warn("upgrade: a scan's metadata is not in a form this version reads; it is left as it is",
			"subject", path.Subject, "session", path.Session, "scan", path.Scan)
		return nil
	}
	n, err := u.rewrite(ctx, store, sc.Storage, path, key)
	switch {
	case errors.Is(err, errDisagree) || errors.Is(err, sidecar.ErrFormat) || errors.Is(err, sidecar.ErrOrder) || errors.Is(err, sidecar.ErrIncomplete):
		u.rep.ScansLeft++
		u.cfg.Logger.Warn("upgrade: a scan's metadata is left as it is for the next pass", "subject", path.Subject,
			"session", path.Session, "scan", path.Scan, "reason", err.Error())
		return nil
	case err != nil:
		return err
	case n > 0:
		u.rep.ScansDone++
		u.rep.InstancesDone += n
	}
	return nil
}

// rewrite replaces the scan's sidecar of version 2 by one of version 3, under
// the scan's lock so no reception writes the scan meanwhile and the rows read
// are the ones its lines were written for. It returns how many lines it
// wrote, 0 when the sidecar was not of version 2 under the lock.
func (u *upgrader) rewrite(ctx context.Context, store blob.Store, storage string, path label.Path, key string) (int64, error) {
	unlock := u.cfg.Lock(storage, path)
	defer unlock()
	head, ok, err := readHead(ctx, store, key, sidecar.HeadLength)
	if err != nil {
		return 0, err
	}
	if !ok || sidecar.VersionOf(head) != sidecar.VersionFrames {
		return 0, nil
	}
	sc, err := u.cat.CatalogScan(ctx, path)
	if err != nil {
		return 0, u.catalogError(err)
	}
	scan, err := u.header(ctx, sc, path)
	if err != nil {
		return 0, err
	}
	old, err := store.Read(ctx, key, 0, -1)
	if err != nil {
		return 0, storageError(err, "the scan's metadata cannot be read")
	}
	defer func() { _ = old.Close() }()
	held := &heldRows{cat: u.cat, path: path}
	var n int64
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		got, err := sidecar.Upgrade(pw, old, scan, func(in sidecar.Instance) (sidecar.Instance, error) {
			return u.fill(ctx, held, in)
		})
		if err == nil {
			// A row the sidecar has no line for is a scan the lines do not
			// describe whole; the put is cut off and nothing is replaced.
			var more bool
			if more, err = held.more(ctx); err == nil && more {
				err = fmt.Errorf("%w: the catalog holds an instance the sidecar has no line for", errDisagree)
			}
		}
		n = int64(got)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	_, perr := store.Put(ctx, key, pr)
	_ = pr.CloseWithError(errors.New("the storage stopped reading"))
	if merr := <-done; merr != nil && !errors.Is(merr, io.ErrClosedPipe) {
		return 0, merr
	}
	if perr != nil {
		return 0, storageError(perr, "the scan's metadata cannot be written")
	}
	return n, nil
}

// catalogError keeps the catalog's failure with its kind; a scan or session
// the catalog no longer has is a scan gone since it was listed, left for the
// next pass.
func (u *upgrader) catalogError(err error) error {
	if fault.KindOf(err) == fault.NotFound {
		return fmt.Errorf("%w: the catalog no longer has the scan", errDisagree)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	k := fault.KindOf(err)
	if k == fault.Internal {
		k = fault.Unavailable
	}
	return fault.Wrap(k, err, "the catalog cannot be read")
}

// header returns the scan's header from the catalog: its session's study,
// receiver, routing and study date, its own series, receiver, values and
// times. A receiver the catalog holds none for is written empty, as the
// catalog says, so such a scan is rewritten once like any other.
func (u *upgrader) header(ctx context.Context, sc tenant.CatalogScan, path label.Path) (sidecar.ScanHeader, error) {
	se, err := u.cat.CatalogSession(ctx, path.Subject, path.Session)
	if err != nil {
		return sidecar.ScanHeader{}, u.catalogError(err)
	}
	labels, err := u.cat.CatalogLabels(ctx, se.StudyUID, []string{sc.SeriesUID})
	if err != nil {
		return sidecar.ScanHeader{}, u.catalogError(err)
	}
	values := sc.Values
	values.StudyDate = se.StudyDate
	h := sidecar.ScanHeader{StudyUID: se.StudyUID, SeriesUID: sc.SeriesUID, SessionReceiver: se.Receiver, Receiver: sc.Receiver,
		Values: sidecar.ValuesOf(values), Created: sc.Created.UTC(), Updated: sc.Updated.UTC()}
	if labels.Session != nil {
		h.Route = sidecar.RouteOf(labels.Session.Route)
	}
	if err := h.Validate(); err != nil {
		return sidecar.ScanHeader{}, fmt.Errorf("%w: %w", errDisagree, err)
	}
	return h, nil
}

// fill gives a line of the sidecar what the catalog's row for the same
// instance holds, after checking that they describe the same bytes.
func (u *upgrader) fill(ctx context.Context, held *heldRows, in sidecar.Instance) (sidecar.Instance, error) {
	row, ok, err := held.seek(ctx, in.SOPInstanceUID)
	if err != nil {
		return in, err
	}
	if !ok {
		return in, fmt.Errorf("%w: the sidecar's next line and the catalog's next row are not the same instance", errDisagree)
	}
	archived := row.ArchivedSHA256
	if archived.IsZero() {
		archived = row.SHA256
	}
	if row.Key != in.Key || row.Size != in.Size || archived.String() != in.SHA256 {
		return in, fmt.Errorf("%w: a line and its row name other bytes", errDisagree)
	}
	// A line written before its profile was recorded is given the row's.
	if in.Profile == "" {
		if !row.Profile.IsZero() {
			in.Profile = row.Profile.String()
		}
	} else if in.Profile != row.Profile.String() {
		return in, fmt.Errorf("%w: a line and its row name other profile versions", errDisagree)
	}
	in.Reception, in.HeldAt = row.ReceptionID, row.Held.UTC()
	if row.ReceptionID == "" || row.Held.IsZero() {
		return in, fmt.Errorf("%w: a row without its reception or time", errDisagree)
	}
	if archived != row.SHA256 {
		in.ReceivedSHA256 = row.SHA256.String()
	}
	if row.Profile.IsZero() {
		// An instance archived as received arrived in the syntax it is in.
		in.ReceivedSyntax = in.TransferSyntax
	} else if in.ProfileSHA256, err = u.profileSum(ctx, row.Profile); err != nil {
		return in, err
	}
	return in, nil
}

// profileSum returns the checksum of a profile version's expanded text, or
// "" when the database holds no such version, reading each profile's
// versions once without their texts.
func (u *upgrader) profileSum(ctx context.Context, ref tenant.ProfileRef) (string, error) {
	vs, ok := u.profiles[ref.Name]
	if !ok {
		vs = map[int32]string{}
		cursor := ""
		for {
			page, next, err := u.cat.ListProfileVersions(ctx, ref.Name, master.Page{Limit: 100, Cursor: cursor})
			if fault.KindOf(err) == fault.NotFound {
				break
			}
			if err != nil {
				return "", u.catalogError(err)
			}
			for _, v := range page {
				vs[v.Version] = v.Checksum
			}
			if next == "" {
				break
			}
			cursor = next
		}
		u.profiles[ref.Name] = vs
	}
	return vs[ref.Version], nil
}

// heldRows reads a scan's held instances in order of SOP Instance UID, a
// page at a time, alongside the sidecar's lines in the same order.
type heldRows struct {
	cat    UpgradeCatalog
	path   label.Path
	cursor string
	rows   []tenant.ScanInstance
	pos    int
	done   bool
}

// fetch makes sure the next row is at hand, reading a page when it is not;
// false when the scan has no more.
func (h *heldRows) fetch(ctx context.Context) (bool, error) {
	for h.pos >= len(h.rows) {
		if h.done {
			return false, nil
		}
		rows, next, err := h.cat.ListHeldInstances(ctx, h.path, master.Page{Limit: heldPage, Cursor: h.cursor})
		if err != nil {
			if fault.KindOf(err) == fault.NotFound {
				return false, fmt.Errorf("%w: the catalog no longer has the scan", errDisagree)
			}
			return false, fault.Wrap(fault.Unavailable, err, "the catalog cannot be read")
		}
		h.rows, h.pos, h.cursor, h.done = rows, 0, next, next == ""
	}
	return true, nil
}

// seek returns the row for the instance uid, skipping the rows before it
// (instances the sidecar has no line for: that is a disagreement, found as
// the first row that is not the line's), and false when the next row is
// another instance.
func (h *heldRows) seek(ctx context.Context, uid string) (tenant.ScanInstance, bool, error) {
	ok, err := h.fetch(ctx)
	if err != nil || !ok {
		return tenant.ScanInstance{}, false, err
	}
	row := h.rows[h.pos]
	if row.SOPInstanceUID != uid {
		return tenant.ScanInstance{}, false, nil
	}
	h.pos++
	return row, true, nil
}

// more reports whether the scan has a row not yet matched to a line.
func (h *heldRows) more(ctx context.Context) (bool, error) { return h.fetch(ctx) }
