// Package s3 is the S3 storage driver: a blob.Store on a bucket of Amazon S3
// or of any server that speaks its API, below an optional prefix of the
// bucket. A key is an object name below that prefix, so the archive's tree
// can be read, listed and copied with any S3 tool.
//
// # Whole or not at all
//
// An object up to one part (8 MiB by default) is one PutObject request, which
// S3 applies whole or not at all. A larger one is a multipart upload: parts
// are sent in parallel from a bounded set of buffers, and the object appears
// only when CompleteMultipartUpload succeeds
// (docs.aws.amazon.com/AmazonS3/latest/userguide/mpuoverview.html). The
// upload is aborted when a part fails, when the reader fails, when the
// context ends, and when the object is too large for 10,000 parts, so a Put
// that returns an error leaves no upload behind unless the process dies
// while it runs. The SHA-256 of the whole object is computed here as the
// bytes pass and is what Put returns.
//
// # What a successful Put rests on
//
// A Put returns nil only when the server said something about these bytes,
// never because it answered 200:
//
//   - Every PutObject and every part carries its SHA-256 in
//     x-amz-checksum-sha256, which a server that supports it checks before it
//     keeps anything, and the server must echo it back: an answer without the
//     echo, or with another, is a failure. Amazon S3, MinIO and its forks, and
//     RustFS echo. A server that stores objects but does not is refused at
//     Open (see below) and never trusted silently.
//   - For a multipart upload, the answer to CompleteMultipartUpload must name
//     the object and carry an ETag, and then either its composite checksum
//     must be the SHA-256 of the parts' digests, in order, with "-" and the
//     number of parts (docs.aws.amazon.com/AmazonS3/latest/userguide/
//     checking-object-integrity.html, "Using part-level checksums for
//     multipart uploads"), which Amazon S3, MinIO and its forks, and RustFS
//     all return when the upload declares SHA-256, or, from a server that
//     gives none, a HEAD must show the object with the size put and the ETag
//     completed. A bare 200, an error under a 200 and an answer for another
//     object are failures.
//
// So on a server that echoes and gives a composite checksum, a success means
// the server computed and reported the checksum of every part and of the
// whole object over the bytes it was sent; on a server that echoes but gives
// no composite, it means the same for every part, and that the object exists
// with the right size and ETag. Neither is a read-back of the object: a
// server that reports a checksum it did not compute is not caught here, which
// is what the archive's own verification by reading is for.
//
// Open puts a small object under the prefix, with a name no key of the port
// can have, and deletes it, to see that the server echoes. That needs the
// right to write at Open, which is acceptable because a server that does not
// echo would fail every Put, each with a message that says less; credentials
// that may not write still open the storage, and Store.Notes says the check
// was not made.
//
// # Memory
//
// The memory of part buffers is bounded for the whole store, not per Put:
// Config.MemoryLimit (sixteen default parts) is shared by every Put. A Put
// takes one part from it before it reads a byte, waiting for that alone and
// no longer than its context, then takes more only when they are free, so a
// Put that holds a part always progresses by itself and none deadlocks. A
// small object holds only the bytes it has. An answer that is not an object's
// bytes is cut at 4 MiB (see below), and an object's bytes are streamed, never
// held.
//
// # Uploads left by a crash
//
// A process that dies midway leaves an unfinished multipart upload, whose
// parts cost storage until it is aborted. Open lists the unfinished uploads
// under the storage's prefix and aborts those begun more than
// Config.StaleUploadAge ago (a day by default), so another process still
// finishing a large Put is not cut off. The cleanup is bounded to ten pages
// of 1,000 uploads per call, begins where the prefix sorts, and never fails
// Open: what it did and what went wrong is in Store.Notes, and when the bound
// was reached the uploads beyond are unseen until the next call of
// AbortStaleUploads, which continues from where the last stopped. A bucket
// should also carry a lifecycle rule that aborts incomplete multipart uploads
// after a few days
// (docs.aws.amazon.com/AmazonS3/latest/userguide/mpu-abort-incomplete-mpu-lifecycle-config.html),
// which covers an account that may not list uploads.
//
// # Credentials and addresses
//
// A storage uses only the access key it is given, held in Secrets that print
// as nothing however they are printed or encoded. The driver never reads the
// environment, the shared configuration or credential files, or instance
// metadata, and never uses a proxy from the environment: it does not import
// the SDK's configuration loader. A bucket of the S3 Express form is refused,
// and the client cannot open an Express session, so there is no second
// credential path. The address of every connection it makes is passed to
// Config.AllowAddress after the name is resolved, in the dialer, so a name
// that resolves to an address the caller refuses is refused whenever it
// resolves so, at any call. AllowPublic passes global unicast addresses only,
// refusing every block of the IANA special-purpose registries and the
// transition prefixes that embed an IPv4 address. A redirect is never
// followed. A connection already open is reused, as it was checked when it
// was made.
//
// # Answers
//
// Every answer that is not an object's bytes is read through a limit of
// 4 MiB, derived from the largest honest answer (a listing page of 1,000
// keys of 1,024 bytes), and an answer longer is a malformed answer, not
// retried. A Read returns exactly the bytes asked for, checked against the
// Content-Range, the size and the length the server states, or an error. A
// listing answer that names something outside the prefix, out of order,
// repeated, or that does not move the listing on is an error, never a
// shorter listing.
//
// # Key length
//
// S3 accepts names of at most 1,024 bytes. A storage's prefix is at most
// MaxPrefixLength (128) bytes with its "/", and the port's longest key
// (blob.MaxKeyLength) is 1,024 less that, so every key the port accepts fits
// under every prefix. The driver still refuses a name that would pass 1,024
// bytes, as a second guard, which the port's limit normally prevents.
//
// # Errors
//
// Errors carry the port's kinds. Their text is the driver's own: the
// server's message, the endpoint's address, the bucket's host name and every
// part of a credential are left out of Error and of fault.Message, because
// both go to logs and to callers who must not learn them.
//
// # What this package must never do
//
// It must never read the environment or a credential file, follow a
// redirect, dial an address Config.AllowAddress has not passed, hold a
// whole object in memory or more part buffers than the store's budget,
// read an answer without a bound, report a success the server did not
// confirm, leave a multipart upload it began unfinished when it can abort
// it, touch a key outside the storage's prefix, or put a server's text, an
// address or a credential in an error.
package s3
