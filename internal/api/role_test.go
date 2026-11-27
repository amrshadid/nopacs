package api_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
)

// roleLines reads the "Role:" line of every rpc of the contract's proto
// files, by full procedure name. The generated descriptors carry no
// comments, so the files themselves are read.
func roleLines(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "api", "proto", "nopacs", "v1", "*.proto"))
	if err != nil || len(files) == 0 {
		t.Fatalf("the proto files: %v (%d found)", err, len(files))
	}
	service := regexp.MustCompile(`^service (\w+) \{`)
	rpc := regexp.MustCompile(`^\s*rpc (\w+)\(`)
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: the repository's own proto files.
		if err != nil {
			t.Fatal(err)
		}
		var svc string
		var comment []string
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case service.MatchString(line):
				svc = service.FindStringSubmatch(line)[1]
				comment = nil
			case strings.HasPrefix(trimmed, "//"):
				comment = append(comment, strings.TrimSpace(strings.TrimPrefix(trimmed, "//")))
			case rpc.MatchString(line):
				name := rpc.FindStringSubmatch(line)[1]
				proc := "/nopacs.v1." + svc + "/" + name
				out[proc] = roleOf(comment)
				comment = nil
			default:
				comment = nil
			}
		}
	}
	return out
}

// roleOf returns the text of the Role line in a method's comment, with its
// continuation lines, up to the Audit line; "" when there is none.
func roleOf(comment []string) string {
	var parts []string
	in := false
	for _, l := range comment {
		switch {
		case strings.HasPrefix(l, "Role:"):
			in = true
			parts = append(parts, strings.TrimSpace(strings.TrimPrefix(l, "Role:")))
		case in && (l == "" || strings.HasPrefix(l, "Audit:") || strings.HasPrefix(l, "Errors:")):
			in = false
		case in:
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " ")
}

// The shapes of requirement a Role line can state.
type roleShape int

const (
	// fixedAdmin: admin on the installation, whatever the request.
	fixedAdmin roleShape = iota + 1
	// fixedAuthenticated: any caller the server authenticates.
	fixedAuthenticated
	// registration: reader on the tenant the request names, or reader on
	// the installation.
	registration
	// scopeReader and scopeAdmin: the role on the scope the request names.
	scopeReader
	scopeAdmin
	// settingChange: admin on the scope the request names, except for a
	// setting set on a tenant by the installation's admin only, which needs
	// admin on the installation whatever the scope.
	settingChange
	// tenantReader, tenantOperator and tenantAdmin: the role on the tenant
	// the request names by its field "tenant".
	tenantReader
	tenantOperator
	tenantAdmin
	// tenantReaderOrOperator: reader on the tenant, and operator on it when
	// the request sets one of the fields operatorWhen names for the line.
	tenantReaderOrOperator
	// monitorScope: reader on the tenant the request names, operator on
	// the installation for every tenant.
	monitorScope
	// tenantOperatorOrAdmin: operator on the tenant, and admin on it when
	// the request sets the message field adminWhen names for the line.
	tenantOperatorOrAdmin
	// grantsScope: admin on the installation, or on the tenant the
	// request's scope names (access.TenantGrants); grantsTenant the same
	// for a tenant the request names by its field "tenant", admin on the
	// installation without one.
	grantsScope
	grantsTenant
)

// rolePhrases is every Role line the contract has, with the shape it
// states. A Role line not here fails the test, so a change of the proto
// text is a change of this table, made on purpose.
var rolePhrases = map[string]roleShape{
	"admin on the installation.":                 fixedAdmin,
	"none; any caller the server authenticates.": fixedAuthenticated,
	// The table lets any authenticated caller reach RotateKey; package access
	// decides admin or the key itself before anything else (TestRotateKey*
	// there, and the API's own test of a key rotating another).
	"admin on the installation; or the key being rotated itself, so a system renews its own key with no person involved. Any other key is answered permission denied.": fixedAuthenticated,
	"none beyond an authenticated caller; the caller's grants decide what is listed.":                                                                                  fixedAuthenticated,
	"reader on that tenant, or reader on the installation.":                                                                                                            registration,
	"reader on the scope asked for.": scopeReader,
	"admin on the scope set.":        scopeAdmin,
	"admin on the scope.":            scopeAdmin,
	"admin on the scope asked for.":  scopeAdmin,
	"admin on the scope set; for a setting set on a tenant by the installation's admin only, admin on the installation, whatever the scope.": settingChange,
	"admin on the scope; for a setting set on a tenant by the installation's admin only, admin on the installation, whatever the scope.":     settingChange,
	"reader on the tenant.":   tenantReader,
	"operator on the tenant.": tenantOperator,
	"admin on the tenant.":    tenantAdmin,
	"reader on the tenant; operator on the tenant to filter by Study Instance UID, since looking a study up by an identifier the caller already holds is a search for a person's data.": tenantReaderOrOperator,
	"reader on the tenant; operator on the tenant to filter by a UID, since looking a file up by an identifier the caller already holds is a search for a person's data.":               tenantReaderOrOperator,
	"reader on the tenant; operator on the tenant with include_header, since the header holds what the sender sent, identifying data included.":                                         tenantReaderOrOperator,
	"reader on the tenant asked for; operator on the installation for every tenant.":                                                                                                    monitorScope,
	"operator on the tenant; admin on the tenant to name a profile version, since a version decides what leaves a file, as a receiver's does.":                                          tenantOperatorOrAdmin,
	"admin on the installation; on a tenant, admin on the installation or on that tenant, a tenant's admin held to its bounds (the service says which).":                                grantsScope,
	"admin on the installation; for the grants on a tenant, admin on the installation or on that tenant. A key's grants on every scope are the installation's admin's alone to read.":   grantsScope,
	"admin on the installation; with a tenant, admin on the installation or on that tenant.":                                                                                            grantsTenant,
}

// adminWhen names, for each Role line of the shape tenantOperatorOrAdmin,
// the message field of the request whose presence needs admin.
var adminWhen = map[string]string{
	"operator on the tenant; admin on the tenant to name a profile version, since a version decides what leaves a file, as a receiver's does.": "profile",
}

// operatorWhen names, for each Role line of the shape tenantReaderOrOperator,
// the fields of the request whose presence needs operator.
var operatorWhen = map[string][]string{
	"reader on the tenant; operator on the tenant to filter by Study Instance UID, since looking a study up by an identifier the caller already holds is a search for a person's data.": {"study_instance_uid"},
	"reader on the tenant; operator on the tenant to filter by a UID, since looking a file up by an identifier the caller already holds is a search for a person's data.":               {"study_instance_uid", "sop_instance_uid"},
	"reader on the tenant; operator on the tenant with include_header, since the header holds what the sender sent, identifying data included.":                                         {"include_header"},
}

// originalsWhen names, for each method whose answer can hold a value not in
// its archived, de-identified form, the fields of the request whose presence
// makes it so, which a masked key is refused (access.Requirement
// .AnswersOriginals); no field means every request of the method. A method
// not here never answers one by its requirement.
var originalsWhen = map[string][]string{
	nopacsv1connect.QuarantineServiceGetQuarantinedFileProcedure:   {"include_header"},
	nopacsv1connect.QuarantineServiceListQuarantinedFilesProcedure: {"study_instance_uid", "sop_instance_uid"},
	nopacsv1connect.ProfileServiceTryProfileProcedure:              {"show_values", "quarantine_records"},
	nopacsv1connect.ProfileServiceExplainProfileProcedure:          {"quarantine_record"},
	nopacsv1connect.ReceptionServiceListReceptionsProcedure:        {"study_instance_uid"},
	nopacsv1connect.DeidServiceCodeProcedure:                       nil,
	nopacsv1connect.DeidServiceImportLookupCodesProcedure:          nil,
	nopacsv1connect.JobServiceListImportFilesProcedure:             nil,
	nopacsv1connect.JobServiceListImportUnitsProcedure:             nil,
	nopacsv1connect.BackupServiceCreateBackupProcedure:             nil,
	nopacsv1connect.BackupServiceExportTenantProcedure:             nil,
}

// answersOriginals reports whether a request to proc answers original
// values by originalsWhen.
func answersOriginals(proc string, req proto.Message) bool {
	fields, ok := originalsWhen[proc]
	if !ok {
		return false
	}
	if len(fields) == 0 {
		return true
	}
	if req == nil {
		return false
	}
	m := req.ProtoReflect()
	for _, f := range fields {
		if fd := m.Descriptor().Fields().ByName(protoreflect.Name(f)); fd != nil && m.Has(fd) {
			return true
		}
	}
	return false
}

// destinationWhen names, for each method that can decide where the
// archive's bytes go, when a request does, which a masked key is refused
// (access.Requirement.ChoosesDestination); nil means every request of the
// method. A method not here never does by its requirement.
var destinationWhen = map[string]func(proto.Message) bool{
	nopacsv1connect.StorageServiceAddStorageProcedure:      nil,
	nopacsv1connect.StorageServiceSetStorageProcedure:      nil,
	nopacsv1connect.SenderServiceAddSenderProcedure:        nil,
	nopacsv1connect.SenderServiceSetSenderProcedure:        nil,
	nopacsv1connect.ReplicaServiceAttachReplicaProcedure:   nil,
	nopacsv1connect.ReplicaServiceBackfillReplicaProcedure: nil,
	nopacsv1connect.JobServiceCreateJobProcedure: func(m proto.Message) bool {
		r, _ := m.(*v1.CreateJobRequest)
		return r.GetSend() != nil
	},
	nopacsv1connect.ReceiverServiceAddReceiverProcedure: func(m proto.Message) bool {
		r, _ := m.(*v1.AddReceiverRequest)
		return forwards(r.GetReceiver())
	},
	nopacsv1connect.ReceiverServiceSetReceiverProcedure: func(m proto.Message) bool {
		r, _ := m.(*v1.SetReceiverRequest)
		return forwards(r.GetReceiver())
	},
}

// forwards reports whether a receiver names a sender to forward to, by
// forward_to or in a route.
func forwards(r *v1.Receiver) bool {
	if len(r.GetForwardTo()) > 0 {
		return true
	}
	for _, rt := range r.GetRouting().GetRoutes() {
		if len(rt.GetForwardTo().GetSenders()) > 0 {
			return true
		}
	}
	return false
}

// choosesDestination reports whether a request to proc chooses where
// archived data goes by destinationWhen.
func choosesDestination(proc string, req proto.Message) bool {
	when, ok := destinationWhen[proc]
	if !ok {
		return false
	}
	return when == nil || when(req)
}

// writesValues names the methods whose every request writes values the
// caller chooses into archived files, which a masked key is refused
// (access.Requirement.WritesValues). A method not here never does.
var writesValues = map[string]bool{
	nopacsv1connect.CorrectionServiceEditProcedure: true,
}

// norm returns r as marked to answer original values, to choose a
// destination and to write values, so two requirements are compared by
// kind, role and tenant alone; the marks are checked apart.
func norm(r access.Requirement) access.Requirement {
	return r.AnswersOriginals().ChoosesDestination().WritesValues()
}

// withField returns msg with the top-level field named name set: a string
// to a value it could hold (a UID, a name), a bool to true.
func withField(t *testing.T, msg proto.Message, name, value string) proto.Message {
	t.Helper()
	m := proto.Clone(msg).ProtoReflect()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		t.Fatalf("%s has no field %s", m.Descriptor().FullName(), name)
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		m.Set(fd, protoreflect.ValueOfBool(true))
	case protoreflect.StringKind:
		m.Set(fd, protoreflect.ValueOfString(value))
	default:
		t.Fatalf("%s.%s is neither a string nor a bool", m.Descriptor().FullName(), name)
	}
	return m.Interface()
}

