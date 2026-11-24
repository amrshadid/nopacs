package scrub

import (
	"bytes"
	"crypto/sha256"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// ghostDeleted puts on the storage a scan tree the catalog does not hold,
// two instances and the manifest naming them, a committed delete's record
// of that scan with no row, as encode writes it, and an object beside them
// that the manifest does not name. It returns the two instances' keys and
// the stray object's key.
func (r *corrRig) ghostDeleted(encode func(record.Correction) []byte) ([]string, string) {
	r.t.Helper()
	ghost := label.Path{Subject: "sub-ghost", Session: "ses-ghost", Scan: "scan-ghost"}
	var lines []manifest.File
	var keys []string
	for i, b := range [][]byte{[]byte("first instance of a scan deleted"), []byte("second instance of a scan deleted")} {
		uid := "1.2.826.0.1.3680043.2.1125.77." + string(rune('1'+i))
		key, err := layout.InstanceKey(ghost, uid)
		if err != nil {
			r.t.Fatal(err)
		}
		if _, err := r.store.Put(r.ctx, key, bytes.NewReader(b)); err != nil {
			r.t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		lines = append(lines, manifest.File{Key: key, Length: int64(len(b)), Size: int64(len(b)), SHA256: tenant.SHA256(sum).String(),
			SOPInstanceUID: uid})
		keys = append(keys, key)
	}
	r.putManifest(ghost, lines)
	now := r.clock.Now().UTC().Truncate(time.Second)
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		r.t.Fatal(err)
	}
	b := encode(record.Correction{Format: record.Format, ID: id, Kind: record.KindDelete, Level: record.LevelScan,
		State: record.StateCommitted, ReasonSHA256: record.ReasonDigest("consent withdrawn"), Created: now, CommittedAt: now,
		PurgeAfter: now.Add(24 * time.Hour), Scans: []record.Scan{{Subject: ghost.Subject, Session: ghost.Session, Scan: ghost.Scan,
			StudyUID: "1.2.826.0.1.3680043.2.1125.77", SeriesUID: "1.2.826.0.1.3680043.2.1125.77.0"}}})
	rk, _ := layout.CorrectionKey(id)
	if _, err := r.store.Put(r.ctx, rk, bytes.NewReader(b)); err != nil {
		r.t.Fatal(err)
	}
	stray, _ := layout.InstanceKey(ghost, "1.2.826.0.1.3680043.2.1125.77.9")
	r.putStray(stray)
	return keys, stray
}

// macField matches a record's MAC field, the last of its fields.
var macField = regexp.MustCompile(`,"mac":"[0-9a-f]{64}"`)

// TestScrubTrustsNoUnauthenticatedRecord guards that a correction record
// with no row whose MAC is missing or does not verify for the tenant (one
// with no MAC, one of another tenant, one of another installation's key
// file, one with a byte changed after signing) explains nothing: the
// objects its scan's manifest names are orphans as the stray beside them
// is, and the record is tallied unauthenticated as well as with no row.
func TestScrubTrustsNoUnauthenticatedRecord(t *testing.T) {
	other := testkit.InstallationKeys(t)
	for name, forge := range map[string]func(r *corrRig, b []byte, c record.Correction) []byte{
		"no mac": func(_ *corrRig, b []byte, _ record.Correction) []byte { return macField.ReplaceAll(b, nil) },
		"another tenant": func(r *corrRig, _ []byte, c record.Correction) []byte {
			return encodeFor(r.t, c, "T0000000000002", r.installationKeys)
		},
		"another installation": func(r *corrRig, _ []byte, c record.Correction) []byte {
			return encodeFor(r.t, c, fixtureTenantID, other)
		},
		// The reason's digest, which no reader acts on, so only the MAC
		// can tell the record from the one signed.
		"one byte": func(_ *corrRig, b []byte, c record.Correction) []byte {
			d := c.ReasonSHA256
			return bytes.Replace(b, []byte(d), []byte(d[:63]+map[bool]string{true: "1", false: "0"}[d[63] == '0']), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newCorrRig(t)
			keys, stray := r.ghostDeleted(func(c record.Correction) []byte {
				return forge(r, encodeFor(t, c, fixtureTenantID, r.installationKeys), c)
			})
			j := r.scrubNow()
			got := r.findings(false)
			for _, k := range append(keys, stray) {
				if !slices.Contains(got, string(tenant.FindingOrphan)+" "+k) {
					t.Errorf("the scrub holds %v, want %s an orphan: a record that does not verify explains nothing", got, k)
				}
			}
			tallies := r.tallies(j.ID)
			if tallies[tenant.TallyCorrectionUnauthenticated] != 1 || tallies[tenant.TallyCorrectionRecordOnly] != 1 {
				t.Errorf("the tallies are %v, want the record counted once with no row and once unauthenticated", tallies)
			}
		})
	}
}

// TestScrubNeedsRecordKeys guards that no supervisor is made without the
// installation keys or a way to learn a tenant's id, so no record explains
// an object unchecked.
func TestScrubNeedsRecordKeys(t *testing.T) {
	f := &fixture{t: t, reads: &checkedread.Slots{}, installationKeys: testkit.InstallationKeys(t)}
	for name, change := range map[string]func(*Config){
		"no keys":      func(c *Config) { c.Keys = nil },
		"empty keys":   func(c *Config) { c.Keys = &secret.Keys{} },
		"no tenant id": func(c *Config) { c.TenantID = nil },
	} {
		cfg := f.config()
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: a supervisor was made", name)
		}
	}
	if _, err := New(f.config()); err != nil {
		t.Fatalf("the whole configuration is refused: %v", err)
	}
}

// encodeFor writes c's record for the tenant tenantID under keys.
func encodeFor(t testing.TB, c record.Correction, tenantID string, keys record.Keyring) []byte {
	t.Helper()
	b, err := record.EncodeCorrection(c, tenantID, keys)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
