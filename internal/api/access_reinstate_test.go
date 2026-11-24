package api_test

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// testReinstateKeys guards ReinstateKeys: after every key is suspended, as a
// restore leaves them, a suspended key is refused as no good; a key with
// admin on the installation is denied, before its request is read; the
// local operator's invalid and unknown requests are answered and recorded;
// a key it reinstates by id is answered, audited with its id as the object,
// and works again at once, while a key named that was not suspended is not
// listed; and all answers only the count, with one entry per key, and a
// second all reinstates nothing.
func testReinstateKeys(t *testing.T) {
	f := newFixture(t)
	victim, victimID := f.key(f.onTenant("lab1", master.RoleReader))
	ks, ok := f.master.(master.KeySuspension)
	if !ok {
		t.Fatal("the test's master cannot suspend keys")
	}
	if _, err := ks.SuspendKeys(context.Background(), f.now(), f.setupEntry()); err != nil {
		t.Fatal(err)
	}
	admin, adminID := f.key(onInstallation(master.RoleAdmin)) // made after: not suspended
	reinstate := func(ids []string, all bool) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.access.ReinstateKeys, &v1.ReinstateKeysRequest{KeyIds: ids, All: all})
		}
	}
	whoAmI := func(c clients) outcome { return call(c.access.WhoAmI, &v1.WhoAmIRequest{}) }
	tooMany := make([]string, master.MaxReinstateKeys+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("k%011d", i)
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: reinstate([]string{victimID}, false), code: connect.CodeUnauthenticated},
		{name: "a suspended key", as: victim, call: whoAmI, code: connect.CodeUnauthenticated},
		{name: "a suspended admin key", as: f.instAdmin, call: reinstate(nil, true), code: connect.CodeUnauthenticated},
		{name: "an admin key", as: admin, call: reinstate([]string{victimID}, false), code: connect.CodePermissionDenied,
			audit: &wantAudit{outcome: master.OutcomeRefused, reason: "only the local operator"}},
		{name: "an admin key, all", as: admin, call: reinstate(nil, true), code: connect.CodePermissionDenied},
		{name: "an admin key's malformed request", as: admin, call: reinstate([]string{"not valid"}, true), code: connect.CodePermissionDenied},
		{name: "ids and all", as: local, call: reinstate([]string{victimID}, true), code: connect.CodeInvalidArgument,
			audit: failed("not both")},
		{name: "neither", as: local, call: reinstate(nil, false), code: connect.CodeInvalidArgument},
		{name: "1001 ids", as: local, call: reinstate(tooMany, false), code: connect.CodeInvalidArgument},
		{name: "an id not valid", as: local, call: reinstate([]string{"not valid"}, false), code: connect.CodeInvalidArgument},
		{name: "an unknown id", as: local, call: reinstate([]string{victimID, "nokey0000000"}, false), code: connect.CodeNotFound,
			msg: "nokey0000000", audit: failed("nokey0000000")},
		{name: "still suspended after the unknown id", as: victim, call: whoAmI, code: connect.CodeUnauthenticated},
		{name: "by id", as: local, call: reinstate([]string{adminID, victimID, victimID}, false),
			audit: &wantAudit{outcome: master.OutcomeDone, object: victimID, details: map[string]string{"all": "false"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ReinstateKeysResponse)
				if r.GetReinstated() != 1 || len(r.GetKeys()) != 1 || r.GetKeys()[0].GetId() != victimID || r.GetKeys()[0].GetSuspended() {
					t.Errorf("answer %v; want the one suspended key, not suspended", r)
				}
			}},
		{name: "the reinstated key works", as: victim, call: whoAmI},
		{name: "a key not named stays suspended", as: f.instReader, call: whoAmI, code: connect.CodeUnauthenticated},
		{name: "all", as: local, call: reinstate(nil, true),
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ReinstateKeysResponse)
				if r.GetReinstated() != 7 || len(r.GetKeys()) != 0 {
					t.Errorf("answer %v; want the count of the fixture's seven keys and no keys", r)
				}
				objects := map[string]bool{}
				for _, en := range entriesOf(f.masterLog(), o.requestID) {
					if en.Outcome != master.OutcomeDone || en.Details["all"] != "true" || en.LocalUser != "501" {
						t.Errorf("entry %+v", en)
					}
					objects[en.Object] = true
				}
				if len(objects) != 7 || !objects[f.instAdminID] || objects[victimID] || objects[adminID] {
					t.Errorf("entries name %v; want one per key reinstated", objects)
				}
			}},
		{name: "an admin key reinstated works", as: f.instAdmin, call: func(c clients) outcome {
			return call(c.access.GetKey, &v1.GetKeyRequest{Id: victimID})
		}},
		{name: "all again", as: local, call: reinstate(nil, true),
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.ReinstateKeysResponse); r.GetReinstated() != 0 {
					t.Errorf("answer %v; want nothing reinstated", r)
				}
				if got := entriesOf(f.masterLog(), o.requestID); len(got) != 0 {
					t.Errorf("a reinstatement that changed nothing wrote %+v", got)
				}
			}},
	})
}
