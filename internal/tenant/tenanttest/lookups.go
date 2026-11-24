package tenanttest

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// lookupKey returns a well-formed key of the table: a keyed hash's text,
// sixteen characters of lower case base32, made from n.
func lookupKey(table string, n int) tenant.LookupKey {
	k := fmt.Sprintf("%016d", n)
	k = strings.Map(func(r rune) rune { return 'a' + (r - '0') }, k)
	return tenant.LookupKey{Table: table, Key: k}
}

// testLookupCodes guards a lookup table's codes: none before one is
// assigned; a batch gives each key it has no code for the table's next
// number in decimal from 1, in the batch's order, a key named twice one
// code; a key keeps its code when assigned again, alone or among new ones;
// each table counts on its own; LookupCode answers what was assigned.
func testLookupCodes(e *penv) {
	a1, a2, a3 := lookupKey("accession", 1), lookupKey("accession", 2), lookupKey("accession", 3)
	if _, found, err := e.p.LookupCode(e.ctx, a1); err != nil || found {
		e.Fatalf("LookupCode before any code: found %v, %v", found, err)
	}
	got, err := e.p.AssignLookupCodes(e.ctx, []tenant.LookupKey{a2, a1, a2}, at(1))
	e.ok("AssignLookupCodes", err)
	if !slices.Equal(got, []string{"1", "2", "1"}) {
		e.Errorf("the first batch's codes: %q", got)
	}
	got, err = e.p.AssignLookupCodes(e.ctx, []tenant.LookupKey{a3, a1, lookupKey("subject", 1), a2}, at(2))
	e.ok("AssignLookupCodes again", err)
	if !slices.Equal(got, []string{"3", "2", "1", "1"}) {
		e.Errorf("the second batch's codes: %q", got)
	}
	for k, want := range map[tenant.LookupKey]string{a1: "2", a2: "1", a3: "3", lookupKey("subject", 1): "1"} {
		code, found, err := e.p.LookupCode(e.ctx, k)
		if err != nil || !found || code != want {
			e.Errorf("LookupCode %v = %q, found %v, %v; want %q", k, code, found, err, want)
		}
	}
	if _, found, _ := e.p.LookupCode(e.ctx, lookupKey("subject", 2)); found {
		e.Error("a key never assigned has a code")
	}
	if n := len(e.allAudit(master.AuditFilter{})); n != 0 {
		e.Errorf("assigning codes wrote %d audit entries; it is the server's work", n)
	}
}

// testLookupRefusals guards what a lookup takes: a table's name under the
// rule of a tenant's name, a key that is a keyed hash's text, a batch of 1
// to MaxLookupBatch; a refused batch assigns nothing, not even its valid
// keys.
func testLookupRefusals(e *penv) {
	good := lookupKey("accession", 1)
	for _, c := range []struct {
		what string
		key  tenant.LookupKey
	}{
		{"a malformed table", tenant.LookupKey{Table: "Accession", Key: good.Key}},
		{"a short key", tenant.LookupKey{Table: "accession", Key: good.Key[:15]}},
		{"a key in upper case", tenant.LookupKey{Table: "accession", Key: strings.ToUpper(good.Key)}},
		{"a key outside base32", tenant.LookupKey{Table: "accession", Key: "0000000000000001"}},
	} {
		_, _, err := e.p.LookupCode(e.ctx, c.key)
		e.wantKind("LookupCode of "+c.what, err, fault.Invalid)
		_, err = e.p.AssignLookupCodes(e.ctx, []tenant.LookupKey{good, c.key}, at(1))
		e.wantKind("AssignLookupCodes of "+c.what, err, fault.Invalid)
	}
	_, err := e.p.AssignLookupCodes(e.ctx, nil, at(1))
	e.wantKind("AssignLookupCodes of none", err, fault.Invalid)
	many := make([]tenant.LookupKey, tenant.MaxLookupBatch+1)
	for i := range many {
		many[i] = lookupKey("accession", i)
	}
	_, err = e.p.AssignLookupCodes(e.ctx, many, at(1))
	e.wantKind("AssignLookupCodes past the bound", err, fault.Invalid)
	if _, found, _ := e.p.LookupCode(e.ctx, good); found {
		e.Error("a refused batch assigned a code")
	}
	got, err := e.p.AssignLookupCodes(e.ctx, many[:tenant.MaxLookupBatch], at(2))
	e.ok("AssignLookupCodes at the bound", err)
	if got[0] != "1" || got[len(got)-1] != strconv.Itoa(tenant.MaxLookupBatch) {
		e.Errorf("a full batch's codes run %q to %q", got[0], got[len(got)-1])
	}
}

