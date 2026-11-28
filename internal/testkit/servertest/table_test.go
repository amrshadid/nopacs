package servertest_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/obs"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/testkit/servertest"
)

// notServedFile is the list of the services the contract defines and the
// server does not serve yet (its header says why such a list exists and
// when a line leaves it). It is one file, read here, by the tests of internal/api, and
// by scripts/prove/steps.py, so the three cannot disagree.
var notServedFile = filepath.Join("..", "..", "..", "api", "not-served.txt")

// notServedName is the form of a line of notServedFile: a full service
// name, or a full service name, "/" and a method's name.
var notServedName = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*\.[A-Z][A-Za-z0-9]*(/[A-Z][A-Za-z0-9]*)?$`)

// readNotServed returns the lines of notServedFile, in its order. A line of
// neither form, or a line given twice, fails the test.
func readNotServed(t testing.TB) []string {
	t.Helper()
	b, err := os.ReadFile(notServedFile)
	if err != nil {
		t.Fatalf("the list of services not served: %v", err)
	}
	var out []string
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !notServedName.MatchString(line) || slices.Contains(out, line) {
			t.Fatalf("%s:%d: %q is not a full service or method name given once", notServedFile, i+1, line)
		}
		out = append(out, line)
	}
	return out
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

// methodLines returns the lines of entries that name one method, as full
// procedure names.
func methodLines(entries []string) []string {
	var out []string
	for _, e := range entries {
		if strings.Contains(e, "/") {
			out = append(out, "/"+e)
		}
	}
	return out
}

// fileRPCs returns the full procedure name of every rpc of the contract's
// proto files, read from the files themselves.
func fileRPCs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "api", "proto", "nopacs", "v1", "*.proto"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no proto files: %v", err)
	}
	rpc := regexp.MustCompile(`(?m)^\s*rpc (\w+)\(`)
	var out []string
	for _, file := range files {
		b, err := os.ReadFile(file) //nolint:gosec // G304: the repository's own proto files.
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		services := servicePattern.FindAllStringSubmatchIndex(text, -1)
		for _, m := range rpc.FindAllStringSubmatchIndex(text, -1) {
			svc := ""
			for _, s := range services {
				if s[0] < m[0] {
					svc = text[s[2]:s[3]]
				}
			}
			out = append(out, "/nopacs.v1."+svc+"/"+text[m[2]:m[3]])
		}
	}
	return out
}

// notServedProblems compares a list of what is not served with the rpcs of
// the proto files and the table's procedures: each entry must name at
// least one rpc of the files and none in the table.
func notServedProblems(entries, rpcs, procs []string) []string {
	var problems []string
	for _, s := range entries {
		n := 0
		for _, p := range rpcs {
			if !inServices([]string{s}, p) {
				continue
			}
			n++
			if slices.Contains(procs, p) {
				problems = append(problems, p+" is listed as not served, yet the table serves it")
			}
		}
		if n == 0 {
			problems = append(problems, s+" is listed as not served, and no proto file defines it")
		}
	}
	slices.Sort(problems)
	return problems
}

// TestNotServedServices guards the list of what is not served yet, so it
// cannot hide a method that is served: every listed service or method is
// in the proto files and the generated descriptors, none of the methods it
// names is in the table, the method lines are exactly the stand-in
// table's pending methods, and a call to each named method over a real
// server, as the local operator, is answered UNIMPLEMENTED with "no such
// method", the answer to any procedure no handler serves, after the
// caller is authenticated, while the other methods of such a service are
// still routed to their handlers. It also shows the check itself reports a
// served service or method put on the list and a listed service no file
// defines.
func TestNotServedServices(t *testing.T) {
	notServed := readNotServed(t)
	rpcs, procs := fileRPCs(t), tableProcs()
	for _, p := range notServedProblems(notServed, rpcs, procs) {
		t.Error(p)
	}
	served := notServedProblems(append(slices.Clone(notServed), "nopacs.v1.AccessService"), rpcs, procs)
	if len(served) == 0 || !strings.Contains(served[0], "AccessService") {
		t.Errorf("a served service on the list was not reported: %v", served)
	}
	servedMethod := notServedProblems(append(slices.Clone(notServed), "nopacs.v1.AccessService/WhoAmI"), rpcs, procs)
	if len(servedMethod) != 1 || !strings.Contains(servedMethod[0], "WhoAmI") {
		t.Errorf("a served method on the list was not reported: %v", servedMethod)
	}
	missing := notServedProblems(append(slices.Clone(notServed), "nopacs.v1.NoSuchService"), rpcs, procs)
	if len(missing) != 1 || !strings.Contains(missing[0], "NoSuchService") {
		t.Errorf("a listed service no file defines was not reported: %v", missing)
	}
	lines, pending := methodLines(notServed), servertest.PendingMethods()
	slices.Sort(lines)
	slices.Sort(pending)
	if !slices.Equal(lines, pending) {
		t.Errorf("the method lines of the list are %q, the stand-in table's pending methods %q", lines, pending)
	}

	cl := localClient(t)
	for _, s := range notServed {
		svc, method, isMethod := strings.Cut(s, "/")
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(svc))
		sd, ok := d.(protoreflect.ServiceDescriptor)
		if err != nil || !ok {
			t.Errorf("%s is not a service of the generated descriptors: %v", svc, err)
			continue
		}
		for i := range sd.Methods().Len() {
			md := sd.Methods().Get(i)
			if isMethod && string(md.Name()) != method {
				continue
			}
			proc := "/" + svc + "/" + string(md.Name())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			req, res := dynamicpb.NewMessage(md.Input()), dynamicpb.NewMessage(md.Output())
			var err error
			if md.IsStreamingServer() {
				st, serr := cl.CallServerStream(ctx, connect.Spec{StreamType: connect.StreamTypeServer, Procedure: proc}, req)
				err = serr
				if err == nil {
					err = st.Receive(res)
					_ = st.Close()
				}
			} else {
				err = cl.CallUnary(ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: proc}, req, res)
			}
			cancel()
			var ce *connect.Error
			if connect.CodeOf(err) != connect.CodeUnimplemented || !errors.As(err, &ce) || ce.Message() != "no such method" {
				t.Errorf("%s: %v, want UNIMPLEMENTED, no such method", proc, err)
			}
		}
	}

	// A served method of a service one of whose methods is listed is still
	// routed: WhoAmI answers the local operator.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var who nopacsv1.WhoAmIResponse
	if err := cl.CallUnary(ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: nopacsv1connect.AccessServiceWhoAmIProcedure},
		&nopacsv1.WhoAmIRequest{}, &who); err != nil {
		t.Errorf("WhoAmI beside a listed method of its service: %v", err)
	}
}

// localClient starts a real server with the stand-in handlers and table over
// a fresh SQLite master, on its local socket only, and returns a client of
// it that calls as the local operator. The server stops when t ends.
func localClient(t *testing.T) *connect.Client {
	t.Helper()
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "mgs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory, private to its owner.
		t.Fatal(err)
	}
	m, err := sqlite.OpenMaster(ctx, filepath.Join(dir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	log := obs.NewLogger(io.Discard, obs.LogOptions{Format: obs.JSON})
	a, err := access.New(access.Config{Store: m, Clock: time.Now, NewID: server.NewID, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	h := &servertest.Standins{Access: a, Release: make(chan struct{})}
	t.Cleanup(func() { close(h.Release) })
	lock, err := server.TakeLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Config{
		DataDir: dir, Lock: lock, Access: a, Master: m, Log: log,
		Register: h.Register, Methods: servertest.Contract(),
		WithDecision: func(ctx context.Context, _ access.Decision) context.Context { return ctx },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	path := server.SocketPath(dir)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: &p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
	t.Cleanup(func() {
		hc.CloseIdleConnections()
		if err := srv.Stop(); err != nil {
			t.Errorf("stop: %v", err)
		}
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Close(cctx)
	})
	return connect.NewClient(connecthttp.NewTransport(hc, "http://local", connecthttp.WithGRPC()))
}

// contractFiles are the generated descriptors of the contract's files.
var contractFiles = []protoreflect.FileDescriptor{
	nopacsv1.File_nopacs_v1_access_proto,
	nopacsv1.File_nopacs_v1_tenant_proto,
	nopacsv1.File_nopacs_v1_settings_proto,
	nopacsv1.File_nopacs_v1_audit_proto,
	nopacsv1.File_nopacs_v1_scope_proto,
	nopacsv1.File_nopacs_v1_storage_proto,
	nopacsv1.File_nopacs_v1_receiver_proto,
	nopacsv1.File_nopacs_v1_reception_proto,
	nopacsv1.File_nopacs_v1_quarantine_proto,
	nopacsv1.File_nopacs_v1_monitor_proto,
	nopacsv1.File_nopacs_v1_metadata_proto,
	nopacsv1.File_nopacs_v1_catalog_proto,
	nopacsv1.File_nopacs_v1_sender_proto,
	nopacsv1.File_nopacs_v1_job_proto,
	nopacsv1.File_nopacs_v1_data_proto,
	nopacsv1.File_nopacs_v1_profile_proto,
	nopacsv1.File_nopacs_v1_deid_proto,
	nopacsv1.File_nopacs_v1_audit_seal_proto,
	nopacsv1.File_nopacs_v1_correction_proto,
	nopacsv1.File_nopacs_v1_backup_proto,
	nopacsv1.File_nopacs_v1_replica_proto,
	nopacsv1.File_nopacs_v1_retention_proto,
	nopacsv1.File_nopacs_v1_route_proto,
	nopacsv1.File_nopacs_v1_transfer_proto,
	nopacsv1.File_nopacs_v1_search_proto,
}

// contractMethods returns every served method of the contract by procedure
// name: the methods of contractFiles but the pending ones of served
// services (servertest.PendingMethods, which TestNotServedServices holds
// to the list).
func contractMethods() map[string]protoreflect.MethodDescriptor {
	out := map[string]protoreflect.MethodDescriptor{}
	pending := servertest.PendingMethods()
	for _, f := range contractFiles {
		for i := range f.Services().Len() {
			svc := f.Services().Get(i)
			for j := range svc.Methods().Len() {
				m := svc.Methods().Get(j)
				proc := "/" + string(svc.FullName()) + "/" + string(m.Name())
				if !slices.Contains(pending, proc) {
					out[proc] = m
				}
			}
		}
	}
	return out
}

// TestTableCoversContract guards the method-to-requirement table against
// the generated descriptors in both directions: every method of the
// contract has an entry, and every entry names a method of the contract or
// the health service.
func TestTableCoversContract(t *testing.T) {
	problems := tableProblems(contractMethods(), tableProcs())
	for _, p := range problems {
		t.Error(p)
	}
}

// TestTableTestCatchesGaps guards the table test itself: with an entry
// removed, and with an entry for a method that does not exist, it reports
// each.
func TestTableTestCatchesGaps(t *testing.T) {
	procs := tableProcs()
	dropped := slices.DeleteFunc(slices.Clone(procs), func(p string) bool { return strings.HasSuffix(p, "/WhoAmI") })
	if p := tableProblems(contractMethods(), dropped); len(p) != 1 || !strings.Contains(p[0], "WhoAmI") {
		t.Fatalf("a missing entry was not reported: %v", p)
	}
	extra := append(slices.Clone(procs), "/nopacs.v1.AccessService/Nothing")
	if p := tableProblems(contractMethods(), extra); len(p) != 1 || !strings.Contains(p[0], "Nothing") {
		t.Fatalf("an entry naming no method was not reported: %v", p)
	}
}

// tableProblems compares the contract's methods with the table's
// procedures.
func tableProblems(methods map[string]protoreflect.MethodDescriptor, procs []string) []string {
	var problems []string
	have := map[string]bool{}
	for _, p := range procs {
		have[p] = true
		if _, ok := methods[p]; !ok && p != server.HealthCheckProcedure && p != server.HealthWatchProcedure {
			problems = append(problems, "the table has "+p+", which is no method of the contract")
		}
	}
	for p := range methods {
		if !have[p] {
			problems = append(problems, "the method "+p+" has no entry in the table")
		}
	}
	slices.Sort(problems)
	return problems
}

// rolePattern finds each method's "Role:" line, which may run onto the
// next comment line, and the rpc it belongs to.
var rolePattern = regexp.MustCompile(`(?s)// Role: (.*?)\n\s*// (?:Audit|Errors):.*?rpc (\w+)\(`)

// servicePattern finds the services of a proto file and where each starts.
var servicePattern = regexp.MustCompile(`(?m)^service (\w+) \{`)

// TestTableMatchesRoleLines guards that each entry gives the requirement
// its method's "Role:" line in api/proto/nopacs/v1 states. The lines are
// read from the proto files, since the generated descriptors carry no
// comments; each phrase the contract uses is mapped to what it means, and
// a phrase not known here fails the test, so a new kind of role line is
// looked at before it is served.
func TestTableMatchesRoleLines(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "api", "proto", "nopacs", "v1", "*.proto"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no proto files: %v", err)
	}
	methods := contractMethods()
	notServed := readNotServed(t)
	seen := map[string]bool{}
	for _, file := range files {
		b, err := os.ReadFile(file) //nolint:gosec // G304: the repository's own proto files.
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		services := servicePattern.FindAllStringSubmatchIndex(text, -1)
		for _, m := range rolePattern.FindAllStringSubmatchIndex(text, -1) {
			role := strings.Join(strings.Fields(strings.ReplaceAll(text[m[2]:m[3]], "//", " ")), " ")
			name := text[m[4]:m[5]]
			svc := ""
			for _, s := range services {
				if s[0] < m[0] {
					svc = text[s[2]:s[3]]
				}
			}
			proc := "/nopacs.v1." + svc + "/" + name
			if inServices(notServed, proc) {
				// Held by TestNotServedServices.
				continue
			}
			md, ok := methods[proc]
			if !ok {
				t.Errorf("%s: a Role line for %s, which is no method", filepath.Base(file), proc)
				continue
			}
			seen[proc] = true
			checkRole(t, proc, role, md)
		}
	}
	for p := range methods {
		if !seen[p] {
			t.Errorf("the method %s has no Role line the test could read", p)
		}
	}
}

