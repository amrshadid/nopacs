package obs

import (
	"io"
	"log/slog"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/amrshadid/nopacs/internal/obs/redact"
)

// Format is how a log line is written.
type Format string

// The formats NewLogger writes.
const (
	// JSON writes one JSON object per line, for a log collector. It is the
	// default because a server's log is collected and searched by programs;
	// the text form is for a person reading at a terminal.
	JSON Format = "json"
	// Text writes key=value pairs, for a person reading a terminal.
	Text Format = "text"
)

// HiddenValue is what an attribute named "authorization" reads in the log.
const HiddenValue = "[hidden]"

// LogOptions configures NewLogger.
type LogOptions struct {
	// Format is JSON or Text; empty is JSON.
	Format Format
	// Level is the lowest level written; the zero value is Info.
	Level slog.Leveler
}

// NewLogger returns a logger writing to w with the guards described in the
// package documentation. An unknown Format is written as JSON.
func NewLogger(w io.Writer, opts LogOptions) *slog.Logger {
	return newLogger(w, opts, true)
}

// newLogger is NewLogger with the redaction of protobuf messages on or off.
// The binary always has it on; a test turns it off to show that the test of
// the redaction fails without it.
func newLogger(w io.Writer, opts LogOptions, redactMessages bool) *slog.Logger {
	hopts := &slog.HandlerOptions{Level: opts.Level, ReplaceAttr: guard(redactMessages)}
	if opts.Format == Text {
		return slog.New(slog.NewTextHandler(w, hopts))
	}
	return slog.New(slog.NewJSONHandler(w, hopts))
}

// ParseLevel reads a level's name: debug, info, warn or error, in any case.
// It reports false for anything else.
func ParseLevel(s string) (slog.Level, bool) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo, false
	}
	return l, true
}

// guard returns the handler's ReplaceAttr: it hides an authorization
// attribute and, when redactMessages is set, redacts a protobuf message,
// wherever either appears.
func guard(redactMessages bool) func([]string, slog.Attr) slog.Attr {
	return func(_ []string, a slog.Attr) slog.Attr {
		if strings.EqualFold(a.Key, "authorization") {
			return slog.String(a.Key, HiddenValue)
		}
		if a.Value.Kind() == slog.KindAny {
			if m, ok := a.Value.Any().(proto.Message); ok && redactMessages {
				return slog.Attr{Key: a.Key, Value: redact.LogValue(m)}
			}
		}
		return a
	}
}
