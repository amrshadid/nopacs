package frames

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// The scans of the tests' archive.
var (
	scanA = label.Path{Subject: "S1", Session: "E1", Scan: "A"}
	scanB = label.Path{Subject: "S1", Session: "E1", Scan: "B"}
	scanC = label.Path{Subject: "S2", Session: "E1", Scan: "C"}
)

// The files of scan A: RLE of two frames, JPEG of three frames of three
// fragments with an empty Basic Offset Table, JPEG 2000, and native.
var filesA = []file{
	{name: "SC_rgb_rle_2frame.dcm", uid: "1.2.3.1"},
	{name: "jpeg_3frame_nobot.dcm", uid: "1.2.3.2"},
	{name: "MR_small_jp2klossless.dcm", uid: "1.2.3.3"},
	{name: "MR_small.dcm", uid: "1.2.3.4"},
}

// TestPassGivesOldScansTheirTables guards the pass over an archive made
// before frame tables: each scan whose sidecar is version 1 becomes version
// 2 with every line's table equal to the one read from its file itself
// (several frames, several fragments a frame, native), a file missing from
// the storage gets an empty table, a scan already of this version is left
// byte for byte, every rewrite is made under the scan's lock, and the
// progress is one event a scan rewritten and one at the end.
func TestPassGivesOldScansTheirTables(t *testing.T) {
	a := newArchive(t)
	filesC := []file{{name: "MR_small_RLE.dcm", uid: "1.2.5.1"}, {name: "MR_small.dcm", uid: "1.2.5.2", missing: true}}
	a.scan(scanA, true, filesA...)
	a.scan(scanB, false, file{name: "MR_small.dcm", uid: "1.2.4.1"})
	a.scan(scanC, true, filesC...)
	before, _, _ := a.sidecarOf(scanB)
	rep, err := Run(a.ctx, a.config())
	if err != nil {
		t.Fatal(err)
	}
	if rep != (Report{ScansChecked: 3, ScansDone: 2, InstancesDone: 6}) {
		t.Errorf("report %+v", rep)
	}
	a.wantTables(scanA, filesA...)
	a.wantTables(scanC, filesC...)
	if after, _, _ := a.sidecarOf(scanB); !bytes.Equal(before, after) {
		t.Errorf("a scan of this version was rewritten")
	}
	if a.store.unlocked != 0 {
		t.Errorf("%d sidecars were written without the scan's lock", a.store.unlocked)
	}
	ev := a.feed.events
	if len(ev) != 3 || ev[0].ScanLabel != "A" || ev[0].Instances != 4 || ev[1].ScanLabel != "C" || ev[1].ScansDone != 2 ||
		!ev[2].Finished || ev[2].InstancesDone != 6 {
		t.Errorf("events %+v", ev)
	}
	_, _, lines := a.sidecarOf(scanA)
	if len(lines[1].Frames) != 3 || len(lines[1].Frames[0]) != 3 {
		t.Errorf("the JPEG of three fragments a frame has the table %+v", lines[1].Frames)
	}
}

// TestPassRunAgainChangesNothing guards that a pass is idempotent: a
// second pass rewrites nothing, writes no event, and reads only the first
// bytes of each scan's sidecar, never an instance file.
func TestPassRunAgainChangesNothing(t *testing.T) {
	a := newArchive(t)
	a.scan(scanA, true, filesA...)
	a.scan(scanC, true, file{name: "MR_small_RLE.dcm", uid: "1.2.5.1"})
	if _, err := Run(a.ctx, a.config()); err != nil {
		t.Fatal(err)
	}
	firstA, _, _ := a.sidecarOf(scanA)
	firstC, _, _ := a.sidecarOf(scanC)
	events := len(a.feed.events)
	a.store.reads, a.store.puts = map[string][]int64{}, map[string]int{}
	rep, err := Run(a.ctx, a.config())
	if err != nil {
		t.Fatal(err)
	}
	if rep != (Report{ScansChecked: 2}) || len(a.feed.events) != events || len(a.store.puts) != 0 {
		t.Fatalf("a second pass: %+v, %d events, puts %v", rep, len(a.feed.events)-events, a.store.puts)
	}
	for key, reads := range a.store.reads {
		if !strings.HasSuffix(key, layout.MetadataName) || len(reads) != 1 || reads[0] != int64(sidecar.HeadLength) {
			t.Errorf("a second pass read %s: %v", key, reads)
		}
	}
	if len(a.store.reads) != 2 {
		t.Errorf("a second pass read %d objects, want the 2 sidecars' first bytes", len(a.store.reads))
	}
	if b, _, _ := a.sidecarOf(scanA); !bytes.Equal(b, firstA) {
		t.Error("scan A changed")
	}
	if b, _, _ := a.sidecarOf(scanC); !bytes.Equal(b, firstC) {
		t.Error("scan C changed")
	}
}