// checkRole compares what the table says proc needs with what its Role
// line says.
func checkRole(t *testing.T, proc, role string, md protoreflect.MethodDescriptor) {
	t.Helper()
	public, got, ok := ruleFor(proc, nil)
	if !ok || public {
		t.Errorf("%s: no entry, or a public one", proc)
		return
	}
	switch role {
	case "admin on the installation.":
		expectStatic(t, proc, got, access.OnInstallation(master.RoleAdmin))
	case "none; any caller the server authenticates.",
		"admin on the installation; or the key being rotated itself, so a system renews its own key with no person involved. Any other key is answered permission denied.",
		"none beyond an authenticated caller; the caller's grants decide what is listed.":
		expectStatic(t, proc, got, access.Authenticated())
	case "admin on the installation; with a tenant, admin on the installation or on that tenant.":
		expectGrants(t, proc, md, false)
	case "admin on the installation; on a tenant, admin on the installation or on that tenant, a tenant's admin held to its bounds (the service says which).",
		"admin on the installation; for the grants on a tenant, admin on the installation or on that tenant. A key's grants on every scope are the installation's admin's alone to read.":
		expectGrants(t, proc, md, true)
	case "reader on that tenant, or reader on the installation.":
		msg := newRequest(md)
		setString(msg, "name", "lab")
		_, got, _ = ruleFor(proc, msg)
		if got != access.TenantRegistration("lab") {
			t.Errorf("%s: %q, but the table does not ask for the tenant's registration", proc, role)
		}
	case "reader on the scope asked for.":
		expectScope(t, proc, md, master.RoleReader)
	case "admin on the scope set.", "admin on the scope.", "admin on the scope asked for.":
		expectScope(t, proc, md, master.RoleAdmin)
	case "admin on the scope set; for a setting set on a tenant by the installation's admin only, admin on the installation, whatever the scope.",
		"admin on the scope; for a setting set on a tenant by the installation's admin only, admin on the installation, whatever the scope.":
		expectSettingChange(t, proc, md)
	case "reader on the tenant.":
		expectTenant(t, proc, md, master.RoleReader, nil)
	case "operator on the tenant.":
		expectTenant(t, proc, md, master.RoleOperator, nil)
	case "admin on the tenant.", "admin on the tenant, never a masked key: a person decides how much of the archive goes at once.",
		"admin on the tenant, never a masked key: where archived data is filed is a person's choice.",
		"admin on the tenant, never a masked key: what the archive's files say is a person's choice.",
		"admin on the tenant, and writer on the target tenant, never a masked key: where archived data goes is a person's choice.":
		expectTenant(t, proc, md, master.RoleAdmin, nil)
	case "reader on the tenant; operator on the tenant to filter by Study Instance UID, since looking a study up by an identifier the caller already holds is a search for a person's data.":
		expectTenant(t, proc, md, master.RoleReader, []string{"study_instance_uid"})
	case "reader on the tenant; operator on the tenant to filter by a UID, since looking a file up by an identifier the caller already holds is a search for a person's data.":
		expectTenant(t, proc, md, master.RoleReader, []string{"study_instance_uid", "sop_instance_uid"})
	case "reader on the tenant; operator on the tenant with include_header, since the header holds what the sender sent, identifying data included.":
		expectTenant(t, proc, md, master.RoleReader, []string{"include_header"})
	case "operator on the tenant; admin on the tenant to name a profile version, since a version decides what leaves a file, as a receiver's does.":
		expectTenant(t, proc, md, master.RoleOperator, nil)
		m := newRequest(md)
		setString(m, "tenant", "lab")
		r := m.ProtoReflect()
		fd := r.Descriptor().Fields().ByName("profile")
		r.Set(fd, r.NewField(fd))
		if _, got, _ := ruleFor(proc, m); got != access.OnTenant("lab", master.RoleAdmin) {
			t.Errorf("%s with a profile version: the requirement is not admin on the tenant", proc)
		}
	case "reader on the tenant asked for; operator on the installation for every tenant.":
		expectMonitorScope(t, proc, md)
	default:
		t.Errorf("%s: the Role line %q is a phrase this test does not know; add what it means", proc, role)
	}
}

