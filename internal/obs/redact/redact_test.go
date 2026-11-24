package redact_test

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/anypb"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/obs/redact"
)

// The secrets the tests plant and then search for.
const (
	secretKey     = "nopacs_SECRETKEYID1_SECRETSECRETSECRETSECRETSECRETSECRETSECRETS" //nolint:gosec // G101: a planted test secret.
	secretAddress = "postgres://admin:hunter2-password@db.example/lab"                //nolint:gosec // G101: a planted test secret.
)

// TestContractSecrets guards the two secret fields of the contract: after
// Message neither value is in any printed form of the message, the other
// fields are kept, and the message given is unchanged.
func TestContractSecrets(t *testing.T) {
	req := &nopacsv1.CreateTenantRequest{Name: "lab", DatabaseType: nopacsv1.DatabaseType_DATABASE_TYPE_POSTGRES, DatabaseAddress: secretAddress}
	res := &nopacsv1.CreateKeyResponse{Key: &nopacsv1.Key{Id: "SECRETKEYID1", Holder: "robot"}, Secret: secretKey}
	for _, tc := range []struct {
		m      proto.Message
		secret string
		keep   string
	}{
		{req, secretAddress, "lab"},
		{res, secretKey, "robot"},
	} {
		r := redact.Message(tc.m)
		for _, form := range printedForms(r) {
			if strings.Contains(form, tc.secret) || strings.Contains(form, "hunter2") {
				t.Fatalf("%T still prints its secret: %s", tc.m, form)
			}
			if !strings.Contains(form, tc.keep) {
				t.Fatalf("%T lost a field that is not secret: %s", tc.m, form)
			}
		}
		if !strings.Contains(redact.LogValue(tc.m).String(), redact.Placeholder) {
			t.Fatalf("%T: the log form does not show a value was redacted", tc.m)
		}
	}
	if req.GetDatabaseAddress() != secretAddress || res.GetSecret() != secretKey {
		t.Fatal("Message changed the message it was given")
	}
}

// TestUnredactedPrintsSecret guards the premise of this package: the protobuf
// library itself prints a marked field. If it ever stops, this test says so,
// and the package is still right but no longer the only guard.
func TestUnredactedPrintsSecret(t *testing.T) {
	res := &nopacsv1.CreateKeyResponse{Secret: secretKey}
	found := false
	for _, form := range printedForms(res) {
		found = found || strings.Contains(form, secretKey)
	}
	if !found {
		t.Fatal("the protobuf library no longer prints a debug_redact field; whether this package is still needed needs a look")
	}
}

// printedForms is every way a message reaches a log: protojson, prototext,
// fmt's %v (which is prototext for a generated message) and the wire bytes.
func printedForms(m proto.Message) []string {
	b, _ := proto.Marshal(m)
	return []string{jsonOf(m), prototext.Format(m), fmt.Sprintf("%v", m), string(b)}
}

