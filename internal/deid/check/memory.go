package check

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/go-dicom/config"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// What a check holds beside the elements it read, for its reservation.
// The copies are counted in bytes of the decoded text at its largest
// (textSpread times the bytes read), and were measured against the heap
// the search holds at its peak (TestReservationCoversDecodedText).
const (
	// textSpread is the most bytes of UTF-8 a byte of text becomes once
	// decoded: three for a byte of a single-byte set outside ASCII (TIS
	// 620's Thai letters, U+0E01 and on, are three bytes of UTF-8 each),
	// and no more for the two bytes of a character of a multi-byte set;
	// NFC (UAX #15) grows a text of UTF-8 by at most three times too. A
	// text of ASCII is decoded as it is.
	textSpread = 3
	// textCopies is how many times its decoded bytes a text value is held
	// while it is searched: as the library's decoder builds it and as the
	// string it returns, in NFC when it was not, and folded. Measured for a
	// text of 4 MiB in TIS 620 (12 MiB decoded): at most 26 MiB held, a
	// little over two copies, without an NFC copy.
	textCopies = 4
	// looseCopies is how many times its bytes, read, a value of unknown
	// kind read whole (at most looseMax) is held: as read and as UTF-8,
	// and under two character sets and folded, each of those up to
	// textSpread times its bytes, with the decoder's own copies.
	looseCopies = 16
	// streamRoom is what reading a long value as a text stream holds
	// beside the window: the reader's buffers, folded.
	streamRoom = 160 << 10
	// automatonBytes is the memory of an automaton a byte of its patterns
	// at most (a node a byte): 19 bytes a node (textmatch.Matcher), in slices that
	// grow by doubling and so hold up to three times that while one is
	// copied, and the patterns and their index beside it.
	automatonBytes = 64
)

// reservation is what a check of the size bytes of p, for set, takes from
// Options.Memory, once the raw forms are written (rawBytes of distinct
// forms, the longest rawLongest): a window of the raw bytes and the
// longest raw form on top of it (at most the file), the reader's buffer,
// the longest text value textCopies times and the longest value of
// unknown kind read whole looseCopies times (each at most MaxValueBytes,
// the text counted decoded, textSpread times a byte not ASCII),
// streamRoom when a value is read as a stream, and the automata
// (formBytes). It does not grow with the file: a 128 MiB image with a
// header of a few kilobytes takes some 1.1 MiB. The elements read (the
// file's header, no value past heldBinary of a binary VR) are not in it:
// they are bounded as the engine's own data set of the header is, by the
// receiver's header limit.
func (c *Checker) reservation(size int64, p *parsed, set formSet, rawBytes, rawLongest int) int64 {
	n := min(size, window) + int64(rawLongest+2*edgeRoom) + min(size, readerBuffer)
	n += textCopies*p.largestText + looseCopies*p.largestLoose + c.formBytes(set, rawBytes)
	if p.streams {
		n += streamRoom
	}
	return n
}

// formBytes is what the two automata of set hold, automatonBytes a byte of
// their patterns: the text's, of the folded forms, and the raw bytes', of
// the rawBytes of distinct raw forms, never more than the bound on the
// forms' bytes together, past which the raw search refuses the file
// before it builds its automaton.
func (c *Checker) formBytes(set formSet, rawBytes int) int64 {
	return automatonBytes * int64(set.bytes+min(rawBytes, c.opts.MaxFormBytes))
}

// Reservation is what a check with the default options takes from the
// server's bound at most, for a file of size bytes whose longest text value
// is largestText bytes as written, for needles, the file received in
// characterSet: a window of its raw bytes, the reader's buffer, a value of
// unknown kind read whole or as a stream, the longest text decoded
// (textSpread) textCopies times, the automata of the needles' forms,
// written as the check writes them, and those its text lists are searched
// with (listsBytes). A caller that takes the check's memory
// itself, for a file it holds, takes this. Needles the check would refuse,
// or whose forms pass its budget, count nothing: the check refuses them
// before it holds anything. ctx is read as the forms are written.
func Reservation(ctx context.Context, size, largestText int64, needles engine.Needles, characterSet *string) int64 {
	n := min(size, window) + min(size, readerBuffer) + looseCopies*min(size, looseMax)
	if size > looseMax {
		n += streamRoom
	}
	n += textCopies * textSpread * min(largestText, DefaultMaxValueBytes)
	c := New(Options{})
	set, err := c.forms(needles)
	if err != nil {
		return n
	}
	quiet := config.WithLogger(ctx, slog.New(slog.DiscardHandler))
	s := newSearcher(ctx, quiet, nil, set, sourceEncodings(characterSet), c.opts, nil, seams{})
	if s.encodeRaw() != nil || s.prepareLists() != nil {
		return n
	}
	return n + int64(s.rawLongest+2*edgeRoom) + c.formBytes(set, s.rawBytes) + s.listsBytes()
}

