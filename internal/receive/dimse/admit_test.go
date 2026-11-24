package dimse_test

import (
	"errors"
	"io"
	"testing"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/dimse"
)

// TestStatusIsSuccessOnlyForAFileKeptWhole guards the mapping of what the
// Sink answered, and how the file's writing ended, to the C-STORE status:
// success only for a file received whose writing ended cleanly; A700 for a
// refusal, a failure, or a file whose writing failed; 0117 and C000 for a
// command that cannot be written as a file.
func TestStatusIsSuccessOnlyForAFileKeptWhole(t *testing.T) {
	received := receive.Outcome{Status: receive.Received}
	cases := []struct {
		name string
		out  receive.Outcome
		werr error
		want uint16
	}{
		{"received", received, nil, 0x0000},
		{"received but the writing failed", received, io.ErrUnexpectedEOF, 0xA700},
		{"refused", receive.Outcome{Status: receive.Refused}, nil, 0xA700},
		{"failed", receive.Outcome{Status: receive.Failed}, nil, 0xA700},
		{"invalid instance UID", receive.Outcome{Status: receive.Refused}, errors.Join(network.ErrInvalidSOPInstanceUID), 0x0117},
		{"invalid command", receive.Outcome{Status: receive.Refused}, errors.Join(network.ErrInvalidStoreCommand), 0xC000},
	}
	for _, c := range cases {
		if got := dimse.StatusOf(c.out, c.werr); got != c.want {
			t.Errorf("%s: %04X, want %04X", c.name, got, c.want)
		}
	}
}
