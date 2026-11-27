package codes

import (
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Domain is the domain of every google.rpc.ErrorInfo this server answers
// (access.proto, "Every error carries a google.rpc.ErrorInfo detail").
const Domain = "nopacs"

// The full names of the two error details the contract names
// (google/rpc/error_details.proto). The messages are Google's own generated
// Go types, so the bytes are those every gRPC library reads, and the types
// are registered under these names for any code that decodes a detail by
// its name (connectproto.UnmarshalErrorDetail).
const (
	ErrorInfoType = "google.rpc.ErrorInfo"
	RetryInfoType = "google.rpc.RetryInfo"
)

// deterministic writes a message's map entries in order of key, so the
// bytes of a detail are the same each time.
var deterministic = proto.MarshalOptions{Deterministic: true}

// errorInfo returns a google.rpc.ErrorInfo detail of reason, Domain and
// metadata.
func errorInfo(reason string, metadata map[string]string) *connect.ErrorDetail {
	return detail(ErrorInfoType, &errdetails.ErrorInfo{Reason: reason, Domain: Domain, Metadata: metadata})
}

// retryInfo returns a google.rpc.RetryInfo detail of the delay d.
func retryInfo(d time.Duration) *connect.ErrorDetail {
	return detail(RetryInfoType, &errdetails.RetryInfo{RetryDelay: durationpb.New(d)})
}

// detail encodes m as the error detail of the type name typ. The messages
// are made here of strings and a duration, which always encode, so an
// error is a broken library: the detail is then left out rather than sent
// half written.
func detail(typ string, m proto.Message) *connect.ErrorDetail {
	b, err := deterministic.Marshal(m)
	if err != nil {
		return nil
	}
	return &connect.ErrorDetail{Type: typ, Value: b}
}

// StatusBytes returns e as an encoded google.rpc.Status, its details as
// google.protobuf.Any: the value of the gRPC trailer grpc-status-details-bin
// before its base64. The server writes it itself only for the answers it
// builds without the RPC library (a refusal before a call is routed); the
// library writes it for every other.
func StatusBytes(e *connect.Error) []byte {
	st := &status.Status{Code: int32(e.Code()), Message: e.Message()} //nolint:gosec // G115: a code is 0 to 16.
	for _, d := range e.Details() {
		st.Details = append(st.Details, &anypb.Any{TypeUrl: "type.googleapis.com/" + d.Type, Value: d.Value})
	}
	b, err := deterministic.Marshal(st)
	if err != nil {
		return nil
	}
	return b
}

// Reason is what a google.rpc.ErrorInfo detail of an API error says.
type Reason struct {
	// Reason and Domain are the detail's; Metadata its pairs, never nil.
	Reason, Domain string
	Metadata       map[string]string
}

// ReasonOfError returns the google.rpc.ErrorInfo detail of an API error,
// and false when it has none, or none that decodes.
func ReasonOfError(e *connect.Error) (Reason, bool) {
	for _, d := range e.Details() {
		if d.Type != ErrorInfoType {
			continue
		}
		var info errdetails.ErrorInfo
		if err := proto.Unmarshal(d.Value, &info); err != nil {
			return Reason{}, false
		}
		out := Reason{Reason: info.GetReason(), Domain: info.GetDomain(), Metadata: info.GetMetadata()}
		if out.Metadata == nil {
			out.Metadata = map[string]string{}
		}
		return out, true
	}
	return Reason{}, false
}

// maxRetryDelay is the longest retry delay a client believes: a server that
// names more, or a negative value, is ignored, so a hostile answer cannot
// park a client for days or make it retry at once.
const maxRetryDelay = 24 * time.Hour

// RetryOfError returns the delay of the google.rpc.RetryInfo detail of an
// API error, and false when it has none, none that decodes, or one outside
// 0 to 24 hours.
func RetryOfError(e *connect.Error) (time.Duration, bool) {
	for _, d := range e.Details() {
		if d.Type != RetryInfoType {
			continue
		}
		var info errdetails.RetryInfo
		if err := proto.Unmarshal(d.Value, &info); err != nil || info.GetRetryDelay() == nil {
			return 0, false
		}
		// Seconds and nanoseconds are checked before they are scaled: a huge
		// value would wrap and turn a hostile delay into no wait at all.
		s, n := info.GetRetryDelay().GetSeconds(), info.GetRetryDelay().GetNanos()
		if s < 0 || s > int64(maxRetryDelay/time.Second) || n < 0 || n >= int32(time.Second) {
			return 0, false
		}
		out := time.Duration(s)*time.Second + time.Duration(n)
		if out > maxRetryDelay {
			return 0, false
		}
		return out, true
	}
	return 0, false
}