// expectTenant checks a role on the tenant a request names by its field
// "tenant", and on the installation when it names none; with each field of
// operatorIf set, operator instead.
func expectTenant(t *testing.T, proc string, md protoreflect.MethodDescriptor, role master.Role, operatorIf []string) {
	t.Helper()
	named := newRequest(md)
	setString(named, "tenant", "lab")
	if _, got, _ := ruleFor(proc, named); got != access.OnTenant("lab", role) {
		t.Errorf("%s: the requirement on a tenant is not %s there", proc, role)
	}
	if _, got, _ := ruleFor(proc, newRequest(md)); got != access.OnInstallation(role) {
		t.Errorf("%s: the requirement with no tenant is not %s on the installation", proc, role)
	}
	for _, f := range operatorIf {
		m := proto.Clone(named)
		r := m.ProtoReflect()
		fd := r.Descriptor().Fields().ByName(protoreflect.Name(f))
		if fd.Kind() == protoreflect.BoolKind {
			r.Set(fd, protoreflect.ValueOfBool(true))
		} else {
			r.Set(fd, protoreflect.ValueOfString("2.25.1"))
		}
		if _, got, _ := ruleFor(proc, m); got != access.OnTenant("lab", master.RoleOperator) {
			t.Errorf("%s with %s: the requirement is not operator on the tenant", proc, f)
		}
	}
}

