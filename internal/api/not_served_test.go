package api_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api"
)

// notServedFile is the list of the services, and the single methods of
// served services, the contract defines and the server does not serve yet
// (its header says why such a list exists and when a line leaves it). It
// is one file, read here, by the tests of internal/server and
// internal/testkit/servertest, and by scripts/prove/steps.py, so they
// cannot disagree.
var notServedFile = filepath.Join("..", "..", "api", "not-served.txt")

// notServedName is the form of a line of notServedFile: a full service
// name, or a full service name, "/" and a method's name.
var notServedName = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*\.[A-Z][A-Za-z0-9]*(/[A-Z][A-Za-z0-9]*)?$`)

// readNotServed returns the lines of notServedFile, in its order. A line
// of neither form, a line given twice, or a method of a service the list
// already names whole, fails the test.
func readNotServed(t testing.TB) []string {
	t.Helper()
	b, err := os.ReadFile(notServedFile)
	if err != nil {
		t.Fatalf("the list of services not served: %v", err)
	}
	out, err := parseNotServed(string(b))
	if err != nil {
		t.Fatalf("%s: %v", notServedFile, err)
	}
	return out
}

// parseNotServed returns the lines of a list of what is not served, or the
// first line that breaks its form.
func parseNotServed(text string) ([]string, error) {
	var out []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !notServedName.MatchString(line) || slices.Contains(out, line) {
			return nil, fmt.Errorf("line %d: %q is not a full service or method name given once", i+1, line)
		}
		out = append(out, line)
	}
	for _, line := range out {
		if svc, _, isMethod := strings.Cut(line, "/"); isMethod && slices.Contains(out, svc) {
			return nil, fmt.Errorf("%q is a method of %s, which the list names whole", line, svc)
		}
	}
	return out, nil
}

// inServices reports whether proc, a full procedure name, is named by one
// of entries: a method of a service an entry names whole, or the method an
// entry names.
func inServices(entries []string, proc string) bool {
	return slices.ContainsFunc(entries, func(e string) bool {
		if strings.Contains(e, "/") {
			return proc == "/"+e
		}
		return strings.HasPrefix(proc, "/"+e+"/")
	})
}

// notServedProblems compares a list of what is not served with the rpcs of
// the proto files (lines) and with what the table serves (known): each
// entry must name at least one rpc of the files and none the table knows.
func notServedProblems(entries []string, lines map[string]string, known func(string) bool) []string {
	var problems []string
	for _, s := range entries {
		rpcs := 0
		for proc := range lines {
			if !inServices([]string{s}, proc) {
				continue
			}
			rpcs++
			if known(proc) {
				problems = append(problems, proc+" is listed as not served, yet api.Methods serves it")
			}
		}
		if rpcs == 0 {
			problems = append(problems, s+" is listed as not served, and no proto file defines it")
		}
	}
	slices.Sort(problems)
	return problems
}

// listedMethods returns the methods an entry of the list names, from the
// generated descriptors: every method of a service named whole, or the one
// method named; an error for a name the descriptors do not have.
func listedMethods(entry string) ([]protoreflect.MethodDescriptor, error) {
	svc, method, isMethod := strings.Cut(entry, "/")
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if err != nil || !ok {
		return nil, fmt.Errorf("%s is not a service of the generated descriptors: %w", svc, err)
	}
	if isMethod {
		md := sd.Methods().ByName(protoreflect.Name(method))
		if md == nil {
			return nil, fmt.Errorf("%s has no method %s in the generated descriptors", svc, method)
		}
		return []protoreflect.MethodDescriptor{md}, nil
	}
	var out []protoreflect.MethodDescriptor
	for i := range sd.Methods().Len() {
		out = append(out, sd.Methods().Get(i))
	}
	return out, nil
}

// TestNotServedListForms guards the two forms of a line of the list: a
// service, and one method of a service, are read; a method line is matched
// to its procedure alone, not to the rest of its service; and a line that
// is neither, given twice, or a method of a service the list names whole,
// is refused.
func TestNotServedListForms(t *testing.T) {
	got, err := parseNotServed("# a comment\nnopacs.v1.BackupService\n\nnopacs.v1.TenantService/ReclaimTenant\n")
	if err != nil || !slices.Equal(got, []string{"nopacs.v1.BackupService", "nopacs.v1.TenantService/ReclaimTenant"}) {
		t.Fatalf("parse: %q, %v", got, err)
	}
	if !inServices(got, "/nopacs.v1.TenantService/ReclaimTenant") || inServices(got, "/nopacs.v1.TenantService/GetTenant") {
		t.Error("a method line does not name exactly its own procedure")
	}
	if !inServices(got, "/nopacs.v1.BackupService/CreateBackup") {
		t.Error("a service line does not name its methods")
	}
	for _, bad := range []string{
		"nopacs.v1.TenantService/reclaim",
		"nopacs.v1.TenantService/ReclaimTenant/More",
		"nopacs.v1.BackupService\nnopacs.v1.BackupService",
		"nopacs.v1.BackupService\nnopacs.v1.BackupService/CreateBackup",
	} {
		if _, err := parseNotServed(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestNotServedServices guards the list of what is not served yet, so it
// cannot hide a method that is served: every listed service or method is
// in the proto files and in the generated descriptors, none of the methods
// it names is in api.Methods, each is in api.Methods' pending set, and a
// call to each over the test server, as the local operator, is answered
// UNIMPLEMENTED; the other methods of a service one of whose methods is
// listed stay served. It also shows the check itself reports a served
// service or method put on the list and a listed service no file defines.
func TestNotServedServices(t *testing.T) {
	notServed := readNotServed(t)
	lines := roleLines(t)
	var m api.Methods
	known := func(proc string) bool { _, ok := m.RequestFor(proc); return ok }
	for _, p := range notServedProblems(notServed, lines, known) {
		t.Error(p)
	}
	served := notServedProblems(append(slices.Clone(notServed), "nopacs.v1.AccessService"), lines, known)
	if len(served) == 0 || !strings.Contains(served[0], "AccessService") {
		t.Errorf("a served service on the list was not reported: %v", served)
	}
	servedMethod := notServedProblems(append(slices.Clone(notServed), "nopacs.v1.TenantService/GetTenant"), lines, known)
	if len(servedMethod) != 1 || !strings.Contains(servedMethod[0], "GetTenant") {
		t.Errorf("a served method on the list was not reported: %v", servedMethod)
	}
	missing := notServedProblems(append(slices.Clone(notServed), "nopacs.v1.NoSuchService"), lines, known)
	if len(missing) != 1 || !strings.Contains(missing[0], "NoSuchService") {
		t.Errorf("a listed service no file defines was not reported: %v", missing)
	}

	e := newEnv(t)
	asLocal := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok {
				info.RequestHeader().Set(localHeader, local.local)
			}
			return next(ctx, spec)
		}
	}
	cl := connect.NewClient(connecthttp.NewTransport(e.client, e.url), asLocal)
	for _, s := range notServed {
		mds, err := listedMethods(s)
		if err != nil {
			t.Error(err)
			continue
		}
		for _, md := range mds {
			proc := "/" + string(md.Parent().FullName()) + "/" + string(md.Name())
			if !m.Pending(proc) {
				t.Errorf("%s is listed as not served, yet api.Methods does not hold it pending", proc)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			req, res := dynamicpb.NewMessage(md.Input()), dynamicpb.NewMessage(md.Output())
			if md.IsStreamingServer() {
				st, err := cl.CallServerStream(ctx, connect.Spec{StreamType: connect.StreamTypeServer, Procedure: proc}, req)
				if err == nil {
					err = st.Receive(res)
					_ = st.Close()
				}
				if connect.CodeOf(err) != connect.CodeUnimplemented {
					t.Errorf("%s: %v, want UNIMPLEMENTED", proc, err)
				}
			} else {
				err := cl.CallUnary(ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: proc}, req, res)
				if connect.CodeOf(err) != connect.CodeUnimplemented {
					t.Errorf("%s: %v, want UNIMPLEMENTED", proc, err)
				}
			}
			cancel()
		}
	}

	// A served method of a service one of whose methods is listed still
	// reaches its handler: ListTenants answers, not UNIMPLEMENTED.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var res v1.ListTenantsResponse
	if err := cl.CallUnary(ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: nopacsv1connect.TenantServiceListTenantsProcedure},
		&v1.ListTenantsRequest{}, &res); err != nil {
		t.Errorf("ListTenants beside a listed method of its service: %v", err)
	}
}
