package sqlkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCursorRoundTrip guards that a cursor gives back the fields it was made
// with, including empty fields and fields holding the separator, and is
// URL-safe.
func TestCursorRoundTrip(t *testing.T) {
	fields := []string{"", "a:b", "12:34", "名前", strings.Repeat("x", 100)}
	c := EncodeCursor("audit", Binding("lab", "K1"), fields...)
	got, err := DecodeCursor(c, "audit", Binding("lab", "K1"), len(fields))
	if err != nil {
		t.Fatal(err)
	}
	for i := range fields {
		if got[i] != fields[i] {
			t.Errorf("field %d = %q, want %q", i, got[i], fields[i])
		}
	}
	if strings.ContainsAny(c, "+/=") {
		t.Errorf("cursor %q is not URL-safe", c)
	}
}

// TestBinding guards that a binding tells apart arguments that differ in
// any part, including where one part ends and the next begins.
func TestBinding(t *testing.T) {
	if first, again := Binding("a", "b"), Binding("a", "b"); first != again {
		t.Error("the same parts give different bindings")
	}
	distinct := [][]string{{"a", "bc"}, {"ab", "c"}, {"abc"}, {"a", "bc", ""}, {"A", "bc"}, {}}
	seen := map[string]int{}
	for i, parts := range distinct {
		b := Binding(parts...)
		if j, ok := seen[b]; ok {
			t.Errorf("parts %q and %q give one binding", distinct[j], parts)
		}
		seen[b] = i
	}
}

// sealRaw makes a cursor from a raw body with a correct check, so the tests
// below reach the parser rather than stopping at the check.
func sealRaw(body string) string {
	sum := sha256.Sum256([]byte(body))
	return base64.RawURLEncoding.EncodeToString(append([]byte(body), sum[:checkLength]...))
}

