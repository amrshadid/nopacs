package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/receive/header"
)

// explicitLE is data elements written in Explicit VR Little Endian (PS3.5
// 7.1.2): a tag, its VR, a 2-byte length for the short VRs or 2 reserved
// bytes and a 4-byte length for OB, SQ, UT and UN.
type explicitLE []byte

// el appends one element.
func (w *explicitLE) el(group, elem uint16, vr string, value []byte) {
	b := binary.LittleEndian.AppendUint16(*w, group)
	b = binary.LittleEndian.AppendUint16(b, elem)
	b = append(b, vr...)
	switch vr {
	case "OB", "SQ", "UT", "UN":
		b = append(b, 0, 0)
		b = binary.LittleEndian.AppendUint32(b, length32(value))
	default:
		b = binary.LittleEndian.AppendUint16(b, uint16(length32(value))) // #nosec G115 -- the test's short values.
	}
	*w = append(b, value...)
}

// length32 is len(b) as the 4-byte length a test's small value takes.
func length32(b []byte) uint32 {
	if len(b) > 1<<16 {
		panic("a test value longer than 64 KiB")
	}
	return uint32(len(b)) // #nosec G115 -- bounded above.
}

// even pads s to an even length with pad (PS3.5 7.1.1).
func even(s string, pad byte) []byte {
	if len(s)%2 == 1 {
		return append([]byte(s), pad)
	}
	return []byte(s)
}

// charsetItemFile returns a synthetic CT file whose top level declares
// ISO_IR 100 (Latin-1) and whose Issuer of Patient ID Qualifiers Sequence
// item declares ISO_IR 192 (UTF-8) of its own, its Universal Entity ID
// "Hôpital" written in UTF-8: read under the top level's character set it
// would be "HÃ´pital".
func charsetItemFile() []byte {
	var item explicitLE
	item.el(0x0008, 0x0005, "CS", even("ISO_IR 192", ' '))
	item.el(0x0040, 0x0032, "UT", even("Hôpital", ' '))
	items := binary.LittleEndian.AppendUint16(nil, 0xFFFE)
	items = binary.LittleEndian.AppendUint16(items, 0xE000)
	items = binary.LittleEndian.AppendUint32(items, length32(item))
	items = append(items, item...)

	var ds explicitLE
	ds.el(0x0008, 0x0005, "CS", even("ISO_IR 100", ' '))
	ds.el(0x0008, 0x0016, "UI", even("1.2.840.10008.5.1.4.1.1.2", 0))
	ds.el(0x0008, 0x0018, "UI", even("2.25.1001", 0))
	ds.el(0x0008, 0x0020, "DA", even("20200610", ' '))
	ds.el(0x0010, 0x0020, "LO", even("PAT001", ' '))
	ds.el(0x0010, 0x0024, "SQ", items)
	ds.el(0x0020, 0x000D, "UI", even("2.25.1002", 0))
	ds.el(0x0020, 0x000E, "UI", even("2.25.1003", 0))

	var meta explicitLE
	meta.el(0x0002, 0x0001, "OB", []byte{0, 1})
	meta.el(0x0002, 0x0002, "UI", even("1.2.840.10008.5.1.4.1.1.2", 0))
	meta.el(0x0002, 0x0003, "UI", even("2.25.1001", 0))
	meta.el(0x0002, 0x0010, "UI", even("1.2.840.10008.1.2.1", 0))
	file := explicitLE(append(make([]byte, 128), "DICM"...))
	file.el(0x0002, 0x0000, "UL", binary.LittleEndian.AppendUint32(nil, length32(meta)))
	file = append(file, meta...)
	return append(file, ds...)
}

