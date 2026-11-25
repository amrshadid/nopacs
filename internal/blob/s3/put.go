package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// abortTimeout bounds the abort of an upload after a failure. It runs on a
// context that no longer follows the caller's, since the caller's has often
// ended, which is why the upload is being aborted.
const abortTimeout = 30 * time.Second

// putJob is the state of one Put.
type putJob struct {
	s    *Store
	key  *string
	hash hash.Hash
	size int64
	// held is the bytes of the store's budget this Put holds. Only the
	// goroutine that runs Put changes it.
	held int64
	// confirmed is set when a server whose checksums Open tested
	// (Store.checksumsChecked) answered for every byte: the echo of one
	// request's checksum, the whole object's SHA-256; or, for a multipart
	// upload, each part's echoed SHA-256 and a composite checksum equal to
	// the one of those parts in order, which confirms the object part by
	// part. A HEAD's size and ETag alone confirm nothing.
	confirmed bool
}

// Put writes r to key whole or not at all; see blob.Store. An object of at
// most one part is one PutObject request, a larger one a multipart upload
// that is completed, or aborted on every failure.
func (s *Store) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if err := s.beginWrite(ctx, "the put"); err != nil {
		return blob.Written{}, err
	}
	if err := blob.CheckKey(key); err != nil {
		return blob.Written{}, err
	}
	if !s.fits(key) {
		return blob.Written{}, fault.New(fault.Invalid, "storage key is too long for this storage's prefix: the name in the bucket would pass 1024 bytes")
	}
	j := &putJob{s: s, key: s.object(key), hash: sha256.New()}

	// The Put's memory comes from the store's budget, shared by every Put:
	// one part before a byte is read, taken by waiting, and then more only
	// when free (see budget). A Put that waits holds nothing.
	unit := s.set.partSize + 1
	if err := s.budget.take(ctx, unit); err != nil {
		return blob.Written{}, canceled(ctx, "the put")
	}
	j.held = unit
	defer func() { s.budget.give(j.held) }()

	// One byte more than a part is read before anything is sent: an object of
	// exactly one part is one request, and the extra byte, when it comes,
	// proves a second part exists before an upload is begun for it.
	limit := int(unit)
	first, err := j.fill(ctx, r, nil, limit)
	if err != nil {
		return blob.Written{}, err
	}
	if len(first) < limit {
		// A small object keeps only the memory it filled.
		keep := min(int64(cap(first)), unit)
		s.budget.give(unit - keep)
		j.held = keep
		if err := j.putObject(ctx, first); err != nil {
			return blob.Written{}, err
		}
		return j.written(), nil
	}
	if err := j.multipart(ctx, r, first); err != nil {
		return blob.Written{}, err
	}
	return j.written(), nil
}

func (j *putJob) written() blob.Written {
	w := blob.Written{Size: j.size, Confirmed: j.confirmed}
	j.hash.Sum(w.SHA256[:0])
	return w
}

// fill appends to buf from r until len(buf) reaches limit or r ends, hashing
// and counting what it reads, and checking ctx between reads. The buffer
// grows as bytes come, so an object smaller than a part costs its own size.
// A short result means r ended.
func (j *putJob) fill(ctx context.Context, r io.Reader, buf []byte, limit int) ([]byte, error) {
	idle := 0
	for len(buf) < limit {
		if ctx.Err() != nil {
			return nil, canceled(ctx, "the put")
		}
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, min(max(len(buf), 64<<10), limit-len(buf)))
		}
		room := buf[len(buf):min(cap(buf), limit)]
		n, err := r.Read(room)
		if n > 0 {
			idle = 0
			j.hash.Write(room[:n]) // A hash never returns an error.
			j.size += int64(n)
			buf = buf[:len(buf)+n]
		}
		switch {
		case errors.Is(err, io.EOF):
			return buf, nil
		case err != nil:
			return nil, fmt.Errorf("storage put: reading the object: %w", err)
		case n == 0:
			if idle++; idle >= 100 {
				return nil, fmt.Errorf("storage put: reading the object: %w", io.ErrNoProgress)
			}
		}
	}
	return buf, nil
}

// checksum returns b's SHA-256 as S3 writes it in x-amz-checksum-sha256.
func checksum(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// putObject sends b as one PutObject request. The request carries its own
// SHA-256, which a server that supports it checks before it keeps anything,
// and which the SDK's checksum setting (when required only) does not
// replace. The server's echo of the checksum, when it sends one, is
// compared too.
func (j *putJob) putObject(ctx context.Context, b []byte) error {
	ctx, cancel := context.WithTimeout(ctx, j.s.set.dataTimeout)
	defer cancel()
	sum := checksum(b)
	out, err := j.s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:            aws.String(j.s.set.bucket),
		Key:               j.key,
		Body:              bytes.NewReader(b),
		ContentLength:     aws.Int64(int64(len(b))),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		ChecksumSHA256:    aws.String(sum),
	})
	if err != nil {
		return fail(ctx, err, "the put")
	}
	if err := confirmed(out.ChecksumSHA256, sum); err != nil {
		return err
	}
	j.confirmed = j.s.checksumsChecked
	return nil
}

