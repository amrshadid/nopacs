package settings

import "testing"

// TestBaseProfileValue guards the form of deid.base-profile: empty for no
// base, otherwise a name under a tenant's rule and a version from 1 written
// without a sign or leading zeros, so one version has one spelling.
func TestBaseProfileValue(t *testing.T) {
	for v, want := range map[string]int32{"": 0, "research@1": 1, "a-b9@12": 12, "x@2147483647": 2147483647} {
		name, n, err := ParseBaseProfile(v)
		if err != nil || n != want || (v != "" && name+"@" != v[:len(name)+1]) {
			t.Errorf("%q: %q, %d, %v; want version %d", v, name, n, err, want)
		}
	}
	for _, v := range []string{"research", "research@", "research@0", "research@01", "research@-1", "research@1.5", "@1",
		"Research@1", "9a@1", "research@2147483648", "research@1 ", " research@1", "a@b@1"} {
		if _, _, err := ParseBaseProfile(v); err == nil {
			t.Errorf("%q is taken", v)
		}
	}
	d, ok := MustBuiltin().Lookup(DeidBaseProfile)
	if !ok || !d.TenantOverride || !d.TenantOnly || d.TenantSetByInstallation || d.Default != "" {
		t.Errorf("the definition is %+v: a tenant's own, no installation value, set by the tenant's admin", d)
	}
}
