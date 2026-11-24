package auditchain

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

// knownEntry is the entry whose encoding knownEntryHex pins.
var knownEntry = Entry{
	Seq:           7,
	ID:            "a1b2",
	Time:          time.UnixMicro(1795000000123456).UTC(),
	KeyID:         "key1",
	RemoteAddress: "192.0.2.10:5000",
	Method:        "/nopacs.v1.TenantService/CreateTenant",
	TenantName:    "lab1",
	Object:        "tenant",
	Outcome:       "done",
	Details:       map[string]string{"role": "admin", "Zeta": "z", "ünï": "ç"},
	RequestID:     "req-9",
}

// knownEntryHex and knownNextHex are knownEntry's encoding and the chain's
// value after it from the zero digest, computed by the encoder in
// scripts/check/auditchain.py (struct and hashlib), not by this package.
const (
	knownEntryHex = "000000156e6f706163732f61756469742d656e7472792f76310000000800000000000000070000000461316232000000080006608b01611240000000046b657931000000000000000f3139322e302e322e31303a35303030000000252f6e6f706163732e76312e54656e616e74536572766963652f43726561746554656e616e74000000046c6162310000000674656e616e7400000004646f6e650000000000000003000000045a657461000000017a00000004726f6c650000000561646d696e00000005c3bc6ec3af00000002c3a7000000057265712d39"
	knownNextHex  = "11d411403f6dc8a2e2db085fc93f2729a04b6886c17f0f1f4d067022b7c61772"
)

// TestEncodeKnownAnswer guards the entry encoding against drift: the bytes
// equal those an independent implementation made from the documented
// layout, and so does the chain's value after the entry. A change in field
// order, width, the name, or the order of details fails here, and would
// otherwise make every sealed log fail to verify.
func TestEncodeKnownAnswer(t *testing.T) {
	enc, err := Encode(knownEntry)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(enc); got != knownEntryHex {
		t.Fatalf("Encode = %s\nwant     %s", got, knownEntryHex)
	}
	next, err := Next(Digest{}, knownEntry)
	if err != nil {
		t.Fatal(err)
	}
	if next.String() != knownNextHex {
		t.Fatalf("Next = %s, want %s", next, knownNextHex)
	}
}

// TestEncodeIsCanonical guards that one entry has one encoding: the details
// map, which Go iterates in a random order, is always written in byte order
// of its names (an upper-case letter before a lower-case one, ASCII before
// any other UTF-8), and a time is taken in microseconds, whatever its zone
// and whatever it holds below a microsecond.
func TestEncodeIsCanonical(t *testing.T) {
	want, err := Encode(knownEntry)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		e := knownEntry
		e.Details = map[string]string{"ünï": "ç", "role": "admin", "Zeta": "z"}
		got, err := Encode(e)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("the same entry encoded two ways")
		}
	}
	e := knownEntry
	e.Time = knownEntry.Time.Add(999 * time.Nanosecond).In(time.FixedZone("x", 5*3600))
	if got, _ := Encode(e); !bytes.Equal(got, want) {
		t.Error("a time in another zone or with nanoseconds encodes differently")
	}
	e.Time = time.Date(1969, 12, 31, 23, 59, 59, 0, time.UTC)
	if _, err := Encode(e); err != nil {
		t.Errorf("a time before 1970 is refused: %v", err)
	}
}

// TestEncodeFieldsDoNotShift guards the length prefixes: moving bytes from
// one field to its neighbor, or from a detail's name to its value, gives
// another encoding, so no two entries share one.
func TestEncodeFieldsDoNotShift(t *testing.T) {
	pairs := []struct {
		name string
		a, b func(*Entry)
	}{
		{"method and tenant",
			func(e *Entry) { e.Method, e.TenantName = "ab", "c" },
			func(e *Entry) { e.Method, e.TenantName = "a", "bc" }},
		{"detail name and value",
			func(e *Entry) { e.Details = map[string]string{"ab": "c"} },
			func(e *Entry) { e.Details = map[string]string{"a": "bc"} }},
		{"one detail and two",
			func(e *Entry) { e.Details = map[string]string{"a": "b"} },
			func(e *Entry) { e.Details = map[string]string{"a": "b", "": ""} }},
		{"no details and empty details",
			func(e *Entry) { e.Details = nil },
			func(e *Entry) { e.Details = map[string]string{} }},
	}
	for _, p := range pairs {
		a, b := knownEntry, knownEntry
		p.a(&a)
		p.b(&b)
		ea, err := Encode(a)
		if err != nil {
			t.Fatal(err)
		}
		eb, err := Encode(b)
		if err != nil {
			t.Fatal(err)
		}
		same := bytes.Equal(ea, eb)
		if p.name == "no details and empty details" {
			if !same {
				t.Errorf("%s: a nil and an empty map encode differently", p.name)
			}
			continue
		}
		if same {
			t.Errorf("%s: two entries share one encoding", p.name)
		}
	}
}

