package check

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filewriter"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// refusal checks path for needles under opts and fails unless the check
// refuses with cause, never a pass.
func refusal(t *testing.T, opts Options, path string, cause Cause, needles ...engine.Needle) {
	t.Helper()
	_, err := New(opts).checkPath(t, context.Background(), path, needles, nil)
	var r *Refusal
	if !errors.As(err, &r) || r.Cause != cause {
		t.Fatalf("want a refusal %s, got %v", cause, err)
	}
	if r.Reason() != ReasonFailed || r.Detail() != string(cause) {
		t.Errorf("reason %q, detail %q", r.Reason(), r.Detail())
	}
}

// TestBoundsRefuseNeverPass guards every bound: a file past MaxFileBytes,
// a value past MaxValueBytes, more needles than MaxNeedles, a needle past
// MaxNeedleBytes, sequences nested past the deepest walked, a file the
// reader cannot read and a missing file are each refused, never passed.
func TestBoundsRefuseNeverPass(t *testing.T) {
	n := needle(engine.NeedleText, "MRN-778812")
	p := writeFile(t, el{0x00204000, "LT", strings.Repeat("x", 3000)})
	t.Run("file size", func(t *testing.T) {
		refusal(t, Options{MaxFileBytes: 1000}, p, CauseTooLarge, n)
	})
	t.Run("value length", func(t *testing.T) {
		refusal(t, Options{MaxValueBytes: 1000}, p, CauseTooLarge, n)
	})
	t.Run("needle count", func(t *testing.T) {
		refusal(t, Options{MaxNeedles: 2}, p, CauseTooManyNeedles, n, n, n)
	})
	t.Run("needle length", func(t *testing.T) {
		refusal(t, Options{MaxNeedleBytes: 8}, p, CauseTooManyNeedles, n)
	})
	t.Run("depth", func(t *testing.T) {
		inner := build(t, el{0x00081030, "LO", "deep"})
		for range maxDepth + 2 {
			inner = build(t, el{0x00081115, "SQ", []*dataset.Dataset{inner}})
		}
		deep := writeDS(t, build(t, append(base(), el{0x00081110, "SQ", []*dataset.Dataset{inner}})...), filewriter.FileMetaInfo{})
		// The reader stops at its deepest nesting and keeps the rest
		// unread, so the file was not read to its end.
		refusal(t, Options{}, deep, CauseUnreadable, n)
	})
	t.Run("unreadable", func(t *testing.T) {
		b, err := os.ReadFile(p) //nolint:gosec // G304: the test's own temporary file.
		if err != nil {
			t.Fatal(err)
		}
		cut := filepath.Join(t.TempDir(), "cut.dcm")
		// The file ends inside the long value's stated length.
		if err := os.WriteFile(cut, b[:len(b)-1500], 0o600); err != nil { //nolint:gosec // G703: the test's own temporary directory.
			t.Fatal(err)
		}
		refusal(t, Options{}, cut, CauseUnreadable, n)
	})
	t.Run("missing", func(t *testing.T) {
		_, err := New(Options{}).checkPath(t, context.Background(), filepath.Join(t.TempDir(), "none"), engine.Needles{n}, nil)
		if err == nil {
			t.Fatal("a missing file passed")
		}
	})
}

// TestLongBinaryValueIsSearchedRaw guards that a binary value longer than
// MaxValueBytes, left in the file unread, is still searched in the raw
// bytes rather than passed over.
func TestLongBinaryValueIsSearchedRaw(t *testing.T) {
	v := []byte(strings.Repeat("\x01", 1500) + "MRN-778812" + strings.Repeat("\x01", 500))
	p := writeFile(t, el{0x00090010, "LO", "ACME 1.0"}, el{0x00091003, "OB", v})
	rep, err := New(Options{MaxValueBytes: 1000}).checkPath(t, context.Background(), p, engine.Needles{needle(engine.NeedleText, "MRN-778812")}, nil)
	if res := (*Residual)(nil); !errors.As(err, &res) || rep.Findings[0].Path != "00091003" || !rep.Findings[0].Raw {
		t.Fatalf("got %v %+v", err, rep.Findings)
	}
}

// TestAdversarialTextStaysLinear guards the search against text built to
// make it slow: a megabyte of digits, where every position ends a match of
// a numeric needle whose edges then fail, and many needles, is searched in
// one pass that spends one unit of work for each such match and no more.
func TestAdversarialTextStaysLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a megabyte file")
	}
	const size = 1 << 20
	p := writeFile(t, el{0x00204000, "UT", strings.Repeat("1", size)})
	needles := engine.Needles{needle(engine.NeedleText, "11111")}
	for i := range 200 {
		needles = append(needles, needle(engine.NeedleText, "ZZZ"+strings.Repeat("Q", i%20)+"Y"))
	}
	c := New(Options{})
	var spent int64
	c.seams.work = func(n int64) { spent = n }
	if _, err := c.checkPath(t, context.Background(), p, needles, nil); err != nil {
		t.Fatalf("got %v", err)
	}
	// The text and the raw bytes each end a match at nearly every digit.
	if spent < size || spent > 3*size {
		t.Errorf("%d units spent on %d digits", spent, size)
	}
}

// TestCancelStops guards that a canceled context ends the check with its
// error, not a pass.
func TestCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := writeFile(t)
	if _, err := New(Options{}).checkPath(t, ctx, p, engine.Needles{needle(engine.NeedleText, "MRN-778812")}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
