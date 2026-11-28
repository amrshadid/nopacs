package api

import (
	"encoding/base64"
	"strings"
)

// The kinds of the page tokens this package makes for the lists it pages
// itself, rather than a store. Neither the stores' cursors nor each other's
// start with these, so a token of one list is refused by every other.
const (
	// tokenGrantedTenants is ListTenants for a caller who sees only the
	// tenants its key holds a grant on; it is bound to the key.
	tokenGrantedTenants = "g1"
	// tokenInstallationSettings is ListSettings on the installation, which
	// pages the list of settings the server knows.
	tokenInstallationSettings = "s1"
	// tokenCoverage is GetCoverage, bound to the tenant and to the job that
	// made the report it pages, so a token of an older report is refused.
	tokenCoverage = "c1"
)

// afterToken returns the page token of a list this package pages itself that
// goes on after the item named last. kind names the list and bind what it was
// made for (a key's id), so a token is refused with any other list or caller
// (access.proto: a token is "valid only with the request it came from").
// bind holds no "/".
func afterToken(kind, bind, last string) string {
	return kind + "." + base64.RawURLEncoding.EncodeToString([]byte(bind+"/"+last))
}

// readAfterToken returns the last name of a token afterToken made with kind
// and bind, or the API's Invalid for any other token. The caller checks the
// name is one its list can hold.
func readAfterToken(token, kind, bind string) (string, error) {
	bad := invalid("page_token", "is not one this list returned for these arguments")
	raw, ok := strings.CutPrefix(token, kind+".")
	if !ok {
		return "", bad
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", bad
	}
	got, last, ok := strings.Cut(string(b), "/")
	if !ok || got != bind || last == "" {
		return "", bad
	}
	return last, nil
}
