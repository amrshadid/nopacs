package api

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
)

// DownloadPattern is the route of the plain HTTP download of one instance
// (data.proto, DataService), as an http.ServeMux pattern. It is also what
// the download's audit entry and log line name as the method: the route,
// never the request's path, which holds an instance id.
const DownloadPattern = "GET /tenants/{tenant}/instances/{subject}/{session}/{scan}/{instance}"

// DownloadNeed is the requirement and the audit facts of a request to
// DownloadPattern, read from the path values the server sets on r before
// the check: "tenant" (empty when the path's name is not a valid tenant
// name), "subject", "session", "scan" and "instance", each the path's
// segment as sent, never decoded and not yet checked. It reads nothing
// else of the request but whether it carries a Range header and whether
// its query names a window.
func DownloadNeed(r *http.Request) (access.Requirement, access.Request) {
	window := false
	if q, err := url.ParseQuery(r.URL.RawQuery); err == nil {
		window = q.Has("offset") || q.Has("length")
	} else {
		// A query that does not parse is answered 400 by the handler; the
		// entry still says a window was asked for in some form.
		window = r.URL.RawQuery != ""
	}
	return downloadNeed(download{
		Tenant:   r.PathValue("tenant"),
		Subject:  r.PathValue("subject"),
		Session:  r.PathValue("session"),
		Scan:     r.PathValue("scan"),
		Instance: r.PathValue("instance"),
		Ranged:   r.Header.Get("Range") != "",
		Window:   window,
	})
}

// download is the parts of the path and query of a plain HTTP download of
// an instance, as the route has read them, unchecked.
type download struct {
	// Tenant, Subject, Session and Scan are the path's names.
	Tenant, Subject, Session, Scan string
	// Instance is the path's instance id.
	Instance string
	// Ranged is whether the request carries a Range header; Window is
	// whether its query names a part of the instance (offset and length).
	Ranged, Window bool
}

// downloadNeed is the requirement and the audit facts of a plain HTTP
// download of an instance. It is checked before the rest of the path is
// read and the instance looked up, so a refusal says nothing of what the
// tenant holds: reader on the tenant the path names, or on the
// installation for a malformed name (doc.go), as for every method.
func downloadNeed(d download) (access.Requirement, access.Request) {
	details := map[string]string{
		detailRanged: strconv.FormatBool(d.Ranged),
		detailWindow: strconv.FormatBool(d.Window),
	}
	if d.Instance != "" {
		details[detailInstance] = presentValue
	}
	return onTenantNamed(d.Tenant, master.RoleReader), access.Request{Object: labelPath(d.Subject, d.Session, d.Scan), Details: details}
}
