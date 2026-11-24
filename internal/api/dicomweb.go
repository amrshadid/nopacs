package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// DICOMwebPattern is the route of a tenant's DICOMweb services, QIDO-RS
// (PS3.18 10.6) and WADO-RS (PS3.18 10.4), as an http.ServeMux pattern:
// one route for the subtree, whose handler reads the resource from the
// rest of the path (package dicomweb). It is also what the audit entry and
// the log line name as the method, never the request's path, which holds
// UIDs.
const DICOMwebPattern = "GET /tenants/{tenant}/dicom-web/{rest...}"

// The details of a DICOMweb request's audit entry.
const (
	// detailResource is the shape of the resource asked for, its UIDs
	// replaced by what each is (dicomweb.ShapeOf).
	detailResource = "resource"
	// detailKeys are the keywords of the attributes a search matched,
	// never their values.
	detailKeys = "keys"
	// detailResults counts the results a search answered, or the
	// instances, frames or bulk values a retrieve sent.
	detailResults = "results"
)

// The messages of what a DICOMweb request finds missing.
const (
	messageNoStudy         = "the tenant holds no such study"
	messageNoSeries        = "the tenant holds no such series"
	messageNoWebInstance   = "the tenant holds no such instance"
	messageServedMediaType = "the archive holds none of the instances in a form the request accepts"
)

// DICOMwebNeed is the requirement and the audit facts of a request to
// DICOMwebPattern, read from the path values the server sets before the
// check: reader on the tenant the path names (on the installation for a
// malformed name, as for every method), with the resource's shape in the
// details. It reads nothing else of the request, so a refusal says
// nothing of what the tenant holds.
func DICOMwebNeed(r *http.Request) (access.Requirement, access.Request) {
	return onTenantNamed(r.PathValue("tenant"), master.RoleReader),
		access.Request{Details: map[string]string{detailResource: dicomweb.ShapeOf(r.PathValue("rest"))}}
}

// DICOMweb serves DICOMwebPattern once the server's check has allowed the
// request and every value of its path has passed the server's rule of a
// segment: a search answered from the catalog with real paging and from
// the scans' metadata files for the page returned; a retrieve of
// instances as they are stored, each checked against its SHA-256 as it is
// sent; their metadata from the metadata files; frames and bulk values by
// ranged reads of the stored file. Its errors before the answer starts are
// returned for the server to write, as for every route; a refusal of the
// protocol (400, 406, 413) is written here with its message.
func (h Handlers) DICOMweb(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	return h.c.dicomweb(ctx, w, r)
}

// webRequest is one request to a tenant's DICOMweb services, once its
// tenant is in hand.
type webRequest struct {
	c      *core
	d      access.Decision
	st     tenant.Store
	p      tenant.Pipeline
	w      http.ResponseWriter
	r      *http.Request
	res    dicomweb.Resource
	tenant string
	// details are the audit details of the check, which every entry of
	// the request carries.
	details map[string]string
	// object is the path of labels the request reached, once known.
	object string
	// release ends the open read the request holds, nil before it holds
	// one (holdRead).
	release func()
}

// holdRead takes one of the caller's open reads for the rest of the
// request, once however often it is asked: a search, a frame's plan and a
// streamed answer each read the storage, and a request counts as one open
// read for all of them, so a caller cannot read past MaxOpenReads by
// asking for what does not stream.
func (x *webRequest) holdRead() error {
	if x.release != nil {
		return nil
	}
	release, err := x.c.reads.Take(ownerOf(x.d))
	if err != nil {
		return err
	}
	x.release = release
	return nil
}

