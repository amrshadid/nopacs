package s3

import (
	"context"
	"errors"
	"net"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The classes of a failure a caller may tell apart, such as a test of a
// storage that answers each with its own fault. One of them is in the chain
// of an error (errors.Is) whose cause is of that class; an error with none
// is a storage that could not be reached or did not answer as it should in
// time. They are values, not words, so a message of this driver can be
// reworded without a caller's reading of it changing.
var (
	// ErrAddressNotAllowed is an endpoint whose host resolved to an address
	// the storage's address check (Config.AllowAddress) refuses; nothing
	// was sent to it.
	ErrAddressNotAllowed = errors.New("the endpoint's address is not allowed")
	// ErrBucketMissing is a bucket the store says does not exist.
	ErrBucketMissing = errors.New("the bucket does not exist")
	// ErrRefused is a request the store refused: the credentials, their
	// permissions, the object's size or name, or the request itself.
	ErrRefused = errors.New("the storage refused the request")
	// ErrWrongAnswer is an answer that cannot be what the request was for:
	// a redirect, an answer longer than any answer should be, bytes the
	// store says were damaged on their way.
	ErrWrongAnswer = errors.New("the storage's answer was not one the request can have")
)

// classed is a cause with the class it belongs to: its text is the cause's,
// and errors.Is finds both.
type classed struct {
	cause, class error
}

func (c *classed) Error() string   { return c.cause.Error() }
func (c *classed) Unwrap() []error { return []error{c.cause, c.class} }

// inClass returns cause marked as of class.
func inClass(class, cause error) error { return &classed{cause: cause, class: class} }

// The causes an error keeps for the log. They are fixed phrases: the text of
// an SDK or network error holds the endpoint's address, the bucket's host
// name, a request id and the server's own words, none of which may reach a
// log or a caller.
var (
	errTimedOut     = errors.New("the storage did not answer in time")
	errUnreachable  = errors.New("the storage could not be reached")
	errRedirected   = errors.New("the storage answered with a redirect, which is not followed")
	errNotConfirmed = errors.New("the storage said nothing that confirms the object was stored")
	errMalformed    = errors.New("the storage's answer was not what the request was for")
)

// serverCode is the error code a server answered with, kept only when it is
// one of S3's own, so a hostile server cannot put its words in a log.
type serverCode string

func (c serverCode) Error() string { return "the storage answered with the error code " + string(c) }

// knownCodes are the codes S3 documents
// (docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html) that this
// driver can meet.
var knownCodes = map[string]bool{
	"AccessDenied": true, "AccountProblem": true, "AllAccessDisabled": true, "BadDigest": true,
	"BucketAlreadyExists": true, "EntityTooLarge": true, "EntityTooSmall": true, "ExpiredToken": true,
	"IncompleteBody": true, "InternalError": true, "InvalidAccessKeyId": true, "InvalidArgument": true,
	"InvalidDigest": true, "InvalidPart": true, "InvalidPartOrder": true, "InvalidRange": true,
	"InvalidRequest": true, "InvalidToken": true, "MalformedXML": true, "MethodNotAllowed": true,
	"MissingContentLength": true, "NoSuchBucket": true, "NoSuchKey": true, "NoSuchUpload": true,
	"NotFound": true, "NotImplemented": true, "PermanentRedirect": true, "PreconditionFailed": true,
	"RequestTimeTooSkewed": true, "RequestTimeout": true, "ServiceUnavailable": true, "SignatureDoesNotMatch": true,
	"SlowDown": true, "TemporaryRedirect": true, "XAmzContentSHA256Mismatch": true, "InvalidBucketName": true,
	"Conflict": true, "OperationAborted": true, "KeyTooLongError": true,
	"InvalidObjectState": true,
}

// closedError is what every call gives once the store is closed.
func closedError() error { return fault.New(fault.Unavailable, "the storage is closed") }

// canceled is what a call gives when its context has ended: the context's
// own error stays in the chain so errors.Is matches it.
func canceled(ctx context.Context, what string) error {
	return fault.Wrap(fault.Unavailable, ctx.Err(), what+" was canceled")
}

// fail turns an error of the SDK or the network, from the call named by
// what, into one of the port's kinds with a message and a cause that are
// the driver's own.
func fail(ctx context.Context, err error, what string) error {
	var fe *fault.Error
	switch {
	case errors.As(err, &fe):
		return err
	case ctx.Err() != nil:
		return canceled(ctx, what)
	case errors.Is(err, context.Canceled):
		// Not the caller's context: the driver ended the request itself, at
		// the wait it allows for a first byte.
		return fault.Wrap(fault.Unavailable, errTimedOut, what+" failed: the storage did not answer in time")
	case errors.Is(err, errAnswerTooLarge):
		return fault.Wrap(fault.Unavailable, inClass(ErrWrongAnswer, errAnswerTooLarge), what+" failed: the storage's answer was longer than any answer should be")
	case errors.Is(err, ErrAddressNotAllowed):
		return fault.Wrap(fault.Unavailable, ErrAddressNotAllowed, what+" failed: the endpoint's address is not allowed")
	}
	if code, status := apiError(err); code != "" || status != 0 {
		return failCode(code, status, what)
	}
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return fault.Wrap(fault.Unavailable, errTimedOut, what+" failed: the storage did not answer in time")
	}
	return fault.Wrap(fault.Unavailable, errUnreachable, what+" failed: the storage could not be reached")
}

