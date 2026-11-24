package place

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// echoSealer seals by prefixing the context, so a test sees what a seal is
// bound to.
type echoSealer struct{ fail error }

func (s echoSealer) Seal(context string, plaintext []byte) ([]byte, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	return append([]byte(context+"|"), plaintext...), nil
}

// TestSealCredentialBindsToTheRow guards that a sender's credential is
// sealed for its tenant's id and its own name, that the plaintext is what is
// sealed, and that a seal that fails or a sealed value a store would refuse
// is the server's fault.
func TestSealCredentialBindsToTheRow(t *testing.T) {
	got, err := SealCredential(echoSealer{}, "t1", "pacs", "s3cret")
	if err != nil || !bytes.Equal(got, []byte("tenant/t1/sender/pacs/credential|s3cret")) {
		t.Fatalf("%q, %v", got, err)
	}
	if CredentialContext("t1", "pacs") == CredentialContext("t2", "pacs") || CredentialContext("t1", "pacs") == CredentialContext("t1", "other") {
		t.Error("two rows share a context")
	}
	if _, err := SealCredential(echoSealer{fail: errors.New("no key")}, "t1", "pacs", "x"); fault.KindOf(err) != fault.Internal {
		t.Errorf("a failed seal: %v", err)
	}
	if _, err := SealCredential(echoSealer{}, "t1", "pacs", strings.Repeat("x", 9000)); fault.KindOf(err) != fault.Internal {
		t.Errorf("a sealed value too long: %v", err)
	}
}
