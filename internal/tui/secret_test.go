package tui

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// holds reports whether any string or byte slice reachable from v, through
// pointers, interfaces, slices, arrays, maps and struct fields (unexported
// ones too), contains text. A function's captured variables cannot be reached
// this way; the screens keep no closure that captures a secret, and the test
// of leaving shows the value itself is zeroed.
func holds(v reflect.Value, text string, seen map[uintptr]bool) bool {
	switch v.Kind() {
	case reflect.String:
		return strings.Contains(v.String(), text)
	case reflect.Pointer:
		if v.IsNil() || seen[v.Pointer()] {
			return false
		}
		seen[v.Pointer()] = true
		return holds(v.Elem(), text, seen)
	case reflect.Interface:
		return !v.IsNil() && holds(v.Elem(), text, seen)
	case reflect.Struct:
		for i := range v.NumField() {
			if holds(v.Field(i), text, seen) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return bytes.Contains(v.Bytes(), []byte(text))
		}
		if v.Kind() == reflect.Slice && !v.IsNil() {
			if seen[v.Pointer()] && v.Len() > 0 {
				// Same backing array seen before; its elements were walked.
				return false
			}
			seen[v.Pointer()] = true
		}
		for i := range v.Len() {
			if holds(v.Index(i), text, seen) {
				return true
			}
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if holds(it.Key(), text, seen) || holds(it.Value(), text, seen) {
				return true
			}
		}
	}
	return false
}

// stateHolds says whether the model's state, everything the program keeps
// between two messages, holds text anywhere: every field of the root model but
// the shared context, which is left out because it holds
// the HTTP client, whose connections are being used by other goroutines while
// the test looks, and nothing a screen puts there but its own id.
func stateHolds(m model, text string) bool {
	seen := map[uintptr]bool{}
	v := reflect.ValueOf(&m).Elem()
	for i := range v.NumField() {
		if v.Type().Field(i).Name == "c" {
			continue
		}
		if holds(v.Field(i), text, seen) {
			return true
		}
	}
	return false
}

// makeKey drives the program through "n", a holder and the default expiry, to
// the screen that shows the new key.
func makeKey(r *rig, holder string) {
	r.t.Helper()
	r.key("2", "n")
	r.typeText(holder)
	r.key("enter", "enter")
}

// TestNewKeyIsShownOnOneScreenOnly guards the central secret rule over a
// scripted session: the key is drawn on the screen made to show it, with the
// words that it will not be shown again, and in no frame of any other screen,
// no error line, no formatted dump of the program's state and no structured
// log; and once that screen is left no state of the program holds it, and the
// bytes that held it are zeroed.
func TestNewKeyIsShownOnOneScreenOnly(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, func(f *fakeserver.Server) { addTenants(f, 3) })
		makeKey(r, "ci runner")
		r.expect(testSecret, "New key", "ci runner", "k00000000001", "This is the only time the key is shown. It will not be shown again.", "enter leave (asks once)")
		if _, ok := r.m.top().(secretScreen); !ok {
			t.Fatalf("the top screen is %T, want the key's screen", r.m.top())
		}
		// The walker must be able to see the secret where it is, or its
		// silence later proves nothing.
		if !stateHolds(r.m, testSecret) {
			t.Fatal("the state walker did not find the secret on the screen that holds it")
		}
		kept := r.m.top().(secretScreen).secret

		// The secret never prints, by any verb, and never logs.
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if got := fmt.Sprintf(verb, kept); strings.Contains(got, "SSSS") || got != "[secret hidden]" {
				t.Errorf("%s of a secret prints %q", verb, got)
			}
		}
		var logged bytes.Buffer
		slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "key", kept, "state", r.m.c.pending)
		if strings.Contains(logged.String(), "SSSS") {
			t.Errorf("a log line holds the secret: %s", logged.String())
		}
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(verb, r.m.stack), "SSSS") {
				t.Errorf("%s of the screen stack holds the secret", verb)
			}
		}

		// Leaving asks once, and any other key stays.
		r.key("esc")
		r.expect(testSecret, "Leave? The key will be gone for good.", "y leaves, any other key stays")
		r.key("x")
		r.expect(testSecret, "Press enter when it is stored.")
		r.key("enter", "y")

		// The key list it returns to shows the new key's id, and nothing
		// of the secret anywhere.
		r.expect("Keys", "k00000000001", "ci runner")
		if stateHolds(r.m, testSecret) || stateHolds(r.m, "SSSSSSSS") {
			t.Error("the program's state still holds the secret after the screen was left")
		}
		if got := string(*kept.b); strings.Trim(got, "\x00") != "" {
			t.Errorf("the bytes of the secret were not zeroed: %q", got)
		}
		for i, f := range r.frames {
			if strings.Contains(f, "SSSSSSSS") && r.tops[i] != "tui.secretScreen" {
				t.Errorf("frame %d, of %s, shows the secret:\n%s", i, r.tops[i], f)
			}
		}
	})
}

