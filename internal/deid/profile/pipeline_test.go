package profile_test

import (
	"fmt"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestPipelineHandling guards the one list of the elements the pipeline
// handles in every file, which the engine acts by and the language
// refuses to name: what each element is handled as, and that a tag rule
// naming any handled element is refused while one naming an element next
// to them is not.
func TestPipelineHandling(t *testing.T) {
	cases := map[uint32]profile.Handling{
		0x00001000: profile.HandledRemoved, // Affected SOP Instance UID
		0x00020010: profile.HandledRemoved, // Transfer Syntax UID
		0x00100000: profile.HandledRemoved, // a group length
		0x00041500: profile.HandledRemoved, // Referenced File ID
		0x00041511: profile.HandledRemoved, // Referenced SOP Instance UID in File
		0x04000561: profile.HandledRemoved, // Original Attributes Sequence
		0x04000500: profile.HandledRemoved, // Encrypted Attributes Sequence
		0x4FFE0001: profile.HandledRemoved, // MAC Parameters Sequence
		0xFFFAFFFA: profile.HandledRemoved, // Digital Signatures Sequence
		0xFFFCFFFC: profile.HandledRemoved, // Data Set Trailing Padding
		0xFFFEE000: profile.HandledRemoved, // Item
		0x7FE00020: profile.HandledRemoved, // Coefficients SDVN, retired
		0x7FE00003: profile.HandledRemoved, // not a table of frames
		0x7FE00001: profile.HandledKept,    // Extended Offset Table
		0x7FE00002: profile.HandledKept,    // Extended Offset Table Lengths
		0x7FE00008: profile.HandledKept,    // Float Pixel Data
		0x7FE00009: profile.HandledKept,    // Double Float Pixel Data
		0x7FE00010: profile.HandledKept,    // Pixel Data
		0x00080005: profile.HandledWritten, // Specific Character Set
		0x00080016: profile.NotHandled,
		0x00100010: profile.NotHandled,
		0x00290010: profile.NotHandled, // a private creator follows its block
		0x04000562: profile.NotHandled, // Attribute Modification DateTime
	}
	cat := testCatalog(t)
	for tag, want := range cases {
		got, why := profile.Pipeline(tag)
		if got != want || (got != profile.NotHandled) != (why != "") {
			t.Errorf("%08X: %v %q, want %v", tag, got, why, want)
		}
		if tag>>16%2 == 1 {
			continue
		}
		_, problems := profile.Expand(&v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", tg(fmt.Sprintf("%08X", tag)), keep())}}, cat)
		refused := len(problems) == 1 && problems[0].Code == profile.CodeReservedElement
		if refused != (want != profile.NotHandled) {
			t.Errorf("%08X: a tag rule naming it gave %v", tag, problems)
		}
	}
}