// dicomweb is DICOMweb.
func (c *core) dicomweb(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	d, err := c.decision(ctx, DICOMwebPattern)
	if err != nil {
		return err
	}
	_, facts := DICOMwebNeed(r)
	name := r.PathValue("tenant")
	// Searches and retrievals only read (a store is the STOW route's), so a
	// moved or fenced tenant serves them.
	p, st, err := c.inTenant(readingOnly(ctx), d, name, facts.Details)
	if err != nil {
		return err
	}
	x := &webRequest{c: c, d: d, st: st, p: p, w: w, r: r, tenant: name, details: facts.Details}
	defer func() {
		if x.release != nil {
			x.release()
		}
	}()
	// A response is data for a program; a browser that guessed its type
	// could run what a stored value holds.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	res, perr := dicomweb.ParseResource(r.PathValue("rest"))
	if perr != nil {
		return x.refuse(perr)
	}
	x.res = res
	switch res.Kind {
	case dicomweb.Search:
		return x.search(ctx)
	case dicomweb.Retrieve:
		return x.retrieve(ctx)
	case dicomweb.Metadata:
		return x.metadata(ctx)
	case dicomweb.Frames:
		return x.frames(ctx)
	case dicomweb.BulkData:
		return x.bulkData(ctx)
	}
	return x.refuse(&dicomweb.Error{Status: http.StatusNotFound, Message: "there is no such DICOMweb resource"})
}

// facts returns the audit facts of the request with more details added.
func (x *webRequest) facts(more map[string]string) access.Facts {
	return access.Facts{Object: x.object, Details: withDetails(x.details, more)}
}

// refuse answers a request the protocol refuses with its status and
// message as plain text, and records it as failed with that message.
func (x *webRequest) refuse(e *dicomweb.Error) error {
	h := x.w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(e.Message)+1))
	x.w.WriteHeader(e.Status)
	_, _ = x.w.Write([]byte(e.Message + "\n")) // #nosec G705 -- plain text with nosniff, never HTML.
	x.c.record(x.d, x.st, master.OutcomeFailed, e.Message, x.facts(nil))
	return nil
}

// fail records a request that did not complete with err, before its
// answer began, and returns the API error for the server to write.
func (x *webRequest) fail(err error) error {
	return x.c.failWith(x.d, x.st, x.facts(nil), err)
}

// done records a request answered.
func (x *webRequest) done(results int) {
	x.c.record(x.d, x.st, master.OutcomeDone, "", x.facts(map[string]string{detailResults: strconv.Itoa(results)}))
}

// base returns the URL of the tenant's DICOMweb root (webBase).
func (x *webRequest) base() string { return webBase(x.r, x.tenant) }

// webBase returns the URL of the DICOMweb root of the tenant named
// tenantName, as the client of r reached it: the request's scheme and host.
// A Retrieve URL (0008,1190) and a BulkDataURI are absolute (PS3.18 F.2.6),
// so a client resolves them the same however it took the answer. Behind a
// proxy that changes the scheme, the URL names the scheme the server saw.
func webBase(r *http.Request, tenantName string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/tenants/" + tenantName + "/dicom-web"
}

// session returns the session of the study, or NotFound.
func (x *webRequest) session(ctx context.Context, study string) (tenant.CatalogSession, error) {
	labels, err := x.p.CatalogLabels(ctx, study, nil)
	if err != nil {
		return tenant.CatalogSession{}, err
	}
	if labels.Session == nil {
		return tenant.CatalogSession{}, fault.New(fault.NotFound, messageNoStudy)
	}
	s, err := x.p.CatalogSession(ctx, labels.Session.SubjectLabel, labels.Session.SessionLabel)
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoStudy)
	}
	return s, err
}

// scan returns the scan of the series of the study, or NotFound.
func (x *webRequest) scan(ctx context.Context, study, series string) (tenant.CatalogScan, error) {
	labels, err := x.p.CatalogLabels(ctx, study, []string{series})
	if err != nil {
		return tenant.CatalogScan{}, err
	}
	scanLabel, ok := labels.Scans[series]
	if labels.Session == nil || !ok {
		return tenant.CatalogScan{}, fault.New(fault.NotFound, messageNoSeries)
	}
	s, err := x.p.CatalogScan(ctx, label.Path{Subject: labels.Session.SubjectLabel, Session: labels.Session.SessionLabel, Scan: scanLabel})
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.NotFound, err, messageNoSeries)
	}
	return s, err
}

// pathOf returns the labels of a scan.
func pathOf(s tenant.CatalogScan) label.Path {
	return label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
}