// TestPassStoppedMidwayResumes guards that a pass stopped in the middle of
// a scan, by the end of its context while reading an instance or by a
// process cut off while its sidecar is written, leaves that sidecar as it
// was (version 1), keeps the scans done before it, and that the next pass
// finishes it without rewriting a scan done.
func TestPassStoppedMidwayResumes(t *testing.T) {
	for _, how := range []string{"while reading an instance", "while writing the sidecar"} {
		t.Run(how, func(t *testing.T) {
			a := newArchive(t)
			filesC := []file{{name: "MR_small_RLE.dcm", uid: "1.2.5.1"}, {name: "SC_rgb_rle_2frame.dcm", uid: "1.2.5.2"}}
			a.scan(scanA, true, filesA...)
			a.scan(scanC, true, filesC...)
			keyA, _ := layout.MetadataKey(scanA)
			keyC, _ := layout.MetadataKey(scanC)
			instC, _ := layout.InstanceKey(scanC, "1.2.5.2")
			ctx, cancel := context.WithCancel(a.ctx)
			defer cancel()
			switch how {
			case "while reading an instance":
				a.store.failRead = func(key string) error {
					if key == instC {
						cancel()
						return context.Canceled
					}
					return nil
				}
			default:
				a.store.failPut = func(key string) bool { return key == keyC }
			}
			oldC, _, _ := a.sidecarOf(scanC)
			rep, err := Run(ctx, a.config())
			if err == nil {
				t.Fatal("the pass stopped midway reported no failure")
			}
			if rep.ScansDone != 1 {
				t.Errorf("the stopped pass did %d scans, want 1", rep.ScansDone)
			}
			if b, version, _ := a.sidecarOf(scanC); version != 1 || !bytes.Equal(b, oldC) {
				t.Fatalf("the scan the pass stopped in is version %d and changed %v", version, !bytes.Equal(b, oldC))
			}
			a.wantTables(scanA, filesA...)
			a.store.failRead, a.store.failPut = nil, nil
			a.store.puts = map[string]int{}
			rep, err = Run(a.ctx, a.config())
			if err != nil {
				t.Fatal(err)
			}
			if rep.ScansDone != 1 || a.store.puts[keyA] != 0 || a.store.puts[keyC] != 1 {
				t.Errorf("the resumed pass: %+v, puts %v", rep, a.store.puts)
			}
			a.wantTables(scanC, filesC...)
		})
	}
}

