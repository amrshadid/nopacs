package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/recall"
)

// TestByteRangeOf guards the reading of a Range header against a body of
// 1000 bytes and of none, as RFC 9110 14 and the contract say.
func TestByteRangeOf(t *testing.T) {
	for _, c := range []struct {
		h           string
		size        int64
		kind        rangeKind
		first, last int64
	}{
		{"bytes=0-0", 1000, rangeOne, 0, 0},
		{"bytes=0-", 1000, rangeOne, 0, 999},
		{"bytes=10-20", 1000, rangeOne, 10, 20},
		{"bytes=990-5000", 1000, rangeOne, 990, 999},
		{"bytes=-1", 1000, rangeOne, 999, 999},
		{"bytes=-2000", 1000, rangeOne, 0, 999},
		{"bytes= 5-6 ", 1000, rangeOne, 5, 6},
		{"Bytes=0005-0006", 1000, rangeOne, 5, 6},
		{"bytes=999-", 1000, rangeOne, 999, 999},
		{"bytes=1000-", 1000, rangeUnsatisfiable, 0, 0},
		{"bytes=1000-2000", 1000, rangeUnsatisfiable, 0, 0},
		{"bytes=-0", 1000, rangeUnsatisfiable, 0, 0},
		{"bytes=0-", 0, rangeUnsatisfiable, 0, 0},
		{"bytes=-5", 0, rangeUnsatisfiable, 0, 0},
		{"bytes=99999999999999999999999-", 1000, rangeUnsatisfiable, 0, 0},
		{"bytes=0-99999999999999999999999", 1000, rangeOne, 0, 999},
		{"", 1000, rangeNone, 0, 0},
		{"bytes=0-1,3-4", 1000, rangeNone, 0, 0},
		{"bytes=5-4", 1000, rangeNone, 0, 0},
		{"bytes=-", 1000, rangeNone, 0, 0},
		{"bytes=a-b", 1000, rangeNone, 0, 0},
		{"bytes=+1-2", 1000, rangeNone, 0, 0},
		{"bytes 0-1", 1000, rangeNone, 0, 0},
		{"items=0-1", 1000, rangeNone, 0, 0},
		{"bytes=0x10-", 1000, rangeNone, 0, 0},
	} {
		r, kind := byteRangeOf(c.h, c.size)
		if kind != c.kind || (kind == rangeOne && (r.first != c.first || r.last != c.last)) {
			t.Errorf("%q of %d: %v %+v, want %v %d-%d", c.h, c.size, kind, r, c.kind, c.first, c.last)
		}
	}
}

// TestWindowOf guards the reading of a download's query: none, or exactly
// offset and length, each once, in decimal without sign or leading zero,
// length at least 1, and their sum within an int64.
func TestWindowOf(t *testing.T) {
	good := map[string]window{
		"":                                    {},
		"offset=0&length=1":                   {given: true, offset: 0, length: 1},
		"length=7&offset=10":                  {given: true, offset: 10, length: 7},
		"offset=9223372036854775806&length=1": {given: true, offset: 9223372036854775806, length: 1},
	}
	for q, want := range good {
		if got, err := windowOf(q); err != nil || got != want {
			t.Errorf("%q: %+v %v, want %+v", q, got, err, want)
		}
	}
	for _, q := range []string{"offset=0", "length=1", "offset=0&length=0", "offset=00&length=1", "offset=0&length=01",
		"offset=-0&length=1", "offset=+1&length=1", "offset=0&length=1&x=", "offset=0&length=1&offset=0", "offset&length=1",
		"offset=0&length=1&", "x=1", "offset=9223372036854775807&length=1", "offset=0&length=99999999999999999999",
		"offset=0%20&length=1", "offset=0;length=1"} {
		if _, err := windowOf(q); fault.KindOf(err) != fault.Invalid {
			t.Errorf("%q: %v, want invalid", q, err)
		}
	}
}

