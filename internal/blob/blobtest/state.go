package blobtest

import (
	"bytes"
	"crypto/sha256"
	"errors"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// testStatState guards what Stat says beyond the size: an object just put
// reads at once (Online, no end to it), and the storage's own SHA-256 is
// there exactly when the driver declared it keeps one, and is then the
// SHA-256 of the bytes the key holds now, also after a replace.
func testStatState(e *env) {
	data := PatternBytes(41, 1000)
	w := e.put("st/a.dcm", data)
	check := func(want [sha256.Size]byte) {
		e.Helper()
		info, err := e.s.Stat(e.ctx, "st/a.dcm")
		if err != nil {
			e.Fatalf("Stat: %v", err)
		}
		if info.State != blob.Online || !info.OnlineUntil.IsZero() {
			e.Fatalf("Stat of an object just put says %v until %v, want online with no end", info.State, info.OnlineUntil)
		}
		if info.HasSHA256 != e.d.KeepsSHA256 {
			e.Fatalf("Stat says HasSHA256 %v, but the driver declared %v", info.HasSHA256, e.d.KeepsSHA256)
		}
		if info.HasSHA256 && info.SHA256 != want {
			e.Fatalf("Stat says the storage holds SHA-256 %x, but the object's is %x", info.SHA256, want)
		}
		if !info.HasSHA256 && info.SHA256 != ([sha256.Size]byte{}) {
			e.Fatalf("Stat gives a SHA-256 %x it says it does not have", info.SHA256)
		}
	}
	check(w.SHA256)
	other := PatternBytes(42, 1000)
	check(e.put("st/a.dcm", other).SHA256)
}

// testRecall guards Recall on every driver: an object held is answered
// online, with nothing asked of a store whose objects all read at once; a
// key that holds nothing is NotFound; a bad key, days out of range or an
// unknown speed are Invalid, touching nothing; a closed store refuses it
// as Unavailable.
func testRecall(e *env) {
	r, ok := e.s.(blob.Recaller)
	if !ok {
		e.Fatalf("the store %T does not implement blob.Recaller, which every driver does", e.s)
	}
	e.put("rc/a.dcm", []byte("recalled"))
	ans, err := r.Recall(e.ctx, "rc/a.dcm", blob.RecallRequest{Days: 7})
	if err != nil || ans.State != blob.RecallOnline || !ans.OnlineUntil.IsZero() {
		e.Fatalf("Recall of an object just put = %+v, %v; want online, no end", ans, err)
	}
	for _, speed := range []blob.RecallSpeed{blob.RecallBulk, blob.RecallExpedited} {
		if ans, err := r.Recall(e.ctx, "rc/a.dcm", blob.RecallRequest{Days: blob.MaxRecallDays, Speed: speed}); err != nil || ans.State != blob.RecallOnline {
			e.Fatalf("Recall at speed %d = %+v, %v; want online", speed, ans, err)
		}
	}
	if _, err := r.Recall(e.ctx, "rc/none.dcm", blob.RecallRequest{Days: 1}); fault.KindOf(err) != fault.NotFound {
		e.Fatalf("Recall of a key that holds nothing = %v, want NotFound", err)
	}
	for _, bad := range []struct {
		key string
		req blob.RecallRequest
	}{
		{"rc/./a", blob.RecallRequest{Days: 1}},
		{"rc/a.dcm", blob.RecallRequest{Days: 0}},
		{"rc/a.dcm", blob.RecallRequest{Days: blob.MaxRecallDays + 1}},
		{"rc/a.dcm", blob.RecallRequest{Days: -1}},
		{"rc/a.dcm", blob.RecallRequest{Days: 1, Speed: blob.RecallExpedited + 1}},
	} {
		if _, err := r.Recall(e.ctx, bad.key, bad.req); fault.KindOf(err) != fault.Invalid {
			e.Fatalf("Recall(%q, %+v) = %v, want Invalid", bad.key, bad.req, err)
		}
	}
	e.wantObject("rc/a.dcm", []byte("recalled"))
	s := e.d.Open(e.T)
	if err := s.Close(); err != nil {
		e.Fatal(err)
	}
	if _, err := s.(blob.Recaller).Recall(e.ctx, "rc/a.dcm", blob.RecallRequest{Days: 1}); fault.KindOf(err) != fault.Unavailable {
		e.Fatalf("Recall after Close = %v, want Unavailable", err)
	}
}

// testPutConfirmed guards blob.PutConfirmed on the driver: the bytes
// expected are put, and Written.Confirmed is what the driver declared; bytes
// with another SHA-256, fewer or more than the size expected fail as
// DataLoss and leave the key as it was, absent or holding its old object,
// for an object of one request and for one large enough to be sent in
// parts.
func testPutConfirmed(e *env) {
	data := PatternBytes(51, 3000)
	sum := sha256.Sum256(data)
	w, err := blob.PutConfirmed(e.ctx, e.s, "pc/a.dcm", bytes.NewReader(data), int64(len(data)), sum)
	if err != nil {
		e.Fatalf("PutConfirmed of the bytes expected: %v", err)
	}
	if w.Size != int64(len(data)) || w.SHA256 != sum || w.Confirmed != e.d.ConfirmsPuts {
		e.Fatalf("PutConfirmed wrote %+v; want %d bytes, %x, confirmed %v", w, len(data), sum, e.d.ConfirmsPuts)
	}
	e.wantObject("pc/a.dcm", data)

	wantRefused := func(key string, err error) {
		e.Helper()
		if fault.KindOf(err) != fault.DataLoss || !errors.Is(err, blob.ErrNotTheBytesExpected) {
			e.Fatalf("PutConfirmed(%q) of bytes not the ones expected = %v, want DataLoss with ErrNotTheBytesExpected", key, err)
		}
	}
	changed := bytes.Clone(data)
	changed[len(changed)-1] ^= 1
	for _, c := range []struct {
		name string
		body []byte
		size int64
	}{
		{"another checksum", changed, int64(len(data))},
		{"fewer bytes", data[:len(data)-1], int64(len(data))},
		{"more bytes", append(bytes.Clone(data), 'x'), int64(len(data))},
		{"a size too small", data, int64(len(data)) - 1},
	} {
		_, err := blob.PutConfirmed(e.ctx, e.s, "pc/b.dcm", bytes.NewReader(c.body), c.size, sum)
		wantRefused("pc/b.dcm: "+c.name, err)
		e.wantAbsent("pc/b.dcm")
		_, err = blob.PutConfirmed(e.ctx, e.s, "pc/a.dcm", bytes.NewReader(c.body), c.size, sum)
		wantRefused("pc/a.dcm: "+c.name, err)
		e.wantObject("pc/a.dcm", data)
	}

	// Larger than a part of any driver that sends parts (an S3 part is 8 MiB
	// by default), so the mismatch is found after parts were sent.
	const large = 9<<20 + 7
	big := Pattern(52, large)
	_, err = blob.PutConfirmed(e.ctx, e.s, "pc/big.dcm", big, large, PatternSum(53, large))
	wantRefused("pc/big.dcm", err)
	e.wantAbsent("pc/big.dcm")
	if _, err := blob.PutConfirmed(e.ctx, e.s, "pc/big.dcm", Pattern(52, large), large, PatternSum(52, large)); err != nil {
		e.Fatalf("PutConfirmed of a large object: %v", err)
	}
	if info, err := e.s.Stat(e.ctx, "pc/big.dcm"); err != nil || info.Size != large {
		e.Fatalf("Stat of the large object = %+v, %v", info, err)
	} else if info.HasSHA256 && info.SHA256 != PatternSum(52, large) {
		// A store may keep no whole checksum of an object sent in parts, but
		// one it gives must be the object's.
		e.Fatalf("Stat of the large object gives the SHA-256 %x, not the object's", info.SHA256)
	}
	if _, err := blob.PutConfirmed(e.ctx, e.s, "pc/c.dcm", bytes.NewReader(nil), -1, sum); fault.KindOf(err) != fault.Invalid {
		e.Fatalf("PutConfirmed with a negative size = %v, want Invalid", err)
	}
}

// testEncryption guards the answer of a store that reports its encryption
// at rest: one of the states, with an algorithm when and only when it says
// it encrypts, and a key named only then.
func testEncryption(e *env) {
	r, ok := e.s.(blob.EncryptionReporter)
	if !ok {
		return // The record is then what a person states of the storage.
	}
	enc, err := r.Encryption(e.ctx)
	if err != nil {
		e.Fatalf("Encryption: %v", err)
	}
	switch enc.State {
	case blob.EncryptionOn:
		if enc.Algorithm == "" {
			e.Fatalf("Encryption says it encrypts but names no algorithm: %+v", enc)
		}
	case blob.EncryptionNone, blob.EncryptionUnknown:
		if enc.Algorithm != "" || enc.KeyNamed {
			e.Fatalf("Encryption names an algorithm or a key without saying it encrypts: %+v", enc)
		}
	default:
		e.Fatalf("Encryption answered the state %d", enc.State)
	}
}
