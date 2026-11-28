package leaktest

import (
	"log/slog"
	"os"
	"testing"
)

// TestMain keeps the DICOM library's warnings (it notes that Other Patient
// IDs, which the seeded files carry, is retired) out of the test's output.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.DiscardHandler))
	os.Exit(m.Run())
}
