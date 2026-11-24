package server

import (
	"fmt"
	"log/slog"
	"regexp"

	"google.golang.org/protobuf/proto"

	"github.com/amrshadid/nopacs/internal/obs/redact"
)

// maxPanicText bounds the text of a panic's value that reaches the log. A
// panic's value is whatever the code that panicked passed, and it may hold
// anything the handler had: a request, a database address, a key. Only short
// texts and errors are written, and only after userinfo is removed.
const maxPanicText = 256

// userinfo finds the user and password part of an address such as
// postgres://user:password@host, which a panic or an error from a database
// driver may repeat.
var userinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]*@`)

// scrubText removes the userinfo of every address in s and cuts it to
// maxPanicText bytes.
func scrubText(s string) string {
	return cut(userinfo.ReplaceAllString(s, "${1}[hidden]@"), maxPanicText)
}

// panicAttrs are the log attributes of a recovered panic's value: always
// its type; its text when it is an error or a string, with userinfo removed
// and cut to maxPanicText bytes; a protobuf message only through the
// redactor; nothing more for any other value, whose printed form could hold
// anything.
func panicAttrs(p any) []slog.Attr {
	attrs := []slog.Attr{slog.String("panic_type", fmt.Sprintf("%T", p))}
	switch v := p.(type) {
	case proto.Message:
		attrs = append(attrs, slog.Any("panic", redact.LogValue(v)))
	case error:
		attrs = append(attrs, slog.String("panic", scrubText(v.Error())))
	case string:
		attrs = append(attrs, slog.String("panic", scrubText(v)))
	}
	return attrs
}