// withSettingName returns msg with the setting's name (a field "name", or
// "name" inside "setting") set.
func withSettingName(t *testing.T, msg proto.Message, name string) proto.Message {
	t.Helper()
	m := proto.Clone(msg).ProtoReflect()
	target := m
	if s := m.Descriptor().Fields().ByName("setting"); s != nil {
		target = m.Mutable(s).Message()
	}
	target.Set(target.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString(name))
	return m.Interface()
}

// checkTenantRole checks a requirement of role on the tenant a request
// names by its field "tenant": on that tenant when it is valid, and on the
// installation when it is malformed or missing.
func checkTenantRole(t *testing.T, proc string, need func(proto.Message) access.Requirement, msg proto.Message, role master.Role) {
	t.Helper()
	for _, c := range []struct {
		name, tenant string
		want         access.Requirement
	}{
		{"a tenant", "lab1", access.OnTenant("lab1", role)},
		{"a malformed tenant", "Lab 1", access.OnInstallation(role)},
		{"no tenant", "", access.OnInstallation(role)},
	} {
		named := withField(t, msg, "tenant", c.tenant)
		if c.tenant == "" {
			named = msg
		}
		if got := need(named); got != norm(c.want) {
			t.Errorf("%s with %s: %+v, want %+v", proc, c.name, got, c.want)
		}
	}
}

