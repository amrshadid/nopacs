package dicomweb

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// The media types of the answers (PS3.18 8.7.3).
const (
	// MediaDICOMJSON is DICOM JSON (PS3.18 F); MediaJSON the plain JSON a
	// client may ask for in its place (PS3.18 8.7.3.2).
	MediaDICOMJSON = "application/dicom+json"
	MediaJSON      = "application/json"
	// MediaDICOM is a Part 10 file (PS3.18 8.7.3.1).
	MediaDICOM = "application/dicom"
	// MediaOctetStream is bytes: a native frame or a bulk value (PS3.18
	// 8.7.3.3.1).
	MediaOctetStream = "application/octet-stream"
	// MediaMultipart is the multipart body of retrieved resources (PS3.18
	// 8.7.3, RFC 2387).
	MediaMultipart = "multipart/related"
)

// The transfer syntaxes the negotiation reasons about (PS3.5 A).
const (
	// AnySyntax asks for each instance or frame as it is stored (PS3.18
	// 8.7.3.5.2).
	AnySyntax = "*"
	// ImplicitLittle is Implicit VR Little Endian.
	ImplicitLittle = "1.2.840.10008.1.2"
	// ExplicitLittle is Explicit VR Little Endian, the default of a request
	// that names no transfer syntax for an instance or a native frame
	// (PS3.18 8.7.3.5.2).
	ExplicitLittle = "1.2.840.10008.1.2.1"
)

// acceptEntry is one media range of an Accept header (RFC 9110 12.5.1).
type acceptEntry struct {
	media  string
	params map[string]string
}

// acceptOf reads the media ranges a request accepts, from the accept query
// parameter where it is given (PS3.18 8.3.3.1) and from the Accept header
// otherwise, in the order given, without those of quality 0. A request
// that names none accepts anything (RFC 9110 12.5.1), which is one entry
// of "*/*".
func acceptOf(r *http.Request) []acceptEntry {
	accept := strings.Join(r.Header.Values("Accept"), ",")
	if p := r.URL.Query().Get(paramAccept); p != "" {
		accept = p
	}
	if strings.TrimSpace(accept) == "" {
		return []acceptEntry{{media: "*/*", params: map[string]string{}}}
	}
	var out []acceptEntry
	for _, part := range splitAccept(accept) {
		media, params, err := mime.ParseMediaType(part)
		if err != nil {
			continue
		}
		if q, err := strconv.ParseFloat(params["q"], 64); err == nil && q == 0 {
			continue
		}
		out = append(out, acceptEntry{media: strings.ToLower(media), params: params})
	}
	return out
}

