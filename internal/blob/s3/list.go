package s3

import (
	"context"
	"encoding/base64"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// tokenTag begins every token this driver gives, so a token from anywhere
// else, or one that was damaged, is refused as Invalid without a request.
const tokenTag = "s3."

// prefixEnd, appended to a prefix, sorts after every name that begins with
// that prefix and holds valid UTF-8 below U+10FFFF.
const prefixEnd = "\U0010FFFF"

// maxToken bounds the length of a token read back; a key is at most
// blob.MaxKeyLength bytes.
const maxToken = 2048

// maxListCalls bounds the requests one page costs. A page costs one except
// when the server returns fewer entries than asked while saying more
// follow, when everything it returned is something the port cannot name, or
// when the page ends exactly where the listing might (see List).
const maxListCalls = 50

// entry is one name of a listing, a key or a prefix that ends in "/".
type entry struct {
	name     string // without the storage's prefix
	isPrefix bool
}

// List returns one page of keys with ListObjectsV2.
//
// The token is not the server's continuation token but the last entry
// returned, and the next page asks the server for the names after it
// (start-after). Two reasons. A continuation token says "more follows" when
// a page is exactly full, on some servers even when nothing does, and the
// port forbids a token with nothing after it; so one entry more than the
// page is asked for, and the extra proves what follows. And a name is what
// the port's stability rule is about: a key put behind the last entry is not
// seen and one ahead of it is, on every server, whatever a token encodes.
// After a prefix (a group of keys folded by the delimiter) the next page
// starts after every key the prefix could hold: prefixEnd, the largest
// character there is, after any name that is valid UTF-8 in the folded
// group, so a name outside the port's alphabet inside it cannot make the
// prefix come up again.
//
// S3 returns keys in the byte order of their UTF-8 text, which for the
// characters a key may hold is the port's order. An object under the prefix
// that no key of this storage could name (a folder marker made by a
// console, a name with a space) is left out: the port cannot read or delete
// it.
func (s *Store) List(ctx context.Context, opts blob.ListOptions) (blob.Page, error) {
	if err := s.begin(ctx, "the listing"); err != nil {
		return blob.Page{}, err
	}
	if err := blob.CheckListOptions(opts); err != nil {
		return blob.Page{}, err
	}
	floor, err := decodeToken(opts.Token, opts.Prefix)
	if err != nil {
		return blob.Page{}, err
	}
	limit := blob.ListLimit(opts)
	var entries []entry
	cursor := floor.after()
	more := false
	want := s.set.prefix + opts.Prefix // Every name the server returns begins with it.
	for calls := 0; ; calls++ {
		if calls == maxListCalls {
			// Each call advanced (see below), so what has been collected is a
			// true start of the listing and there is more after it.
			more = true
			break
		}
		in := &awss3.ListObjectsV2Input{
			Bucket:  aws.String(s.set.bucket),
			MaxKeys: aws.Int32(int32(min(limit+1-len(entries), blob.MaxListLimit))), // #nosec G115 -- from 1 to 1000.
		}
		if want != "" {
			in.Prefix = aws.String(want)
		}
		if opts.Delimiter != "" {
			in.Delimiter = aws.String(opts.Delimiter)
		}
		if cursor != "" {
			in.StartAfter = aws.String(s.set.prefix + cursor)
		}
		lctx, cancel := context.WithTimeout(ctx, s.set.callTimeout)
		out, err := s.client.ListObjectsV2(lctx, in)
		if err != nil {
			err = fail(lctx, err, "the listing")
			cancel()
			return blob.Page{}, err
		}
		cancel()
		got, last, err := s.checkListing(out, want, s.set.prefix+cursor)
		if err != nil {
			return blob.Page{}, err
		}
		entries = append(entries, got...)
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		// Truncated: go on from the last name the server named, kept or not
		// (checkListing has seen to it that there is one beyond the point this
		// request started after), until there is an entry beyond the page,
		// which proves there is more, or the server says it has no more.
		if len(entries) > limit {
			break
		}
		cursor = strings.TrimPrefix(last, s.set.prefix)
	}
	if len(entries) > limit {
		entries = entries[:limit]
		more = true
	}
	var page blob.Page
	for _, e := range entries {
		if e.isPrefix {
			page.Prefixes = append(page.Prefixes, e.name)
		} else {
			page.Keys = append(page.Keys, e.name)
		}
	}
	if more && len(entries) > 0 {
		page.Next = encodeToken(entries[len(entries)-1])
	}
	return page, nil
}

// malformedListing is the error for a listing answer that is not what was
// asked: the driver never turns one into a shorter listing.
func malformedListing() error {
	return fault.Wrap(fault.Unavailable, errMalformed, "the listing failed: the storage's answer is not a listing of what was asked")
}

// checkListing validates one page of the server's answer and returns the
// entries the port could have written, in order, and the greatest name the
// server named (a prefix as the cursor that skips it), kept or not. An answer
// is malformed, and the listing fails as Unavailable, when a name lies
// outside the listing's prefix, names are out of order or repeated, a name
// is not beyond the point the request started after, or a truncated answer
// would not move the listing forward: ending the listing there instead
// would hide keys with no error.
func (s *Store) checkListing(out *awss3.ListObjectsV2Output, want, after string) (kept []entry, last string, err error) {
	var all []entry // Keys and prefixes together, to order them.
	prev := ""
	for i, o := range out.Contents {
		k := aws.ToString(o.Key)
		if !strings.HasPrefix(k, want) || k <= after || (i > 0 && k <= prev) {
			return nil, "", malformedListing()
		}
		prev = k
		if rest, ok := s.ours(k, false); ok {
			all = append(all, entry{name: rest})
		}
		last = max(last, k)
	}
	prev = ""
	for i, p := range out.CommonPrefixes {
		k := aws.ToString(p.Prefix)
		if !strings.HasPrefix(k, want) || k <= after || (i > 0 && k <= prev) {
			return nil, "", malformedListing()
		}
		prev = k
		if rest, ok := s.ours(k, true); ok {
			all = append(all, entry{name: rest, isPrefix: true})
		}
		last = max(last, k+prefixEnd)
	}
	slices.SortFunc(all, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	if aws.ToBool(out.IsTruncated) && last <= after {
		return nil, "", malformedListing()
	}
	return all, last, nil
}

func encodeToken(e entry) string {
	kind := "k"
	if e.isPrefix {
		kind = "p"
	}
	return tokenTag + base64.RawURLEncoding.EncodeToString([]byte(kind+e.name))
}

// after returns the name a request starts after to get everything beyond e:
// the key itself, or, for a prefix, a name above every key the prefix holds.
func (e entry) after() string {
	switch {
	case e.name == "":
		return ""
	case e.isPrefix:
		return e.name + prefixEnd
	}
	return e.name
}

// decodeToken returns the entry a token names, checking that it is one this
// driver made for a listing under prefix.
func decodeToken(t, prefix string) (entry, error) {
	if t == "" {
		return entry{}, nil
	}
	bad := fault.New(fault.Invalid, "storage listing: the token is not one this listing gave")
	raw, ok := strings.CutPrefix(t, tokenTag)
	if !ok || len(t) > maxToken {
		return entry{}, bad
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) < 2 || (b[0] != 'k' && b[0] != 'p') {
		return entry{}, bad
	}
	e := entry{name: string(b[1:]), isPrefix: b[0] == 'p'}
	check := e.name
	if e.isPrefix {
		if check, ok = strings.CutSuffix(check, "/"); !ok {
			return entry{}, bad
		}
	}
	if blob.CheckKey(check) != nil || !strings.HasPrefix(e.name, prefix) {
		return entry{}, bad
	}
	return e, nil
}

// ours returns the key (or, for a common prefix, the prefix) the server named
// without the storage's prefix, and whether the port could have written it.
// A name outside the storage's prefix is never returned, whatever the
// server sent.
func (s *Store) ours(name string, isPrefix bool) (string, bool) {
	rest, ok := strings.CutPrefix(name, s.set.prefix)
	if !ok || rest == "" {
		return "", false
	}
	if isPrefix {
		trimmed, ok := strings.CutSuffix(rest, "/")
		if !ok || blob.CheckKey(trimmed) != nil {
			return "", false
		}
		return rest, true
	}
	if blob.CheckKey(rest) != nil {
		return "", false
	}
	return rest, true
}