// withScope returns msg with its scope (a field "scope", or "scope" inside
// "setting") set to s, nil to leave it unset.
func withScope(t *testing.T, msg proto.Message, s *v1.Scope) proto.Message {
	t.Helper()
	m := proto.Clone(msg).ProtoReflect()
	target := m
	fd := m.Descriptor().Fields().ByName("scope")
	if fd == nil {
		setting := m.Descriptor().Fields().ByName("setting")
		if setting == nil {
			t.Fatalf("%s has no scope", m.Descriptor().FullName())
		}
		target = m.Mutable(setting).Message()
		fd = target.Descriptor().Fields().ByName("scope")
	}
	if s != nil {
		target.Set(fd, protoreflect.ValueOfMessage(s.ProtoReflect()))
	}
	return m.Interface()
}

// requirementTable is what the server asks of a method table: api.Methods
// for the services served, api.Pending for those not served yet.
type requirementTable interface {
	RequestFor(procedure string) (proto.Message, bool)
	Requirement(procedure string, msg proto.Message) (access.Requirement, access.Request)
}

// TestRequirementsFollowTheRoleLines guards the method-to-requirement table
// against the proto text itself, in both directions: every rpc of the proto
// files has a Role line of a known shape and a line with that shape,
// request by request, in api.Methods for a service served and in
// api.Pending for a service not served yet, so a method is held to its
// Role line from the change that writes its contract; and every method of
// the descriptors is an rpc of the files. A malformed or missing scope or
// name is held to the role on the installation (the accepted reference).
func TestRequirementsFollowTheRoleLines(t *testing.T) {
	lines := roleLines(t)
	notServed := readNotServed(t)
	methods := contractMethods()
	for proc := range methods {
		if _, ok := lines[proc]; !ok {
			t.Errorf("%s is in the descriptors but not an rpc of the proto files", proc)
		}
	}
	for proc, text := range lines {
		var m requirementTable = api.Methods{}
		if inServices(notServed, proc) {
			// What is not served is left out of contractMethods, which
			// holds the served methods; its descriptor is looked up by
			// name.
			m = api.Pending{}
			if _, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(strings.ReplaceAll(strings.TrimPrefix(proc, "/"), "/", "."))); err != nil {
				t.Errorf("%s is an rpc of the files but not in the descriptors: %v", proc, err)
			}
		} else if _, ok := methods[proc]; !ok {
			t.Errorf("%s is an rpc of the files but not in the descriptors", proc)
		}
		shape, ok := rolePhrases[text]
		if !ok {
			t.Errorf("%s: the Role line %q is not one this test knows; add it to rolePhrases with its shape", proc, text)
			continue
		}
		msg, known := m.RequestFor(proc)
		if !known {
			t.Errorf("%s: not in the table (%T)", proc, m)
			continue
		}
		need := func(req proto.Message) access.Requirement {
			r, _ := m.Requirement(proc, req)
			// Whether the request answers originals, which a masked key is
			// refused, is checked here; the role is compared below.
			if has, want := r.AnswersOriginals() == r, answersOriginals(proc, req); has != want {
				t.Errorf("%s: answers original values %v, want %v (request %v)", proc, has, want, req)
			}
			if has, want := r.ChoosesDestination() == r, choosesDestination(proc, req); has != want {
				t.Errorf("%s: chooses a destination %v, want %v (request %v)", proc, has, want, req)
			}
			if has, want := r.WritesValues() == r, writesValues[proc]; has != want {
				t.Errorf("%s: writes values %v, want %v (request %v)", proc, has, want, req)
			}
			return norm(r)
		}
		switch shape {
		case fixedAdmin, fixedAuthenticated:
			want := access.OnInstallation(master.RoleAdmin)
			if shape == fixedAuthenticated {
				want = access.Authenticated()
			}
			if msg != nil {
				t.Errorf("%s: a fixed Role line, but the table reads the request", proc)
			}
			if got := need(nil); got != norm(want) {
				t.Errorf("%s: %+v, want %+v", proc, got, want)
			}
		case registration:
			if msg == nil {
				t.Fatalf("%s: the table does not read the request", proc)
			}
			named := proto.Clone(msg)
			named.ProtoReflect().Set(named.ProtoReflect().Descriptor().Fields().ByName("name"), protoreflect.ValueOfString("lab1"))
			if got := need(named); got != norm(access.TenantRegistration("lab1")) {
				t.Errorf("%s with a tenant: %+v", proc, got)
			}
			if got := need(msg); got != norm(access.OnInstallation(master.RoleReader)) {
				t.Errorf("%s with no name: %+v", proc, got)
			}
		case scopeReader, scopeAdmin:
			role := master.RoleReader
			if shape == scopeAdmin {
				role = master.RoleAdmin
			}
			if msg == nil {
				t.Fatalf("%s: the table does not read the request", proc)
			}
			for _, c := range []struct {
				name  string
				scope *v1.Scope
				want  access.Requirement
			}{
				{"a tenant", tenantScope("lab1"), access.OnTenant("lab1", role)},
				{"the installation", installation(), access.OnInstallation(role)},
				{"a malformed tenant", tenantScope("Lab 1"), access.OnInstallation(role)},
				{"no scope", nil, access.OnInstallation(role)},
			} {
				if got := need(withScope(t, msg, c.scope)); got != norm(c.want) {
					t.Errorf("%s with %s: %+v, want %+v", proc, c.name, got, c.want)
				}
			}
		case grantsScope, grantsTenant:
			if msg == nil {
				t.Fatalf("%s: the table does not read the request", proc)
			}
			admin := access.OnInstallation(master.RoleAdmin)
			for _, c := range []struct {
				name   string
				tenant string
				scope  *v1.Scope
				want   access.Requirement
			}{
				{"a tenant", "lab1", tenantScope("lab1"), access.TenantGrants("lab1")},
				{"the installation", "", installation(), admin},
				{"a malformed tenant", "Lab 1", tenantScope("Lab 1"), admin},
				{"none", "", nil, admin},
			} {
				named := msg
				switch {
				case shape == grantsScope:
					named = withScope(t, msg, c.scope)
				case c.tenant != "":
					named = withField(t, msg, "tenant", c.tenant)
				}
				if got := need(named); got != norm(c.want) {
					t.Errorf("%s with %s: %+v, want %+v", proc, c.name, got, c.want)
				}
			}
		case monitorScope:
			if msg == nil {
				t.Fatalf("%s: the table does not read the request", proc)
			}
			for _, c := range []struct {
				name  string
				scope *v1.Scope
				want  access.Requirement
			}{
				{"a tenant", tenantScope("lab1"), access.OnTenant("lab1", master.RoleReader)},
				{"the installation", installation(), access.OnInstallation(master.RoleOperator)},
				{"a malformed tenant", tenantScope("Lab 1"), access.OnInstallation(master.RoleOperator)},
				{"no scope", nil, access.OnInstallation(master.RoleOperator)},
			} {
				if got := need(withScope(t, msg, c.scope)); got != norm(c.want) {
					t.Errorf("%s with %s: %+v, want %+v", proc, c.name, got, c.want)
				}
			}
		case settingChange:
			if msg == nil {
				t.Fatalf("%s: the table does not read the request", proc)
			}
			// An ordinary setting: admin on the scope named.
			ordinary := withSettingName(t, msg, settings.EventsRetention)
			for _, c := range []struct {
				name  string
				scope *v1.Scope
				want  access.Requirement
			}{
				{"a tenant", tenantScope("lab1"), access.OnTenant("lab1", master.RoleAdmin)},
				{"the installation", installation(), access.OnInstallation(master.RoleAdmin)},
				{"a malformed tenant", tenantScope("Lab 1"), access.OnInstallation(master.RoleAdmin)},
				{"no scope", nil, access.OnInstallation(master.RoleAdmin)},
			} {
				if got := need(withScope(t, ordinary, c.scope)); got != norm(c.want) {
					t.Errorf("%s of an ordinary setting with %s: %+v, want %+v", proc, c.name, got, c.want)
				}
			}
			// Each setting the installation's admin alone sets on a tenant:
			// admin on the installation, whatever the scope.
			for _, n := range []string{settings.ReceiverPorts, settings.IncomingLimit} {
				named := withSettingName(t, msg, n)
				for _, sc := range []*v1.Scope{tenantScope("lab1"), installation(), tenantScope("Lab 1"), nil} {
					if got := need(withScope(t, named, sc)); got != norm(access.OnInstallation(master.RoleAdmin)) {
						t.Errorf("%s of %s with scope %v: %+v, want admin on the installation", proc, n, sc, got)
					}
				}
			}
		case tenantReader, tenantOperator, tenantAdmin, tenantReaderOrOperator:
			role := map[roleShape]master.Role{tenantReader: master.RoleReader, tenantOperator: master.RoleOperator, tenantAdmin: master.RoleAdmin, tenantReaderOrOperator: master.RoleReader}[shape]
			if msg == nil {
				t.Fatalf("%s: the table does not read the request", proc)
			}
			checkTenantRole(t, proc, need, msg, role)
			for _, f := range operatorWhen[text] {
				checkTenantRole(t, proc+" with "+f, need, withField(t, msg, f, "2.25.1"), master.RoleOperator)
			}
		case tenantOperatorOrAdmin:
			if msg == nil {
				t.Fatalf("%s: the table does not read the request", proc)
			}
			checkTenantRole(t, proc, need, msg, master.RoleOperator)
			with := proto.Clone(msg)
			r := with.ProtoReflect()
			fd := r.Descriptor().Fields().ByName(protoreflect.Name(adminWhen[text]))
			if fd == nil || fd.Kind() != protoreflect.MessageKind {
				t.Fatalf("%s: the request has no message field %q", proc, adminWhen[text])
			}
			r.Set(fd, r.NewField(fd))
			checkTenantRole(t, proc+" with "+adminWhen[text], need, with, master.RoleAdmin)
		}
	}
}