// splitAccept splits an Accept value at the commas between media ranges,
// leaving those inside a quoted parameter, as a type parameter
// ("type=\"application/dicom\"") may not hold one but a client may quote
// any value.
func splitAccept(s string) []string {
	var out []string
	quoted, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// AcceptJSON returns the media type of a JSON answer (a search, metadata):
// DICOM JSON, or plain JSON when that is all the request takes; 406 when it
// takes neither (PS3.18 8.7.3.2).
func AcceptJSON(r *http.Request) (string, *Error) {
	plain := false
	for _, e := range acceptOf(r) {
		switch e.media {
		case MediaDICOMJSON, "application/*", "*/*":
			return MediaDICOMJSON, nil
		case MediaJSON:
			plain = true
		}
	}
	if plain {
		return MediaJSON, nil
	}
	return "", refuse(http.StatusNotAcceptable, "this resource is answered as "+MediaDICOMJSON)
}

// InstanceWant is what a retrieve of instances accepts (PS3.18 8.7.3.5).
type InstanceWant struct {
	// Any accepts every transfer syntax: each instance as it is stored.
	Any bool
	// Syntaxes are the transfer syntaxes accepted, when not Any.
	Syntaxes []string
	// Bare asks for the file without a multipart body around it, which only
	// a retrieve of one instance may be answered with.
	Bare bool
}

// Accepts reports whether an instance stored in the transfer syntax ts is
// answered: it is sent as it is stored or not at all.
func (w InstanceWant) Accepts(ts string) bool {
	if w.Any {
		return true
	}
	for _, s := range w.Syntaxes {
		if s == ts {
			return true
		}
	}
	return false
}

// AcceptInstances reads what a retrieve of instances accepts: the
// multipart form multipart/related; type="application/dicom" with a
// transfer-syntax, "*" for each as stored, or none for Explicit VR Little
// Endian (PS3.18 8.7.3.5.2); application/dicom alone for one file; and
// "*/*" or "multipart/related" with no type for each as stored. A request
// that takes only application/dicom alone is answered so only when it
// takes no multipart form. Anything else is 406.
func AcceptInstances(r *http.Request) (InstanceWant, *Error) {
	var (
		w        InstanceWant
		multi    bool
		bareOnly = true
	)
	for _, e := range acceptOf(r) {
		ts := e.params["transfer-syntax"]
		switch {
		case e.media == MediaMultipart && strings.EqualFold(e.params["type"], MediaDICOM):
			multi, bareOnly = true, false
			w.add(ts)
		case e.media == MediaDICOM:
			multi = true
			w.add(ts)
		case e.media == "*/*" || e.media == "multipart/*" || (e.media == MediaMultipart && anyOr(e.params["type"], "")):
			multi, bareOnly = true, false
			w.Any = true
		}
	}
	if !multi {
		return w, refuse(http.StatusNotAcceptable, `instances are retrieved as multipart/related; type="`+MediaDICOM+`"`)
	}
	w.Bare = bareOnly
	return w, nil
}

// add accepts the transfer syntax a media range names.
func (w *InstanceWant) add(ts string) {
	switch ts {
	case AnySyntax:
		w.Any = true
	case "":
		w.Syntaxes = append(w.Syntaxes, ExplicitLittle)
	default:
		w.Syntaxes = append(w.Syntaxes, ts)
	}
}

// FrameMedia returns the media type of a frame stored in the transfer
// syntax ts, as PS3.18 8.7.3.3.2 names it (Table 8.7.3-2): the type of its
// compression for an encapsulated syntax, application/octet-stream for a
// native one and for any syntax it does not know.
func FrameMedia(ts string) string {
	switch ts {
	case "1.2.840.10008.1.2.4.50", "1.2.840.10008.1.2.4.51", "1.2.840.10008.1.2.4.57", "1.2.840.10008.1.2.4.70":
		return "image/jpeg"
	case "1.2.840.10008.1.2.4.80", "1.2.840.10008.1.2.4.81":
		return "image/jls"
	case "1.2.840.10008.1.2.4.90", "1.2.840.10008.1.2.4.91":
		return "image/jp2"
	case "1.2.840.10008.1.2.4.92", "1.2.840.10008.1.2.4.93":
		return "image/jpx"
	case "1.2.840.10008.1.2.4.201", "1.2.840.10008.1.2.4.202", "1.2.840.10008.1.2.4.203":
		return "image/jphc"
	case "1.2.840.10008.1.2.5":
		return "image/dicom-rle"
	case "1.2.840.10008.1.2.4.100", "1.2.840.10008.1.2.4.101":
		return "video/mpeg2"
	case "1.2.840.10008.1.2.4.102", "1.2.840.10008.1.2.4.103", "1.2.840.10008.1.2.4.104",
		"1.2.840.10008.1.2.4.105", "1.2.840.10008.1.2.4.106":
		return "video/mp4"
	case "1.2.840.10008.1.2.4.107", "1.2.840.10008.1.2.4.108":
		return "video/H265"
	}
	return MediaOctetStream
}

// nativeLittle reports whether ts is a native little endian syntax, whose
// frames are the same bytes in either: the frames of Explicit VR Little
// Endian, the default a request for application/octet-stream names
// (PS3.18 8.7.3.5.2).
func nativeLittle(ts string) bool { return ts == ImplicitLittle || ts == ExplicitLittle }

// AcceptFrames returns the media type a frame stored in the transfer syntax
// ts is answered as, and the transfer syntax its part names, or 406 when
// the request takes no form the stored bytes are (PS3.18 8.7.3.5): a frame
// is answered as stored, never decoded. A media range takes it when its
// type is the frame's own (FrameMedia), application/octet-stream, or any;
// and its transfer-syntax is "*", the stored one, or absent, which names
// the default of its type: Explicit VR Little Endian for
// application/octet-stream (a native little endian frame is the same
// bytes), the stored syntax for the frame's own type.
func AcceptFrames(r *http.Request, ts string) (media, syntax string, err *Error) {
	own := FrameMedia(ts)
	for _, e := range acceptOf(r) {
		typ, params := e.media, e.params
		if typ == MediaMultipart {
			typ = strings.ToLower(params["type"])
			if typ == "" {
				typ = "*/*"
			}
		}
		want := params["transfer-syntax"]
		switch typ {
		case "*/*", "*":
			if want == "" || want == AnySyntax || want == ts {
				return own, ts, nil
			}
		case MediaOctetStream:
			if want == AnySyntax || want == ts {
				return MediaOctetStream, ts, nil
			}
			if want == "" && nativeLittle(ts) {
				return MediaOctetStream, ExplicitLittle, nil
			}
		case own:
			if want == "" || want == AnySyntax || want == ts {
				return own, ts, nil
			}
		}
	}
	return "", "", refuse(http.StatusNotAcceptable, "frames are answered as stored, transfer syntax "+ts+" as "+own+"; the request accepts no such form")
}

// AcceptBulk reports whether a request for bulk data is answered as a
// multipart body (multipart/related; type="application/octet-stream", the
// default) or as the bytes alone (application/octet-stream), or 406
// (PS3.18 8.7.3.3.1).
func AcceptBulk(r *http.Request) (multipart bool, err *Error) {
	bare := false
	for _, e := range acceptOf(r) {
		switch {
		case e.media == MediaMultipart && anyOr(e.params["type"], MediaOctetStream),
			e.media == "*/*", e.media == "multipart/*":
			return true, nil
		case e.media == MediaOctetStream:
			bare = true
		}
	}
	if bare {
		return false, nil
	}
	return false, refuse(http.StatusNotAcceptable, `bulk data is answered as multipart/related; type="`+MediaOctetStream+`"`)
}

// anyOr reports whether the type parameter of a multipart media range
// takes the media type want: absent, "*/*" or "application/*" (RFC 9110
// 12.5.1), or want itself.
func anyOr(typ, want string) bool {
	typ = strings.ToLower(typ)
	return typ == "" || typ == "*/*" || typ == "application/*" || (want != "" && typ == want)
}
