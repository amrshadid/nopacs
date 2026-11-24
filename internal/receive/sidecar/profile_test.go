package sidecar

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestProfileRoundTrips guards the profile field: written only for a line
// that has one, between transfer_syntax and pixel_data, read back as
// written; a sidecar written before the field existed (no line has it)
// still reads, its lines with none, and the version stays 2; a value that
// is not name@version is refused on writing and on reading.
func TestProfileRoundTrips(t *testing.T) {
	with := inst(t, "1.2.3", false)
	with.TransferSyntax, with.Profile = "1.2.840.10008.1.2.1", "research@3"
	without := inst(t, "1.2.4", true)
	var b bytes.Buffer
	w := NewWriter(&b)
	for _, in := range []Instance{with, without} {
		if err := w.Add(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	if strings.Count(text, `"profile"`) != 1 || !strings.Contains(text, `"transfer_syntax":"1.2.840.10008.1.2.1","profile":"research@3","pixel_data"`) {
		t.Errorf("the field is not where the form puts it, or is written for a line without one:\n%s", text)
	}
	if VersionOf([]byte(text[:HeadLength])) != VersionFrames || VersionFrames != 2 {
		t.Errorf("the sidecar's version is %d", VersionOf([]byte(text[:HeadLength])))
	}
	got := readAll(t, b.Bytes())
	if got[0].Profile != "research@3" || got[1].Profile != "" {
		t.Errorf("profiles %q and %q read back", got[0].Profile, got[1].Profile)
	}
	older := strings.Replace(text, `,"profile":"research@3"`, "", 1)
	if got := readAll(t, []byte(older)); len(got) != 2 || got[0].Profile != "" {
		t.Errorf("a sidecar written before the field: %+v", got)
	}
	for _, p := range []string{"research", "research@0", "research@03", "Research@1", "r@10001", "@1", "research@1@2"} {
		bad := inst(t, "1.2.5", false)
		bad.Profile = p
		if err := NewWriter(&bytes.Buffer{}).Add(bad); !errors.Is(err, ErrFormat) {
			t.Errorf("profile %q written: %v, want ErrFormat", p, err)
		}
	}
	tampered := strings.Replace(text, "research@3", "research@x", 1)
	r := NewReader(strings.NewReader(tampered))
	if _, _, err := r.Next(); !errors.Is(err, ErrFormat) {
		t.Errorf("a line with a malformed profile read: %v, want ErrFormat", err)
	}
}
