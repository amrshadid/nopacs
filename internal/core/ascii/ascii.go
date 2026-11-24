package ascii

// Printable reports whether s is printable ASCII with no space: every byte
// from 0x21 to 0x7e. A value of this class has one spelling on every
// system, never holds a separator a list of values uses, and cannot carry a
// control character into a log or a terminal.
func Printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}
