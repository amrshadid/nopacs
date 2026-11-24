package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Store is a blob.Store on a bucket. Open makes one.
type Store struct {
	set    settings
	client *awss3.Client
	tr     *http.Transport
	closed atomic.Bool
	budget *budget
	notes  Notes
	sweep  sweepState
	purge  purgeState
}

var _ blob.Store = (*Store)(nil)

// Open checks cfg, connects to the bucket, aborts the unfinished uploads a
// crash left under the prefix (see Config.StaleUploadAge), and returns the
// store. It fails as Invalid when cfg is wrong or the bucket does not exist
// or refuses the credentials, and as Unavailable when the server cannot be
// reached or the address check refuses it.
//
// Open also puts and deletes a small object under the prefix, to be sure the
// server confirms uploads by their checksum (see probeUpload), and cleans up
// old unfinished uploads. Neither fails Open for a reason of the account
// (credentials that may not write, or may not list uploads): Notes says what
// happened. Credentials that may not write still open the storage for
// reading.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	set, err := cfg.check()
	if err != nil {
		return nil, err
	}
	s := newStore(set)
	if err := s.probe(ctx); err != nil {
		s.tr.CloseIdleConnections()
		return nil, err
	}
	if set.forReading {
		return s, nil
	}
	if err := s.probeUpload(ctx); err != nil {
		s.tr.CloseIdleConnections()
		return nil, err
	}
	if set.staleAge > 0 {
		s.notes.Cleanup = s.AbortStaleUploads(ctx)
	}
	if ctx.Err() != nil {
		s.tr.CloseIdleConnections()
		return nil, canceled(ctx, "opening the storage")
	}
	return s, nil
}

// newStore makes a store from checked settings, without connecting.
func newStore(set settings) *Store {
	s := &Store{set: set, tr: newTransport(set), budget: newBudget(set.memory)}
	s.client = awss3.New(clientOptions(set, s.tr))
	return s
}

// clientOptions builds the SDK client's options from nothing but set. It is
// the one place a client is configured, so what the driver does not do (read
// the environment, follow a redirect, use a proxy, send a checksum a server
// may not know) is visible in one function.
func clientOptions(set settings, tr *http.Transport) awss3.Options {
	o := awss3.Options{
		Region:       set.region,
		UsePathStyle: set.pathStyle,
		HTTPClient:   &http.Client{Transport: limitTransport{tr}, CheckRedirect: noRedirect},
		// Static credentials, from the configuration and nowhere else.
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: set.accessKey.Reveal(), SecretAccessKey: set.secretKey.Reveal(), Source: "nopacs storage configuration"}, nil
		}),
		// Since early 2025 the SDK adds a CRC32 checksum to every upload as an
		// aws-chunked trailer, and validates one on every download, which
		// several servers other than Amazon's reject or cannot produce.
		// "When required" sends and checks one only where the API demands it;
		// the driver sets x-amz-checksum-sha256 itself on every upload.
		// A bucket of the S3 Express form would make the SDK create a session and
		// sign with credentials the server returns; Open refuses such a name,
		// and this makes the SDK unable to do it whatever a name says.
		DisableS3ExpressSessionAuth: aws.Bool(true),
		RequestChecksumCalculation:  aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation:  aws.ResponseChecksumValidationWhenRequired,
		// Three attempts in all, backing off exponentially to five seconds,
		// for what is safe to repeat: every request here is idempotent, a
		// body is a buffer the SDK can send again, and a refused address is
		// never retried. The SDK's own default is three attempts too, with
		// backoff up to twenty seconds and a retry budget that fails calls
		// when many fail at once; neither suits a service that needs a
		// prompt, clear answer.
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = 3
			o.MaxBackoff = 5 * time.Second
			o.RateLimiter = ratelimit.None
			o.Retryables = append([]retry.IsErrorRetryable{retry.IsErrorRetryableFunc(func(err error) aws.Ternary {
				if errors.Is(err, ErrAddressNotAllowed) || errors.Is(err, errAnswerTooLarge) {
					return aws.FalseTernary
				}
				return aws.UnknownTernary
			})}, o.Retryables...)
		}),
	}
	if set.endpoint != nil {
		o.BaseEndpoint = aws.String(set.endpoint.String())
	}
	return o
}

// probe checks that the bucket can be listed under the prefix, which is what
// every other call depends on, so a wrong endpoint, bucket or key shows at
// Open and not at the first Put.
func (s *Store) probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	in := &awss3.ListObjectsV2Input{Bucket: aws.String(s.set.bucket), MaxKeys: aws.Int32(1)}
	if s.set.prefix != "" {
		in.Prefix = aws.String(s.set.prefix)
	}
	if _, err := s.client.ListObjectsV2(ctx, in); err != nil {
		return openError(fail(ctx, err, "opening the storage"))
	}
	return nil
}

