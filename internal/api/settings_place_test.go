package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
)

// set and unset are calls of SetSetting and UnsetSetting.
func set(name string, scope *v1.Scope, value string) func(clients) outcome {
	return setSetting(name, scope, value)
}

// unset is a call of UnsetSetting.
func unset(name string, scope *v1.Scope) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.settings.UnsetSetting, &v1.UnsetSettingRequest{Name: name, Scope: scope})
	}
}

// TestSettingKinds guards who may set each kind of setting, and where: a
// setting the installation's admin alone sets on a tenant is refused to the
// tenant's own admin, as for no grant, and recorded in the installation's
// log, while the installation's admin sets it on the tenant, with the entry
// in the tenant's log; such a setting with no installation value refuses
// the installation scope; an installation-only setting refuses a tenant
// scope; an ordinary one a tenant may override is the tenant admin's.
func TestSettingKinds(t *testing.T) {
	f := newPlaceFixture(t)
	f.run(t, []tcase{
		{name: "ports, by the tenant's admin", as: f.lab1Admin, call: set(settings.ReceiverPorts, tenantScope("lab1"), "11112"), code: connect.CodePermissionDenied, msg: access.MessageDenied, audit: refused()},
		{name: "ports, by a reader on the installation", as: f.instReader, call: set(settings.ReceiverPorts, tenantScope("lab1"), "11112"), code: connect.CodePermissionDenied},
		{name: "ports, by the installation's admin", as: f.instAdmin, call: set(settings.ReceiverPorts, tenantScope("lab1"), "11112,11120-11129"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: settings.ReceiverPorts, tenant: "lab1", details: map[string]string{"scope": "tenant/lab1"}}},
		{name: "ports, read by a reader on the tenant", as: f.lab1Reader, call: getSetting(settings.ReceiverPorts, tenantScope("lab1")),
			check: func(t *testing.T, o outcome) {
				if s := o.res.(*v1.GetSettingResponse).GetSetting(); s.GetValue() != "11112,11120-11129" || s.GetSource() != v1.SettingSource_SETTING_SOURCE_TENANT {
					t.Errorf("%v", s)
				}
			}},
		{name: "ports, on the installation", as: f.instAdmin, call: set(settings.ReceiverPorts, installation(), "11112"), code: connect.CodeInvalidArgument, msg: "no installation value"},
		{name: "ports, unset on the installation", as: f.instAdmin, call: unset(settings.ReceiverPorts, installation()), code: connect.CodeInvalidArgument, msg: "no installation value"},
		{name: "ports, a malformed value", as: f.instAdmin, call: set(settings.ReceiverPorts, tenantScope("lab1"), "11112-"), code: connect.CodeInvalidArgument, msg: "setting.value"},
		{name: "ports, a tenant that does not exist", as: f.instAdmin, call: set(settings.ReceiverPorts, tenantScope("nolab"), "11112"), code: connect.CodeNotFound, msg: access.MessageNoTenant},
		{name: "ports, a tenant that does not exist, no right to know", as: f.lab1Admin, call: set(settings.ReceiverPorts, tenantScope("nolab"), "11112"), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "ports, unset by the tenant's admin", as: f.lab1Admin, call: unset(settings.ReceiverPorts, tenantScope("lab1")), code: connect.CodePermissionDenied},
		{name: "incoming limit, by the tenant's admin", as: f.lab1Admin, call: set(settings.IncomingLimit, tenantScope("lab1"), "200GiB"), code: connect.CodePermissionDenied},
		{name: "incoming limit, on the tenant by the installation's admin", as: f.instAdmin, call: set(settings.IncomingLimit, tenantScope("lab1"), "200GiB")},
		{name: "incoming limit, on the installation", as: f.instAdmin, call: set(settings.IncomingLimit, installation(), "1TiB")},
		{name: "incoming limit, not a size", as: f.instAdmin, call: set(settings.IncomingLimit, installation(), "1TB"), code: connect.CodeInvalidArgument},
		{name: "retention, by the tenant's admin", as: f.lab1Admin, call: set(settings.EventsRetention, tenantScope("lab1"), "30d"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: settings.EventsRetention}},
		{name: "retention, below a day", as: f.lab1Admin, call: set(settings.EventsRetention, tenantScope("lab1"), "23h"), code: connect.CodeInvalidArgument},
		{name: "roots, on a tenant", as: f.lab1Admin, call: set(settings.StorageDirectoryRoots, tenantScope("lab1"), "a=/x"), code: connect.CodeInvalidArgument, msg: "installation"},
		{name: "roots, a relative path", as: f.instAdmin, call: set(settings.StorageDirectoryRoots, installation(), "a=x"), code: connect.CodeInvalidArgument},
		{name: "roots, nested", as: f.instAdmin, call: set(settings.StorageDirectoryRoots, installation(), "fast="+f.root+"\ninner="+f.root+"/inner"), code: connect.CodeInvalidArgument},
		{name: "reserve, a value", as: f.instAdmin, call: set(settings.StorageReserve, installation(), "5%,20GiB")},
		{name: "reserve, on a tenant", as: f.lab1Admin, call: set(settings.StorageReserve, tenantScope("lab1"), "5%"), code: connect.CodeInvalidArgument},
		{name: "bind addresses, a spelling not canonical", as: f.instAdmin, call: set(settings.ReceiverBindAddresses, installation(), "2001:DB8::1"), code: connect.CodeInvalidArgument},
	})
	o := set(settings.ReceiverPorts, tenantScope("lab1"), "1")(f.as(f.lab1Admin))
	f.refusedNotInTenant(t, "a refusal", "lab1", o)
	// A setting of that kind the method table does not know: the tenant's
	// admin passes the check, and the handler refuses it all the same.
	o = set(testAllotted, tenantScope("lab1"), "x")(f.as(f.lab1Admin))
	if o.code() != connect.CodePermissionDenied || o.message() != access.MessageDenied {
		t.Errorf("a setting the table does not know, by the tenant's admin: %v %q", o.code(), o.message())
	}
	f.refusedNotInTenant(t, "a refusal by the handler", "lab1", o)
}

