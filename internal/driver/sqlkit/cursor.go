package sqlkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// MaxCursorLength bounds a cursor a caller hands back. A cursor holds the
// sort key of one row and a short binding; anything longer was not made here.
const MaxCursorLength = 1024

// cursorVersion is the first field of every cursor. A cursor of another
// version, from an older or newer binary, is refused rather than misread.
const cursorVersion = "c1"

// checkLength is how many bytes of SHA-256 end a cursor. It is a check
// against a cursor altered or cut short by accident, not a signature: a
// cursor holds nothing the caller was not shown, and a forged one can only
// start its own list at another place (a list that leaves a bound of its
// filter to the cursor checks the cursor against the filter first).
const checkLength = 8

// errBadCursor is what every refused cursor reports, so a caller learns
// nothing about which part was wrong.
func errBadCursor() error {
	return fault.New(fault.Invalid, "the page token is not one this list returned with these arguments")
}

// Binding returns a short, fixed-length text that stands for parts: the
// arguments a list was called with, such as a filter's fields. Two calls
// with the same parts give the same binding, and with different parts a
// different one. Each part is written with its length, so ("a", "bc") and
// ("ab", "c") differ.
func Binding(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// EncodeCursor returns an opaque cursor for a list, named by list, called
// with the arguments binding stands for (a key id, a scope, or Binding of a
// filter; empty when the list takes none), resuming after the row whose
// sort key is fields. A list resumes with a keyset condition such as
// "WHERE (a, b) > (?, ?)", never an offset, so rows inserted or removed
// meanwhile neither shift a page nor repeat a row.
//
// The API contract refuses a page token used with other arguments than it
// was made with; binding is what lets DecodeCursor do that.
func EncodeCursor(list, binding string, fields ...string) string {
	var b bytes.Buffer
	writeField(&b, cursorVersion)
	writeField(&b, list)
	writeField(&b, binding)
	for _, f := range fields {
		writeField(&b, f)
	}
	sum := sha256.Sum256(b.Bytes())
	b.Write(sum[:checkLength])
	return base64.RawURLEncoding.EncodeToString(b.Bytes())
}

// writeField writes one field with its length, so a field may hold any byte.
func writeField(b *bytes.Buffer, f string) {
	b.WriteString(strconv.Itoa(len(f)))
	b.WriteByte(':')
	b.WriteString(f)
}

// DecodeCursor returns the n sort-key fields of a cursor that EncodeCursor
// made for the same list and binding. A cursor that is too long, malformed,
// altered, cut short, of another version, made for another list or with
// another binding, or holding another number of fields, is Invalid, with one
// message for all of them.
func DecodeCursor(cursor, list, binding string, n int) ([]string, error) {
	if len(cursor) > MaxCursorLength {
		return nil, errBadCursor()
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) < checkLength {
		return nil, errBadCursor()
	}
	body, check := raw[:len(raw)-checkLength], raw[len(raw)-checkLength:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:checkLength], check) {
		return nil, errBadCursor()
	}
	var fields []string
	s := string(body)
	for s != "" {
		lengthText, rest, ok := strings.Cut(s, ":")
		if !ok {
			return nil, errBadCursor()
		}
		length, err := strconv.Atoi(lengthText)
		if err != nil || length < 0 || length > len(rest) || lengthText != strconv.Itoa(length) {
			return nil, errBadCursor()
		}
		fields = append(fields, rest[:length])
		s = rest[length:]
	}
	if len(fields) != 3+n || fields[0] != cursorVersion || fields[1] != list || fields[2] != binding {
		return nil, errBadCursor()
	}
	return fields[3:], nil
}

// DecodeCursorInt parses a field of a cursor that holds an integer, such as
// a time in microseconds. A field that is not one is Invalid.
func DecodeCursorInt(field string) (int64, error) {
	v, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return 0, errBadCursor()
	}
	return v, nil
}

