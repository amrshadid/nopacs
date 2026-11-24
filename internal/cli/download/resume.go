package download

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// resumePart finishes a partial file with the plain HTTP route: a range from
// where the .part stops, valid only while the file is the one whose SHA-256
// is its ETag (RFC 9110 13.1.5), so bytes of two versions are never joined.
// The ETag asked for is the SHA-256 the .part's record names (begin
// continues a .part only when it is the announced one), never only the
// announcement's, so a .part of an earlier version of the file is not
// continued with the bytes of the new one.
// The whole file is checked against its announced SHA-256 at the end, the
// bytes already on disk included. Anything the route does not answer as a
// range or a whole file (the file changed, was removed, or a limit was met)
// gives the partial copy up: it is removed and the stream sends the file
// whole, which is also where a real error is reported.
func (d *downloader) resumePart(ctx context.Context, o *partial) error {
	f := o.ann
	part := o.final + partSuffix
	h := sha256.New()
	have, err := hashFile(part, h)
	if err != nil {
		d.giveUp(o)
		return nil
	}
	sum := recorded(o.final)
	if sum != f.GetSha256() {
		d.giveUp(o)
		return nil
	}
	res, err := d.c.OpenInstance(ctx, d.tenant, f.GetSubjectLabel(), f.GetSessionLabel(), f.GetScanLabel(), f.GetInstanceId(), have, `"`+sum+`"`)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	flags := os.O_WRONLY | os.O_APPEND
	switch {
	case res.StatusCode == http.StatusPartialContent && strings.HasPrefix(res.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", have)):
		d.resumed++
	case res.StatusCode == http.StatusOK:
		// The server answered the whole file: the range was not honored or
		// the file is no longer the one the part came from.
		h.Reset()
		have = 0
		flags = os.O_WRONLY | os.O_TRUNC
	default:
		d.giveUp(o)
		return nil
	}
	file, err := os.OpenFile(part, flags, 0o600) // #nosec G304 -- validated labels under --to
	if err != nil {
		return fault.Wrap(fault.Internal, err, "cannot write "+part+": "+err.Error())
	}
	o.file, o.h, o.n = file, h, have
	buf := make([]byte, 256<<10)
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if d.overflows(o, n) {
				return nil
			}
			if _, err := file.Write(buf[:n]); err != nil {
				_ = file.Close()
				return fault.Wrap(fault.Internal, err, "writing "+part+": "+err.Error())
			}
			_, _ = h.Write(buf[:n])
			o.n += int64(n)
			d.bytes += int64(n)
			d.report()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = file.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fault.Wrap(fault.Unavailable, rerr, "the download of "+output.Clean(f.GetInstanceId())+" broke off: "+rerr.Error())
		}
	}
	return d.close(o)
}

// giveUp discards a partial copy that cannot be continued, so the next stream
// fetches the file whole and does not try the range again.
func (d *downloader) giveUp(o *partial) {
	_ = os.Remove(o.final + partSuffix)
	_ = os.Remove(o.final + recordSuffix)
	d.tried[o.final] = true
}

// hashFile feeds a file to h and returns its length.
func hashFile(path string, h io.Writer) (int64, error) {
	f, err := os.Open(path) // #nosec G304 -- validated labels under --to
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return io.Copy(h, f)
}
