package api_test

import (
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestQuarantineWarningsShowInStatus guards what a tenant's status says of
// its quarantine: the oldest file's age, the files and bytes held, and a
// warning for each setting past which they are, measured at the request. An
// empty quarantine and one under both limits warn of nothing; the age warning
// stands past quarantine.warn-age; the byte warning stands past
// quarantine.warn-bytes, and a value of 0 means no byte warning however much
// is held.
func TestQuarantineWarningsShowInStatus(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	status := func() *v1.QuarantineWarnings {
		t.Helper()
		res := mustCall[*v1.GetStatusResponse](t, "status", call(f.as(f.lab1Reader).monitor.GetStatus, &v1.GetStatusRequest{Scope: tenantScope("lab1")}))
		q := res.GetTenants()[0].GetQuarantine()
		if q == nil {
			t.Fatal("the status of a tenant whose database answers has no quarantine part")
		}
		return q
	}
	set := func(name, value string) {
		t.Helper()
		mustCall[*v1.SetSettingResponse](t, "set "+name, call(f.as(f.lab1Admin).settings.SetSetting,
			&v1.SetSettingRequest{Setting: &v1.Setting{Name: name, Scope: tenantScope("lab1"), Value: value}}))
	}
	want := func(what string, q *v1.QuarantineWarnings, age, bytes bool) {
		t.Helper()
		if q.GetAgePassed() != age || q.GetBytesPassed() != bytes {
			t.Errorf("%s: age warning %v, bytes warning %v; want %v, %v (%v)", what, q.GetAgePassed(), q.GetBytesPassed(), age, bytes, q)
		}
	}

	q := status()
	want("an empty quarantine", q, false, false)
	if q.GetHeldFiles() != 0 || q.GetHeldBytes() != 0 || q.GetOldestAge().AsDuration() != 0 {
		t.Errorf("an empty quarantine holds %v", q)
	}

	f.heldFile(t, "scanner1", false)
	f.advance(24 * time.Hour)
	q = status()
	want("a day old, under both limits", q, false, false)
	if q.GetHeldFiles() != 1 || q.GetHeldBytes() != 528650 || q.GetOldestAge().AsDuration() < 24*time.Hour {
		t.Errorf("one file held a day: %v", q)
	}

	f.advance(40 * 24 * time.Hour)
	want("past the default age of 30 days", status(), true, false)

	set("quarantine.warn-age", "90d")
	want("under a raised age", status(), false, false)

	set("quarantine.warn-bytes", "1000")
	want("past the byte limit", status(), false, true)

	set("quarantine.warn-bytes", "10MiB")
	want("under a raised byte limit", status(), false, false)

	set("quarantine.warn-bytes", "0")
	want("a byte limit of 0, which is none", status(), false, false)
}