// TestPassInBatches guards a scan done in several batches: a pass stopped
// after the first batch leaves the sidecar at version 1 with that batch's
// lines given their tables and kept, and the next pass gives the others
// theirs and makes the sidecar version 2.
func TestPassInBatches(t *testing.T) {
	a := newArchive(t)
	a.scan(scanA, true, filesA...)
	key, _ := layout.MetadataKey(scanA)
	cfg := a.config()
	cfg.seams.batchInstances = 1
	puts := 0
	a.store.failPut = func(k string) bool {
		if k == key {
			puts++
		}
		return k == key && puts == 2
	}
	if _, err := Run(a.ctx, cfg); err == nil {
		t.Fatal("the pass whose second rewrite failed reported no failure")
	}
	_, version, lines := a.sidecarOf(scanA)
	if version != 1 || lines[0].Frames == nil || lines[1].Frames != nil {
		t.Fatalf("after one batch: version %d, tables %v and %v", version, lines[0].Frames, lines[1].Frames)
	}
	a.store.failPut = nil
	a.store.puts = map[string]int{}
	rep, err := Run(a.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.InstancesDone != 3 || a.store.puts[key] != 3 {
		t.Errorf("the resumed pass: %+v, %d rewrites, want 3 and 3", rep, a.store.puts[key])
	}
	a.wantTables(scanA, filesA...)
}

// TestPassYieldsToReception guards the pace: while the pipeline is busy,
// the pass waits at least busyFloor after each instance and each scan, so
// reception has the larger share; while it is idle and the work takes no
// time, it does not wait.
func TestPassYieldsToReception(t *testing.T) {
	a := newArchive(t)
	a.scan(scanA, true, filesA...)
	a.busy = true
	if _, err := Run(a.ctx, a.config()); err != nil {
		t.Fatal(err)
	}
	if len(a.clock.waits) != len(filesA)+1 {
		t.Fatalf("%d waits, want one after each of %d instances and one after the scan", len(a.clock.waits), len(filesA))
	}
	for _, d := range a.clock.waits {
		if d < busyFloor {
			t.Errorf("a wait of %v while busy, want at least %v", d, busyFloor)
		}
	}
	b := newArchive(t)
	b.scan(scanA, true, filesA...)
	if _, err := Run(b.ctx, b.config()); err != nil {
		t.Fatal(err)
	}
	if len(b.clock.waits) != 0 {
		t.Errorf("an idle pass whose work took no time waited %v", b.clock.waits)
	}
}

// TestPassLeavesAScanItCannotRead guards that a sidecar not in its form is
// left as it is, for a person to look at, and the pass goes on to the next
// scan; and that a storage failing on a sidecar stops the pass with that
// failure, to be run again.
func TestPassLeavesAScanItCannotRead(t *testing.T) {
	a := newArchive(t)
	a.scan(scanA, true, filesA...)
	a.scan(scanC, true, file{name: "MR_small_RLE.dcm", uid: "1.2.5.1"})
	keyA, _ := layout.MetadataKey(scanA)
	broken := []byte(`{"version":1,"instances":[` + "\n" + `{"sop_instance_uid":"x"}` + "\n]}\n")
	if _, err := a.store.Store.Put(a.ctx, keyA, bytes.NewReader(broken)); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(a.ctx, a.config())
	if err != nil || rep.ScansDone != 1 {
		t.Fatalf("%+v, %v", rep, err)
	}
	rc, err := a.store.Store.Read(a.ctx, keyA, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	var kept bytes.Buffer
	_, _ = kept.ReadFrom(rc)
	_ = rc.Close()
	if !bytes.Equal(kept.Bytes(), broken) {
		t.Error("a sidecar not in its form was rewritten")
	}
	c := newArchive(t)
	c.scan(scanA, true, filesA...)
	failure := errors.New("the storage does not answer")
	c.store.failRead = func(string) error { return failure }
	if _, err := Run(c.ctx, c.config()); !errors.Is(err, failure) {
		t.Errorf("a failing storage gave %v", err)
	}
}

// TestPassChecksumsOnlyTheArchivedFile guards the frames' checksums the
// pass records: they are read from the file on the storage only when it is
// the file its line names, so a file changed there since it was archived
// (its size the same) gets an empty table, which a reader answers as
// frames it cannot read by range, never a checksum of the changed bytes
// that would let them pass as archived.
func TestPassChecksumsOnlyTheArchivedFile(t *testing.T) {
	a := newArchive(t)
	files := []file{{name: "MR_small_RLE.dcm", uid: "1.2.6.1", changed: true}, {name: "MR_small.dcm", uid: "1.2.6.2"}}
	a.scan(scanA, true, files...)
	if _, err := Run(a.ctx, a.config()); err != nil {
		t.Fatal(err)
	}
	a.wantTables(scanA, files...)
}
