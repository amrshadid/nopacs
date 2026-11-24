package settings

import (
	"strings"
	"testing"
)

// TestParseOrigins guards the form of api.cors-origins: only an origin as a
// browser writes it in the Origin header is accepted, so every listed
// origin can match; "*" stands alone; anything looser (a path, a trailing
// slash, upper case, a default port, "null", a wildcard inside a host) is
// refused rather than read as something the operator may not have meant.
func TestParseOrigins(t *testing.T) {
	for v, ok := range map[string]bool{
		"":                                     true,
		"*":                                    true,
		"*\n":                                  true,
		"https://viewer.example.org":           true,
		"http://localhost:5173":                true,
		"https://a.example\nhttp://b.example":  true,
		"https://a.example\n":                  true,
		"http://127.0.0.1:8080":                true,
		"https://[2001:db8::1]":                true,
		"https://[2001:db8::1]:8443":           true,
		"https://xn--bcher-kva.example":        true,
		"https://viewer.example.org/":          false,
		"https://viewer.example.org/app":       false,
		"https://Viewer.example.org":           false,
		"HTTPS://viewer.example.org":           false,
		"https://viewer.example.org:443":       false,
		"http://viewer.example.org:80":         false,
		"https://viewer.example.org:0":         false,
		"https://viewer.example.org:65536":     false,
		"https://viewer.example.org:0443":      false,
		"https://*.example.org":                false,
		"null":                                 false,
		"ftp://viewer.example.org":             false,
		"viewer.example.org":                   false,
		"https://":                             false,
		"https://user@viewer.example.org":      false,
		"https://[2001:DB8::1]":                false,
		"https://[2001:0db8::1]":               false,
		"https://[::ffff:1.2.3.4]":             false,
		"https://[2001:db8::1":                 false,
		"https://a.example\n*":                 false,
		"https://a.example\nhttps://a.example": false,
		"https://a.example\n\nhttps://b.ex":    false,
		"https://-a.example":                   false,
		"https://8.8.8.8":                      true,
		"http://127.000.0.1":                   false,
		"http://010.0.0.1":                     false,
		"http://1.2.3":                         false,
		"http://256.1.1.1":                     false,
		"http://a.example.123":                 false,
		"https://a..example":                   false,
		" https://a.example":                   false,
	} {
		_, err := ParseOrigins(v)
		if (err == nil) != ok {
			t.Errorf("%q: accepted %v, want %v (%v)", v, err == nil, ok, err)
		}
	}
	many := strings.Repeat("https://a.example\n", MaxOrigins+1)
	if _, err := ParseOrigins(many); err == nil {
		t.Errorf("%d origins accepted", MaxOrigins+1)
	}
}

// TestOriginsAllows guards the match: exact, byte for byte, with "*"
// allowing any origin but never an empty one, and the zero value allowing
// none.
func TestOriginsAllows(t *testing.T) {
	o, err := ParseOrigins("https://a.example\nhttp://localhost:5173")
	if err != nil {
		t.Fatal(err)
	}
	for origin, want := range map[string]bool{
		"https://a.example":     true,
		"http://localhost:5173": true,
		"https://a.example:443": false,
		"https://A.example":     false,
		"http://a.example":      false,
		"https://b.example":     false,
		"null":                  false,
		"":                      false,
	} {
		if got := o.Allows(origin); got != want {
			t.Errorf("%q: allowed %v, want %v", origin, got, want)
		}
	}
	if (Origins{}).Allows("https://a.example") {
		t.Error("the zero value allows an origin")
	}
	anyOrigin, _ := ParseOrigins("*")
	if !anyOrigin.Allows("https://b.example") || anyOrigin.Allows("") {
		t.Error("* does not allow every origin and only those")
	}
	if o.String() != "https://a.example\nhttp://localhost:5173" || anyOrigin.String() != "*" {
		t.Errorf("String: %q, %q", o.String(), anyOrigin.String())
	}
}