// openError turns the failures that mean the configuration is wrong, a
// bucket that does not exist and credentials refused, into Invalid, since
// trying again changes nothing; the rest stay as they are.
func openError(err error) error {
	var sc serverCode
	if errors.As(err, &sc) {
		switch sc {
		case "NoSuchBucket", "InvalidBucketName":
			return fault.Wrap(fault.Invalid, inClass(ErrBucketMissing, sc), "opening the storage failed: the bucket does not exist")
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccountProblem", "AllAccessDisabled":
			return fault.Wrap(fault.Invalid, inClass(ErrRefused, sc), "opening the storage failed: the storage refused the credentials it was given or their permissions")
		}
	}
	return err
}

// errReadOnly answers a Put or a Delete on a store opened with
// Config.ForReading: the caller's mistake, never the storage's.
var errReadOnly = fault.New(fault.Internal, "the storage was opened for reading only")

// beginWrite is begin for a call that writes, which a store opened for
// reading refuses.
func (s *Store) beginWrite(ctx context.Context, what string) error {
	if s.set.forReading {
		return errReadOnly
	}
	return s.begin(ctx, what)
}

// begin is the start of every call: the store is open and ctx has not ended.
func (s *Store) begin(ctx context.Context, what string) error {
	if s.closed.Load() {
		return closedError()
	}
	if ctx.Err() != nil {
		return canceled(ctx, what)
	}
	return nil
}

// fits reports whether key, with the storage's prefix, is a name S3 accepts:
// at most MaxKeyBytes. A port key that does not fit cannot be in this
// storage.
func (s *Store) fits(key string) bool { return len(s.set.prefix)+len(key) <= MaxKeyBytes }

// object returns the name of key in the bucket.
func (s *Store) object(key string) *string { return aws.String(s.set.prefix + key) }

// Close releases the store's idle connections. A stream from Read that is
// open may go on being read.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.tr.CloseIdleConnections()
	return nil
}

// Usage says the volume's space is unknown: a bucket has no size to report,
// and what it holds is counted from the catalog.
func (s *Store) Usage(ctx context.Context) (blob.Usage, error) {
	if err := s.begin(ctx, "the usage call"); err != nil {
		return blob.Usage{}, err
	}
	return blob.Usage{}, nil
}

// Stat returns the size of key with a HEAD request.
func (s *Store) Stat(ctx context.Context, key string) (blob.Info, error) {
	if err := s.begin(ctx, "the stat"); err != nil {
		return blob.Info{}, err
	}
	if err := blob.CheckKey(key); err != nil {
		return blob.Info{}, err
	}
	if !s.fits(key) {
		return blob.Info{}, fault.New(fault.NotFound, "the storage holds no object under that key")
	}
	ctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.set.bucket), Key: s.object(key)})
	if err != nil {
		return blob.Info{}, fail(ctx, err, "the stat")
	}
	return blob.Info{Size: aws.ToInt64(out.ContentLength)}, nil
}

// Delete removes key with a DELETE request, which S3 answers with success
// for a key that holds nothing, as the port requires.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.beginWrite(ctx, "the delete"); err != nil {
		return err
	}
	if err := blob.CheckKey(key); err != nil {
		return err
	}
	if !s.fits(key) {
		return nil // Nothing of that name can be here.
	}
	ctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(s.set.bucket), Key: s.object(key)})
	if err == nil {
		return nil
	}
	if err = fail(ctx, err, "the delete"); fault.KindOf(err) == fault.NotFound {
		return nil
	}
	return err
}

// Read returns a range of key with a ranged GET. The caller gets exactly the
// bytes asked for, min(length, size-offset) of them, or an error: the answer's
// Content-Range is checked against the range asked and the size it states,
// and the stream is counted, so a server that sends fewer or more bytes than
// it said is an error and never a short or a long success.
func (s *Store) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if err := s.begin(ctx, "the read"); err != nil {
		return nil, err
	}
	if err := blob.CheckKey(key); err != nil {
		return nil, err
	}
	if err := blob.CheckRange(offset, length); err != nil {
		return nil, err
	}
	if !s.fits(key) {
		return nil, fault.New(fault.NotFound, "the storage holds no object under that key")
	}
	if length == 0 {
		// A range of no bytes cannot be asked of S3. The size answers whether
		// the key exists and whether the offset is past the end.
		info, err := s.Stat(ctx, key)
		if err != nil {
			return nil, err
		}
		if offset > info.Size {
			return nil, blob.ErrPastEnd()
		}
		return io.NopCloser(eofReader{}), nil
	}
	in := &awss3.GetObjectInput{Bucket: aws.String(s.set.bucket), Key: s.object(key)}
	if offset > 0 || length > 0 {
		in.Range = aws.String(rangeHeader(offset, length))
	}
	rctx, cancel := context.WithCancel(ctx)
	// The wait for the first byte, retries included, is bounded like every
	// other call; the stream after it is cut by its idle timeout.
	first := time.AfterFunc(s.set.callTimeout, cancel)
	out, err := s.client.GetObject(context.WithValue(rctx, objectStream{}, struct{}{}), in)
	timedOut := !first.Stop()
	if err != nil {
		cancel()
		if timedOut && ctx.Err() == nil {
			return nil, fault.Wrap(fault.Unavailable, errTimedOut, "the read failed: the storage did not answer in time")
		}
		var ae smithy.APIError
		if errors.As(err, &ae) && ae.ErrorCode() == "InvalidRange" {
			return s.pastEnd(ctx, key, offset)
		}
		return nil, fail(ctx, err, "the read")
	}
	want, ok := expectedBytes(out, offset, length, in.Range != nil)
	if !ok {
		_ = out.Body.Close() // The answer is refused whatever closing says.
		cancel()
		return nil, fault.Wrap(fault.Unavailable, errMalformed, "the read failed: the storage did not answer the range asked")
	}
	return &body{idle: s.set.idleTimeout, caller: ctx, cancel: cancel, rc: out.Body, want: want}, nil
}

