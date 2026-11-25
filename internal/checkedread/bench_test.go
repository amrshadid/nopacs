package checkedread_test

import (
	"io"
	"testing"

	"github.com/amrshadid/nopacs/internal/checkedread"
)

// BenchmarkOpenFromPrimary measures the normal read of a 64 KiB instance,
// opened and read whole and checked, with no replica and with a replica in
// step: the turn to a replica asks the database only after the primary
// fails, so the two must cost the same.
func BenchmarkOpenFromPrimary(b *testing.B) {
	for _, c := range []struct {
		name          string
		replica, bare bool
	}{{"storage read alone", false, true}, {"no replica", false, false}, {"replica in step", true, false}} {
		b.Run(c.name, func(b *testing.B) {
			f := newFixture(b, 1, 64<<10)
			if c.replica {
				f.attach("copy1")
				f.inStep("copy1")
			}
			s, _ := f.stores()
			in := f.held(uid(1))
			obj := checkedread.InstanceObject(in, f.series, f.path)
			bufs := [2][]byte{make([]byte, 32<<10), make([]byte, 32<<10)}
			sum := in.ArchivedSHA256
			b.SetBytes(in.Length)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var body io.ReadCloser
				if c.bare {
					// The read as every reader made it before the turn to a
					// replica: the storage opened by name and read.
					st, err := s.Get(f.ctx, in.Storage)
					if err != nil {
						b.Fatal(err)
					}
					if body, err = st.Read(f.ctx, in.Key, in.Offset, in.Length); err != nil {
						b.Fatal(err)
					}
				} else {
					o, err := s.Open(f.ctx, obj, in.Offset, in.Length)
					if err != nil {
						b.Fatal(err)
					}
					body = o.Body
				}
				if _, err := checkedread.CopyChecked(body, in.Length, &sum, bufs, func([]byte) error { return nil }); err != nil {
					b.Fatal(err)
				}
				_ = body.Close()
			}
		})
	}
}

// BenchmarkOpenFromReplica measures a read the replica serves because the
// primary's object is missing: the cost of the turn (one failed open, one
// lookup of the replicas in step, one open of the replica).
func BenchmarkOpenFromReplica(b *testing.B) {
	f := newFixture(b, 1, 64<<10)
	f.attach("copy1")
	f.inStep("copy1")
	in := f.held(uid(1))
	if err := f.dirOf("main").Delete(f.ctx, in.Key); err != nil {
		b.Fatal(err)
	}
	s, _ := f.stores()
	obj := checkedread.InstanceObject(in, f.series, f.path)
	bufs := [2][]byte{make([]byte, 32<<10), make([]byte, 32<<10)}
	sum := in.ArchivedSHA256
	b.SetBytes(in.Length)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		o, err := s.Open(f.ctx, obj, in.Offset, in.Length)
		if err != nil || !o.Replica {
			b.Fatal(err)
		}
		if _, err := checkedread.CopyChecked(o.Body, in.Length, &sum, bufs, func([]byte) error { return nil }); err != nil {
			b.Fatal(err)
		}
		_ = o.Body.Close()
	}
}
