package auditchain

import (
	"bytes"
	"crypto/ed25519"
	"strconv"
	"testing"
	"time"
)

// testSigner signs with an Ed25519 key made from a fixed seed, standing in
// for the signer derived from the installation key.
type testSigner struct {
	version uint32
	key     ed25519.PrivateKey
}

// newTestSigner returns a signer whose seed is 32 bytes of seed.
func newTestSigner(version uint32, seed byte) *testSigner {
	return &testSigner{version: version, key: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))}
}

// KeyVersion returns the signer's version.
func (s *testSigner) KeyVersion() uint32 { return s.version }

// Sign signs message with pure Ed25519.
func (s *testSigner) Sign(message []byte) ([]byte, error) { return ed25519.Sign(s.key, message), nil }

// public returns the signer's public key.
func (s *testSigner) public() ed25519.PublicKey {
	pub, _ := s.key.Public().(ed25519.PublicKey)
	return pub
}

// baseTime is the time the first test entry is made.
var baseTime = time.Date(2026, 11, 21, 9, 30, 0, 123456000, time.UTC)

// testEntries returns entries with seq from first to first+n-1, varied so
// every field and the details are exercised: empty and non-ASCII text, key
// and local user, each outcome.
func testEntries(first uint64, n int) []Entry {
	outcomes := []string{"done", "refused", "failed"}
	out := make([]Entry, n)
	for i := range n {
		seq := first + uint64(i) //nolint:gosec // G115: a test's small index.
		s := strconv.FormatUint(seq, 10)
		e := Entry{
			Seq:           seq,
			ID:            "entry-" + s,
			Time:          baseTime.Add(time.Duration(i) * time.Second),
			RemoteAddress: "192.0.2." + s + ":4" + s,
			Method:        "/nopacs.v1.TenantService/GetTenant",
			TenantName:    "lab1",
			Object:        "subject/SUBJ" + s,
			Outcome:       outcomes[i%3],
			RequestID:     "req-" + s,
		}
		if i%2 == 0 {
			e.KeyID = "key" + s
		} else {
			e.LocalUser = "operator"
		}
		if i%3 != 0 {
			e.Reason = "a reason with ünïcödé " + s
			e.Details = map[string]string{"role": "admin", "Zone": "b", "änder": "ç", "": "empty name"}
		}
		out[i] = e
	}
	return out
}

// sealedLog is a log of entries sealed by several checkpoints, as a sealing
// pass would leave it.
type sealedLog struct {
	entries     []Entry
	checkpoints []Checkpoint
	keys        PublicKeys
	final       State
}

// makeLog seals entries 1..sum(sizes) in checkpoints of the given sizes; the
// signer changes from version 1 to version 2 after the first checkpoint, as
// after a key rotation.
func makeLog(t *testing.T, installation, log string, sizes ...int) sealedLog {
	t.Helper()
	s1, s2 := newTestSigner(1, 0x41), newTestSigner(2, 0x42)
	l := sealedLog{keys: PublicKeys{1: s1.public(), 2: s2.public()}}
	var state State
	for i, size := range sizes {
		batch := testEntries(state.Seq+1, size)
		signer := s1
		if i > 0 {
			signer = s2
		}
		c, next, err := Seal(signer, installation, log, state, batch, baseTime.Add(time.Duration(i+1)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		l.entries = append(l.entries, batch...)
		l.checkpoints = append(l.checkpoints, c)
		state = next
	}
	l.final = state
	return l
}

// verifyLog feeds entries and checkpoints to a new Verifier in seq order,
// each checkpoint after the entry that is its To, and returns Finish's
// result.
func verifyLog(keys PublicKeys, installation, log string, from State, entries []Entry, checkpoints []Checkpoint) (State, error) {
	v := NewVerifier(keys, installation, log, from)
	next := 0
	for _, e := range entries {
		if err := v.Entry(e); err != nil {
			return State{}, err
		}
		for next < len(checkpoints) && checkpoints[next].To <= e.Seq {
			if err := v.Checkpoint(checkpoints[next]); err != nil {
				return State{}, err
			}
			next++
		}
	}
	for ; next < len(checkpoints); next++ {
		if err := v.Checkpoint(checkpoints[next]); err != nil {
			return State{}, err
		}
	}
	return v.Finish()
}
