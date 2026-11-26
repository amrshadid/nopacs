package codes

import (
	"maps"
	"slices"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protowire"
)

// Domain is the domain of every google.rpc.ErrorInfo this server answers
// (access.proto, "Where a kind above names a reason").
const Domain = "nopacs"

// The full names of the two error details the contract names
// (google/rpc/error_details.proto). A connect.ErrorDetail is a message's
// type name and its encoded bytes, so the server writes both messages with
// the protobuf wire encoding itself (protowire, part of the protobuf module
// it already takes) rather than take the generated Go package that holds
// them, a dependency of its own. A client decodes them with its own copy
// of google/rpc/error_details.proto, as for any server.
const (
	ErrorInfoType = "google.rpc.ErrorInfo"
	RetryInfoType = "google.rpc.RetryInfo"
)

// errorInfo returns a google.rpc.ErrorInfo detail: reason (field 1),
// Domain (field 2) and each metadata pair (field 3, a map<string, string>,
// whose entries are messages of key 1 and value 2), in order of key so the
// bytes are the same each time.
func errorInfo(reason string, metadata map[string]string) *connect.ErrorDetail {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, reason)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendString(b, Domain)
	for _, k := range slices.Sorted(maps.Keys(metadata)) {
		var entry []byte
		entry = protowire.AppendTag(entry, 1, protowire.BytesType)
		entry = protowire.AppendString(entry, k)
		entry = protowire.AppendTag(entry, 2, protowire.BytesType)
		entry = protowire.AppendString(entry, metadata[k])
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, entry)
	}
	return &connect.ErrorDetail{Type: ErrorInfoType, Value: b}
}

// retryInfo returns a google.rpc.RetryInfo detail: retry_delay (field 1),
// a google.protobuf.Duration of whole seconds (field 1) and nanoseconds
// (field 2).
func retryInfo(d time.Duration) *connect.ErrorDetail {
	var dur []byte
	if s := int64(d / time.Second); s != 0 {
		dur = protowire.AppendTag(dur, 1, protowire.VarintType)
		dur = protowire.AppendVarint(dur, uint64(s)) //nolint:gosec // G115: a retry delay is positive.
	}
	if n := int64(d % time.Second); n != 0 {
		dur = protowire.AppendTag(dur, 2, protowire.VarintType)
		dur = protowire.AppendVarint(dur, uint64(n)) //nolint:gosec // G115: a retry delay is positive.
	}
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, dur)
	return &connect.ErrorDetail{Type: RetryInfoType, Value: b}
}

// Reason is what a google.rpc.ErrorInfo detail of an API error says.
type Reason struct {
	// Reason and Domain are the detail's; Metadata its pairs.
	Reason, Domain string
	Metadata       map[string]string
}

// ReasonOfError returns the google.rpc.ErrorInfo detail of an API error,
// and false when it has none, or none this package can read.
func ReasonOfError(e *connect.Error) (Reason, bool) {
	for _, d := range e.Details() {
		if d.Type != ErrorInfoType {
			continue
		}
		out := Reason{Metadata: map[string]string{}}
		b := d.Value
		for len(b) > 0 {
			num, typ, n := protowire.ConsumeTag(b)
			if n < 0 || typ != protowire.BytesType {
				return Reason{}, false
			}
			b = b[n:]
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return Reason{}, false
			}
			b = b[m:]
			switch num {
			case 1:
				out.Reason = string(v)
			case 2:
				out.Domain = string(v)
			case 3:
				k, val, ok := mapEntry(v)
				if !ok {
					return Reason{}, false
				}
				out.Metadata[k] = val
			}
		}
		return out, true
	}
	return Reason{}, false
}

// mapEntry reads one map<string, string> entry: key 1, value 2.
func mapEntry(b []byte) (string, string, bool) {
	var k, v string
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 || typ != protowire.BytesType {
			return "", "", false
		}
		b = b[n:]
		s, m := protowire.ConsumeBytes(b)
		if m < 0 {
			return "", "", false
		}
		b = b[m:]
		switch num {
		case 1:
			k = string(s)
		case 2:
			v = string(s)
		}
	}
	return k, v, true
}

// RetryOfError returns the delay of the google.rpc.RetryInfo detail of an
// API error, and false when it has none, or none this package can read.
func RetryOfError(e *connect.Error) (time.Duration, bool) {
	for _, d := range e.Details() {
		if d.Type != RetryInfoType {
			continue
		}
		num, typ, n := protowire.ConsumeTag(d.Value)
		if n < 0 || num != 1 || typ != protowire.BytesType {
			return 0, false
		}
		dur, m := protowire.ConsumeBytes(d.Value[n:])
		if m < 0 {
			return 0, false
		}
		var out time.Duration
		for len(dur) > 0 {
			f, t, k := protowire.ConsumeTag(dur)
			if k < 0 || t != protowire.VarintType {
				return 0, false
			}
			dur = dur[k:]
			v, j := protowire.ConsumeVarint(dur)
			if j < 0 {
				return 0, false
			}
			dur = dur[j:]
			switch f {
			case 1:
				out += time.Duration(v) * time.Second //nolint:gosec // G115: a server's retry delay is small.
			case 2:
				out += time.Duration(v) //nolint:gosec // G115: nanoseconds below a second.
			}
		}
		return out, true
	}
	return 0, false
}