// TestCursorRefuses guards that a cursor is refused as Invalid when it was
// made for another list, with another binding (another key, scope or
// filter), with another number of fields, by another format version, or
// was altered, cut short or is not a cursor at all; never read as some other
// place in a list.
func TestCursorRefuses(t *testing.T) {
	good := EncodeCursor("keys", "", "123", "k1")
	if _, err := DecodeCursor(good, "keys", "", 2); err != nil {
		t.Fatalf("the good cursor is refused: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(good)
	flipped := bytes.Clone(raw)
	flipped[3] ^= 0x01
	// One digit of a field changed, the rest well formed and the old check
	// kept: only the check can refuse this one.
	digit := bytes.Replace(bytes.Clone(raw), []byte("3:123"), []byte("3:124"), 1)
	if bytes.Equal(digit, raw) {
		t.Fatal("the cursor's body does not hold the field as written")
	}
	cases := map[string]string{
		"other list":        EncodeCursor("tenants", "", "123", "k1"),
		"prefix list":       EncodeCursor("keysx", "", "123", "k1"),
		"other binding":     EncodeCursor("keys", "K2", "123", "k1"),
		"fewer fields":      EncodeCursor("keys", "", "123"),
		"more fields":       EncodeCursor("keys", "", "123", "k1", "x"),
		"altered byte":      base64.RawURLEncoding.EncodeToString(flipped),
		"altered digit":     base64.RawURLEncoding.EncodeToString(digit),
		"altered field":     strings.Replace(good, good[6:8], "AA", 1),
		"cut short":         good[:len(good)-2],
		"cut to the check":  base64.RawURLEncoding.EncodeToString(raw[len(raw)-checkLength:]),
		"not base64":        "!!!",
		"empty":             "=",
		"too long":          strings.Repeat("A", MaxCursorLength+1),
		"other version":     sealRaw("2:c2" + "4:keys" + "0:" + "3:123" + "2:k1"),
		"leading zero size": sealRaw("2:c1" + "4:keys" + "0:" + "03:123" + "2:k1"),
		"negative size":     sealRaw("2:c1" + "4:keys" + "0:" + "-1:123" + "2:k1"),
		"size past the end": sealRaw("2:c1" + "4:keys" + "0:" + "3:123" + "9:k1"),
	}
	for name, c := range cases {
		_, err := DecodeCursor(c, "keys", "", 2)
		if fault.KindOf(err) != fault.Invalid {
			t.Errorf("%s: err = %v, want Invalid", name, err)
		}
	}
	if _, err := DecodeCursor(sealRaw("2:c1"+"4:keys"+"0:"+"3:123"+"2:k1"), "keys", "", 2); err != nil {
		t.Errorf("a hand-made well-formed cursor is refused (%v), so the cases above test nothing", err)
	}
	if _, err := DecodeCursorInt("12x"); fault.KindOf(err) != fault.Invalid {
		t.Errorf("DecodeCursorInt(12x) = %v, want Invalid", err)
	}
}

// TestEventCursorHistory guards the history an event cursor names: a cursor
// gives back its id and history; one of another history, or past the
// newest event, is refused as a changed history, and one before the mark
// as expired; a cursor of the format before histories, which names none,
// is refused as a changed history rather than as never valid; another
// tenant's is foreign; and the empty cursor starts at the mark.
func TestEventCursorHistory(t *testing.T) {
	const list, tenantBinding = "tenant-events", "t1"
	h1, h2 := strings.Repeat("a", 32), strings.Repeat("b", 32)
	id, history, err := EventPosition(EventCursor(list, tenantBinding, 41, h1), list, tenantBinding)
	if err != nil || id != 41 || history != h1 {
		t.Fatalf("EventPosition = %d, %q, %v; want 41, %q", id, history, err, h1)
	}
	if from, err := EventFrom(41, h1, 50, 10, h1); err != nil || from != 41 {
		t.Errorf("EventFrom of the same history = %d, %v; want 41", from, err)
	}
	if from, err := EventFrom(-1, "", 50, 10, h1); err != nil || from != 10 {
		t.Errorf("EventFrom of the empty cursor = %d, %v; want the mark 10", from, err)
	}
	refused := []struct {
		name          string
		pos           int64
		history       string
		target        error
		reason        string
		historyChange bool
	}{
		{"another history", 41, h2, tenant.ErrHistoryChanged, tenant.ReasonHistoryChanged, true},
		{"another history before the mark", 3, h2, tenant.ErrHistoryChanged, tenant.ReasonHistoryChanged, true},
		{"past the newest event", 51, h1, tenant.ErrHistoryChanged, tenant.ReasonHistoryChanged, true},
		{"before the mark", 3, h1, tenant.ErrCursorExpired, tenant.ReasonCursorExpired, false},
	}
	for _, c := range refused {
		_, err := EventFrom(c.pos, c.history, 50, 10, h1)
		if !errors.Is(err, c.target) || !errors.Is(err, tenant.ErrCursorExpired) || fault.KindOf(err) != fault.OutOfRange ||
			errors.Is(err, tenant.ErrHistoryChanged) != c.historyChange || !strings.HasPrefix(fault.Message(err), c.reason+":") {
			t.Errorf("%s: EventFrom = %v (kind %v), want %v of kind OutOfRange beginning %s", c.name, err, fault.KindOf(err), c.target, c.reason)
		}
	}
	old := EncodeCursor(list, "", tenantBinding, "41")
	id, history, err = EventPosition(old, list, tenantBinding)
	if err != nil || id != 41 || history != "" {
		t.Fatalf("EventPosition of a cursor without a history = %d, %q, %v; want 41 and no history", id, history, err)
	}
	if _, err := EventFrom(id, history, 50, 10, h1); !errors.Is(err, tenant.ErrHistoryChanged) {
		t.Errorf("EventFrom of a cursor without a history = %v, want ErrHistoryChanged", err)
	}
	if _, _, err := EventPosition(EventCursor(list, "t2", 41, h1), list, tenantBinding); !errors.Is(err, tenant.ErrForeignCursor) {
		t.Errorf("EventPosition of another tenant's cursor = %v, want ErrForeignCursor", err)
	}
	if _, _, err := EventPosition(EncodeCursor(list, "", tenantBinding, "41", h1, "x"), list, tenantBinding); fault.KindOf(err) != fault.Invalid {
		t.Errorf("EventPosition of a cursor with a field too many = %v, want Invalid", err)
	}
}
