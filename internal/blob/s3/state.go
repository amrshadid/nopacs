package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

var (
	_ blob.Recaller           = (*Store)(nil)
	_ blob.EncryptionReporter = (*Store)(nil)
)

// infoOf turns the answer to a HEAD into what the port knows of an object:
// its size, whether it reads at once, and the server's own SHA-256 of it.
func infoOf(out *awss3.HeadObjectOutput) blob.Info {
	info := blob.Info{Size: aws.ToInt64(out.ContentLength)}
	info.State, info.OnlineUntil = onlineState(out)
	info.SHA256, info.HasSHA256 = wholeChecksum(out.ChecksumSHA256, out.ChecksumType)
	return info
}

// The parts of x-amz-restore
// (docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html, "x-amz-restore"):
// ongoing-request="true" while a restore runs; ongoing-request="false" and
// expiry-date="<HTTP date>" once the object is back, until that date.
var (
	restoreOngoing = regexp.MustCompile(`ongoing-request="(true|false)"`)
	restoreExpiry  = regexp.MustCompile(`expiry-date="([^"]{1,64})"`)
)

// onlineState reads whether an object reads at once from a HEAD's answer.
// An object in a cold class (GLACIER, DEEP_ARCHIVE, or an Intelligent-Tiering
// archive tier, x-amz-archive-status) is archived until a restore brings it
// back; x-amz-restore says a restore runs, or until when a restored copy
// stays. GLACIER_IR reads at once and is online. A restore header that
// cannot be read is ignored, and the class alone decides.
func onlineState(out *awss3.HeadObjectOutput) (blob.OnlineState, time.Time) {
	if h := aws.ToString(out.Restore); h != "" {
		if m := restoreOngoing.FindStringSubmatch(h); m != nil {
			if m[1] == "true" {
				return blob.Restoring, time.Time{}
			}
			var until time.Time
			if e := restoreExpiry.FindStringSubmatch(h); e != nil {
				if t, err := http.ParseTime(e[1]); err == nil {
					until = t.UTC()
				}
			}
			return blob.Online, until
		}
	}
	if coldClass(out.StorageClass) || out.ArchiveStatus != "" {
		return blob.Archived, time.Time{}
	}
	return blob.Online, time.Time{}
}

// coldClass reports whether objects of class must be restored before they
// are read (docs.aws.amazon.com/AmazonS3/latest/userguide/restoring-objects.html).
func coldClass(class types.StorageClass) bool {
	return class == types.StorageClassGlacier || class == types.StorageClassDeepArchive
}

// wholeChecksum returns the server's SHA-256 of a whole object, when the
// answer carries one. A multipart object's checksum is composite, the
// SHA-256 of its parts' digests followed by "-" and their number, and says
// nothing a caller holding the object's own SHA-256 can compare: it is not
// returned, nor is a value that does not decode to exactly 32 bytes.
func wholeChecksum(v *string, kind types.ChecksumType) ([sha256.Size]byte, bool) {
	var sum [sha256.Size]byte
	if kind == types.ChecksumTypeComposite {
		return sum, false
	}
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(v))
	if err != nil || len(raw) != sha256.Size {
		return sum, false
	}
	copy(sum[:], raw)
	return sum, true
}

// recallTiers are the Tier of RestoreObject for each speed.
var recallTiers = map[blob.RecallSpeed]types.Tier{
	blob.RecallStandard:  types.TierStandard,
	blob.RecallBulk:      types.TierBulk,
	blob.RecallExpedited: types.TierExpedited,
}

// hotClass reports whether objects of class read at once and never need a
// restore, so Recall asks nothing for one (docs.aws.amazon.com/AmazonS3/
// latest/userguide/storage-class-intro.html). A class not listed, one this
// version does not know included, is asked: the server's answer then says.
// An Intelligent-Tiering object is hot unless x-amz-archive-status names an
// archive tier, which onlineState reads first.
func hotClass(class types.StorageClass) bool {
	switch class {
	case "", types.StorageClassStandard, types.StorageClassStandardIa, types.StorageClassOnezoneIa,
		types.StorageClassReducedRedundancy, types.StorageClassGlacierIr, types.StorageClassIntelligentTiering,
		types.StorageClassExpressOnezone:
		return true
	}
	return false
}

