package download

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

const (
	// MaxRefs bounds how many references one list names, so a mistaken input
	// (a whole disk piped in) is refused rather than listed.
	MaxRefs = 1_000_000
	// maxRefLine bounds one line of a list of references: three labels of
	// 64 bytes and their slashes fit with room.
	maxRefLine = 1024
)

// ParseRef reads one reference, subject[/session[/scan]], and checks that
// every part is a label.
func ParseRef(s string) ([]string, error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) > 3 {
		return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not a subject, session or scan: use <subject>[/<session>[/<scan>]]", output.Clean(s)))
	}
	for _, p := range parts {
		if p == "" {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not a subject, session or scan: a part is empty", output.Clean(s)))
		}
		if err := label.Check(p); err != nil {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("%q is not a label: %s", output.Clean(p), err))
		}
	}
	return parts, nil
}

// ReadRefs reads a list of references, one per line: subject[/session[/scan]].
// A blank line and a line that starts with "#" are ignored, and so is
// whitespace around a reference. Any other line that is not a reference is an
// error that names its line number; nothing is guessed. This is the one form
// every way of choosing a set produces: a person's list, an earlier command's
// output, or a search's result.
func ReadRefs(r io.Reader) ([][]string, error) {
	var refs [][]string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxRefLine)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		ref, err := ParseRef(text)
		if err != nil {
			return nil, fault.Wrap(fault.Invalid, err, fmt.Sprintf("line %d: %s", line, fault.Message(err)))
		}
		if len(refs) >= MaxRefs {
			return nil, fault.New(fault.Invalid, fmt.Sprintf("the list names more than %d references", MaxRefs))
		}
		refs = append(refs, ref)
	}
	if err := sc.Err(); err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read the list of references: "+err.Error())
	}
	return refs, nil
}
