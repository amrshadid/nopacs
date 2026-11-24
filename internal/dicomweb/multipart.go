package dicomweb

import (
	"io"
	"mime/multipart"
	"net/textproto"
)

// Multipart writes a multipart/related body of retrieved resources (PS3.18
// 8.7.3, RFC 2387), one part at a time, so a study is never held whole.
type Multipart struct {
	w *multipart.Writer
	// typ is the media type of its parts, its type parameter.
	typ string
}

// NewMultipart returns a body whose parts are of the media type typ,
// written to out.
func NewMultipart(out io.Writer, typ string) *Multipart {
	return &Multipart{w: multipart.NewWriter(out), typ: typ}
}

// ContentType is the Content-Type of the body: multipart/related with its
// type and boundary parameters (RFC 2387 3.1, PS3.18 8.7.3).
func (m *Multipart) ContentType() string {
	return MediaMultipart + `; type="` + m.typ + `"; boundary=` + m.w.Boundary()
}

// Part starts a part of the media type media, with its transfer syntax
// when ts is not empty (PS3.18 8.7.3.5.2), and returns where its bytes go.
func (m *Multipart) Part(media, ts string) (io.Writer, error) {
	h := textproto.MIMEHeader{}
	ct := media
	if ts != "" {
		ct += "; transfer-syntax=" + ts
	}
	h.Set("Content-Type", ct)
	return m.w.CreatePart(h)
}

// Close writes the body's closing boundary. A body cut before it is not
// whole, which is how a client learns that an answer begun failed.
func (m *Multipart) Close() error { return m.w.Close() }
