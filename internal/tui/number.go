package tui

import (
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
)

// wholeIn reads text as a whole number from lo to hi, for a field that takes one:
// parsed as 32 bits, so a number that does not fit is refused here and not
// wrapped, and the error says the range the person must keep to.
func wholeIn(text, name string, lo, hi int32) (int32, error) {
	n, err := strconv.ParseInt(text, 10, 32)
	if err != nil || n < int64(lo) || n > int64(hi) {
		return 0, fault.New(fault.Invalid, name+" is a whole number from "+strconv.Itoa(int(lo))+" to "+strconv.Itoa(int(hi)))
	}
	return int32(n), nil
}