// TestEncodeRefuses guards the refusals: seq 0, which no sealed entry has,
// and text that is not valid UTF-8 in any field or detail, which an export
// in JSON could not carry as it was hashed.
func TestEncodeRefuses(t *testing.T) {
	e := knownEntry
	e.Seq = 0
	if _, err := Encode(e); err == nil {
		t.Error("seq 0 was encoded")
	}
	bad := string([]byte{0xff, 'a'})
	for name, set := range map[string]func(*Entry){
		"id":           func(e *Entry) { e.ID = bad },
		"key id":       func(e *Entry) { e.KeyID = bad },
		"object":       func(e *Entry) { e.Object = bad },
		"reason":       func(e *Entry) { e.Reason = bad },
		"request id":   func(e *Entry) { e.RequestID = bad },
		"detail name":  func(e *Entry) { e.Details = map[string]string{bad: "v"} },
		"detail value": func(e *Entry) { e.Details = map[string]string{"n": bad} },
	} {
		e := knownEntry
		set(&e)
		if _, err := Encode(e); err == nil {
			t.Errorf("%s: invalid UTF-8 was encoded", name)
		}
		if _, err := Next(Digest{}, e); err == nil {
			t.Errorf("%s: invalid UTF-8 was folded into the chain", name)
		}
	}
}

// TestEveryFieldIsInTheChain guards that a change to any one field of an
// entry changes the chain's value there and at every later position.
func TestEveryFieldIsInTheChain(t *testing.T) {
	entries := testEntries(1, 5)
	heads := func(es []Entry) []Digest {
		var h Digest
		out := make([]Digest, len(es))
		for i, e := range es {
			var err error
			if h, err = Next(h, e); err != nil {
				t.Fatal(err)
			}
			out[i] = h
		}
		return out
	}
	want := heads(entries)
	changes := map[string]func(*Entry){
		"id":             func(e *Entry) { e.ID += "x" },
		"time":           func(e *Entry) { e.Time = e.Time.Add(time.Microsecond) },
		"key id":         func(e *Entry) { e.KeyID += "x" },
		"local user":     func(e *Entry) { e.LocalUser = "root" },
		"remote address": func(e *Entry) { e.RemoteAddress += "x" },
		"method":         func(e *Entry) { e.Method += "x" },
		"tenant name":    func(e *Entry) { e.TenantName += "x" },
		"object":         func(e *Entry) { e.Object += "x" },
		"outcome":        func(e *Entry) { e.Outcome = "failed" },
		"reason":         func(e *Entry) { e.Reason += "x" },
		"detail added":   func(e *Entry) { e.Details = map[string]string{"role": "admin", "x": "y"} },
		"request id":     func(e *Entry) { e.RequestID += "x" },
	}
	for name, change := range changes {
		changed := append([]Entry(nil), entries...)
		change(&changed[1])
		got := heads(changed)
		if got[0] != want[0] {
			t.Errorf("%s: a change at seq 2 changed the head at seq 1", name)
		}
		for i := 1; i < len(got); i++ {
			if got[i] == want[i] {
				t.Errorf("%s: a change at seq 2 left the head at seq %d as it was", name, i+1)
			}
		}
	}
}

// TestParseDigest guards that a digest read back from its text is the same,
// and that text of another length or not hex is refused.
func TestParseDigest(t *testing.T) {
	d, err := Next(Digest{}, knownEntry)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseDigest(d.String())
	if err != nil || back != d {
		t.Fatalf("ParseDigest(String) = %s, %v", back, err)
	}
	for _, bad := range []string{"", d.String()[:62], d.String() + "00", "zz" + d.String()[2:]} {
		if _, err := ParseDigest(bad); err == nil {
			t.Errorf("ParseDigest(%q) took it", bad)
		}
	}
}