// expectedBytes returns how many bytes the answer to a read must carry, and
// whether the answer is what was asked for. For a ranged read the
// Content-Range must start at offset, end where the range asked ends or the
// object does, and agree with the object's size and with Content-Length; for
// a whole object the length must be stated and no range given.
func expectedBytes(out *awss3.GetObjectOutput, offset, length int64, ranged bool) (int64, bool) {
	if !ranged {
		if out.ContentRange != nil || out.ContentLength == nil || *out.ContentLength < 0 {
			return 0, false
		}
		return *out.ContentLength, true
	}
	var start, end, total int64
	if n, _ := fmt.Sscanf(aws.ToString(out.ContentRange), "bytes %d-%d/%d", &start, &end, &total); n != 3 {
		return 0, false
	}
	wantEnd := total - 1
	if length > 0 && length-1 < wantEnd-offset {
		wantEnd = offset + length - 1
	}
	if total < 1 || start != offset || offset >= total || end != wantEnd {
		return 0, false
	}
	want := end - start + 1
	if out.ContentLength != nil && *out.ContentLength != want {
		return 0, false
	}
	return want, true
}

// pastEnd handles a range S3 refused as unsatisfiable: with the offset at the
// end of the object the answer is an empty stream, past it Invalid. The
// object's size says which.
func (s *Store) pastEnd(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if offset > info.Size {
		return nil, blob.ErrPastEnd()
	}
	if offset == info.Size {
		return io.NopCloser(eofReader{}), nil
	}
	return nil, fault.Wrap(fault.Unavailable, errMalformed, "the read failed: the storage refused a range inside the object")
}

// rangeHeader writes the Range header for offset and length (-1 for to the
// end), with the end clamped so that offset+length-1 cannot overflow.
func rangeHeader(offset, length int64) string {
	const maxInt64 = 1<<63 - 1
	if length < 0 || length > maxInt64-offset {
		return fmt.Sprintf("bytes=%d-", offset)
	}
	return fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// body is the stream of a Read. It ends the request's context when it is
// closed, and cuts the stream when the server stalls: a Read that moves no
// byte for idleTimeout fails, so a hung server cannot hold a caller that has
// no deadline of its own. It counts: the stream yields exactly want bytes,
// then ends; fewer before the server ends the stream, or more, is an error.
type body struct {
	idle   time.Duration
	caller context.Context
	cancel context.CancelFunc
	rc     io.ReadCloser
	once   sync.Once
	want   int64
	got    int64
}

func (b *body) Read(p []byte) (int, error) {
	if remaining := b.want - b.got; remaining <= 0 {
		// The bytes asked for are all delivered. The server must have nothing
		// more to send.
		var one [1]byte
		n, err := b.read(one[:])
		switch {
		case n > 0:
			return 0, fault.Wrap(fault.Unavailable, errMalformed, "the read failed: the storage sent more bytes than the range asked")
		case errors.Is(err, io.EOF):
			return 0, io.EOF
		case err != nil:
			return 0, err
		}
		return 0, nil
	} else if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := b.read(p)
	b.got += int64(n)
	if errors.Is(err, io.EOF) && b.got < b.want {
		return n, fault.Wrap(fault.Unavailable, errMalformed, "the read failed: the storage sent fewer bytes than the range asked")
	}
	return n, err
}

// read reads from the server's stream under the idle timeout, and gives an
// error of the port's kinds.
func (b *body) read(p []byte) (int, error) {
	t := time.AfterFunc(b.idle, b.cancel)
	n, err := b.rc.Read(p)
	stalled := !t.Stop()
	if err == nil || errors.Is(err, io.EOF) && !stalled {
		return n, err
	}
	switch {
	case b.caller.Err() != nil:
		return n, canceled(b.caller, "the read")
	case stalled:
		return n, fault.Wrap(fault.Unavailable, errTimedOut, "the read failed: the storage stopped sending the object")
	}
	return n, fault.Wrap(fault.Unavailable, errUnreachable, "the read failed: the storage could not be reached")
}

func (b *body) Close() error {
	var err error
	b.once.Do(func() {
		err = b.rc.Close()
		b.cancel()
	})
	if err != nil {
		return fault.Wrap(fault.Unavailable, errUnreachable, "the read's stream could not be closed")
	}
	return nil
}
