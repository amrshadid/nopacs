package cli

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestScansOrder guards the order "catalog scans" asks for without --order:
// the scans of a subject named alone are served by label only, so asking
// for them newest first was refused by the server and the command could not
// list a subject's scans as its help says; with a session, or with a filter
// only the newest order serves, the server's default order is kept.
func TestScansOrder(t *testing.T) {
	label, unset := v1.CatalogOrder_CATALOG_ORDER_LABEL, v1.CatalogOrder_CATALOG_ORDER_UNSPECIFIED
	for _, c := range []struct {
		name                                 string
		subject, session, modality, receiver string
		span                                 bool
		want                                 v1.CatalogOrder
	}{
		{"every subject", "", "", "", "", false, unset},
		{"a subject alone", "sub-a", "", "", "", false, label},
		{"a session", "sub-a", "ses-1", "", "", false, unset},
		{"a subject and a modality", "sub-a", "", "MR", "", false, unset},
		{"a subject and a receiver", "sub-a", "", "", "scanner1", false, unset},
		{"a subject and a span of time", "sub-a", "", "", "", true, unset},
		{"a modality alone", "", "", "CT", "", false, unset},
	} {
		if got := scansOrder(c.subject, c.session, c.modality, c.receiver, c.span); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