// EventCursor returns the cursor of a tenant's event feed after the event
// with this id, in the history named, for the feed's list and the tenant's
// binding. The tenant and the history are fields rather than the cursor's
// binding, so that a cursor of another tenant or of another history is told
// apart from one altered (EventPosition).
func EventCursor(list, tenantBinding string, id int64, history string) string {
	return EncodeCursor(list, "", tenantBinding, strconv.FormatInt(id, 10), history)
}

// EventPosition decodes a cursor EventCursor made into the id it is after
// and its history, or -1 for the empty cursor. A cursor of another tenant
// holds tenant.ErrForeignCursor. A cursor of the feed made by an earlier
// version, which names no history, answers the empty history, which is
// none a feed holds: it is refused as one of another history, and its
// reader starts over from a status rather than being told its cursor was
// never valid. Any other cursor is Invalid.
func EventPosition(cursor, list, tenantBinding string) (int64, string, error) {
	if cursor == "" {
		return -1, "", nil
	}
	f, err := DecodeCursor(cursor, list, "", 3)
	if err != nil {
		old, oldErr := DecodeCursor(cursor, list, "", 2)
		if oldErr != nil {
			return 0, "", err
		}
		f = append(old, "")
	}
	if f[0] != tenantBinding {
		return 0, "", tenant.ForeignCursor()
	}
	id, err := DecodeCursorInt(f[1])
	if err != nil || id < 0 {
		return 0, "", errBadCursor()
	}
	return id, f[2], nil
}

// EventFrom returns the id a read of the feed starts after, from the
// position and history of its cursor (EventPosition) and the feed's place
// in the read's snapshot: the newest event, the mark of what was pruned,
// and the history. The empty cursor starts at the mark. A cursor of
// another history, or after the newest event, is refused with
// tenant.HistoryChanged; one before the mark with tenant.CursorExpired.
func EventFrom(pos int64, cursorHistory string, high, pruned int64, history string) (int64, error) {
	switch {
	case pos < 0:
		return pruned, nil
	case cursorHistory != history, pos > high:
		return 0, tenant.HistoryChanged()
	case pos < pruned:
		return 0, tenant.CursorExpired()
	}
	return pos, nil
}

// TimePosition is where a newest-first list resumes: the time and id of the
// last row its previous page returned.
type TimePosition struct {
	// At is the row's time, to the microsecond, in UTC.
	At time.Time
	// ID is the row's id, which orders rows of one time.
	ID string
}

// PageAfterTime checks page and decodes its cursor, made by TimeCursor for
// the same list and binding, or returns nil for the first page.
func PageAfterTime(page master.Page, list, binding string) (*TimePosition, error) {
	if err := page.Validate(); err != nil {
		return nil, err
	}
	if page.Cursor == "" {
		return nil, nil
	}
	f, err := DecodeCursor(page.Cursor, list, binding, 2)
	if err != nil {
		return nil, err
	}
	at, err := DecodeCursorInt(f[0])
	if err != nil {
		return nil, err
	}
	return &TimePosition{At: time.UnixMicro(at).UTC(), ID: f[1]}, nil
}

// TimeCursor returns the cursor after a row of a newest-first list, whose
// time is at and id is id. Times are kept to the microsecond, as every
// driver stores them, so the cursor names exactly the row's time.
func TimeCursor(list, binding string, at time.Time, id string) string {
	return EncodeCursor(list, binding, strconv.FormatInt(at.UnixMicro(), 10), id)
}

// MicrosText writes a filter's time for a binding: its microseconds, or the
// empty string for the zero time (no bound), so two filters bind alike
// exactly when they bound alike.
func MicrosText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.UnixMicro(), 10)
}

// Cut trims a page read with limit+1 rows to limit, and reports whether
// there was more and which row is the last one returned, whose sort key
// the next page's cursor holds. Reading one row past the page is how every
// list learns whether it ended without a second query.
func Cut[T any](rows []T, limit int) ([]T, T, bool) {
	var zero T
	if len(rows) <= limit {
		return rows, zero, false
	}
	rows = rows[:limit]
	return rows, rows[limit-1], true
}