// confirmed checks a server's echo of the SHA-256 of what it was sent. The
// echo is required: a success is believed only because the server said
// something about these bytes, and an answer without it, like one with
// another checksum, is a failure.
func confirmed(echo *string, sum string) error {
	switch got := aws.ToString(echo); {
	case got == "":
		return fault.Wrap(fault.Unavailable, errNotConfirmed, "the put failed: the storage did not confirm the checksum of the bytes it was sent")
	case got != sum:
		return fault.Wrap(fault.Unavailable, errMalformed, "the put failed: the storage kept bytes whose checksum is not the one sent")
	}
	return nil
}

// multipart sends the object as parts, beginning with first, which holds one
// part and one byte of the next. Parts are read here, one buffer at a time,
// and sent by at most Concurrency goroutines, each holding one buffer, so the
// memory in use is Concurrency parts however large the object. Whatever
// fails, the upload is aborted before multipart returns.
func (j *putJob) multipart(ctx context.Context, r io.Reader, first []byte) (err error) {
	s := j.s
	create, err := func() (*awss3.CreateMultipartUploadOutput, error) {
		cctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
		defer cancel()
		out, err := s.client.CreateMultipartUpload(cctx, &awss3.CreateMultipartUploadInput{
			Bucket:            aws.String(s.set.bucket),
			Key:               j.key,
			ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		})
		if err != nil {
			return nil, fail(cctx, err, "the put")
		}
		if aws.ToString(out.UploadId) == "" {
			return nil, fault.Wrap(fault.Unavailable, errMalformed, "the put failed: the storage began no upload")
		}
		return out, nil
	}()
	if err != nil {
		return err
	}
	uploadID := create.UploadId

	// pctx ends at the first failure, so the other parts stop being sent.
	pctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		stop()
		wg.Wait()
		if err != nil {
			_ = s.abort(ctx, j.key, uploadID) // The Put's own error is the one to give.
		}
	}()

	var mu sync.Mutex
	var firstErr error
	var parts []types.CompletedPart
	digests := map[int32][sha256.Size]byte{}
	failWith := func(e error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = e
			stop()
		}
		mu.Unlock()
	}
	failed := func() error {
		mu.Lock()
		defer mu.Unlock()
		return firstErr
	}

	partSize := int(s.set.partSize)
	free := make(chan []byte, s.set.concurrency)
	made := 1 // The first buffer is already made.
	buf := first
	for n := int32(1); ; n++ {
		if n > s.set.maxParts {
			failWith(fault.New(fault.Invalid, "the put failed: the object needs more than 10,000 parts; raise the part size"))
			break
		}
		// buf holds the next part, partSize bytes or the last short one, and
		// when more follows, one byte beyond it, which starts the next part.
		last := len(buf) <= partSize
		var carry byte
		if !last {
			carry = buf[partSize]
			buf = buf[:partSize]
		}
		wg.Add(1)
		go func(n int32, part []byte) {
			defer wg.Done()
			defer func() { free <- part[:0] }()
			pctx2, cancel := context.WithTimeout(pctx, s.set.dataTimeout)
			defer cancel()
			raw := sha256.Sum256(part)
			sum := base64.StdEncoding.EncodeToString(raw[:])
			out, e := s.client.UploadPart(pctx2, &awss3.UploadPartInput{
				Bucket:            aws.String(s.set.bucket),
				Key:               j.key,
				UploadId:          uploadID,
				PartNumber:        aws.Int32(n),
				Body:              bytes.NewReader(part),
				ContentLength:     aws.Int64(int64(len(part))),
				ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
				ChecksumSHA256:    aws.String(sum),
			})
			if e != nil {
				failWith(fail(pctx2, e, "the put"))
				return
			}
			if e := confirmed(out.ChecksumSHA256, sum); e != nil {
				failWith(e)
				return
			}
			mu.Lock()
			digests[n] = raw
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag, ChecksumSHA256: aws.String(sum)})
			mu.Unlock()
		}(n, buf)
		if last {
			break
		}

		// Get a buffer for the next part: a returned one, a new one while fewer
		// than Concurrency exist, else wait for one.
		var nb []byte
		select {
		case nb = <-free:
		default:
			if made < s.set.concurrency && s.budget.tryTake(s.set.partSize+1) {
				made++
				j.held += s.set.partSize + 1
				nb = make([]byte, 0, partSize+1)
			} else {
				select {
				case nb = <-free:
				case <-pctx.Done():
				}
			}
		}
		if pctx.Err() != nil {
			break
		}
		var ferr error
		buf, ferr = j.fill(pctx, r, append(nb, carry), partSize+1)
		if ferr != nil {
			// When a part failed, pctx ended and the failure is the error to
			// give; otherwise ferr is the reader's, or the caller's context's.
			if failed() == nil {
				failWith(ferr)
			}
			break
		}
	}
	wg.Wait()
	if e := failed(); e != nil {
		return e
	}
	if ctx.Err() != nil {
		return canceled(ctx, "the put")
	}

	sort.Slice(parts, func(a, b int) bool { return aws.ToInt32(parts[a].PartNumber) < aws.ToInt32(parts[b].PartNumber) })
	cctx, cancel := context.WithTimeout(ctx, s.set.callTimeout+s.set.dataTimeout/5)
	defer cancel()
	// Completing is the one request not repeated by the SDK: a repeat after a
	// success is answered "no such upload", which would report a failure for
	// an object that is in place. The port allows a failure after the object
	// is in place; it does not allow a success that is not true.
	done, cerr := s.client.CompleteMultipartUpload(cctx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.set.bucket),
		Key:             j.key,
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}, func(o *awss3.Options) { o.Retryer = aws.NopRetryer{} })
	if cerr != nil {
		return fail(cctx, cerr, "the put")
	}
	return j.confirmComplete(cctx, done, parts, digests)
}

