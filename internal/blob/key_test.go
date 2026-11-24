package blob_test

import (
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/blobtest"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestCheckKeyAccepts guards that the keys the archive's layout makes, and
// others that are safe on every storage, pass: a check that refused them
// would stop the archive from storing anything.
func TestCheckKeyAccepts(t *testing.T) {
	for _, key := range []string{
		"a",
		"data/S001/20260101-1.2.3/1-1.2.3.4/1.2.840.10008.5.1.4.1.1.2.dcm",
		"meta/S001/sess/scan/manifest.json",
		".hidden",
		"a..b",
		"con-not-a-device",
		"COM10",
		"LPT",
		"Az09._-",
		"1.2.840.10008.1.2.4.50",
		"blob-tmp-not-reserved",
		"x.blob-",
		strings.Repeat("a", blob.MaxSegmentLength),
		// A key of exactly the longest length, in long segments.
		strings.Repeat(strings.Repeat("a", 254)+"/", 3) + strings.Repeat("b", blob.MaxKeyLength-3*255),
		strings.Repeat("a/", blob.MaxKeySegments-1) + "a",
	} {
		if len(key) > blob.MaxKeyLength {
			t.Fatalf("test key is %d bytes", len(key))
		}
		if err := blob.CheckKey(key); err != nil {
			t.Errorf("blob.CheckKey(%q) = %v, want nil", key, err)
		}
	}
}

// TestCheckKeyRefuses guards every rule of blob.CheckKey with a key that breaks
// it alone: each must be refused as Invalid, and the message must not echo
// the key, which may hold an archive label.
func TestCheckKeyRefuses(t *testing.T) {
	for _, key := range blobtest.BadKeys() {
		err := blob.CheckKey(key)
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("blob.CheckKey(%q) = %v, want Invalid", key, err)
			continue
		}
		if len(key) > 3 && key != blob.ReservedPrefix && strings.Contains(fault.Message(err), key) {
			t.Errorf("blob.CheckKey(%q): the message echoes the key: %v", key, err)
		}
	}
}

// TestCheckPrefix guards that a prefix may stop inside a segment, may be
// empty or end in "/", and is otherwise held to the key rules.
func TestCheckPrefix(t *testing.T) {
	for _, p := range []string{"", "a", "a/", "a/b", "a/b/", "data/S0", ".", "a/.", "a/..", "a/b.", "a/con"} {
		if err := blob.CheckPrefix(p); err != nil {
			t.Errorf("blob.CheckPrefix(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"/", "/a", "a//", "a//b", "../a", "./", "a/../b/", "a\\b", "a/b:c", "a\x00", "\xff", "con/x", ".blob-tmp/x", strings.Repeat("a", blob.MaxKeyLength+1)} {
		if err := blob.CheckPrefix(p); fault.KindOf(err) != fault.Invalid {
			t.Errorf("blob.CheckPrefix(%q) = %v, want Invalid", p, err)
		}
	}
}

// TestCheckListOptions guards the delimiter and limit bounds a driver can
// rely on once blob.CheckListOptions has passed, and the default page size.
func TestCheckListOptions(t *testing.T) {
	for _, o := range []blob.ListOptions{{}, {Delimiter: "/"}, {Limit: 1}, {Limit: blob.MaxListLimit}} {
		if err := blob.CheckListOptions(o); err != nil {
			t.Errorf("blob.CheckListOptions(%+v) = %v, want nil", o, err)
		}
	}
	for _, o := range []blob.ListOptions{{Delimiter: "|"}, {Delimiter: "//"}, {Limit: -1}, {Limit: blob.MaxListLimit + 1}, {Prefix: "/"}} {
		if err := blob.CheckListOptions(o); fault.KindOf(err) != fault.Invalid {
			t.Errorf("blob.CheckListOptions(%+v) = %v, want Invalid", o, err)
		}
	}
	if blob.ListLimit(blob.ListOptions{}) != blob.MaxListLimit || blob.ListLimit(blob.ListOptions{Limit: 7}) != 7 {
		t.Error("blob.ListLimit does not default 0 to blob.MaxListLimit")
	}
}

// TestCheckRange guards the range bounds known before an object's size.
func TestCheckRange(t *testing.T) {
	for _, r := range [][2]int64{{0, 0}, {0, -1}, {5, 10}} {
		if err := blob.CheckRange(r[0], r[1]); err != nil {
			t.Errorf("blob.CheckRange(%d, %d) = %v, want nil", r[0], r[1], err)
		}
	}
	for _, r := range [][2]int64{{-1, 0}, {0, -2}} {
		if err := blob.CheckRange(r[0], r[1]); fault.KindOf(err) != fault.Invalid {
			t.Errorf("blob.CheckRange(%d, %d) = %v, want Invalid", r[0], r[1], err)
		}
	}
}
