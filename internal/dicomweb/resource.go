package dicomweb

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/uid"
)

// Kind is what a resource asks for.
type Kind int

// The kinds of resource.
const (
	// Search is a search, QIDO-RS (PS3.18 10.6).
	Search Kind = iota + 1
	// Retrieve is the instances of a study, a series or one instance, as
	// Part 10 files (PS3.18 10.4.1.1.1).
	Retrieve
	// Metadata is their headers as DICOM JSON (PS3.18 10.4.1.1.2).
	Metadata
	// Frames is frames of one instance's pixel data (PS3.18 10.4.1.1.4).
	Frames
	// BulkData is one bulk value of one instance, at a place of its file a
	// BulkDataURI of the metadata names (PS3.18 10.4.1.1.5).
	BulkData
)

// Level is the level of a search, or of what a retrieve names.
type Level int

// The levels (PS3.4 C.6.2.1).
const (
	// Study is a study, which is a session of the catalog.
	Study Level = iota + 1
	// Series is a series, which is a scan.
	Series
	// Instance is an instance.
	Instance
)

// String names the level as an audit entry's details write it.
func (l Level) String() string {
	switch l {
	case Study:
		return "study"
	case Series:
		return "series"
	case Instance:
		return "instance"
	}
	return "unknown"
}

// MaxFrameList is the most frame numbers one request names: more than an
// instance of any modality holds in one request a viewer makes, and few
// enough that the answer's plan is small.
const MaxFrameList = 1024

// Resource is what a request path under a tenant's DICOMweb root names.
type Resource struct {
	// Kind is what it asks for.
	Kind Kind
	// Level is the level searched, or the level of what is retrieved: a
	// study, a series or an instance.
	Level Level
	// Study, Series and Instance are the UIDs the path names, each empty
	// when the path does not name it.
	Study, Series, Instance string
	// FrameNumbers are the frames asked for, counted from 1, in the order
	// asked (Frames only).
	FrameNumbers []int
}

// Error is a request the protocol refuses, with the HTTP status that says
// why and a message for the client, which describes the request and never
// the archive.
type Error struct {
	// Status is the HTTP status of the answer.
	Status int
	// Message is the text of the answer.
	Message string
}

// Error returns the message.
func (e *Error) Error() string { return e.Message }

// refuse makes an Error.
func refuse(status int, message string) *Error {
	return &Error{Status: status, Message: message}
}

// errNoResource is a path that names no resource of the services served.
var errNoResource = refuse(http.StatusNotFound, "there is no such DICOMweb resource")

// ParseResource reads the path under a tenant's DICOMweb root, its
// segments joined by "/" as sent (no leading slash), as one of the
// resources served. A path of no resource is 404; a UID that is not one
// (PS3.5 9.1), and a frame list that is not one, are 400 (PS3.18 8.5).
func ParseResource(rest string) (Resource, *Error) {
	s := strings.Split(rest, "/")
	var r Resource
	n := len(s)
	switch {
	case n == 1 && s[0] == "studies":
		r = Resource{Kind: Search, Level: Study}
	case n == 1 && s[0] == "series":
		r = Resource{Kind: Search, Level: Series}
	case n == 1 && s[0] == "instances":
		r = Resource{Kind: Search, Level: Instance}
	case n >= 2 && s[0] == "studies":
		r.Study = s[1]
		var err *Error
		if r, err = studyResource(r, s[2:]); err != nil {
			return r, err
		}
	default:
		return r, errNoResource
	}
	for _, u := range []string{r.Study, r.Series, r.Instance} {
		if u != "" && !uid.Valid(u) {
			return r, refuse(http.StatusBadRequest, "a UID of the path is not a UID (PS3.5 9.1)")
		}
	}
	return r, nil
}