// apiError returns the S3 error code and the HTTP status in err, either of
// which may be absent.
func apiError(err error) (string, int) {
	var code string
	var ae smithy.APIError
	if errors.As(err, &ae) {
		code = ae.ErrorCode()
	}
	var re *awshttp.ResponseError
	status := 0
	if errors.As(err, &re) {
		status = re.HTTPStatusCode()
	}
	return code, status
}

// failCode maps what a server answered to a kind.
func failCode(code string, status int, what string) error {
	cause := func() error {
		if knownCodes[code] {
			return serverCode(code)
		}
		return errMalformed
	}()
	switch {
	case status >= 300 && status < 400, code == "PermanentRedirect", code == "TemporaryRedirect":
		return fault.Wrap(fault.Unavailable, inClass(ErrWrongAnswer, errRedirected), what+" failed: the storage answered with a redirect, which is not followed")
	case code == "NoSuchKey" || code == "NotFound" || (code == "" && status == http.StatusNotFound):
		return fault.Wrap(fault.NotFound, cause, "the storage holds no object under that key")
	case code == "InvalidObjectState":
		// S3 answers a GET of an object in an archive storage class, one
		// not restored, with 403 InvalidObjectState
		// (docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html,
		// API_GetObject.html). The object is there and whole: it must
		// neither read as refused credentials, which a scrub would retry
		// without end, nor as missing. Tested before the 403 below.
		return fault.Wrap(fault.Unavailable, inClass(blob.ErrNotOnline, cause), what+" failed: the object is held in a tier the storage cannot read at once")
	case code == "NoSuchBucket":
		return fault.Wrap(fault.Unavailable, inClass(ErrBucketMissing, cause), what+" failed: the bucket does not exist")
	case code == "AccessDenied", code == "AccountProblem", code == "AllAccessDisabled", code == "ExpiredToken",
		code == "InvalidAccessKeyId", code == "InvalidToken", code == "SignatureDoesNotMatch",
		code == "RequestTimeTooSkewed", status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fault.Wrap(fault.Unavailable, inClass(ErrRefused, cause), what+" failed: the storage refused the credentials it was given or their permissions")
	case code == "BadDigest", code == "XAmzContentSHA256Mismatch", code == "InvalidDigest", code == "IncompleteBody":
		return fault.Wrap(fault.Unavailable, inClass(ErrWrongAnswer, cause), what+" failed: the storage found the bytes damaged in transit")
	case code == "EntityTooLarge", code == "KeyTooLongError":
		return fault.Wrap(fault.Invalid, inClass(ErrRefused, cause), what+" failed: the storage refused the object's size or name")
	case code == "SlowDown", code == "RequestTimeout", code == "ServiceUnavailable", status == http.StatusTooManyRequests, status >= 500:
		return fault.Wrap(fault.Unavailable, cause, what+" failed: the storage is busy or failing")
	}
	return fault.Wrap(fault.Unavailable, inClass(ErrRefused, cause), what+" failed: the storage refused the request")
}
