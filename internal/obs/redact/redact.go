package redact

import (
	"log/slog"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/anypb"
)

// Placeholder is what a marked string or bytes field holds after Message:
// it shows a value was there without showing it.
const Placeholder = "[redacted]"

// Message returns a copy of m with every field marked debug_redact blanked,
// at any depth. It returns nil for a nil message. The message given is not
// changed.
func Message(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	c := proto.Clone(m)
	if c == nil {
		return nil
	}
	walk(c.ProtoReflect())
	return c
}

// isMarked reports whether a field carries debug_redact = true.
func isMarked(fd protoreflect.FieldDescriptor) bool {
	opts, ok := fd.Options().(*descriptorpb.FieldOptions)
	return ok && opts.GetDebugRedact()
}

// walk blanks the marked fields of m in place and descends into every
// message it holds. Range visits only the fields that are set, which
// includes the one set field of each oneof.
func walk(m protoreflect.Message) {
	if !m.IsValid() {
		return
	}
	if m.Descriptor().FullName() == "google.protobuf.Any" {
		walkAny(m)
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if isMarked(fd) {
			blank(m, fd)
			return true
		}
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					walk(mv.Message())
					return true
				})
			}
		case fd.IsList():
			if fd.Message() != nil {
				l := v.List()
				for i := range l.Len() {
					walk(l.Get(i).Message())
				}
			}
		case fd.Message() != nil:
			walk(v.Message())
		}
		return true
	})
	// Unknown fields could be a marked field of a newer contract that this
	// binary does not know; they cannot be checked, so they are dropped.
	if len(m.GetUnknown()) > 0 {
		m.SetUnknown(nil)
	}
}

// blank empties a marked field: a singular string or bytes field reads
// Placeholder, anything else (a number, a message, a list, a map) is
// cleared.
func blank(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
	if fd.Cardinality() != protoreflect.Repeated {
		switch fd.Kind() {
		case protoreflect.StringKind:
			m.Set(fd, protoreflect.ValueOfString(Placeholder))
			return
		case protoreflect.BytesKind:
			m.Set(fd, protoreflect.ValueOfBytes([]byte(Placeholder)))
			return
		}
	}
	m.Clear(fd)
}

// walkAny redacts the message packed in an Any. When its type is known the
// message is unpacked, walked and packed again; when it is not, or the bytes
// do not parse, the value is removed, because a value that cannot be walked
// cannot be shown to hold no secret.
func walkAny(m protoreflect.Message) {
	a, ok := m.Interface().(*anypb.Any)
	if !ok {
		// A dynamic Any: copy it into a concrete one through its bytes.
		b, err := proto.Marshal(m.Interface())
		a = &anypb.Any{}
		if err != nil || proto.Unmarshal(b, a) != nil {
			clearAll(m)
			return
		}
	}
	inner, err := a.UnmarshalNew()
	if err != nil {
		a.Value = nil
		writeBack(m, a)
		return
	}
	walk(inner.ProtoReflect())
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(inner)
	if err != nil {
		a.Value = nil
	} else {
		a.Value = b
	}
	writeBack(m, a)
}

// writeBack sets the fields of the Any m from a, which may be m itself.
func writeBack(m protoreflect.Message, a *anypb.Any) {
	if m.Interface() == a {
		return
	}
	fields := m.Descriptor().Fields()
	m.Set(fields.ByName("type_url"), protoreflect.ValueOfString(a.GetTypeUrl()))
	m.Set(fields.ByName("value"), protoreflect.ValueOfBytes(a.GetValue()))
}

// clearAll clears every field of m.
func clearAll(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		m.Clear(fd)
		return true
	})
}

// jsonForm writes a redacted message as compact JSON with the contract's
// field names. The resolver is the global one, so an Any of a known type
// prints its fields.
var jsonForm = protojson.MarshalOptions{Resolver: protoregistry.GlobalTypes}

// LogValue returns m, redacted, as a slog value: its JSON form after
// Message, so a log line shows the message's fields and never a marked one.
// A message that cannot be printed reads as the name of its type.
func LogValue(m proto.Message) slog.Value {
	if m == nil {
		return slog.StringValue("<nil>")
	}
	r := Message(m)
	b, err := jsonForm.Marshal(r)
	if err != nil {
		return slog.StringValue(string(m.ProtoReflect().Descriptor().FullName()))
	}
	return slog.StringValue(string(b))
}