// TestSecretStaysOutOfErrors guards the error path of the same rule: a failure
// right after the key was made, with the server's message on the bottom line,
// never carries the secret, and a whole key typed into a key id field is wiped
// from the field and refused.
func TestSecretStaysOutOfErrors(t *testing.T) {
	r := newRig(t, 80, 24, func(f *fakeserver.Server) { addTenants(f, 1) })
	makeKey(r, "ci")
	r.key("enter", "y")
	r.key("esc")
	r.key("1", "enter", "g", "s")
	r.typeText(testSecret)
	r.key("enter", "enter")
	r.expect("that is a whole key")
	r.refuse("SSSSSSSS")
	for i, f := range r.frames {
		if strings.Contains(f, "SSSSSSSS") && r.tops[i] != "tui.secretScreen" {
			t.Errorf("frame %d, of %s, shows the secret", i, r.tops[i])
		}
	}
}

// TestSecretScreenQuitKeysAsk guards "q asks first only on the screen that
// shows a new key": q and ctrl+c there ask, and y then ends the program with
// the secret wiped; on every other screen they quit at once.
func TestSecretScreenQuitKeysAsk(t *testing.T) {
	for _, quit := range []string{"q", "ctrl+c"} {
		r := newRig(t, 80, 24, nil)
		makeKey(r, "ci")
		kept := r.m.top().(secretScreen).secret
		r.key(quit)
		if r.quit {
			t.Fatalf("%s quit the program at once from the key's screen", quit)
		}
		r.expect(testSecret, "Leave? The key will be gone for good.")
		r.key("n")
		if r.quit {
			t.Fatalf("n quit the program")
		}
		r.key(quit, "y")
		if !r.quit {
			t.Errorf("%s then y did not quit", quit)
		}
		if strings.Trim(string(*kept.b), "\x00") != "" {
			t.Errorf("the secret was not zeroed on the way out")
		}
	}
	for _, quit := range []string{"q", "ctrl+c"} {
		r := newRig(t, 80, 24, nil)
		r.key("2")
		r.key(quit)
		if !r.quit {
			t.Errorf("%s did not quit from the key list", quit)
		}
	}
}

// TestNewKeyFormValidates guards the form before any call: an empty holder and
// an expiry that is not a length of time are refused on the form, and "never"
// is accepted.
func TestNewKeyFormValidates(t *testing.T) {
	r := newRig(t, 80, 24, nil)
	r.key("2", "n")
	r.expect("New key", "Holder", "Expires", "90d")
	r.key("enter", "enter")
	r.expect("the holder is required")
	if got := r.calls("CreateKey"); got != 0 {
		t.Fatalf("CreateKey was called with no holder")
	}
	r.key("shift+tab")
	r.typeText("ci")
	r.key("enter", "ctrl+u")
	r.typeText("soon")
	r.key("enter")
	r.expect("the expiry is not a length of time")
	if got := r.calls("CreateKey"); got != 0 {
		t.Fatalf("CreateKey was called with a bad expiry")
	}
	r.key("ctrl+u")
	r.typeText("never")
	r.key("enter")
	r.expect(testSecret, "Expires  never")
}
