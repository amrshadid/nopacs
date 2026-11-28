package sidecar

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestBaseProfileRoundTrips guards the base profile field: written beside the
// profile it was stacked over, read back as written, left out of a line with
// none, and refused on writing without a profile, when it is not
// name@version, and in a sidecar of version 2.
func TestBaseProfileRoundTrips(t *testing.T) {
	with := full(t, "1.2.3")
	with.BaseProfile = "floor@2"
	without := full(t, "1.2.4")
	text := v3sidecar(t, head(), with, without)
	if strings.Count(string(text), `"base_profile":"floor@2"`) != 1 || strings.Count(string(text), "base_profile") != 1 {
		t.Errorf("the field is not written once, for the line that has it:\n%s", text)
	}
	_, _, got, err := drain(text)
	if err != nil || len(got) != 2 || got[0].BaseProfile != "floor@2" || got[1].BaseProfile != "" {
		t.Fatalf("read back %+v (%v)", got, err)
	}
	for name, mutate := range map[string]func(in *Instance){
		"no profile":      func(in *Instance) { in.Profile, in.ProfileSHA256, in.PixelRiskAccepted = "", "", false },
		"not name@number": func(in *Instance) { in.BaseProfile = "floor" },
		"a zero version":  func(in *Instance) { in.BaseProfile = "floor@0" },
	} {
		in := full(t, "1.2.5")
		in.BaseProfile = "floor@2"
		mutate(&in)
		if err := NewWriterV3(io.Discard, head()).Add(in); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: written: %v, want ErrFormat", name, err)
		}
	}
	if err := NewWriter(io.Discard).Add(with); !errors.Is(err, ErrFormat) {
		t.Errorf("a base profile in a sidecar of version 2: %v, want ErrFormat", err)
	}
}