// expectGrants checks the requirement of a method on grants: admin on the
// installation or on the tenant the request names, by its field "tenant" or,
// when byScope is set, by its scope, and admin on the installation for the
// installation's scope or no tenant.
func expectGrants(t *testing.T, proc string, md protoreflect.MethodDescriptor, byScope bool) {
	t.Helper()
	tenant, inst, none := newRequest(md), newRequest(md), newRequest(md)
	if byScope {
		if !setScope(tenant.ProtoReflect(), &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}}, 2) {
			t.Errorf("%s: no scope field found in its request", proc)
			return
		}
		setScope(inst.ProtoReflect(), installation(), 2)
	} else {
		setString(tenant, "tenant", "lab")
	}
	for _, c := range []struct {
		msg  proto.Message
		want access.Requirement
	}{
		{tenant, access.TenantGrants("lab")},
		{inst, access.OnInstallation(master.RoleAdmin)},
		{none, access.OnInstallation(master.RoleAdmin)},
	} {
		if _, got, _ := ruleFor(proc, c.msg); got != c.want {
			t.Errorf("%s: the requirement for %v is not %v", proc, c.msg, c.want)
		}
	}
}

// expectSettingChange checks the requirement of a change of a setting: admin
// on the scope for an ordinary setting, and admin on the installation,
// whatever the scope, for each setting the server's list says the
// installation's admin alone sets on a tenant.
func expectSettingChange(t *testing.T, proc string, md protoreflect.MethodDescriptor) {
	t.Helper()
	named := func(name string, s *nopacsv1.Scope) proto.Message {
		m := newRequest(md)
		r := m.ProtoReflect()
		target := r
		if fd := r.Descriptor().Fields().ByName("setting"); fd != nil {
			target = r.Mutable(fd).Message()
		}
		target.Set(target.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString(name))
		if s != nil {
			setScope(r, s, 2)
		}
		return m
	}
	lab := &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}}
	if _, got, _ := ruleFor(proc, named("events.retention", lab)); got != access.OnTenant("lab", master.RoleAdmin) {
		t.Errorf("%s: an ordinary setting on a tenant is not admin there", proc)
	}
	if _, got, _ := ruleFor(proc, named("events.retention", installation())); got != access.OnInstallation(master.RoleAdmin) {
		t.Errorf("%s: an ordinary setting on the installation is not admin there", proc)
	}
	for _, def := range settings.Builtin() {
		if !def.TenantSetByInstallation {
			continue
		}
		if _, got, _ := ruleFor(proc, named(def.Name, lab)); got != access.OnInstallation(master.RoleAdmin) {
			t.Errorf("%s: %s on a tenant is not admin on the installation", proc, def.Name)
		}
	}
}