// TestValuesInUse guards the rule that a value in use cannot be taken away:
// ports a receiver uses, ports another tenant holds, a root, an endpoint or
// a bind address a storage or a receiver uses, each refused as a
// precondition, by a set that leaves it out and by an unset; and a change
// that keeps what is used is accepted. A tenant that is not ready cannot be
// checked, so a change is refused as unavailable until it is.
func TestValuesInUse(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	f.setSetting(installation(), settings.StorageS3Endpoints, "https://s3.example.com")
	mustCall[*v1.AddStorageResponse](t, "add", addStorage("lab1", s3Storage("cold", "https://s3.example.com", "secret"))(f.as(local)))
	f.setSetting(installation(), settings.ReceiverBindAddresses, "192.0.2.10,192.0.2.11")
	mustCall[*v1.AddReceiverResponse](t, "add", addReceiver("lab1", receiverWith(func(r *v1.Receiver) {
		r.Name, r.GetDimse().Port, r.GetDimse().BindAddress = "bound", 11120, "192.0.2.10"
	}))(f.as(local)))
	f.run(t, []tcase{
		{name: "lab2's ports over lab1's", as: f.instAdmin, call: set(settings.ReceiverPorts, tenantScope("lab2"), "11125-11130"), code: connect.CodeFailedPrecondition, msg: "allotted to the tenant lab1",
			audit: &wantAudit{log: "lab2", outcome: master.OutcomeFailed, object: settings.ReceiverPorts}},
		{name: "lab2's ports apart", as: f.instAdmin, call: set(settings.ReceiverPorts, tenantScope("lab2"), "12000-12010")},
		{name: "lab1's ports leaving out a receiver's", as: f.instAdmin, call: set(settings.ReceiverPorts, tenantScope("lab1"), "11120-11129"), code: connect.CodeFailedPrecondition, msg: "scanner1"},
		{name: "lab1's ports unset", as: f.instAdmin, call: unset(settings.ReceiverPorts, tenantScope("lab1")), code: connect.CodeFailedPrecondition},
		{name: "lab1's ports, more", as: f.instAdmin, call: set(settings.ReceiverPorts, tenantScope("lab1"), "11112,11120-11139")},
		{name: "lab1's ports over lab2's new ones", as: f.instAdmin, call: set(settings.ReceiverPorts, tenantScope("lab1"), "11112,11120-11139,12005"), code: connect.CodeFailedPrecondition, msg: "lab2"},
		{name: "the root removed", as: f.instAdmin, call: set(settings.StorageDirectoryRoots, installation(), ""), code: connect.CodeFailedPrecondition, msg: "main"},
		{name: "the root moved", as: f.instAdmin, call: set(settings.StorageDirectoryRoots, installation(), "fast="+f.dir), code: connect.CodeFailedPrecondition},
		{name: "the roots unset", as: f.instAdmin, call: unset(settings.StorageDirectoryRoots, installation()), code: connect.CodeFailedPrecondition},
		{name: "a root added beside it", as: f.instAdmin, call: set(settings.StorageDirectoryRoots, installation(), "fast="+f.root+"\nslow="+f.dir+"/slow")},
		{name: "the endpoint removed", as: f.instAdmin, call: set(settings.StorageS3Endpoints, installation(), "https://other.example.com"), code: connect.CodeFailedPrecondition, msg: "cold"},
		{name: "the endpoints unset", as: f.instAdmin, call: unset(settings.StorageS3Endpoints, installation()), code: connect.CodeFailedPrecondition},
		{name: "the endpoint in another spelling", as: f.instAdmin, call: set(settings.StorageS3Endpoints, installation(), "HTTPS://S3.example.com:443/")},
		{name: "the bind address removed", as: f.instAdmin, call: set(settings.ReceiverBindAddresses, installation(), "192.0.2.11"), code: connect.CodeFailedPrecondition, msg: "bound"},
		{name: "the bind addresses unset", as: f.instAdmin, call: unset(settings.ReceiverBindAddresses, installation()), code: connect.CodeFailedPrecondition},
		{name: "a bind address added", as: f.instAdmin, call: set(settings.ReceiverBindAddresses, installation(), "192.0.2.10,192.0.2.11,192.0.2.12")},
	})
	f.downTenant("down")
	f.run(t, []tcase{
		{name: "a tenant not ready", as: f.instAdmin, call: set(settings.StorageDirectoryRoots, installation(), "fast="+f.root), code: connect.CodeUnavailable, msg: "down"},
		{name: "a setting nothing uses, with a tenant not ready", as: f.instAdmin, call: set(settings.StorageReserve, installation(), "1%")},
	})
}
