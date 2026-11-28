package forward

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// versions is a tenant's profile versions held in memory, by name.
type versions struct {
	tenant.Profiles
	stored map[string]tenant.ProfileVersion
}

// ProfileVersion answers a stored version, or NotFound.
func (v versions) ProfileVersion(_ context.Context, name string, version int32) (tenant.ProfileVersion, error) {
	pv, ok := v.stored[name]
	if !ok || pv.Version != version {
		return tenant.ProfileVersion{}, fault.New(fault.NotFound, "no such version")
	}
	return pv, nil
}

// stored returns doc as version 1 of name, as a tenant keeps it.
func stored(t *testing.T, name string, doc *v1.ProfileDocument) tenant.ProfileVersion {
	t.Helper()
	v, err := profile.New(name, 1, doc, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	return tenant.ProfileVersion{Name: name, Version: 1, Source: v.Source(), Expanded: v.ExpandedText(), Checksum: v.Checksum()}
}

// keys are fixed keys of de-identification, as a tenant's are.
func keys(context.Context) (engine.Keys, error) {
	return engine.Keys{UID: bytes.Repeat([]byte{7}, engine.MinKeyLength), Date: bytes.Repeat([]byte{8}, engine.MinKeyLength),
		Hash: bytes.Repeat([]byte{9}, engine.MinKeyLength)}, nil
}

// copies returns Copies over the versions given.
func copies(vs ...tenant.ProfileVersion) *Copies {
	m := map[string]tenant.ProfileVersion{}
	for _, v := range vs {
		m[v.Name] = v
	}
	return &Copies{Profiles: versions{stored: m}, Keys: keys, Options: func(context.Context) (receive.TrialOptions, error) {
		return receive.TrialOptions{HeaderLimit: 64 << 20, Check: func(n int64) receive.Checker { return check.New(check.Options{MaxFileBytes: n}) }}, nil
	}}
}

// TestCopyIsDeidentifiedByTheDestinationProfile guards a copy: it is the
// file de-identified by the destination profile (an attribute the profile
// removes is gone, the UIDs remapped by the tenant's keys, the same for a
// second copy, so a peer sees one study), and the check after ran on it.
func TestCopyIsDeidentifiedByTheDestinationProfile(t *testing.T) {
	file, err := os.ReadFile(filepath.Join("..", "receive", "testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	doc := &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, Rules: []*v1.ProfileRule{{Name: "kvp",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "KVP"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}}}}
	c := copies(stored(t, "dest", doc))
	ref := tenant.ProfileRef{Name: "dest", Version: 1}
	out, cause, err := c.Copy(context.Background(), ref, file)
	if err != nil || cause != "" {
		t.Fatalf("the copy: cause %q, %v", cause, err)
	}
	before, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	after, err := header.Read(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("the copy does not read: %v", err)
	}
	if _, ok := after.Lookup(naming.NewTag(0x0018, 0x0060)); ok {
		t.Error("the copy keeps KVP, which its profile removes")
	}
	was, _ := before.UID(header.SOPInstanceUID)
	now, _ := after.UID(header.SOPInstanceUID)
	if now == "" || now == was {
		t.Errorf("the copy's SOP Instance UID %q, want one remapped from %q", now, was)
	}
	again, _, err := c.Copy(context.Background(), ref, file)
	if err != nil || !bytes.Equal(again, out) {
		t.Errorf("a second copy differs from the first (%v): a peer would see two studies", err)
	}
}

// TestCopyRefused guards a copy that cannot be made: a version the tenant
// does not have, and a file that is not DICOM, are refused with a fixed
// cause, never answered with bytes.
func TestCopyRefused(t *testing.T) {
	c := copies()
	if out, cause, err := c.Copy(context.Background(), tenant.ProfileRef{Name: "none", Version: 1}, []byte("x")); err != nil || cause != CauseNoProfile || out != nil {
		t.Errorf("a version the tenant does not have: %q %v %d bytes", cause, err, len(out))
	}
	c = copies(stored(t, "dest", &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}))
	out, cause, err := c.Copy(context.Background(), tenant.ProfileRef{Name: "dest", Version: 1}, []byte("not a DICOM file"))
	if err != nil || cause != string(tenant.QuarantineNotDICOM)+"/unreadable" || out != nil {
		t.Errorf("a file that is not DICOM: %q %v %d bytes", cause, err, len(out))
	}
}
