package receive

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/version"
)

// heldRows returns the catalog's rows of the scan's instances.
func heldRows(t *testing.T, e *env, path label.Path) map[string]tenant.ScanInstance {
	t.Helper()
	rows, _, err := e.store.ListHeldInstances(e.ctx, path, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]tenant.ScanInstance{}
	for _, r := range rows {
		out[r.SOPInstanceUID] = r
	}
	return out
}

// sidecarOf returns a scan's sidecar as the storage holds it: its header
// and its lines.
func sidecarOf(t *testing.T, e *env, path label.Path) (*sidecar.ScanHeader, []sidecar.Instance) {
	t.Helper()
	key, _ := layout.MetadataKey(path)
	rd := sidecar.NewReader(bytes.NewReader(readObject(t, e.storage, key)))
	var out []sidecar.Instance
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			if rd.Version() != sidecar.Version {
				t.Fatalf("scan %s: sidecar version %d, want %d", path.Scan, rd.Version(), sidecar.Version)
			}
			return rd.Scan(), out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, in)
	}
}

// checkAgainstCatalog checks that every scan's sidecar holds what the
// catalog holds of the scan, its session and its instances, as the database
// itself answers it, and returns how many lines it checked.
func checkAgainstCatalog(t *testing.T, e *env, paths []label.Path, form string) int {
	t.Helper()
	n := 0
	for _, path := range paths {
		h, lines := sidecarOf(t, e, path)
		sc, err := e.store.CatalogScan(e.ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		se, err := e.store.CatalogSession(e.ctx, path.Subject, path.Session)
		if err != nil {
			t.Fatal(err)
		}
		labels, err := e.store.CatalogLabels(e.ctx, se.StudyUID, []string{sc.SeriesUID})
		if err != nil || labels.Session == nil {
			t.Fatalf("the session's labels: %v", err)
		}
		route := sidecar.RouteOf(labels.Session.Route)
		values := sc.Values
		values.StudyDate = se.StudyDate
		if got := h.Values.Tenant(); got != values {
			t.Errorf("scan %s: header values %+v, the catalog holds %+v", path.Scan, got, values)
		}
		if h.StudyUID != se.StudyUID || h.SeriesUID != sc.SeriesUID || h.SessionReceiver != se.Receiver || h.Receiver != sc.Receiver ||
			!reflect.DeepEqual(h.Route, route) || !h.Created.Equal(sc.Created) || !h.Updated.Equal(sc.Updated) {
			t.Errorf("scan %s: header %+v, the catalog holds session %+v and scan %+v", path.Scan, h, se, sc)
		}
		rows := heldRows(t, e, path)
		if len(rows) != len(lines) {
			t.Fatalf("scan %s: %d lines, %d rows", path.Scan, len(lines), len(rows))
		}
		first := lines[0].HeldAt
		for _, in := range lines {
			row, ok := rows[in.SOPInstanceUID]
			if !ok {
				t.Fatalf("scan %s: line %s has no row", path.Scan, in.SOPInstanceUID)
			}
			archived := row.ArchivedSHA256
			if archived.IsZero() {
				archived = row.SHA256
			}
			if in.Reception != row.ReceptionID || !in.HeldAt.Equal(row.Held) || in.SHA256 != archived.String() || in.Key != row.Key {
				t.Errorf("scan %s: line %+v, row %+v", path.Scan, in, row)
			}
			wantReceived := ""
			if archived != row.SHA256 {
				wantReceived = row.SHA256.String()
			}
			if in.ReceivedSHA256 != wantReceived || in.ReceivedForm != form || in.ReceivedSyntax == "" || in.NopacsVersion != sidecar.SafeVersion(version.Get().Version) {
				t.Errorf("scan %s: %s: received %q (want %q), form %q, syntax %q, version %q", path.Scan, in.SOPInstanceUID,
					in.ReceivedSHA256, wantReceived, in.ReceivedForm, in.ReceivedSyntax, in.NopacsVersion)
			}
			if (in.Profile == "") != row.Profile.IsZero() || (in.Profile != "" && in.Profile != row.Profile.String()) {
				t.Errorf("scan %s: profile %q, row %v", path.Scan, in.Profile, row.Profile)
			}
			if in.Profile != "" {
				v, err := e.store.(tenant.Profiles).ProfileVersion(e.ctx, row.Profile.Name, row.Profile.Version)
				if err != nil || in.ProfileSHA256 != v.Checksum {
					t.Errorf("scan %s: profile checksum %q, the version's is %q (%v)", path.Scan, in.ProfileSHA256, v.Checksum, err)
				}
			}
			if in.HeldAt.Before(first) {
				first = in.HeldAt
			}
			n++
		}
		// The scan's created time is the earliest time any of its
		// instances was held, which the reception's one reading of the
		// clock guarantees.
		if !sc.Created.Equal(first) {
			t.Errorf("scan %s: created %s, earliest held %s", path.Scan, sc.Created, first)
		}
	}
	return n
}

// TestArchiveWritesVersion3 guards the sidecar the archive step writes: of
// the current version, with the scan's header and every line's reception,
// time, received form, syntax and build, each equal to what the tenant's
// database holds of the same scan, session and instance, and a scan's
// created time equal to the earliest time one of its lines was held.
func TestArchiveWritesVersion3(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send(framedFiles...)
	rs := e.settle(3)
	for _, r := range rs {
		if !done(r) {
			t.Fatalf("reception %+v", r)
		}
	}
	paths := scanPaths(t, e, rs)
	if n := checkAgainstCatalog(t, e, paths, sidecar.FormDIMSE); n != len(framedFiles) {
		t.Fatalf("%d lines checked, want %d", n, len(framedFiles))
	}
}

// TestDeidentifyingArchiveWritesVersion3 guards the same for a receiver that
// de-identifies: the received checksum is the catalog's keyed one, which
// differs from the archived one, and each line names its profile version and
// the checksum of that version's text.
func TestDeidentifyingArchiveWritesVersion3(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	e.send(studyFiles...)
	rs := e.settle(1)
	if !done(rs[0]) {
		t.Fatalf("reception %+v", rs[0])
	}
	paths := scanPaths(t, e, rs)
	if n := checkAgainstCatalog(t, e, paths, sidecar.FormDIMSE); n != len(studyFiles) {
		t.Fatalf("%d lines checked, want %d", n, len(studyFiles))
	}
	_, lines := sidecarOf(t, e, paths[0])
	if lines[0].ReceivedSHA256 == "" || lines[0].Profile != testProfile.String() || lines[0].ProfileSHA256 == "" {
		t.Errorf("a de-identified line: %+v", lines[0])
	}
}

// TestLaterReceptionKeepsTheScanHeader guards that a second reception into a
// scan whose sidecar is of the current version keeps its created time, its
// receivers and its routing, and moves its updated time to the commit of the
// later reception, equal to the catalog's.
func TestLaterReceptionKeepsTheScanHeader(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send(studyFiles[2:4]...)
	rs := e.settle(1)
	paths := scanPaths(t, e, rs)
	before, _ := sidecarOf(t, e, paths[0])
	e.clock.Advance(time.Hour)
	e.send(studyFiles[4:]...)
	e.waitFor("the later reception to end", 3*time.Second, func() bool {
		sc, err := e.store.CatalogScan(e.ctx, paths[0])
		return err == nil && sc.Instances == int64(len(studyFiles[2:]))
	})
	after, lines := sidecarOf(t, e, paths[0])
	if !after.Created.Equal(before.Created) || !after.Updated.After(before.Updated) || after.Receiver != before.Receiver || len(lines) != len(studyFiles[2:]) {
		t.Errorf("header before %+v, after %+v, %d lines", before, after, len(lines))
	}
	checkAgainstCatalog(t, e, paths, sidecar.FormDIMSE)
}

// downgrade rewrites a scan's sidecar as a version 2 sidecar holds it: no
// header, and lines without the fields only version 3 has.
func downgrade(t *testing.T, e *env, path label.Path) {
	t.Helper()
	_, lines := sidecarOf(t, e, path)
	var b bytes.Buffer
	w := sidecar.NewWriter(&b)
	for _, in := range lines {
		if err := w.Add(withoutV3(in)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	key, _ := layout.MetadataKey(path)
	if _, err := e.storage.Put(e.ctx, key, bytes.NewReader(b.Bytes())); err != nil {
		t.Fatal(err)
	}
}

// TestStartRewritesVersion2FromTheCatalog guards the pass a start runs: an
// archive whose sidecars are of version 2 has each rewritten to version 3
// with a header and lines equal to those the archive step writes, but for
// what only the reception knew (how the file arrived, the build, and the
// syntax a de-identified file arrived in), and a second start leaves every
// byte as it is.
func TestStartRewritesVersion2FromTheCatalog(t *testing.T) {
	for name, opts := range map[string]envOptions{"as received": {}, "de-identified": {deid: &deidOptions{}}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, opts)
			e.send(studyFiles...)
			rs := e.settle(1)
			paths := scanPaths(t, e, rs)
			if err := e.p.Stop(e.ctx); err != nil {
				t.Fatal(err)
			}
			wantHeader := map[string]*sidecar.ScanHeader{}
			wantLines := map[string][]sidecar.Instance{}
			for _, path := range paths {
				wantHeader[path.Scan], wantLines[path.Scan] = sidecarOf(t, e, path)
				downgrade(t, e, path)
			}
			e.start()
			waitV3 := func() {
				e.waitFor("the rewrite to version 3", 10*time.Millisecond, func() bool {
					for _, path := range paths {
						key, _ := layout.MetadataKey(path)
						head := readObject(t, e.storage, key)[:sidecar.HeadLength]
						if sidecar.VersionOf(head) != sidecar.Version {
							return false
						}
					}
					return true
				})
			}
			waitV3()
			after := map[string][]byte{}
			for _, path := range paths {
				h, lines := sidecarOf(t, e, path)
				if !reflect.DeepEqual(h, wantHeader[path.Scan]) {
					t.Errorf("scan %s: header %+v, the archive step wrote %+v", path.Scan, h, wantHeader[path.Scan])
				}
				if len(lines) != len(wantLines[path.Scan]) {
					t.Fatalf("scan %s: %d lines, want %d", path.Scan, len(lines), len(wantLines[path.Scan]))
				}
				for i, in := range lines {
					want := wantLines[path.Scan][i]
					want.ReceivedForm, want.NopacsVersion = "", ""
					// A version 2 sidecar never recorded the elements no rule named,
					// and the catalog the rewrite reads holds none.
					want.Unnamed, want.UnnamedOver = nil, 0
					if opts.deid != nil {
						want.ReceivedSyntax = ""
					}
					if !reflect.DeepEqual(in, want) {
						t.Errorf("scan %s: line %d is\n%+v\nwant\n%+v", path.Scan, i, in, want)
					}
				}
				key, _ := layout.MetadataKey(path)
				after[key] = readObject(t, e.storage, key)
			}
			if err := e.p.Stop(e.ctx); err != nil {
				t.Fatal(err)
			}
			e.start()
			e.clock.Advance(time.Minute)
			time.Sleep(200 * time.Millisecond)
			for key, b := range after {
				if !bytes.Equal(readObject(t, e.storage, key), b) {
					t.Errorf("%s changed at the next start", key)
				}
			}
		})
	}
}