// TestConditions guards If-Range, which lets a range apply only with the
// strong tag itself, and If-None-Match, which matches the tag weakly or
// "*" in a list.
func TestConditions(t *testing.T) {
	const tag = `"abc"`
	for _, c := range []struct {
		values []string
		holds  bool
	}{
		{nil, true}, {[]string{tag}, true}, {[]string{" " + tag + " "}, true},
		{[]string{"W/" + tag}, false}, {[]string{`"abd"`}, false}, {[]string{"Fri, 20 Nov 2026 09:00:00 GMT"}, false},
		{[]string{tag, tag}, false},
	} {
		if got := ifRangeHolds(c.values, tag); got != c.holds {
			t.Errorf("If-Range %q: %v, want %v", c.values, got, c.holds)
		}
	}
	for _, c := range []struct {
		values []string
		fails  bool
	}{
		{nil, false}, {[]string{tag}, true}, {[]string{"W/" + tag}, true}, {[]string{`"x", ` + tag}, true},
		{[]string{`"x"`, tag}, true}, {[]string{"*"}, true}, {[]string{`"x"`}, false}, {[]string{"abc"}, false},
	} {
		if got := noneMatchFails(c.values, tag); got != c.fails {
			t.Errorf("If-None-Match %q: %v, want %v", c.values, got, c.fails)
		}
	}
}

// fakeSender is a fileSender over bytes, which records the size of each
// SendTo and can give fewer bytes than it holds.
type fakeSender struct {
	data  []byte
	short int
	asked []int64
}

// Remaining returns the bytes left.
func (f *fakeSender) Remaining() int64 { return int64(len(f.data)) }

// SendTo writes the next n bytes, short of them by f.short.
func (f *fakeSender) SendTo(w io.Writer, n int64) (int64, error) {
	f.asked = append(f.asked, n)
	n = max(min(n, int64(len(f.data)))-int64(f.short), 0)
	k, err := w.Write(f.data[:n])
	f.data = f.data[k:]
	return int64(k), err
}

// TestNotOnlineAnswer guards what every read path answers for an object its
// storage holds in a tier it cannot read at once (blob.ErrNotOnline, as the
// S3 driver gives it for InvalidObjectState): unavailable, never data loss
// or not found, with a message that starts with the reason NOT_ONLINE and
// carries the word not_online, through instanceFailure (a download, a
// retrieve, a stream of files, a frame) and storageFailure (a scan's
// metadata file) alike; and that a storage that does not answer keeps its
// plain unavailable answer, without the reason.
func TestNotOnlineAnswer(t *testing.T) {
	cold := fault.Wrap(fault.Unavailable, errors.Join(errors.New("the storage answered with the error code InvalidObjectState"), blob.ErrNotOnline),
		"the read failed: the object is held in a tier the storage cannot read at once")
	p := label.Path{Subject: "s1", Session: "a", Scan: "b"}
	for name, err := range map[string]error{
		"instance": instanceFailure(cold, p),
		"metadata": storageFailure(cold, "the scan's metadata file cannot be read"),
	} {
		msg := fault.Message(err)
		if k := fault.KindOf(err); k != fault.Unavailable || codes.Code(k) != connect.CodeUnavailable {
			t.Errorf("%s not online: kind %v, want Unavailable", name, k)
		}
		if !strings.HasPrefix(msg, "NOT_ONLINE: ") || !strings.Contains(msg, "not_online") || !errors.Is(err, blob.ErrNotOnline) {
			t.Errorf("%s not online answers %q", name, msg)
		}
	}
	if !strings.Contains(fault.Message(instanceFailure(cold, p)), "s1/a/b") {
		t.Errorf("an instance not online does not name its scan: %q", fault.Message(instanceFailure(cold, p)))
	}
	down := fault.New(fault.Unavailable, "the read failed: the storage could not be reached")
	if msg := fault.Message(instanceFailure(down, p)); strings.Contains(msg, "NOT_ONLINE") || fault.KindOf(instanceFailure(down, p)) != fault.Unavailable {
		t.Errorf("a storage that does not answer: %q", msg)
	}
}