// studyResource reads the segments after /studies/{study}.
func studyResource(r Resource, s []string) (Resource, *Error) {
	switch {
	case len(s) == 0:
		r.Kind, r.Level = Retrieve, Study
	case len(s) == 1 && s[0] == "metadata":
		r.Kind, r.Level = Metadata, Study
	case len(s) == 1 && s[0] == "series":
		r.Kind, r.Level = Search, Series
	case len(s) == 1 && s[0] == "instances":
		r.Kind, r.Level = Search, Instance
	case len(s) >= 2 && s[0] == "series":
		r.Series = s[1]
		return seriesResource(r, s[2:])
	default:
		return r, errNoResource
	}
	return r, nil
}

// seriesResource reads the segments after /series/{series}.
func seriesResource(r Resource, s []string) (Resource, *Error) {
	switch {
	case len(s) == 0:
		r.Kind, r.Level = Retrieve, Series
	case len(s) == 1 && s[0] == "metadata":
		r.Kind, r.Level = Metadata, Series
	case len(s) == 1 && s[0] == "instances":
		r.Kind, r.Level = Search, Instance
	case len(s) >= 2 && s[0] == "instances":
		r.Instance, r.Level = s[1], Instance
		return instanceResource(r, s[2:])
	default:
		return r, errNoResource
	}
	return r, nil
}

// instanceResource reads the segments after /instances/{instance}.
func instanceResource(r Resource, s []string) (Resource, *Error) {
	switch {
	case len(s) == 0:
		r.Kind = Retrieve
	case len(s) == 1 && s[0] == "metadata":
		r.Kind = Metadata
	case len(s) == 1 && s[0] == "bulkdata":
		r.Kind = BulkData
	case len(s) == 2 && s[0] == "frames":
		frames, err := frameList(s[1])
		if err != nil {
			return r, err
		}
		r.Kind, r.FrameNumbers = Frames, frames
	default:
		return r, errNoResource
	}
	return r, nil
}

// frameList reads a list of frame numbers: positive integers separated by
// commas (PS3.18 10.4.1.1.4), at most MaxFrameList, each once. A frame
// named twice is refused, so a request cannot have the same bytes read
// and sent again and again.
func frameList(s string) ([]int, *Error) {
	parts := strings.Split(s, ",")
	if len(parts) > MaxFrameList {
		return nil, refuse(http.StatusBadRequest, "a request names at most "+strconv.Itoa(MaxFrameList)+" frames")
	}
	out := make([]int, 0, len(parts))
	seen := make(map[int]bool, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || p[0] == '+' {
			return nil, refuse(http.StatusBadRequest, "a frame list is frame numbers from 1, separated by commas")
		}
		if seen[n] {
			return nil, refuse(http.StatusBadRequest, "a frame list names each frame once; frame "+strconv.Itoa(n)+" is named twice")
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// ShapeOf returns the shape of a path under the DICOMweb root, as an audit
// entry records it: its literal segments, with every UID and the frame
// list replaced by the name of what it is, so no UID reaches a log
// (data.proto, "Logs"). A path of no resource is "unknown". It reads the
// path as sent, before any check.
func ShapeOf(rest string) string {
	s := strings.Split(rest, "/")
	if len(s) > 8 {
		return "unknown"
	}
	out := make([]string, len(s))
	for i, seg := range s {
		if i%2 == 0 {
			if !shapeLiterals[seg] {
				return "unknown"
			}
			out[i] = seg
			continue
		}
		name, ok := shapeNames[s[i-1]]
		if !ok {
			return "unknown"
		}
		out[i] = name
	}
	return strings.Join(out, "/")
}

// shapeLiterals are the literal segments of a resource's path.
var shapeLiterals = map[string]bool{"studies": true, "series": true, "instances": true, "metadata": true, "frames": true, "bulkdata": true}

// shapeNames name what the segment after each literal holds.
var shapeNames = map[string]string{"studies": "{study}", "series": "{series}", "instances": "{instance}", "frames": "{frames}"}
