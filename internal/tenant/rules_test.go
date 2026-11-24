package tenant

import (
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestRefusalKinds guards that each refusal of the port carries the kind a
// caller answers by, and its sentinel only as a case inside that kind: a
// precondition is fault.Precondition (never Conflict), a cursor the feed
// can no longer answer is fault.OutOfRange (never Invalid), a closed
// reception is a precondition that still says it is closed, and a step
// that lost its race is a Conflict.
func TestRefusalKinds(t *testing.T) {
	for _, c := range []struct {
		what     string
		err      error
		kind     fault.Kind
		sentinel error
	}{
		{"Precondition", Precondition("the storage holds data"), fault.Precondition, nil},
		{"CursorExpired", CursorExpired(), fault.OutOfRange, ErrCursorExpired},
		{"ReceptionClosed", ReceptionClosed(), fault.Precondition, ErrReceptionClosed},
		{"StateChanged", StateChanged(), fault.Conflict, ErrStateChanged},
		{"LeaseLost", LeaseLost(), fault.Conflict, ErrLeaseLost},
	} {
		if got := fault.KindOf(c.err); got != c.kind {
			t.Errorf("%s has the kind %v, want %v", c.what, got, c.kind)
		}
		if c.sentinel != nil && !errors.Is(c.err, c.sentinel) {
			t.Errorf("%s does not hold %v", c.what, c.sentinel)
		}
		if fault.Message(c.err) == "internal error" || fault.Message(c.err) == "" {
			t.Errorf("%s has no message for the caller", c.what)
		}
	}
	if fault.Message(Precondition("the storage holds data")) != "the storage holds data" {
		t.Error("Precondition does not answer its own message")
	}
}

// TestNarrowStates guards the one rule of a reception filter's steps: each
// named once, in the order given; with in progress, an ended step keeps
// nothing and is left out, and only ended steps keep no reception at all.
func TestNarrowStates(t *testing.T) {
	for _, c := range []struct {
		what string
		f    ReceptionFilter
		want []reception.State
		none bool
	}{
		{"no steps", ReceptionFilter{}, nil, false},
		{"no steps, in progress", ReceptionFilter{InProgress: true}, nil, false},
		{"steps named twice", ReceptionFilter{States: []reception.State{reception.Done, reception.Receiving, reception.Done}},
			[]reception.State{reception.Done, reception.Receiving}, false},
		{"in progress drops ended steps", ReceptionFilter{InProgress: true, States: []reception.State{reception.Done, reception.Receiving, reception.Failed}},
			[]reception.State{reception.Receiving}, false},
		{"in progress with only ended steps", ReceptionFilter{InProgress: true, States: []reception.State{reception.Done, reception.Failed}}, nil, true},
	} {
		got, none := c.f.NarrowStates()
		if !slices.Equal(got, c.want) || none != c.none {
			t.Errorf("%s: NarrowStates = %v, %v; want %v, %v", c.what, got, none, c.want, c.none)
		}
	}
}

// TestAddCallingAETitle guards a reception's list of calling AE titles:
// each once, in the order first seen, an empty title adding nothing, at
// most MaxCallingAETitles with the truncation said, and the slice given
// never changed, so a change run again starts from what it read.
func TestAddCallingAETitle(t *testing.T) {
	titles, truncated := AddCallingAETitle(nil, false, "A")
	titles, truncated = AddCallingAETitle(titles, truncated, "")
	titles, truncated = AddCallingAETitle(titles, truncated, "A")
	titles, truncated = AddCallingAETitle(titles, truncated, "B")
	if !slices.Equal(titles, []string{"A", "B"}) || truncated {
		t.Fatalf("titles = %v, %v; want [A B], false", titles, truncated)
	}
	base := make([]string, 0, 8)
	base = append(base, "A")
	grown, _ := AddCallingAETitle(base, false, "B")
	other, _ := AddCallingAETitle(base, false, "C")
	if grown[1] != "B" || other[1] != "C" {
		t.Errorf("two additions to one slice share their storage: %v and %v", grown, other)
	}
	var full []string
	for i := range MaxCallingAETitles {
		full = append(full, "AE"+strconv.Itoa(i))
	}
	got, truncated := AddCallingAETitle(full, false, "LATE")
	if len(got) != MaxCallingAETitles || !truncated {
		t.Errorf("a title past the bound: %d titles, truncated %v", len(got), truncated)
	}
	if got, truncated := AddCallingAETitle(full, false, "AE3"); len(got) != MaxCallingAETitles || truncated {
		t.Errorf("a title already in a full list said truncated %v", truncated)
	}
}

// TestSortClaims guards the order every driver claims in: by SOP Instance
// UID, then checksum, byte by byte, whatever order the caller gave, so two
// files under one UID keep the lower checksum on every driver.
func TestSortClaims(t *testing.T) {
	files := []InstanceClaim{
		{SOPInstanceUID: "1.2.10", SHA256: sha(1)},
		{SOPInstanceUID: "1.2.9", SHA256: sha(9)},
		{SOPInstanceUID: "1.2.9", SHA256: sha(2)},
		{SOPInstanceUID: "1.2.1", SHA256: sha(5)},
	}
	if got, want := SortClaims(files), []int{3, 0, 2, 1}; !slices.Equal(got, want) {
		t.Errorf("SortClaims = %v, want %v", got, want)
	}
	if got := SortClaims(nil); len(got) != 0 {
		t.Errorf("SortClaims of none = %v", got)
	}
}

// TestReceivable guards what a released file needs to join a reception:
// every UID valid, a checksum and its place in the incoming area; a file
// missing any of them joins none.
func TestReceivable(t *testing.T) {
	ok := QuarantinedFile{SOPInstanceUID: "1.2.3", SeriesUID: "1.2.4", StudyUID: "1.2.5", SOPClassUID: "1.2.840.10008.5.1.4.1.1.4",
		TransferSyntaxUID: "1.2.840.10008.1.2.1", SHA256: sha(1), Incoming: "a/b.dcm"}
	if !ok.Receivable() {
		t.Fatal("a file with every UID, its checksum and its place is not receivable")
	}
	for what, change := range map[string]func(*QuarantinedFile){
		"no instance UID":     func(q *QuarantinedFile) { q.SOPInstanceUID = "" },
		"a bad study UID":     func(q *QuarantinedFile) { q.StudyUID = "1.02" },
		"no transfer syntax":  func(q *QuarantinedFile) { q.TransferSyntaxUID = "" },
		"no checksum":         func(q *QuarantinedFile) { q.SHA256 = SHA256{} },
		"no incoming place":   func(q *QuarantinedFile) { q.Incoming = "" },
		"no series":           func(q *QuarantinedFile) { q.SeriesUID = "" },
		"no SOP class at all": func(q *QuarantinedFile) { q.SOPClassUID = "" },
	} {
		q := ok
		change(&q)
		if q.Receivable() {
			t.Errorf("a file with %s is receivable", what)
		}
	}
}

// TestNewReasonsKept guards how the two reasons added to the contract keep
// bytes: a header too large to read is kept, with its bound, and a lost
// incoming copy has nothing to keep; each the other way is Invalid.
func TestNewReasonsKept(t *testing.T) {
	base := QuarantinedFile{ID: "q1", Receiver: "a", Quarantined: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	header := base
	header.Reason, header.Details, header.Incoming, header.ContentKept = QuarantineHeaderTooLarge, map[string]string{"limit": "65536"}, "q/q1", true
	lost := base
	lost.Reason = QuarantineIncomingLost
	for _, q := range []QuarantinedFile{header, lost} {
		if err := q.Validate(); err != nil {
			t.Errorf("%s: %v", q.Reason, err)
		}
	}
	unkept := header
	unkept.Incoming, unkept.ContentKept = "", false
	kept := lost
	kept.Incoming, kept.ContentKept = "q/q1", true
	withText := header
	withText.Details = map[string]string{"limit": "DOE^JOHN"}
	for what, q := range map[string]QuarantinedFile{"a header too large not kept": unkept, "a lost copy kept": kept, "a limit that is not a number": withText} {
		if fault.KindOf(q.Validate()) != fault.Invalid {
			t.Errorf("%s is not Invalid", what)
		}
	}
}

// TestReleaseValidate guards a release's reason: optional, and when given,
// a person's reason as a rejection's is (1 to 1024 bytes, not only spaces).
func TestReleaseValidate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := Release{ID: "q1", Mode: ReleaseAsIs, ReceptionID: "rc1", Now: now}
	if err := r.Validate(); err != nil {
		t.Errorf("a release with no reason: %v", err)
	}
	r.Reason = "checked by hand"
	if err := r.Validate(); err != nil {
		t.Errorf("a release with a reason: %v", err)
	}
	for _, reason := range []string{"  ", string(make([]byte, MaxDecisionReasonLength+1)), "a\x00b"} {
		r.Reason = reason
		if fault.KindOf(r.Validate()) != fault.Invalid {
			t.Errorf("a release with a reason of %d bytes is not Invalid", len(reason))
		}
	}
}