// expectStatic checks a requirement that must not depend on the request,
// and that the message is not read before the check.
func expectStatic(t *testing.T, proc string, got, want access.Requirement) {
	t.Helper()
	if got != want {
		t.Errorf("%s: the table's requirement is not the Role line's", proc)
	}
	if readsMessage(proc) {
		t.Errorf("%s: its requirement is fixed, yet the message is read before the check", proc)
	}
}

// expectScope checks a role on the request's scope: on the tenant it
// names, on the installation, and on the installation when the scope is
// missing.
func expectScope(t *testing.T, proc string, md protoreflect.MethodDescriptor, role master.Role) {
	t.Helper()
	tenant := newRequest(md)
	if !setScope(tenant.ProtoReflect(), &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}}, 2) {
		t.Errorf("%s: no scope field found in its request", proc)
		return
	}
	inst := newRequest(md)
	setScope(inst.ProtoReflect(), installation(), 2)
	for _, c := range []struct {
		msg  proto.Message
		want access.Requirement
	}{
		{tenant, access.OnTenant("lab", role)},
		{inst, access.OnInstallation(role)},
		{newRequest(md), access.OnInstallation(role)},
	} {
		if _, got, _ := ruleFor(proc, c.msg); got != c.want {
			t.Errorf("%s: the requirement for %v is not %s on that scope", proc, c.msg, role)
		}
	}
}

