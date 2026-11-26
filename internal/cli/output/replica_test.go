package output_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// TestRetentionPeriodRoundTrip guards the words for a retention period: what
// a person types as years and days is read into the contract's two numbers
// and written back the same, and anything else, including a repeated unit or a
// number with no unit, is refused rather than guessed.
func TestRetentionPeriodRoundTrip(t *testing.T) {
	for in, want := range map[string]string{"7y": "7y", "90d": "90d", "2y90d": "2y 90d", " 10y 30d ": "10y 30d", "0y1d": "1d"} {
		p, err := output.ParseRetentionPeriod(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := output.RetentionPeriod(p); got != want {
			t.Errorf("%q is written %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"", "7", "y", "7y7y", "1d1d", "7 years", "-1d", "1.5y", "99999999d"} {
		if p, err := output.ParseRetentionPeriod(in); err == nil {
			t.Errorf("%q was read as %v", in, p)
		}
	}
	if got := output.RetentionPeriod(nil); got != "-" {
		t.Errorf("no period is %q", got)
	}
}

// TestEncryptionIsNeverClaimedMoreStronglyThanSaid guards the wording of
// encryption at rest: a directory's statement is "declared", never plain
// "yes", and a value this client does not know is "unknown".
func TestEncryptionIsNeverClaimedMoreStronglyThanSaid(t *testing.T) {
	for _, c := range []struct {
		e    v1.EncryptionAtRest
		alg  string
		want string
	}{
		{v1.EncryptionAtRest_ENCRYPTION_AT_REST_DECLARED, "", "declared by the admin (not checked by the server)"},
		{v1.EncryptionAtRest_ENCRYPTION_AT_REST_NOT_DECLARED, "", "not declared"},
		{v1.EncryptionAtRest_ENCRYPTION_AT_REST_ENCRYPTED, "AES256", "yes, AES256 (the storage says so)"},
		{v1.EncryptionAtRest_ENCRYPTION_AT_REST_UNKNOWN, "", "unknown"},
		{v1.EncryptionAtRest_ENCRYPTION_AT_REST_UNSPECIFIED, "", "unknown"},
		{v1.EncryptionAtRest(99), "", "unknown"},
	} {
		if got := output.Encryption(c.e, c.alg); got != c.want {
			t.Errorf("%v: %q, want %q", c.e, got, c.want)
		}
	}
}

// TestTierWordsForValuesThisClientDoesNotKnow guards the contract's rule that
// a tier added later is shown as one that may not read at once, never as
// online.
func TestTierWordsForValuesThisClientDoesNotKnow(t *testing.T) {
	if got := output.StorageTier(v1.StorageTier(9)); got == "online" {
		t.Errorf("an unknown tier is shown as %q", got)
	}
	if got := output.StorageTier(v1.StorageTier_STORAGE_TIER_ARCHIVE); got != "archive" {
		t.Errorf("archive is %q", got)
	}
	if got := output.RecallTier(v1.S3RecallTier_S3_RECALL_TIER_UNSPECIFIED); got != "standard" {
		t.Errorf("an unsaid recall tier is %q", got)
	}
}

// TestSpanAndCoverage guards the short lengths of time and the coverage line.
func TestSpanAndCoverage(t *testing.T) {
	for d, want := range map[time.Duration]string{45 * time.Second: "45s", 90 * time.Minute: "1h", 3 * time.Minute: "3m", 49 * time.Hour: "2d"} {
		if got := output.Span(d); got != want {
			t.Errorf("%v is %q, want %q", d, got, want)
		}
	}
	if got := output.Coverage(nil); got != "-" {
		t.Errorf("no coverage is %q", got)
	}
	c := &v1.ScrubCoverage{Objects: 10, Share: 1}
	if got := output.Coverage(c); got != "100.0% of 10 objects read within the interval" {
		t.Errorf("full coverage is %q", got)
	}
	c.Overdue, c.OldestUnverified = 2, durationpb.New(72*time.Hour)
	if got := output.Coverage(c); got != "100.0% of 10 objects read within the interval, 2 overdue, the oldest unread for 3d" {
		t.Errorf("overdue coverage is %q", got)
	}
}
