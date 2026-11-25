package receive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// framedFiles are corpus files of three studies: a CT study (native), RLE
// of two frames, and JPEG of three frames of three fragments each with an
// empty Basic Offset Table.
var framedFiles = append(append([]string{}, studyFiles...), "SC_rgb_rle_2frame.dcm", "jpeg_3frame_nobot.dcm")

// scanPaths returns the path of every scan the receptions archived into.
func scanPaths(t *testing.T, e *env, rs []tenant.Reception) []label.Path {
	t.Helper()
	var out []label.Path
	for _, r := range rs {
		scans, _, err := e.store.ReceptionScans(e.ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, sc := range scans {
			out = append(out, label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: sc.Label})
		}
	}
	return out
}

// sidecarLines reads a scan's sidecar from the storage: its version and
// its lines.
func sidecarLines(t *testing.T, e *env, path label.Path) (int, []sidecar.Instance) {
	t.Helper()
	key, _ := layout.MetadataKey(path)
	rd := sidecar.NewReader(bytes.NewReader(readObject(t, e.storage, key)))
	var out []sidecar.Instance
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return rd.Version(), out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, in)
	}
}

// checkTables checks that every scan's sidecar is of this version and
// that each line's table is the one read, outside the pipeline, from the
// bytes that were sent, with each frame's checksum that of its bytes.
func checkTables(t *testing.T, e *env, paths []label.Path, sent map[string][]byte) int {
	t.Helper()
	lines := 0
	for _, path := range paths {
		version, ins := sidecarLines(t, e, path)
		if version != sidecar.Version {
			t.Errorf("scan %s: sidecar version %d", path.Scan, version)
		}
		for _, in := range ins {
			b := sent[in.SOPInstanceUID]
			h, err := header.Read(bytes.NewReader(b), int64(len(b)))
			if err != nil {
				t.Fatal(err)
			}
			want, err := frames.Table(h, bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(in.Frames)
			w, _ := json.Marshal(want)
			if len(want) == 0 || string(got) != string(w) {
				t.Errorf("%s: table %s, want %s", in.SOPInstanceUID, got, w)
			}
			if len(in.FrameSHA256) != len(want) {
				t.Errorf("%s: %d frame checksums for %d frames", in.SOPInstanceUID, len(in.FrameSHA256), len(want))
			}
			for i, f := range want {
				h := sha256.New()
				for _, r := range f {
					h.Write(b[r.Offset : r.Offset+r.Length])
				}
				if i < len(in.FrameSHA256) && in.FrameSHA256[i] != hex.EncodeToString(h.Sum(nil)) {
					t.Errorf("%s: frame %d has the checksum %s, want that of its bytes sent", in.SOPInstanceUID, i+1, in.FrameSHA256[i])
				}
			}
			lines++
		}
	}
	return lines
}

// TestArchiveRecordsFrameTables guards the archive step: every instance's
// line of its scan's sidecar holds its frame table, equal to the one read
// from the bytes sent, for native pixel data, RLE of two frames and JPEG
// of three frames in three fragments each, and the sidecar is this
// version.
func TestArchiveRecordsFrameTables(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.send(framedFiles...)
	rs := e.settle(3)
	for _, r := range rs {
		if !done(r) {
			t.Fatalf("reception %+v", r)
		}
	}
	if n := checkTables(t, e, scanPaths(t, e, rs), sentMap(t, framedFiles...)); n != len(framedFiles) {
		t.Fatalf("%d lines checked, want %d", n, len(framedFiles))
	}
}

// TestStartGivesOldSidecarsTheirTables guards the pass a start runs: an
// archive whose sidecars were written before frame tables (version 1, no
// line with a table) has every scan's sidecar rewritten to this version
// with the tables the archive step would have written, and the feed holds
// one frames event a scan and one for the pass's end. Each rewrite, of
// both passes, asks the pipeline's write gate of the scan's storage first
// and releases it.
func TestStartGivesOldSidecarsTheirTables(t *testing.T) {
	pause := &fakePause{}
	e := newEnv(t, envOptions{pause: pause})
	e.send(framedFiles...)
	rs := e.settle(3)
	paths := scanPaths(t, e, rs)
	if err := e.p.Stop(e.ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		_, ins := sidecarLines(t, e, path)
		texts := make([]string, len(ins))
		for i, in := range ins {
			in.Frames, in.FrameSHA256 = nil, nil
			in = withoutV3(in)
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			texts[i] = string(b)
		}
		key, _ := layout.MetadataKey(path)
		old := `{"version":1,"instances":[` + "\n" + strings.Join(texts, ",\n") + "\n]}\n"
		if _, err := e.storage.Put(e.ctx, key, strings.NewReader(old)); err != nil {
			t.Fatal(err)
		}
	}
	feed, ok := e.store.(tenant.Events)
	if !ok {
		t.Fatal("the test's database keeps no feed")
	}
	from, err := feed.EventCursor(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, enteredBefore, _ := pause.counts()
	e.start()
	var got []tenant.Event
	e.waitFor("the pass's end", 10*time.Millisecond, func() bool {
		page, err := feed.ReadEvents(e.ctx, from, []tenant.EventType{tenant.EventFramesAdded}, 100)
		if err != nil {
			t.Fatal(err)
		}
		got = page.Events
		return len(got) > 0 && got[len(got)-1].Payload.(*tenant.FramesAdded).Finished
	})
	if len(got) != len(paths)+1 {
		t.Errorf("%d frames events, want one for each of %d scans and the end", len(got), len(paths))
	}
	// The pass of tables is followed by the rewrite of the sidecars to the
	// form that holds the catalog's values.
	e.waitFor("the rewrite to the current version", 10*time.Millisecond, func() bool {
		for _, path := range paths {
			if v, _ := sidecarLines(t, e, path); v != sidecar.Version {
				return false
			}
		}
		return true
	})
	checkTables(t, e, paths, sentMap(t, framedFiles...))
	e.waitFor("every entry of the gate released", 10*time.Millisecond, func() bool {
		_, entered, released := pause.counts()
		return entered == released
	})
	if _, entered, _ := pause.counts(); entered-enteredBefore < 2*len(paths) {
		t.Errorf("the passes entered the write gate %d times for %d scans; want at least once a scan in each pass", entered-enteredBefore, len(paths))
	}
}

// withoutV3 returns a line without the fields only the current version of
// the sidecar has, as an older version wrote it.
func withoutV3(in sidecar.Instance) sidecar.Instance {
	in.Reception, in.HeldAt, in.ReceivedSHA256, in.ProfileSHA256 = "", time.Time{}, "", ""
	in.ReceivedForm, in.ReceivedSyntax, in.NopacsVersion = "", "", ""
	return in
}