// expectMonitorScope checks reader on the tenant a request's scope names,
// and operator on the installation for every tenant or no scope.
func expectMonitorScope(t *testing.T, proc string, md protoreflect.MethodDescriptor) {
	t.Helper()
	tenant := newRequest(md)
	if !setScope(tenant.ProtoReflect(), &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}}, 2) {
		t.Errorf("%s: no scope field found in its request", proc)
		return
	}
	inst := newRequest(md)
	setScope(inst.ProtoReflect(), installation(), 2)
	for _, c := range []struct {
		msg  proto.Message
		want access.Requirement
	}{
		{tenant, access.OnTenant("lab", master.RoleReader)},
		{inst, access.OnInstallation(master.RoleOperator)},
		{newRequest(md), access.OnInstallation(master.RoleOperator)},
	} {
		if _, got, _ := ruleFor(proc, c.msg); got != c.want {
			t.Errorf("%s: the requirement for %v is not %v", proc, c.msg, c.want)
		}
	}
}

// newRequest returns an empty request message of md, as the generated
// type.
func newRequest(md protoreflect.MethodDescriptor) proto.Message {
	mt, err := protoregistryFind(md.Input().FullName())
	if err != nil {
		return dynamicpb.NewMessage(md.Input())
	}
	return mt.New().Interface()
}

