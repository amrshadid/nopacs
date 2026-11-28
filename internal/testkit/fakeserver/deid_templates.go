package fakeserver

import (
	"context"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// templateKey is what makes two pixel templates one in the fake, as the
// server folds it: manufacturer and model in lower case, the size and the
// SOP class.
func templateKey(tenantName, manufacturer, model string, rows, columns int32, class string) string {
	return key(tenantName, strings.Join([]string{strings.ToLower(strings.TrimSpace(manufacturer)), strings.ToLower(strings.TrimSpace(model)),
		strconv.Itoa(int(rows)), strconv.Itoa(int(columns)), class}, "|"))
}

// AddPixelTemplates stores the templates, all or none: one the tenant holds
// already, or given twice, refuses the whole request as the server does.
func (h deidHandler) AddPixelTemplates(_ context.Context, req *v1.AddPixelTemplatesRequest) (*v1.AddPixelTemplatesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.deid.addTemplates = append(s.deid.addTemplates, req)
	if len(req.GetTemplates()) == 0 {
		return nil, fault.New(fault.Invalid, "templates: holds 1 to 1000 templates")
	}
	if s.deid.templates == nil {
		s.deid.templates = map[string]*v1.PixelTemplate{}
	}
	added := map[string]*v1.PixelTemplate{}
	for _, t := range req.GetTemplates() {
		k := templateKey(req.GetTenant(), t.GetManufacturer(), t.GetModel(), t.GetRows(), t.GetColumns(), t.GetSopClass())
		if _, dup := s.deid.templates[k]; dup {
			return nil, fault.New(fault.Precondition, "templates: a template is already held for this device, size and SOP class")
		}
		if _, dup := added[k]; dup {
			return nil, fault.New(fault.Invalid, "templates: a device, size and SOP class is given twice")
		}
		added[k] = t
	}
	for k, t := range added {
		s.deid.templates[k] = t
	}
	return &v1.AddPixelTemplatesResponse{Added: int32(len(added))}, nil //nolint:gosec // G115: a test's few templates.
}

// RemovePixelTemplate removes the template of the key, or answers not found.
func (h deidHandler) RemovePixelTemplate(_ context.Context, req *v1.RemovePixelTemplateRequest) (*v1.RemovePixelTemplateResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	k := templateKey(req.GetTenant(), req.GetManufacturer(), req.GetModel(), req.GetRows(), req.GetColumns(), req.GetSopClass())
	if _, ok := s.deid.templates[k]; !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no such pixel template")
	}
	delete(s.deid.templates, k)
	return &v1.RemovePixelTemplateResponse{}, nil
}

// ListPixelTemplates answers the tenant's templates in order of key, in one
// page.
func (h deidHandler) ListPixelTemplates(_ context.Context, req *v1.ListPixelTemplatesRequest) (*v1.ListPixelTemplatesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	prefix := key(req.GetTenant(), "")
	var keys []string
	for k := range s.deid.templates {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := &v1.ListPixelTemplatesResponse{}
	for _, k := range keys {
		out.Templates = append(out.Templates, s.deid.templates[k])
	}
	return out, nil
}

// PixelTemplateRequests returns the AddPixelTemplates requests received so
// far.
func (s *Server) PixelTemplateRequests() []*v1.AddPixelTemplatesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*v1.AddPixelTemplatesRequest(nil), s.deid.addTemplates...)
}