// deepClass reports whether the object is in a class S3 restores at the
// standard or bulk speed only: DEEP_ARCHIVE, and Intelligent-Tiering's deep
// archive tier (docs.aws.amazon.com/AmazonS3/latest/userguide/restoring-objects-retrieval-options.html).
func deepClass(head *awss3.HeadObjectOutput) bool {
	return head.StorageClass == types.StorageClassDeepArchive || head.ArchiveStatus == types.ArchiveStatusDeepArchiveAccess
}

// Recall asks the server to restore key for req.Days at req.Speed
// (RestoreObject, docs.aws.amazon.com/AmazonS3/latest/API/API_RestoreObject.html).
// It reads the object's state first: an object being restored is answered
// RecallInProgress with no second request; one of a class known to read at
// once, never restored, RecallOnline with no request; an expedited recall of
// a deep class, which S3 never serves, fails at once with
// blob.ErrRecallRefused. Otherwise the server's answer says: 202 a restore
// taken (RecallStarted), 200 a copy already back whose days now run from
// today (RecallOnline), 409 RestoreAlreadyInProgress (RecallInProgress). An
// object in an Intelligent-Tiering archive tier is restored without Days,
// which that class refuses: it stays back until the class moves it again.
//
// It writes no object, so a store opened for reading may ask it; who may
// start a recall, which costs the site money, is the server's to decide.
func (s *Store) Recall(ctx context.Context, key string, req blob.RecallRequest) (blob.Recall, error) {
	if err := s.begin(ctx, "the recall"); err != nil {
		return blob.Recall{}, err
	}
	if err := blob.CheckKey(key); err != nil {
		return blob.Recall{}, err
	}
	if err := blob.CheckRecall(req); err != nil {
		return blob.Recall{}, err
	}
	if !s.fits(key) {
		return blob.Recall{}, fault.New(fault.NotFound, "the storage holds no object under that key")
	}
	ctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	head, err := s.head(ctx, key)
	if err != nil {
		return blob.Recall{}, err
	}
	state, _ := onlineState(head)
	restored := state == blob.Online && restoreOngoing.MatchString(aws.ToString(head.Restore))
	switch {
	case state == blob.Restoring:
		return blob.Recall{State: blob.RecallInProgress}, nil
	case state == blob.Online && !restored && hotClass(head.StorageClass):
		// An x-amz-restore that cannot be read on a hot class asks nothing
		// either: the class reads at once.
		return blob.Recall{State: blob.RecallOnline}, nil
	case state == blob.Archived && req.Speed == blob.RecallExpedited && deepClass(head):
		return blob.Recall{}, fault.Wrap(fault.Unavailable, blob.ErrRecallRefused, "the recall failed: the storage offers no expedited recall for the object's class")
	}
	rr := &types.RestoreRequest{GlacierJobParameters: &types.GlacierJobParameters{Tier: recallTiers[req.Speed]}}
	if head.ArchiveStatus == "" {
		rr.Days = aws.Int32(int32(req.Days)) // #nosec G115 -- CheckRecall bounds Days to 1..365.
	}
	out, err := s.client.RestoreObject(ctx, &awss3.RestoreObjectInput{Bucket: aws.String(s.set.bucket), Key: s.object(key), RestoreRequest: rr})
	if err != nil {
		return s.recallRefused(ctx, key, err)
	}
	if resp, ok := awsmiddleware.GetRawResponse(out.ResultMetadata).(*smithyhttp.Response); ok && resp.StatusCode == http.StatusOK {
		// The copy was back already; its days were set anew from today, and
		// the HEAD's expiry is the old one.
		return blob.Recall{State: blob.RecallOnline}, nil
	}
	return blob.Recall{State: blob.RecallStarted}, nil
}

// head is the HEAD of key, its failure in the port's kinds.
func (s *Store) head(ctx context.Context, key string) (*awss3.HeadObjectOutput, error) {
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.set.bucket), Key: s.object(key)})
	if err != nil {
		return nil, fail(ctx, err, "the recall")
	}
	return out, nil
}

