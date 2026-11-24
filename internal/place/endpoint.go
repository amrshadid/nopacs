package place

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/blob/s3"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// endpointField is the field of a storage's configuration an endpoint's
// refusal names.
const endpointField = "storage.s3.endpoint"

// Endpoint is an S3 storage's endpoint as the installation allows it.
type Endpoint struct {
	// URL is the endpoint in the one spelling the port keeps
	// (tenant.NormalizeEndpoint).
	URL string
	// Entry is the installation's entry for it.
	Entry settings.S3Endpoint
}

// AllowAddress returns the address check of connections to the endpoint:
// every address for an entry that allows private addresses, else the
// public ones only (s3.AllowPublic).
func (e Endpoint) AllowAddress() func(netip.Addr) bool {
	if e.Entry.PrivateAddress {
		return func(netip.Addr) bool { return true }
	}
	return s3.AllowPublic
}

// LookupEndpoint returns the installation's entry for an S3 endpoint, both
// put in the one spelling the port keeps (tenant.NormalizeEndpoint), so two
// spellings of one endpoint are one endpoint and a spelling cannot pass
// where its other spelling would not. An endpoint that is not a valid one
// is Invalid; one the installation does not list, or an http one its entry
// does not allow, is a precondition.
func LookupEndpoint(entries settings.S3Endpoints, endpoint string) (Endpoint, error) {
	norm, err := tenant.NormalizeEndpoint(endpoint)
	if err != nil {
		return Endpoint{}, fault.Wrap(fault.Invalid, err, endpointField+": "+fault.Message(err))
	}
	for _, e := range entries {
		if n, err := tenant.NormalizeEndpoint(e.URL); err == nil && n == norm {
			if strings.HasPrefix(norm, "http://") && !e.PlainHTTP {
				return Endpoint{}, fault.New(fault.Precondition, endpointField+": the installation's entry for this endpoint does not allow plain http")
			}
			return Endpoint{URL: norm, Entry: e}, nil
		}
	}
	return Endpoint{}, fault.New(fault.Precondition, endpointField+": the installation does not list this endpoint")
}

// AllowS3 returns the endpoint of an S3 storage's configuration as the
// installation allows it (LookupEndpoint), and refuses, as a precondition,
// a storage that addresses its bucket in the host name (path style off) at
// an endpoint whose entry allows private addresses. With the bucket in the
// host name, a bucket the tenant chooses becomes part of the name the server
// resolves and connects to ("<bucket>.<endpoint>"), and the entry lets that
// name resolve to any address of the server's own network; with path style
// the server connects to the host the installation listed and no other.
func AllowS3(entries settings.S3Endpoints, s tenant.S3Storage) (Endpoint, error) {
	ep, err := LookupEndpoint(entries, s.Endpoint)
	if err != nil {
		return Endpoint{}, err
	}
	if ep.Entry.PrivateAddress && !s.PathStyle {
		return Endpoint{}, fault.New(fault.Precondition, "storage.s3.path_style: the installation's entry for this endpoint allows private addresses, so the bucket must be addressed in the path: with it in the host name, a bucket's name would choose which host inside the server's network is reached")
	}
	return ep, nil
}

// CheckEndpoints applies the rules of storage.s3-endpoints that need the
// rule a storage's endpoint is held to, beside the rule of its text
// (settings.ParseS3Endpoints): each entry an endpoint a storage could use,
// and no two entries spelling one endpoint. The errors are Invalid.
func CheckEndpoints(entries settings.S3Endpoints) error {
	seen := map[string]bool{}
	for i, e := range entries {
		n, err := tenant.NormalizeEndpoint(e.URL)
		if err != nil {
			return fault.Wrap(fault.Invalid, err, "has, on line "+strconv.Itoa(i+1)+", an endpoint a storage cannot use: "+fault.Message(err))
		}
		if seen[n] {
			return fault.New(fault.Invalid, "lists, on line "+strconv.Itoa(i+1)+", an endpoint another line lists in another spelling")
		}
		seen[n] = true
	}
	return nil
}
