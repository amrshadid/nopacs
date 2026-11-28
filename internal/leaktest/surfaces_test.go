package leaktest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	_ "github.com/amrshadid/nopacs/gen/nopacs/v1" // registers the API's messages and services.
)

// surface is text one place showed one caller: where it came from and who
// saw it.
type surface struct {
	// label names the place: a method, a route, a command, the log.
	label string
	// who names the key (or process) that saw it.
	who string
	// text is everything shown.
	text string
}

// recorder keeps every surface.
type recorder struct {
	mu    sync.Mutex
	items []surface
}

// add records text shown at label to who.
func (r *recorder) add(label, who, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, surface{label: label, who: who, text: text})
}

// method is an API method of the contract the test was built with.
type method struct {
	name, procedure, input string
	streams                bool
}

// contract lists the methods of the API as the generated code registers
// them, by name.
func contract() map[string]method {
	out := map[string]method{}
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Package() != "nopacs.v1" {
			return true
		}
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				out[string(md.Name())] = method{
					name: string(md.Name()), procedure: "/" + string(sd.FullName()) + "/" + string(md.Name()),
					input: string(md.Input().FullName()), streams: md.IsStreamingServer(),
				}
			}
		}
		return true
	})
	return out
}

// requestParses reports whether body is a request of m: the contract's own
// message reads it strictly, with no field unknown to it.
func requestParses(m method, body string) error {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(m.input))
	if err != nil {
		return err
	}
	msg := dynamicpb.NewMessage(mt.Descriptor())
	return protojson.Unmarshal([]byte(body), msg)
}

// api is a caller of the API over the network, with a key or without,
// that records every answer.
type api struct {
	base, key, who string
	hc             *http.Client
	rec            *recorder
}

// newAPI returns a caller of the server at addr.
func newAPI(addr, key, who string, rec *recorder) *api {
	return &api{base: "http://" + addr, key: key, who: who, rec: rec, hc: &http.Client{Timeout: time.Minute, Transport: &http.Transport{DisableCompression: true}}}
}

// longBase64 matches a JSON string of base64 too long to be a name or a
// message: the bytes of an archived file.
var longBase64 = regexp.MustCompile(`"[A-Za-z0-9+/=]{200,}"`)

// show writes what an answer showed: the status, every header, the body,
// and for an error each detail it carries, decoded.
func show(status int, header http.Header, body []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status %d\n", status)
	keys := make([]string, 0, len(header))
	for k := range header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(header[k], ", "))
	}
	b.Write(body)
	b.WriteString(details(body))
	return b.String()
}

// details decodes the details of a Connect error: each detail's value is a
// protobuf message in base64, whose strings are plain in its bytes.
func details(body []byte) string {
	var e struct {
		Details []struct {
			Value string `json:"value"`
		} `json:"details"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	var b strings.Builder
	for _, d := range e.Details {
		raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(d.Value, "="))
		if err == nil {
			b.WriteString("\ndetail: ")
			b.Write(raw)
		}
	}
	return b.String()
}

// do sends one request and records what came back at label.
func (a *api) do(label string, req *http.Request, archived bool) (int, []byte) {
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connect-Accept-Encoding", "identity")
	req.Header.Set("Connect-Protocol-Version", "1")
	if a.key != "" {
		req.Header.Set("Authorization", "Bearer "+a.key)
	}
	res, err := a.hc.Do(req)
	if err != nil {
		a.rec.add(label, a.who, "transport error: "+err.Error())
		return 0, nil
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	text := body
	if archived && res.StatusCode == http.StatusOK {
		text = longBase64.ReplaceAll(body, []byte(`"<bytes>"`))
	}
	a.rec.add(label, a.who, show(res.StatusCode, res.Header, text)+show(0, res.Trailer, nil))
	return res.StatusCode, body
}

// unary calls a method that answers once.
func (a *api) unary(ctx context.Context, label string, m method, body string, archived bool) int {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base+m.procedure, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	status, _ := a.do(label, req, archived)
	return status
}

// stream calls a method that answers with a stream and records every
// message of it, until the stream ends or the context does.
func (a *api) stream(ctx context.Context, label string, m method, body string, archived bool) {
	frame := make([]byte, 5, 5+len(body))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(body))) //nolint:gosec // G115: a short request.
	frame = append(frame, body...)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.base+m.procedure, bytes.NewReader(frame))
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connect-Accept-Encoding", "identity")
	req.Header.Set("Connect-Protocol-Version", "1")
	if a.key != "" {
		req.Header.Set("Authorization", "Bearer "+a.key)
	}
	res, err := a.hc.Do(req)
	if err != nil {
		a.rec.add(label, a.who, "transport error: "+err.Error())
		return
	}
	defer func() { _ = res.Body.Close() }()
	var b strings.Builder
	b.WriteString(show(res.StatusCode, res.Header, nil))
	rd := bufio.NewReader(res.Body)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/connect+") {
		rest, _ := io.ReadAll(rd)
		b.Write(rest)
		b.WriteString(details(rest))
		a.rec.add(label, a.who, b.String())
		return
	}
	for {
		var head [5]byte
		if _, err := io.ReadFull(rd, head[:]); err != nil {
			break
		}
		msg := make([]byte, binary.BigEndian.Uint32(head[1:]))
		if _, err := io.ReadFull(rd, msg); err != nil {
			break
		}
		if archived && head[0]&2 == 0 {
			msg = longBase64.ReplaceAll(msg, []byte(`"<bytes>"`))
		}
		b.Write(msg)
		b.WriteString("\n")
		if head[0]&2 != 0 {
			b.WriteString(details(msg))
		}
	}
	a.rec.add(label, a.who, b.String())
}

// call makes c, whatever its method answers with.
func (a *api) call(m method, c apiCall) {
	label := "api/" + c.label()
	if m.streams {
		// A feed has no end: a few seconds see what it holds now.
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		a.stream(ctx, label, m, c.body, c.archived)
		return
	}
	a.unary(context.Background(), label, m, c.body, c.archived)
}

// get fetches a path with a GET and records the answer at label. The body
// of a successful answer that is a DICOM file is the archived form itself
// and is left out.
func (a *api) get(label, path string) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, a.base+path, nil)
	req.Header.Set("Accept-Encoding", "identity")
	if a.key != "" {
		req.Header.Set("Authorization", "Bearer "+a.key)
	}
	res, err := a.hc.Do(req)
	if err != nil {
		a.rec.add(label, a.who, "transport error: "+err.Error())
		return
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode == http.StatusOK && strings.Contains(res.Header.Get("Content-Type"), "dicom") && !strings.Contains(res.Header.Get("Content-Type"), "json") {
		body = []byte("<an archived file>")
	}
	a.rec.add(label, a.who, show(res.StatusCode, res.Header, body))
}
