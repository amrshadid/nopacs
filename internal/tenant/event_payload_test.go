package tenant

import (
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
)

// eventTime is the time of the events these tests make.
var eventTime = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// TestDecodeEventIsTolerant guards reading what a store holds: a payload
// with a field this version does not know is read with the fields it knows;
// one that cannot be read back (not JSON, empty, a value outside this
// version's lists, a field the type requires missing) answers no payload
// and false, never an error; a type this version does not know answers no
// payload and true.
func TestDecodeEventIsTolerant(t *testing.T) {
	p, ok := DecodeEvent(EventStorageSpace, `{"storage":"store","state":"recovered","used_bytes":7,"added_later":[1,2]}`)
	if s, isSpace := p.(*StorageSpaceChanged); !ok || !isSpace || s.UsedBytes != 7 {
		t.Errorf("a payload with a field added later = %+v, %v; want its known fields", p, ok)
	}
	for _, bad := range []string{``, `not json`, `{}`, `{"storage":"store","state":"half_full"}`, `{"state":"recovered"}`, `[1]`} {
		if p, ok := DecodeEvent(EventStorageSpace, bad); p != nil || ok {
			t.Errorf("DecodeEvent(%q) = %+v, %v; want no payload and false", bad, p, ok)
		}
	}
	if p, ok := DecodeEvent("file_released", `{"x":1}`); p != nil || !ok {
		t.Errorf("a type not known = %+v, %v; want no payload and true", p, ok)
	}
}

// TestReceptionStepEventNamesTheStepLeft guards the step left in a step's
// event: empty for a reception's first step, the step before otherwise, and
// a step outside the core's list refused.
func TestReceptionStepEventNamesTheStepLeft(t *testing.T) {
	r := Reception{ID: "rc1", Receiver: "a", Arrival: 1, State: reception.Failed, StepTime: eventTime}
	e, err := ReceptionStepEvent(reception.Archived, r)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := DecodeEvent(e.Type(), e.Payload())
	if s := p.(*ReceptionStepReached); s.PreviousStep != reception.Archived || s.Step != reception.Failed {
		t.Errorf("the event of a failure from archived reads %+v", s)
	}
	if !strings.Contains(e.Payload(), `"previous_step":"archived"`) {
		t.Errorf("the stored payload %s does not name the step left", e.Payload())
	}
	if _, err := ReceptionStepEvent("halfway", r); err == nil {
		t.Error("a step left outside the core's list was accepted")
	}
}

// TestHealthDetailIsAClosedList guards a receiver health's detail: one of
// the server's phrases for the reason, or empty; any other text, a phrase
// of another reason, or a detail on a receiver that is up is refused, so a
// detail never carries an error's text or anything from outside.
func TestHealthDetailIsAClosedList(t *testing.T) {
	ok := []*ReceiverHealthChanged{
		{Receiver: "a", State: ReceiverDown, DownReason: "tls", Detail: "certificate expired", Since: eventTime},
		{Receiver: "a", State: ReceiverDown, DownReason: "tls", Since: eventTime},
		{Receiver: "a", State: ReceiverUp, Since: eventTime},
	}
	for _, h := range ok {
		if _, err := NewEvent(h, eventTime); err != nil {
			t.Errorf("NewEvent(%+v): %v", h, err)
		}
	}
	bad := []*ReceiverHealthChanged{
		{Receiver: "a", State: ReceiverDown, DownReason: "tls", Detail: "open /etc/ssl/key.pem: permission denied", Since: eventTime},
		{Receiver: "a", State: ReceiverDown, DownReason: "tls", Detail: "the server is stopping", Since: eventTime},
		{Receiver: "a", State: ReceiverUp, Detail: "certificate expired", Since: eventTime},
		{Receiver: "a", State: ReceiverDown, DownReason: "gremlins", Since: eventTime},
	}
	for _, h := range bad {
		if _, err := NewEvent(h, eventTime); err == nil {
			t.Errorf("NewEvent(%+v) was accepted", h)
		}
	}
	for _, phrase := range HealthPhrases("tls") {
		if len(phrase) > 256 {
			t.Errorf("the phrase %q is over the contract's 256 bytes", phrase)
		}
	}
}
