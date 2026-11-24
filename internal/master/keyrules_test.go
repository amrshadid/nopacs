package master

import (
	"testing"
	"time"
)

// TestRotationConflictAllowsSuspended guards that a suspended key can be
// rotated (the way back to a working key after a restore suspended every
// key), while a revoked, expired or already rotated one cannot.
func TestRotationConflictAllowsSuspended(t *testing.T) {
	suspended := Key{ID: "k1", Suspended: t0}
	if err := RotationConflict(suspended, t0.Add(time.Hour)); err != nil {
		t.Fatalf("a suspended key cannot be rotated: %v", err)
	}
	for name, k := range map[string]Key{
		"revoked":  {ID: "k2", Suspended: t0, Revoked: t0},
		"expired":  {ID: "k3", Suspended: t0, Expires: t0},
		"rotated":  {ID: "k4", Suspended: t0, Successor: "k5"},
		"set time": {ID: "k6", Revoked: SetTime(time.Time{})},
	} {
		if err := RotationConflict(k, t0.Add(time.Hour)); err == nil {
			t.Errorf("a %s key can be rotated", name)
		}
	}
}

// TestSetTimeIsNeverZero guards that a time read from a column that is set
// never reads as the zero time ("never"), and that any other time is left
// as it is.
func TestSetTimeIsNeverZero(t *testing.T) {
	if got := SetTime(time.Time{}); got.IsZero() || !got.After(time.Time{}) {
		t.Fatalf("SetTime of the zero time is %v", got)
	}
	if got := SetTime(t0); !got.Equal(t0) {
		t.Fatalf("SetTime changed %v to %v", t0, got)
	}
}