// TestPendingRules guards the table of the methods not served yet from
// drifting from the list and the contract: each of its lines names a
// method of a service the list names, reads a request of the method's own
// input type when it reads one, and the count says when a method was added
// to or dropped from a service not served without its line. It also holds
// the audit facts of the reading methods, and of the plain HTTP download,
// to their Audit lines: the object is the path of labels the request
// names, a value that does not pass the label rule is recorded as
// "invalid", and an instance id is recorded only as present, never as
// given; a profile method's object is the profile's name only when it
// passes the name rule. Those of the sender and job methods, now served, are
// held below as the served table gives them: a sender method's object is
// the sender's name only when it passes the rule of one, 57 characters at
// most; a job method's object is the job's id only when it has the form of
// one; and a new job's target is recorded as its path of labels.
func TestPendingRules(t *testing.T) {
	notServed := readNotServed(t)
	lines := roleLines(t)
	var p api.Pending
	procs := p.Procedures()
	if len(procs) != 3 {
		t.Errorf("the table of methods not served has %d lines; this test was written for 3, so look at what changed", len(procs))
	}
	for _, proc := range procs {
		if !inServices(notServed, proc) {
			t.Errorf("%s has a line among the methods not served, but its service is not on the list: move the line to the served table", proc)
		}
		if _, ok := lines[proc]; !ok {
			t.Errorf("%s has a line among the methods not served, but is not an rpc of the proto files", proc)
			continue
		}
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(strings.ReplaceAll(strings.TrimPrefix(proc, "/"), "/", ".")))
		md, ok := d.(protoreflect.MethodDescriptor)
		if err != nil || !ok {
			t.Errorf("%s is not a method of the descriptors: %v", proc, err)
			continue
		}
		// A fixed line reads no request; TestRequirementsFollowTheRoleLines
		// holds it to its Role line.
		if msg, _ := p.RequestFor(proc); msg != nil && msg.ProtoReflect().Descriptor().FullName() != md.Input().FullName() {
			t.Errorf("%s: the table reads %v, the method takes a %s", proc, msg, md.Input().FullName())
		}
	}

	const uid = "2.25.4711"
	facts := func(proc string, msg proto.Message) access.Request {
		t.Helper()
		_, f := api.Methods{}.Requirement(proc, msg)
		for k, v := range f.Details {
			if strings.Contains(v, uid) || strings.Contains(v, "Smith") {
				t.Errorf("%s: the detail %s holds what the request gave: %q", proc, k, v)
			}
		}
		if strings.Contains(f.Object, uid) {
			t.Errorf("%s: the object holds a UID: %q", proc, f.Object)
		}
		return f
	}
	for _, c := range []struct {
		name    string
		proc    string
		msg     proto.Message
		object  string
		details map[string]string
	}{
		{"ReadFiles", nopacsv1connect.DataServiceReadFilesProcedure,
			&v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: "S01", SessionLabel: "E01", ScanLabel: "T1", InstanceId: uid, StartAfter: &v1.ReadPosition{InstanceId: uid}},
			"S01/E01/T1", map[string]string{"instance": "present", "start_after": "present", "announce_only": "false"}},
	} {
		f := facts(c.proc, c.msg)
		if f.Object != c.object {
			t.Errorf("%s: the object is %q, want %q", c.name, f.Object, c.object)
		}
		for k, v := range c.details {
			if f.Details[k] != v {
				t.Errorf("%s: the detail %s is %q, want %q", c.name, k, f.Details[k], v)
			}
		}
		for k, v := range f.Details {
			if strings.Contains(v, "data/") {
				t.Errorf("%s: the detail %s holds a storage key: %q", c.name, k, v)
			}
		}
	}

	// A profile method's object is the profile's name when it passes the
	// name rule, and "invalid" otherwise: what a caller sends in its place
	// can be anything, a patient's name included.
	for _, c := range []struct {
		proc   string
		msg    proto.Message
		object string
	}{
		{nopacsv1connect.ProfileServiceGetProfileVersionProcedure, &v1.GetProfileVersionRequest{Tenant: "lab1", Name: "research", Version: 2}, "research"},
		{nopacsv1connect.ProfileServiceGetProfileVersionProcedure, &v1.GetProfileVersionRequest{Tenant: "lab1", Name: "Smith^John"}, "invalid"},
		{nopacsv1connect.ProfileServiceCreateProfileVersionProcedure, &v1.CreateProfileVersionRequest{Tenant: "lab1", Name: "Smith^John"}, "invalid"},
		{nopacsv1connect.ProfileServiceListProfileVersionsProcedure, &v1.ListProfileVersionsRequest{Tenant: "lab1", Name: "research"}, "research"},
		{nopacsv1connect.ProfileServiceTryProfileProcedure, &v1.TryProfileRequest{Tenant: "lab1", Profile: &v1.TrialProfile{Kind: &v1.TrialProfile_Version{Version: &v1.ProfileVersionName{Name: "research", Version: 3}}}}, "research@3"},
		{nopacsv1connect.ProfileServiceTryProfileProcedure, &v1.TryProfileRequest{Tenant: "lab1", Profile: &v1.TrialProfile{Kind: &v1.TrialProfile_Version{Version: &v1.ProfileVersionName{Name: "Smith^John"}}}}, "invalid"},
		{nopacsv1connect.ProfileServiceExplainProfileProcedure, &v1.ExplainProfileRequest{Tenant: "lab1", Profile: &v1.TrialProfile{Kind: &v1.TrialProfile_Document{Document: &v1.ProfileDocument{}}}}, "document"},
		{nopacsv1connect.ProfileServiceDiffProfileVersionsProcedure, &v1.DiffProfileVersionsRequest{Tenant: "lab1", Name: "Smith^John"}, "invalid"},
	} {
		_, f := api.Methods{}.Requirement(c.proc, c.msg)
		if f.Object != c.object {
			t.Errorf("%s: the object is %q, want %q", c.proc, f.Object, c.object)
		}
	}
	if _, f := (api.Methods{}).Requirement(nopacsv1connect.ProfileServiceDiffProfileVersionsProcedure, &v1.DiffProfileVersionsRequest{Tenant: "lab1", Name: "research", ToName: "Smith^John"}); f.Details["to_name"] != "invalid" {
		t.Errorf("DiffProfileVersions's details are %v", f.Details)
	}
	if _, f := (api.Methods{}).Requirement(nopacsv1connect.ProfileServiceGetProfileVersionProcedure, &v1.GetProfileVersionRequest{Tenant: "lab1", Name: "research", Version: 2}); f.Details["version"] != "2" {
		t.Errorf("GetProfileVersion's details are %v", f.Details)
	}

	// A sender's name and a job's id are recorded as given only when they
	// pass their rules: a caller can send anything in their place.
	var methods api.Methods
	long := strings.Repeat("a", 58)
	for _, c := range []struct {
		proc   string
		msg    proto.Message
		object string
	}{
		{nopacsv1connect.SenderServiceGetSenderProcedure, &v1.GetSenderRequest{Tenant: "lab1", Name: "orthanc"}, "orthanc"},
		{nopacsv1connect.SenderServiceGetSenderProcedure, &v1.GetSenderRequest{Tenant: "lab1", Name: long}, "invalid"},
		{nopacsv1connect.SenderServiceGetSenderProcedure, &v1.GetSenderRequest{Tenant: "lab1", Name: long[:57]}, long[:57]},
		{nopacsv1connect.SenderServiceAddSenderProcedure, &v1.AddSenderRequest{Tenant: "lab1", Sender: &v1.Sender{Name: "Smith^John"}}, "invalid"},
		{nopacsv1connect.SenderServiceRemoveSenderProcedure, &v1.RemoveSenderRequest{Tenant: "lab1", Name: "pacs2"}, "pacs2"},
		{nopacsv1connect.JobServiceGetJobProcedure, &v1.GetJobRequest{Tenant: "lab1", Id: "01JD7Q8X3N"}, "01JD7Q8X3N"},
		{nopacsv1connect.JobServiceCancelJobProcedure, &v1.CancelJobRequest{Tenant: "lab1", Id: "Smith^John"}, "invalid"},
		{nopacsv1connect.JobServiceRetryJobProcedure, &v1.RetryJobRequest{Tenant: "lab1", Id: strings.Repeat("a", 65)}, "invalid"},
		{nopacsv1connect.JobServiceListJobFailuresProcedure, &v1.ListJobFailuresRequest{Tenant: "lab1", Id: ""}, "invalid"},
	} {
		_, f := methods.Requirement(c.proc, c.msg)
		if f.Object != c.object {
			t.Errorf("%s: the object is %q, want %q", c.proc, f.Object, c.object)
		}
	}
	if _, f := methods.Requirement(nopacsv1connect.SenderServiceAddSenderProcedure, &v1.AddSenderRequest{Tenant: "lab1", Sender: &v1.Sender{Name: "pacs2", Type: &v1.Sender_Stow{Stow: &v1.StowSender{Credential: "s3cr3t-token"}}}}); f.Details["type"] != "stow" || strings.Contains(fmt.Sprint(f), "s3cr3t-token") {
		t.Errorf("AddSender's facts are %+v: want its type, and never its credential", f)
	}
	if _, f := methods.Requirement(nopacsv1connect.SenderServiceRemoveSenderProcedure, &v1.RemoveSenderRequest{Tenant: "lab1", Name: "pacs2", CancelJobs: true}); f.Details["cancel_jobs"] != "true" {
		t.Errorf("RemoveSender's details are %v", f.Details)
	}
	if _, f := methods.Requirement(nopacsv1connect.JobServiceCreateJobProcedure, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Send{Send: &v1.SendJob{Sender: "orthanc", SubjectLabel: "S01", SessionLabel: "Smith^John"}}}); f.Details["kind"] != "send" || f.Details["sender"] != "orthanc" || f.Details["target"] != "S01/invalid" || f.Object != "" {
		t.Errorf("CreateJob's facts are %+v", f)
	}
	if _, f := methods.Requirement(nopacsv1connect.JobServiceListJobsProcedure, &v1.ListJobsRequest{Tenant: "lab1", States: []v1.JobState{v1.JobState_JOB_STATE_RUNNING}, InProgress: true, Sender: "Smith^John"}); f.Details["states"] != "JOB_STATE_RUNNING" || f.Details["in_progress"] != "true" || f.Details["sender"] != "invalid" {
		t.Errorf("ListJobs' details are %v", f.Details)
	}

	// The download over plain HTTP: reader on the tenant the path names,
	// the scan's path as its object, the instance id only as present.
	need, f := api.DownloadFacts("lab1", "S01", "E01", "T1", uid, true, false)
	if need != access.OnTenant("lab1", master.RoleReader) {
		t.Errorf("a download needs %+v, want reader on the tenant", need)
	}
	if f.Object != "S01/E01/T1" || f.Details["instance"] != "present" || f.Details["range"] != "true" || f.Details["window"] != "false" {
		t.Errorf("a download's facts are %+v", f)
	}
	for k, v := range f.Details {
		if strings.Contains(v, uid) {
			t.Errorf("a download's detail %s holds the instance id: %q", k, v)
		}
	}
	if need, _ := api.DownloadFacts("Lab 1", "S01", "E01", "T1", uid, false, false); need != access.OnInstallation(master.RoleReader) {
		t.Errorf("a download into a malformed tenant needs %+v, want reader on the installation", need)
	}
	if strings.Contains(api.DownloadMethod, "%") || !strings.HasPrefix(api.DownloadMethod, "GET /tenants/{tenant}/instances/") {
		t.Errorf("the download's audit method is %q", api.DownloadMethod)
	}
}
