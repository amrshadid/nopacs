package access

import (
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/master"
)

// auditText returns s as an audit entry may hold it: every character that
// could change how the log reads is replaced by a visible escape, and the
// result is cut to at most max bytes at a whole character or escape.
//
// Escaped are the C0 and C1 control characters and DEL (a newline or a
// carriage return could forge a line of a log, an escape sequence could
// rewrite a terminal), the Unicode line and paragraph separators, the
// bidirectional controls (U+061C, U+200E, U+200F, U+202A to U+202E, U+2066
// to U+2069, which can make text read in an order other than it is stored,
// CVE-2021-42574), and the backslash itself, so an escape in the log is
// always one this function wrote. A byte that is not UTF-8 becomes U+FFFD.
// The escapes are \n, \r, \t, \\, \xHH for the other bytes below U+0100 and
// \uHHHH above, as Go writes them.
//
// It is the one function every caller-controlled text of an entry passes
// through (method, remote address, request id, object, detail keys and
// values, the tenant's name as given, a key's holder, the local user and a
// reason): an entry the master refused for its bounds would be a request
// without a record.
func auditText(s string, max int) string {
	clean := len(s) <= max
	for i := 0; clean && i < len(s); i++ {
		c := s[i]
		clean = c >= 0x20 && c < 0x7f && c != '\\'
	}
	if clean {
		return s
	}
	out := make([]byte, 0, min(len(s), max))
	var piece []byte
	for i, w := 0, 0; i < len(s); i += w {
		r, width := utf8.DecodeRuneInString(s[i:])
		w = width
		piece = piece[:0]
		switch {
		case r == utf8.RuneError && width == 1:
			piece = utf8.AppendRune(piece, utf8.RuneError)
		case r == '\\':
			piece = append(piece, '\\', '\\')
		case r == '\n':
			piece = append(piece, '\\', 'n')
		case r == '\r':
			piece = append(piece, '\\', 'r')
		case r == '\t':
			piece = append(piece, '\\', 't')
		case r < 0x20 || (r >= 0x7f && r < 0xa0):
			piece = append(piece, '\\', 'x', hex[r>>4], hex[r&0xf])
		case escapedRune(r):
			piece = append(piece, '\\', 'u')
			piece = append(piece, hex[r>>12&0xf], hex[r>>8&0xf], hex[r>>4&0xf], hex[r&0xf])
		default:
			piece = append(piece, s[i:i+width]...)
		}
		if len(out)+len(piece) > max {
			break
		}
		out = append(out, piece...)
	}
	return string(out)
}

// hex is the digits of auditText's escapes.
const hex = "0123456789abcdef"

// escapedRune reports whether r is a separator or a bidirectional control
// that auditText escapes.
func escapedRune(r rune) bool {
	switch {
	case r == 0x061c, r == 0x200e, r == 0x200f, r == 0x2028, r == 0x2029:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// keyState returns why a key whose secret matched is refused at now:
// revoked, suspended, expired, or "" when it is good. A key comes only from
// a caller off the local socket (the local operator is known by the
// socket, never by a key), so a suspended key is refused wherever it is
// used, and the local socket keeps working while keys are suspended.
func keyState(k master.Key, now time.Time) string {
	if !k.Revoked.IsZero() {
		return reasonRevoked
	}
	if !k.Suspended.IsZero() {
		return reasonSuspended
	}
	if !k.Expires.IsZero() && !now.Before(k.Expires) {
		return reasonExpired
	}
	return ""
}
