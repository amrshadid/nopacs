package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
)

// OpenInstance sends the plain HTTP GET of one archived instance (the route
// the Data service describes) and returns the open response, which the caller
// closes. The request carries the key as every call does, and, when from is
// above zero, a Range of "bytes=<from>-" with If-Range set to etag, so a
// download resumed after the instance changed is answered whole instead of
// joining the bytes of two versions (RFC 9110 13.1.5, 14.2). The response is
// returned whatever its status: the caller reads 200, 206 and 416 for itself.
// A failure to reach the server is a fault error that says where.
func (c *Client) OpenInstance(ctx context.Context, tenant, subject, session, scan, instance string, from int64, etag string) (*http.Response, error) {
	path := "/tenants/" + url.PathEscape(tenant) + "/instances/" + url.PathEscape(subject) + "/" +
		url.PathEscape(session) + "/" + url.PathEscape(scan) + "/" + url.PathEscape(instance)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot make the download request")
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	if from > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(from, 10)+"-")
		req.Header.Set("If-Range", etag)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, c.classify(ctx, err)
	}
	return res, nil
}

// StreamError turns the error a server stream ended with into a *CallError,
// as Call does for a unary call: an API error maps through the codes table,
// and a connection that failed or a deadline is said in words. what names the
// stream, such as "download".
func (c *Client) StreamError(ctx context.Context, what string, err error) error {
	return &CallError{What: what, err: c.classify(ctx, err)}
}