// Memory is a bound on the bytes the server's de-identification work holds
// in memory at once, across every tenant and every worker: what each
// check after holds (a window of the file, one value), and a trial's or a
// job's copies. Budget is one.
type Memory interface {
	// Reserve takes n bytes of the bound and returns the function that
	// gives them back, which may be called more than once. It waits while
	// the bytes taken by others leave no room, until ctx ends (ctx's
	// error). n past the whole bound is a *Refusal with CauseTooLarge,
	// answered at once: such a file would wait for ever.
	Reserve(ctx context.Context, n int64) (release func(), err error)
}

// recheck is how often a reservation that waits reads the bound again, so a
// bound raised while it waits lets it in without a release by another.
const recheck = time.Second

// BoundInterval is how long a Budget of the server keeps the bound it read
// (NewBudget's every): a setting changed takes effect within it, and a
// reception of many files reads the setting once a second, not once a file.
const BoundInterval = time.Second

// Budget is a Memory whose bound is read live (the installation's
// deid.memory), so a change takes effect for the next file without a
// restart. A bound lowered below what is taken now takes nothing back:
// reservations wait until enough is given back.
//
// Reservations are let in first come, first served (Reserve).
type Budget struct {
	limit func(ctx context.Context) (int64, error)
	every time.Duration

	mu   sync.Mutex
	used int64
	// changed is closed, and replaced, each time bytes are given back or
	// the line changes, so every waiter looks again.
	changed chan struct{}
	// line is the reservations that wait, in the order they came.
	line []*waiter
	// bound is the bound last read, at read.
	bound int64
	read  time.Time
}

// NewBudget returns a Budget whose bound is what limit answers, read again
// once what was read is older than every (zero reads it at every
// reservation); an error of limit is returned by Reserve, which then takes
// nothing.
func NewBudget(limit func(ctx context.Context) (int64, error), every time.Duration) *Budget {
	return &Budget{limit: limit, every: every, changed: make(chan struct{})}
}

// current returns the bound, read again when what was read is older than
// every.
func (b *Budget) current(ctx context.Context) (int64, error) {
	b.mu.Lock()
	if !b.read.IsZero() && time.Since(b.read) < b.every {
		defer b.mu.Unlock()
		return b.bound, nil
	}
	b.mu.Unlock()
	n, err := b.limit(ctx)
	if err != nil {
		return 0, err
	}
	b.mu.Lock()
	b.bound, b.read = n, time.Now()
	b.mu.Unlock()
	return n, nil
}

// Reserve implements Memory. Reservations are let in first come, first
// served: one that does not fit joins the line, and while anyone waits in
// it a newcomer waits behind them, even one that would fit, so a large
// file is never starved by a stream of small ones (one tenant's small
// files or dry run cannot hold back another tenant's large file).
func (b *Budget) Reserve(ctx context.Context, n int64) (func(), error) {
	if n <= 0 {
		return func() {}, nil
	}
	var me *waiter
	defer func() {
		if me != nil {
			b.leave(me)
		}
	}()
	for {
		limit, err := b.current(ctx)
		if err != nil {
			return nil, err
		}
		if n > limit {
			return nil, &Refusal{Cause: CauseTooLarge}
		}
		b.mu.Lock()
		first := len(b.line) == 0 || (me != nil && b.line[0] == me)
		if first && b.used+n <= limit {
			b.used += n
			if me != nil {
				b.line = b.line[1:]
				me = nil
				// The next in line may fit beside this one.
				b.wakeLocked()
			}
			b.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { b.give(n) }) }, nil
		}
		if me == nil {
			me = &waiter{n: n}
			b.line = append(b.line, me)
		}
		changed := b.changed
		b.mu.Unlock()
		t := time.NewTimer(recheck)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-changed:
		case <-t.C:
		}
		t.Stop()
	}
}

// waiter is one reservation waiting in a Budget's line: n bytes. It has a
// field so each waiter is a distinct pointer (pointers to zero-size values
// may compare equal).
type waiter struct{ n int64 }

// leave takes w out of the line, when it gave up waiting, and wakes the
// others: the next may now be first.
func (b *Budget) leave(w *waiter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, x := range b.line {
		if x == w {
			b.line = append(b.line[:i], b.line[i+1:]...)
			break
		}
	}
	b.wakeLocked()
}

// wakeLocked wakes every reservation that waits, to look again; b.mu is
// held.
func (b *Budget) wakeLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// Used returns the bytes taken now.
func (b *Budget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// give gives n bytes back and wakes every reservation that waits.
func (b *Budget) give(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	b.wakeLocked()
}
