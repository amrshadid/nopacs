package receive

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// liveLimit is a bound read from a setting and kept for an interval, so a
// setting is read once a second at most and not once a file.
type liveLimit struct {
	mu    sync.Mutex
	bytes int64
	read  time.Time
}

// pixelLimit returns the bound of an image decoded that de-identification
// works on the pixels of: the one read from Config.PixelLimit, again once
// HeaderLimitInterval has passed. A read that fails keeps the last bound,
// or DefaultPixelLimit before any, and is logged: a setting that cannot be
// read never stops a reception.
func (p *Pipeline) pixelLimit(ctx context.Context) int64 {
	if p.cfg.PixelLimit == nil {
		return DefaultPixelLimit
	}
	l := &p.deid.pixelLimit
	now := p.cfg.Clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bytes > 0 && now.Sub(l.read) < HeaderLimitInterval && !now.Before(l.read) {
		return l.bytes
	}
	n, err := p.cfg.PixelLimit(ctx)
	switch {
	case err != nil:
		p.log.Warn("receive: the pixel limit could not be read; the last one is kept", "reason", fault.Message(err))
	case n > 0:
		l.bytes = n
	}
	l.read = now
	if l.bytes <= 0 {
		return DefaultPixelLimit
	}
	return l.bytes
}

// image is what the engine is given to work on a file's pixels: the source
// the data set's values are left in, a reader of its stored frames made
// only when a detect rule asks for it, and the bound.
func image(h *header.Header, file, source io.ReaderAt, limit int64) engine.Image {
	return engine.Image{Source: source, Frames: &lazyFrames{h: h, file: file}, Limit: limit}
}

// lazyFrames is a reader of a file's stored frames, found when a frame is
// first read: finding them reads the encapsulated items' headers, which a
// file no detector looks at need not cost.
type lazyFrames struct {
	h    *header.Header
	file io.ReaderAt
	once sync.Once
	fr   dataset.FrameReader
	err  error
}

// open finds the frames once.
func (l *lazyFrames) open() error {
	l.once.Do(func() { l.fr, l.err = l.h.FrameReader(l.file) })
	return l.err
}

// ReadFrame implements dataset.FrameReader.
func (l *lazyFrames) ReadFrame(i int) ([]byte, error) {
	if err := l.open(); err != nil {
		return nil, err
	}
	return l.fr.ReadFrame(i)
}

// ReadFrameBytes implements dataset.FrameReader.
func (l *lazyFrames) ReadFrameBytes(i int, offset int64, b []byte) error {
	if err := l.open(); err != nil {
		return err
	}
	return l.fr.ReadFrameBytes(i, offset, b)
}

// writtenSyntax is the transfer syntax a de-identified file is written in:
// the one it was received in, or the one the data set says once its pixels
// were decoded to be filled (Explicit VR Little Endian).
func writtenSyntax(received string, ds *dataset.Dataset, rec *engine.Record) string {
	if rec != nil && rec.Pixels != nil && rec.Pixels.Decoded {
		return ds.TransferSyntaxUID()
	}
	return received
}

// grown is how much larger than its received bound a de-identified file
// may be, for the check after to read it whole: the image decoded, when
// its pixels were.
func grown(rec *engine.Record) int64 {
	if rec != nil && rec.Pixels != nil && rec.Pixels.Decoded {
		return rec.Pixels.DecodedBytes
	}
	return 0
}

// pixelHold is the hold of a file whose pixels did not decode as it was
// written: deid_failed, pixels_undecodable.
var pixelHold = map[string]string{"cause": string(engine.CausePixelsUndecodable)}

// burnedInHold returns the reason and the details of an image at risk of
// burned-in text no pixel rule matches.
func burnedInHold(failure *engine.Failure) (tenant.QuarantineReason, map[string]string) {
	details := map[string]string{}
	if failure.RiskCause != "" {
		details["cause"] = failure.RiskCause
	}
	return tenant.QuarantineBurnedInRisk, details
}