// setString sets a top-level string field.
func setString(m proto.Message, field, v string) {
	r := m.ProtoReflect()
	r.Set(r.Descriptor().Fields().ByName(protoreflect.Name(field)), protoreflect.ValueOfString(v))
}

// setScope sets the first field of type nopacs.v1.Scope found within
// depth levels of messages, and reports whether it found one.
func setScope(m protoreflect.Message, s *nopacsv1.Scope, depth int) bool {
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.Message() == nil || fd.IsList() || fd.IsMap() {
			continue
		}
		if fd.Message().FullName() == "nopacs.v1.Scope" {
			m.Set(fd, protoreflect.ValueOfMessage(s.ProtoReflect()))
			return true
		}
		if depth > 1 && setScope(m.Mutable(fd).Message(), s, depth-1) {
			return true
		}
	}
	return false
}

// protoregistryFind finds a generated message type by name.
func protoregistryFind(name protoreflect.FullName) (protoreflect.MessageType, error) {
	return protoregistry.GlobalTypes.FindMessageByName(name)
}

// tableProcs returns the procedures of the stand-in contract table.
func tableProcs() []string {
	var out []string
	for p := range servertest.Contract() {
		out = append(out, p)
	}
	return out
}

// ruleFor returns what the table says procedure needs for msg (nil for the
// empty request). public is always false: the table holds no public method,
// since the server owns the health service's rule.
func ruleFor(procedure string, msg proto.Message) (public bool, req access.Requirement, ok bool) {
	t := servertest.Contract()
	empty, ok := t.RequestFor(procedure)
	if !ok {
		return false, access.Requirement{}, false
	}
	if msg == nil {
		msg = empty
	}
	req, _ = t.Requirement(procedure, msg)
	return false, req, true
}

// readsMessage reports whether procedure's requirement needs the request.
func readsMessage(procedure string) bool {
	m, _ := servertest.Contract().RequestFor(procedure)
	return m != nil
}

// installation is the installation scope.
func installation() *nopacsv1.Scope {
	return &nopacsv1.Scope{Kind: &nopacsv1.Scope_Installation{Installation: &nopacsv1.Installation{}}}
}