// confirmComplete decides whether the answer to CompleteMultipartUpload
// shows the object stored. The answer must be well formed: the object's key
// and an ETag. When it carries the object's composite checksum it must be
// the one computed from the parts' digests; when it does not, the object's
// size, and its ETag where the answer gave one, are confirmed with a HEAD,
// so that a bare 200 never counts.
//
// The composite checksum of a multipart upload is the SHA-256 of the parts'
// binary SHA-256 digests joined in order, in base64, with "-" and the number
// of parts after it
// (docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html,
// "Using part-level checksums for multipart uploads").
func (j *putJob) confirmComplete(ctx context.Context, done *awss3.CompleteMultipartUploadOutput, parts []types.CompletedPart, digests map[int32][sha256.Size]byte) error {
	bad := func(msg string) error {
		return fault.Wrap(fault.Unavailable, errNotConfirmed, "the put failed: "+msg)
	}
	if aws.ToString(done.ETag) == "" || aws.ToString(done.Key) != aws.ToString(j.key) {
		return bad("the storage's answer to completing the upload does not name the object")
	}
	if got := aws.ToString(done.ChecksumSHA256); got != "" {
		h := sha256.New()
		for _, p := range parts {
			d := digests[aws.ToInt32(p.PartNumber)]
			h.Write(d[:]) // A hash never returns an error.
		}
		if want := fmt.Sprintf("%s-%d", base64.StdEncoding.EncodeToString(h.Sum(nil)), len(parts)); got != want {
			return fault.Wrap(fault.Unavailable, errMalformed, "the put failed: the storage kept an object whose checksum is not that of the parts sent")
		}
		j.confirmed = j.s.checksumsChecked
		return nil
	}
	head, err := j.s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(j.s.set.bucket), Key: j.key})
	if err != nil {
		if err = fail(ctx, err, "the put"); fault.KindOf(err) == fault.NotFound {
			return bad("the storage does not hold the object it just completed")
		}
		return err
	}
	if aws.ToInt64(head.ContentLength) != j.size {
		return bad("the storage holds an object of another size than the one put")
	}
	if e := aws.ToString(head.ETag); e != "" && strings.Trim(e, `"`) != strings.Trim(aws.ToString(done.ETag), `"`) {
		return bad("the storage holds an object other than the one completed")
	}
	return nil
}

// abort ends an upload that failed. It runs without the caller's
// cancellation, within abortTimeout, because the caller's context has often
// ended by now. A failed abort leaves the upload for the cleanup at Open or
// the bucket's lifecycle rule; the Put's own error is the one returned.
func (s *Store) abort(ctx context.Context, key, uploadID *string) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
	defer cancel()
	_, err := s.client.AbortMultipartUpload(actx, &awss3.AbortMultipartUploadInput{
		Bucket: aws.String(s.set.bucket), Key: key, UploadId: uploadID,
	})
	if err != nil {
		return fail(actx, err, "the abort of an upload")
	}
	return nil
}
