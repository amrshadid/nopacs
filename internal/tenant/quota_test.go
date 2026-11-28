package tenant

import (
	"testing"
	"time"
)

// quotaTime is the time of the events these tests make.
var quotaTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestQuotaState guards where a use stands against its bounds: a bound is
// reached when the use equals it, so a bound of n holds n and refuses the
// next; the warning stands at quota.warn of either bound; no bound is
// never warned or reached; and a share of the largest bound does not
// overflow.
func TestQuotaState(t *testing.T) {
	b := QuotaBounds{Bytes: 1000, Instances: 10, WarnPercent: 90}
	for _, c := range []struct {
		use  QuotaUse
		want QuotaState
	}{
		{QuotaUse{}, QuotaUnder},
		{QuotaUse{HeldInstances: 8, HeldBytes: 899}, QuotaUnder},
		{QuotaUse{HeldInstances: 8, HeldBytes: 899, PurgeBytes: 1}, QuotaWarned},
		{QuotaUse{HeldInstances: 8, ReceivingInstances: 1}, QuotaWarned},
		{QuotaUse{HeldInstances: 9, PurgeInstances: 1}, QuotaReached},
		{QuotaUse{HeldBytes: 600, PurgeBytes: 300, ReceivingBytes: 100}, QuotaReached},
		{QuotaUse{HeldBytes: 999}, QuotaWarned},
	} {
		if got := b.State(c.use); got != c.want {
			t.Errorf("%+v against %+v is %s, want %s", c.use, b, got, c.want)
		}
	}
	none := QuotaBounds{WarnPercent: 90}
	if none.Bounded() || none.Reached(QuotaUse{HeldBytes: 1 << 60, HeldInstances: 1 << 60}) || none.State(QuotaUse{HeldBytes: 1 << 60}) != QuotaUnder {
		t.Error("a quota with no bound is bounded, reached or warned")
	}
	huge := QuotaBounds{Bytes: 1 << 62, WarnPercent: 99}
	if got := huge.State(QuotaUse{HeldBytes: 1<<62 - 1}); got != QuotaWarned {
		t.Errorf("one byte under the largest bound is %s, want warned at 99%%", got)
	}
	if got := huge.State(QuotaUse{HeldBytes: 1 << 61}); got != QuotaUnder {
		t.Errorf("half the largest bound is %s, want under", got)
	}
}

// TestQuotaChangedEvent guards the event's form: a state from the three,
// no negative figure, and the payload read back as written.
func TestQuotaChangedEvent(t *testing.T) {
	p := &QuotaChanged{State: QuotaReached, UsedBytes: 10, UsedInstances: 1, BytesBound: 10}
	e, err := NewEvent(p, quotaTime)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := DecodeEvent(e.Type(), e.Payload())
	if q, isQ := got.(*QuotaChanged); !ok || !isQ || *q != *p {
		t.Errorf("the event reads back as %+v, want %+v", got, p)
	}
	for _, bad := range []*QuotaChanged{{State: "full"}, {State: QuotaUnder, UsedBytes: -1}, {State: QuotaWarned, InstancesBound: -1}} {
		if _, err := NewEvent(bad, quotaTime); err == nil {
			t.Errorf("the event %+v is made", bad)
		}
	}
}