// TestIdentityReadsAnItemUnderItsOwnCharacterSet guards that the file's
// header, which the subject label is made from, and the data set the
// engine reads give one subject identity when an item of the qualifiers
// sequence declares a character set of its own (PS3.5 6.1.2.3): both read
// "Hôpital", the keys are equal, and a file applied with the key read from
// its header shifts as one applied with the identity the engine reads
// itself.
func TestIdentityReadsAnItemUnderItsOwnCharacterSet(t *testing.T) {
	b := charsetItemFile()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	fromHeader, err := naming.ReadSubjectIdentity(h, "")
	if err != nil {
		t.Fatal(err)
	}
	fromData, err := naming.ReadSubjectIdentity(datasetAttributes{readFile(t, b)}, "")
	if err != nil {
		t.Fatal(err)
	}
	if fromHeader.Qualifiers[0] != "Hôpital" || fromData.Qualifiers[0] != "Hôpital" {
		t.Errorf("the item's Universal Entity ID: header %q, data set %q; want Hôpital in both", fromHeader.Qualifiers[0], fromData.Qualifiers[0])
	}
	if !bytes.Equal(fromHeader.Key(), fromData.Key()) {
		t.Fatal("the header and the data set read two subject identities of one file")
	}
	en := newTestEngine(t, testKeys, Options{}, rule("dates", vrSel("DA"), aShift))
	given, self := readFile(t, b), readFile(t, b)
	if _, err := en.Apply(context.Background(), given, Reception{IdentityGiven: true, Identity: string(fromHeader.Key())}); err != nil {
		t.Fatal(err)
	}
	if _, err := en.Apply(context.Background(), self, Reception{}); err != nil {
		t.Fatal(err)
	}
	if a, b := mustText(t, given, tStudyDate), mustText(t, self, tStudyDate); a != b {
		t.Errorf("the key given and the identity the engine read shift apart: %s and %s", a, b)
	}
}

// TestUnreadableIdentityFailsClosed guards that no action keyed by the
// subject identity falls back to the Patient ID alone when the identity
// cannot be read: given as unreadable (the caller's header read none), a
// date shift, a hash and a lookup of the Patient ID, and a template of it,
// are each refused; and a Patient ID holding a NUL, read by the engine
// itself, is refused the same way.
func TestUnreadableIdentityFailsClosed(t *testing.T) {
	unreadable := Reception{IdentityGiven: true}
	for _, c := range []struct {
		what  string
		rule  *Engine
		cause Cause
	}{
		{"shift", newTestEngine(t, testKeys, Options{}, rule("dates", vrSel("DA"), aShift)), CauseNoSubject},
		{"hash", newTestEngine(t, testKeys, Options{}, rule("pid", kw("PatientID"), aHash)), CauseNoSubject},
		{"lookup", newTestEngine(t, testKeys, Options{Lookups: &mapLookups{codes: map[string]string{}}},
			rule("pid", kw("PatientID"), aLookup("subjects", true))), CauseNoSubject},
		{"template", newTestEngine(t, testKeys, Options{}, rule("pid", kw("PatientID"), aTemplate("S{#PatientID}"))), CauseBadValue},
	} {
		ds := patient(t, "HOSP_A")
		_, err := c.rule.Apply(context.Background(), ds, unreadable)
		if f := (*Failure)(nil); !errors.As(err, &f) || f.Cause != c.cause {
			t.Errorf("%s with no identity: %v, want %s", c.what, err, c.cause)
		}
		nul := build(t, el{tPatientID, "LO", "\x00\x01PAT"}, el{tStudyDate, "DA", "20200610"})
		_, err = c.rule.Apply(context.Background(), nul, Reception{})
		if f := (*Failure)(nil); !errors.As(err, &f) || f.Cause != c.cause {
			t.Errorf("%s of a Patient ID holding a NUL: %v, want %s", c.what, err, c.cause)
		}
		huge := patient(t, strings.Repeat("H", 2<<20))
		_, err = c.rule.Apply(context.Background(), huge, Reception{})
		if f := (*Failure)(nil); !errors.As(err, &f) || f.Cause != c.cause {
			t.Errorf("%s of a Patient ID whose issuer is too long to read: %v, want %s", c.what, err, c.cause)
		}
	}
}