// jsonOf prints m as JSON, as it is, without redacting it.
func jsonOf(m proto.Message) string {
	b, err := protojson.Marshal(m)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// TestAnyIsWalked guards that a secret packed in a google.protobuf.Any of a
// known type is redacted, and that an Any of an unknown type loses its value.
func TestAnyIsWalked(t *testing.T) {
	packed, err := anypb.New(&nopacsv1.CreateKeyResponse{Secret: secretKey, Key: &nopacsv1.Key{Holder: "robot"}})
	if err != nil {
		t.Fatal(err)
	}
	r := redact.Message(packed).(*anypb.Any)
	if strings.Contains(string(r.GetValue()), secretKey) {
		t.Fatal("the secret inside an Any was not redacted")
	}
	if !strings.Contains(string(r.GetValue()), "robot") {
		t.Fatal("the Any lost its other fields")
	}
	unknown := &anypb.Any{TypeUrl: "type.googleapis.com/no.such.Type", Value: []byte(secretKey)}
	if v := redact.Message(unknown).(*anypb.Any).GetValue(); len(v) != 0 {
		t.Fatalf("an Any of an unknown type kept its value: %q", v)
	}
}

// TestEveryShape guards the walk over every shape a marked field can take or
// hide in: nested messages, repeated messages, map values, a oneof, a
// repeated marked field, a marked number, a marked message, and unknown
// fields. The message type is built at run time, since the contract has no
// such shapes yet.
func TestEveryShape(t *testing.T) {
	md := shapes(t)
	inner := md.Fields().ByName("inner").Message()
	newInner := func(s string) *dynamicpb.Message {
		m := dynamicpb.NewMessage(inner)
		m.Set(inner.Fields().ByName("secret"), protoreflect.ValueOfString(s))
		m.Set(inner.Fields().ByName("plain"), protoreflect.ValueOfString("visible"))
		return m
	}
	outer := dynamicpb.NewMessage(md)
	f := md.Fields()
	outer.Set(f.ByName("inner"), protoreflect.ValueOfMessage(newInner("S-nested")))
	list := outer.Mutable(f.ByName("many")).List()
	list.Append(protoreflect.ValueOfMessage(newInner("S-list-0")))
	list.Append(protoreflect.ValueOfMessage(newInner("S-list-1")))
	mp := outer.Mutable(f.ByName("by_name")).Map()
	mp.Set(protoreflect.ValueOfString("k").MapKey(), protoreflect.ValueOfMessage(newInner("S-map")))
	outer.Set(f.ByName("choice_secret"), protoreflect.ValueOfString("S-oneof"))
	tags := outer.Mutable(f.ByName("secret_tags")).List()
	tags.Append(protoreflect.ValueOfString("S-tag"))
	outer.Set(f.ByName("secret_number"), protoreflect.ValueOfInt64(424242))
	outer.Set(f.ByName("secret_message"), protoreflect.ValueOfMessage(newInner("S-whole")))
	outer.Set(f.ByName("secret_bytes"), protoreflect.ValueOfBytes([]byte("S-bytes")))
	outer.SetUnknown(protoreflect.RawFields{0xa2, 0x06, 0x05, 'S', '-', 'u', 'n', 'k'})

	r := redact.Message(outer)
	b, err := proto.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b) + jsonOf(r)
	for _, s := range []string{"S-nested", "S-list-0", "S-list-1", "S-map", "S-oneof", "S-tag", "S-whole", "S-bytes", "S-unk", "424242"} {
		if strings.Contains(text, s) {
			t.Errorf("%s survived redaction", s)
		}
	}
	if strings.Count(jsonOf(r), "visible") != 4 {
		t.Errorf("the fields that are not secret were not all kept: %s", jsonOf(r))
	}
	// Five strings show the placeholder; the bytes field holds it too, in
	// base64 in the JSON form.
	if strings.Count(jsonOf(r), redact.Placeholder) != 5 {
		t.Errorf("the redacted strings do not show the placeholder: %s", jsonOf(r))
	}
	// The oneof's other member, unmarked, is kept when it is the one set.
	outer.Set(f.ByName("choice_plain"), protoreflect.ValueOfString("open"))
	if !strings.Contains(jsonOf(redact.Message(outer)), "open") {
		t.Error("an unmarked oneof member was blanked")
	}
}

// TestNil guards that a nil message is answered, not a panic.
func TestNil(t *testing.T) {
	if redact.Message(nil) != nil {
		t.Fatal("nil in, something out")
	}
	var typed *nopacsv1.CreateKeyResponse
	_ = redact.Message(typed)
	_ = redact.LogValue(nil)
}

// shapes builds the descriptor of a message with a marked field in every
// shape.
func shapes(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	marked := &descriptorpb.FieldOptions{DebugRedact: proto.Bool(true)}
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	field := func(name string, n int32, typ *descriptorpb.FieldDescriptorProto_Type, label *descriptorpb.FieldDescriptorProto_Label, typeName string, o *descriptorpb.FieldOptions) *descriptorpb.FieldDescriptorProto {
		fd := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Type: typ, Label: label, Options: o}
		if typeName != "" {
			fd.TypeName = proto.String(typeName)
		}
		return fd
	}
	oneof := func(fd *descriptorpb.FieldDescriptorProto) *descriptorpb.FieldDescriptorProto {
		fd.OneofIndex = proto.Int32(0)
		return fd
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("shapes_test.proto"),
		Package: proto.String("shapestest"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Inner"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("secret", 1, str, opt, "", marked),
					field("plain", 2, str, opt, "", nil),
				},
			},
			{
				Name: proto.String("Outer"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("inner", 1, msg, opt, ".shapestest.Inner", nil),
					field("many", 2, msg, rep, ".shapestest.Inner", nil),
					field("by_name", 3, msg, rep, ".shapestest.Outer.ByNameEntry", nil),
					oneof(field("choice_secret", 4, str, opt, "", marked)),
					oneof(field("choice_plain", 5, str, opt, "", nil)),
					field("secret_tags", 6, str, rep, "", marked),
					field("secret_number", 7, descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(), opt, "", marked),
					field("secret_message", 8, msg, opt, ".shapestest.Inner", marked),
					field("secret_bytes", 9, descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum(), opt, "", marked),
				},
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:    proto.String("ByNameEntry"),
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
					Field: []*descriptorpb.FieldDescriptorProto{
						field("key", 1, str, opt, "", nil),
						field("value", 2, msg, opt, ".shapestest.Inner", nil),
					},
				}},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: proto.String("choice")}},
			},
		},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Messages().ByName("Outer")
}
