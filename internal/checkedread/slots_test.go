package checkedread

import (
	"strconv"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestOpenReadsBounds guards the bounds of open reads: a caller's 17th is
// refused as resource exhausted while another caller's first is not; one
// ended frees its place, once however often its end is called; and the
// server's 257th is refused whoever asks.
func TestOpenReadsBounds(t *testing.T) {
	var o Slots
	var ends []func()
	for range MaxOpenReads {
		end, err := o.Take("key/a")
		if err != nil {
			t.Fatal(err)
		}
		ends = append(ends, end)
	}
	if _, err := o.Take("key/a"); fault.KindOf(err) != fault.Exhausted {
		t.Fatalf("the 17th read of one caller: %v, want exhausted", err)
	}
	endB, err := o.Take("key/b")
	if err != nil {
		t.Fatalf("another caller's first read: %v", err)
	}
	ends[0]()
	ends[0]()
	if _, err := o.Take("key/a"); err != nil {
		t.Fatalf("a read after one ended: %v", err)
	}
	if _, err := o.Take("key/a"); fault.KindOf(err) != fault.Exhausted {
		t.Fatalf("an end called twice freed two places: %v", err)
	}
	endB()
	var all Slots
	for i := range MaxServerReads {
		if _, err := all.Take("key/" + strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := all.Take("local"); fault.KindOf(err) != fault.Exhausted {
		t.Fatalf("the server's 257th read: %v, want exhausted", err)
	}
}

// TestOpenReadsLimitsAreLive guards the bounds read at every Take: a raised
// bound admits the next read, a lowered one refuses the next read but cuts
// none that is open (and the ends of those reads still free their places),
// a caller's bound above the server's is held to the server's, and the
// defaults are those of the settings.
func TestOpenReadsLimitsAreLive(t *testing.T) {
	if MaxOpenReads != settings.DefaultReadPerCaller || MaxServerReads != settings.DefaultReadPerServer {
		t.Fatalf("defaults %d and %d differ from the settings' %d and %d", MaxOpenReads, MaxServerReads, settings.DefaultReadPerCaller, settings.DefaultReadPerServer)
	}
	perCaller, perServer := 2, 16
	o := &Slots{Limits: func() (int, int) { return perCaller, perServer }}
	var ends []func()
	for range 2 {
		end, err := o.Take("key/a")
		if err != nil {
			t.Fatal(err)
		}
		ends = append(ends, end)
	}
	if _, err := o.Take("key/a"); fault.KindOf(err) != fault.Exhausted {
		t.Fatalf("a third read at a bound of 2: %v", err)
	}
	perCaller = 3
	end, err := o.Take("key/a")
	if err != nil {
		t.Fatalf("a third read after the bound was raised to 3: %v", err)
	}
	ends = append(ends, end)
	perCaller = 1
	if _, err := o.Take("key/a"); fault.KindOf(err) != fault.Exhausted {
		t.Fatalf("a read at a bound lowered to 1 with 3 open: %v", err)
	}
	// Ending two of three leaves one open, which fills the bound of 1.
	ends[0]()
	ends[1]()
	if _, err := o.Take("key/a"); fault.KindOf(err) != fault.Exhausted {
		t.Fatalf("a read with one open at a bound of 1: %v", err)
	}
	ends[2]()
	end, err = o.Take("key/a")
	if err != nil {
		t.Fatalf("a read once none is open: %v", err)
	}
	end()
	// A caller's bound above the server's is the server's.
	perCaller, perServer = 100, 16
	var held []func()
	for range 16 {
		end, err := o.Take("key/b")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, end)
	}
	if _, err := o.Take("key/b"); fault.KindOf(err) != fault.Exhausted {
		t.Fatalf("a 17th read at a server bound of 16: %v", err)
	}
	// A server bound lowered below what is open takes nothing back, and
	// refuses another caller's first read.
	perServer = 16
	perCaller = 4
	if _, err := o.Take("key/c"); fault.KindOf(err) != fault.Exhausted {
		t.Fatalf("another caller's read with the server full: %v", err)
	}
	for _, e := range held {
		e()
	}
	if _, err := o.Take("key/c"); err != nil {
		t.Fatalf("a read once the server has room: %v", err)
	}
}
