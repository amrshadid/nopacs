package cli_test

import (
	"strings"
	"testing"
)

// TestKeyCreateMasked guards "key create --masked": the request asks for a
// masked key, and the key's details and --json say it is one; a key made
// without the flag says nothing of it.
func TestKeyCreateMasked(t *testing.T) {
	r := newRig(t)
	r.must("key", "create", "--holder", "review agent", "--masked")
	r.must("key", "create", "--holder", "a person")
	if s := r.run("key", "show", "k00000000001").stdout; !strings.Contains(s, "Masked:") || !strings.Contains(s, "never answered an original value") {
		t.Errorf("a masked key's details:\n%s", s)
	}
	if d := document(t, r.run("key", "show", "k00000000001", "--json")); d["key"].(map[string]any)["masked"] != true {
		t.Errorf("a masked key's --json: %v", d)
	}
	if s := r.run("key", "show", "k00000000002").stdout; strings.Contains(s, "Masked") {
		t.Errorf("a plain key's details speak of masking:\n%s", s)
	}
}