// TestNotOnlineCarriesRecallStateAndRetry guards NOT_ONLINE's details as
// access.proto writes them: the ErrorInfo reason NOT_ONLINE with the
// metadata recall_state the read learned, through instanceFailure and
// storageFailure alike; a RetryInfo of the recall worker's poll only while
// a recall is requested or in progress, none for none or failed; and a
// plain HTTP download's answer then carries Retry-After in seconds.
func TestNotOnlineCarriesRecallStateAndRetry(t *testing.T) {
	cold := fault.Wrap(fault.Unavailable, blob.ErrNotOnline, "the read failed: the object is held in a tier the storage cannot read at once")
	p := label.Path{Subject: "s1", Session: "a", Scan: "b"}
	for _, c := range []struct {
		state string
		retry bool
	}{
		{checkedread.RecallNone, false}, {checkedread.RecallRequested, true}, {checkedread.RecallInProgress, true}, {checkedread.RecallFailed, false},
	} {
		in := &checkedread.NotOnline{RecallState: c.state, Err: cold}
		for name, err := range map[string]error{
			"instance": instanceFailure(fault.Wrap(fault.Unavailable, in, "the read failed"), p),
			"metadata": storageFailure(in, "the scan's metadata file cannot be read"),
		} {
			ce := codes.ToConnect(err)
			r, ok := codes.ReasonOfError(ce)
			if ce.Code() != connect.CodeUnavailable || !ok || r.Reason != "NOT_ONLINE" || r.Domain != codes.Domain || r.Metadata["recall_state"] != c.state {
				t.Errorf("%s with recall %s: %v, reason %+v %v", name, c.state, ce, r, ok)
			}
			delay, has := codes.RetryOfError(ce)
			if has != c.retry || (c.retry && delay != recall.DefaultPoll) {
				t.Errorf("%s with recall %s: retry %v %v; want %v", name, c.state, delay, has, c.retry)
			}
		}
	}
	h := http.Header{}
	setRetryAfter(h, codes.ToConnect(instanceFailure(&checkedread.NotOnline{RecallState: checkedread.RecallRequested, Err: cold}, p)))
	if got := h.Get("Retry-After"); got != "60" {
		t.Errorf("the download's Retry-After while a recall runs is %q; want 60", got)
	}
	h = http.Header{}
	setRetryAfter(h, codes.ToConnect(instanceFailure(&checkedread.NotOnline{RecallState: checkedread.RecallFailed, Err: cold}, p)))
	if got := h.Get("Retry-After"); got != "" {
		t.Errorf("the download's Retry-After with the recall failed is %q; want none", got)
	}
}

// TestSendRange guards the part of a range a storage's stream writes
// itself: every byte but the last piece, in order, in pieces of at most
// sendPiece each preceded by the deadline's step, leaving the last piece
// to the checked copy; a stream that writes fewer bytes than asked is
// checkedread.ErrShort; and a canceled request stops before the next piece with its
// cause.
func TestSendRange(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), (2*sendPiece+3*httpChunk)/16)
	n := int64(len(data))
	f := &fakeSender{data: data}
	var out bytes.Buffer
	befores := 0
	sent, err := sendRange(context.Background(), f, &out, n, func() { befores++ })
	if err != nil || sent != n-httpChunk || !bytes.Equal(out.Bytes(), data[:n-httpChunk]) || f.Remaining() != httpChunk {
		t.Fatalf("%d sent, %v, %d left", sent, err, f.Remaining())
	}
	if befores != len(f.asked) || len(f.asked) != 3 || slices.Max(f.asked) > sendPiece {
		t.Errorf("pieces %v with %d deadline steps", f.asked, befores)
	}
	f = &fakeSender{data: data, short: 1}
	if sent, err := sendRange(context.Background(), f, io.Discard, n, func() {}); !errors.Is(err, checkedread.ErrShort) || sent != sendPiece-1 {
		t.Errorf("a stream short of what was asked: %d, %v, want checkedread.ErrShort", sent, err)
	}
	cause := errors.New("the key was revoked")
	ctx, cancel := context.WithCancelCause(context.Background())
	f = &fakeSender{data: data}
	sent, err = sendRange(ctx, f, io.Discard, n, func() {
		if len(f.asked) == 0 {
			cancel(cause)
		}
	})
	if !errors.Is(err, cause) || sent != sendPiece {
		t.Errorf("a canceled request: %d sent, %v, want one piece and the cause", sent, err)
	}
}