// recallRefused maps a failed RestoreObject: a restore already running is
// RecallInProgress; a tier the server will not serve now
// (GlacierExpeditedRetrievalNotAvailable) is Unavailable with
// blob.ErrRecallRefused. An answer that the object needs no restore
// (InvalidObjectState for a class that is not cold,
// ObjectAlreadyInActiveTierError for an Intelligent-Tiering object back in
// its active tier) is believed only when a second HEAD shows the object
// online: answering online for an object the HEAD still shows cold would
// send a caller round and round, recalling and reading. Anything else is the
// usual failure.
func (s *Store) recallRefused(ctx context.Context, key string, err error) (blob.Recall, error) {
	code, _ := apiError(err)
	switch code {
	case "RestoreAlreadyInProgress":
		return blob.Recall{State: blob.RecallInProgress}, nil
	case "GlacierExpeditedRetrievalNotAvailable":
		return blob.Recall{}, fault.Wrap(fault.Unavailable, inClass(blob.ErrRecallRefused, serverCode(code)), "the recall failed: the storage does not offer that speed now")
	case "InvalidObjectState", "ObjectAlreadyInActiveTierError":
		head, herr := s.head(ctx, key)
		if herr != nil {
			return blob.Recall{}, herr
		}
		switch state, until := onlineState(head); state {
		case blob.Online:
			return blob.Recall{State: blob.RecallOnline, OnlineUntil: until}, nil
		case blob.Restoring:
			return blob.Recall{State: blob.RecallInProgress}, nil
		}
		return blob.Recall{}, fault.Wrap(fault.Unavailable, inClass(errMalformed, serverCode(code)), "the recall failed: the storage says the object needs no recall but holds it in a cold class")
	}
	return blob.Recall{}, fail(ctx, err, "the recall")
}

// The algorithms GetBucketEncryption names that are kept as they are
// (docs.aws.amazon.com/AmazonS3/latest/API/API_ServerSideEncryptionByDefault.html);
// any other is "other", so a server's own text never reaches a status.
var knownAlgorithms = map[types.ServerSideEncryption]bool{
	types.ServerSideEncryptionAes256:     true,
	types.ServerSideEncryptionAwsKms:     true,
	types.ServerSideEncryptionAwsKmsDsse: true,
}

// Encryption asks the bucket's default encryption (GetBucketEncryption,
// docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketEncryption.html).
// A rule with an algorithm is EncryptionOn; no configuration
// (ServerSideEncryptionConfigurationNotFoundError) is EncryptionNone;
// credentials that may not ask (AccessDenied), a server that does not
// implement the call, or an answer with no rule is EncryptionUnknown; wrong
// credentials and a failing server are errors. It is asked on each call,
// not at Open, so a store opened for every read costs no extra request; a
// status asks it.
func (s *Store) Encryption(ctx context.Context) (blob.Encryption, error) {
	if err := s.begin(ctx, "the encryption query"); err != nil {
		return blob.Encryption{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
	defer cancel()
	out, err := s.client.GetBucketEncryption(ctx, &awss3.GetBucketEncryptionInput{Bucket: aws.String(s.set.bucket)})
	if err != nil {
		code, status := apiError(err)
		switch {
		case code == "ServerSideEncryptionConfigurationNotFoundError":
			return blob.Encryption{State: blob.EncryptionNone}, nil
		case code == "NotImplemented" || status == http.StatusNotImplemented, code == "AccessDenied":
			// The key may not ask, or the server has no such call. Wrong
			// credentials (InvalidAccessKeyId, SignatureDoesNotMatch) are not
			// this: they are the storage's failure, answered as an error.
			return blob.Encryption{}, nil
		}
		return blob.Encryption{}, fail(ctx, err, "the encryption query")
	}
	if out.ServerSideEncryptionConfiguration == nil {
		return blob.Encryption{}, nil
	}
	for _, r := range out.ServerSideEncryptionConfiguration.Rules {
		d := r.ApplyServerSideEncryptionByDefault
		if d == nil || d.SSEAlgorithm == "" {
			continue
		}
		e := blob.Encryption{State: blob.EncryptionOn, Algorithm: "other", KeyNamed: aws.ToString(d.KMSMasterKeyID) != ""}
		if knownAlgorithms[d.SSEAlgorithm] {
			e.Algorithm = string(d.SSEAlgorithm)
		}
		return e, nil
	}
	return blob.Encryption{}, nil
}