// testLookupAtOnce guards assignments at once: many batches over two
// tables, overlapping in their keys and naming the tables in either order,
// give each key one code whatever runs at once, and each table's codes are
// 1 to the number of its keys, with no gap and none twice.
func testLookupAtOnce(e *penv) {
	const n, per = 8, 20
	type result struct {
		keys  []tenant.LookupKey
		codes []string
		err   error
	}
	out := make([]result, n)
	done := make(chan struct{}, n)
	for i := range n {
		go func() {
			defer func() { done <- struct{}{} }()
			var keys []tenant.LookupKey
			for j := range per {
				table := []string{"table-a", "table-b"}[(i+j)%2]
				keys = append(keys, lookupKey(table, (i*7+j)%40))
			}
			out[i].keys = keys
			out[i].codes, out[i].err = e.p.AssignLookupCodes(e.ctx, keys, at(1))
		}()
	}
	for range n {
		<-done
	}
	byKey := map[tenant.LookupKey]string{}
	for _, r := range out {
		e.ok("AssignLookupCodes at once", r.err)
		for j, k := range r.keys {
			if code, ok := byKey[k]; ok && code != r.codes[j] {
				e.Errorf("the key %v has the codes %s and %s", k, code, r.codes[j])
			}
			byKey[k] = r.codes[j]
		}
	}
	for _, table := range []string{"table-a", "table-b"} {
		var codes []int
		for k, c := range byKey {
			if k.Table == table {
				v, err := strconv.Atoi(c)
				e.ok("a code is a number", err)
				codes = append(codes, v)
			}
		}
		slices.Sort(codes)
		for i, c := range codes {
			if c != i+1 {
				e.Fatalf("%s's codes are %v: a gap or a code given twice", table, codes)
			}
		}
	}
}

// testDeidentificationKeys guards the tenant's keys of UIDs and dates:
// each is kept apart from the naming key and from the other, each first
// writer's bytes kept and never replaced, and a name the port does not
// keep refused.
func testDeidentificationKeys(e *penv) {
	pp := e.parts()
	for i, name := range []tenant.SecretName{tenant.SecretNaming, tenant.SecretUID, tenant.SecretDate} {
		got, err := pp.KeepTenantSecret(e.ctx, name, []byte{byte(i), 0xff}, at(1))
		e.ok("KeepTenantSecret "+string(name), err)
		if !slices.Equal(got, []byte{byte(i), 0xff}) {
			e.Errorf("KeepTenantSecret %s = %q", name, got)
		}
	}
	for i, name := range []tenant.SecretName{tenant.SecretNaming, tenant.SecretUID, tenant.SecretDate} {
		again, err := pp.KeepTenantSecret(e.ctx, name, []byte("replacement"), at(2))
		e.ok("KeepTenantSecret again "+string(name), err)
		stored, err := pp.TenantSecret(e.ctx, name)
		e.ok("TenantSecret "+string(name), err)
		if !slices.Equal(again, []byte{byte(i), 0xff}) || !slices.Equal(stored, again) {
			e.Errorf("the %s key was replaced: %q, %q", name, again, stored)
		}
	}
	_, err := pp.KeepTenantSecret(e.ctx, "uids", []byte("x"), at(1))
	e.wantKind("KeepTenantSecret of an unknown name", err, fault.Invalid)
}
