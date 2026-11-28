package fakeserver

import (
	"context"
	"sort"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// deid is the fake's DeidService: the answer a test sets for a forward
// lookup, the requests it received, and each tenant's lookup tables as
// value to code. The fake holds the values as they are, in memory, for a
// test's synthetic data only; the server keeps a keyed hash of each.
type deid struct {
	code    *v1.CodeResponse
	codes   []*v1.CodeRequest
	imports []*v1.ImportLookupCodesRequest
	tables  map[string]map[string]string
	// coverage is each tenant's coverage report (deid_coverage.go).
	coverage map[string]*coverage
	// templates are the pixel templates by key, addTemplates the requests that added them.
	templates    map[string]*v1.PixelTemplate
	addTemplates []*v1.AddPixelTemplatesRequest
}

// deidHandler is the fake's DeidService.
type deidHandler struct{ s *Server }

var _ nopacsv1connect.DeidServiceHandler = deidHandler{}

// registerDeid adds DeidService to a Connect server.
func (s *Server) registerDeid(srv *connect.Server) {
	nopacsv1connect.RegisterDeidServiceHandler(srv, deidHandler{s})
}

// SetCodeAnswer makes Code answer res, whatever it is asked.
func (s *Server) SetCodeAnswer(res *v1.CodeResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deid.code = res
}

// CodeRequests returns the Code requests received so far.
func (s *Server) CodeRequests() []*v1.CodeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*v1.CodeRequest(nil), s.deid.codes...)
}

// ImportRequests returns the ImportLookupCodes requests received so far.
func (s *Server) ImportRequests() []*v1.ImportLookupCodesRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*v1.ImportLookupCodesRequest(nil), s.deid.imports...)
}

// Code records the request and answers what the test set.
func (h deidHandler) Code(_ context.Context, req *v1.CodeRequest) (*v1.CodeResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.deid.codes = append(s.deid.codes, req)
	if s.deid.code == nil {
		return nil, fault.New(fault.Unavailable, "the fake has no code answer set")
	}
	return s.deid.code, nil
}

// ImportLookupCodes stores the entries, all or none: a value coded
// otherwise, or a code given to another value, refuses the whole import
// as the server does.
func (h deidHandler) ImportLookupCodes(_ context.Context, req *v1.ImportLookupCodesRequest) (*v1.ImportLookupCodesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.deid.imports = append(s.deid.imports, req)
	if len(req.GetEntries()) == 0 {
		return nil, fault.New(fault.Invalid, "entries: holds 1 to 10000 entries")
	}
	if s.deid.tables == nil {
		s.deid.tables = map[string]map[string]string{}
	}
	k := key(req.GetTenant(), req.GetTable())
	table := map[string]string{}
	for v, c := range s.deid.tables[k] {
		table[v] = c
	}
	out := &v1.ImportLookupCodesResponse{Table: req.GetTable()}
	for _, e := range req.GetEntries() {
		if c, ok := table[e.GetValue()]; ok {
			if c != e.GetCode() {
				return nil, fault.New(fault.Precondition, "entries: the table already gives a value another code")
			}
			out.Unchanged++
			continue
		}
		for _, c := range table {
			if c == e.GetCode() {
				return nil, fault.New(fault.Precondition, "entries: the table already gives a code to another value")
			}
		}
		table[e.GetValue()] = e.GetCode()
		out.Added++
	}
	s.deid.tables[k] = table
	out.Codes = int64(len(table))
	return out, nil
}

// ListLookupTables answers the tenant's tables by name with their sizes,
// in one page.
func (h deidHandler) ListLookupTables(_ context.Context, req *v1.ListLookupTablesRequest) (*v1.ListLookupTablesResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := &v1.ListLookupTablesResponse{}
	prefix := key(req.GetTenant(), "")
	for k, t := range s.deid.tables {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out.Tables = append(out.Tables, &v1.LookupTable{Name: k[len(prefix):], Codes: int64(len(t))})
		}
	}
	sort.Slice(out.Tables, func(i, j int) bool { return out.Tables[i].GetName() < out.Tables[j].GetName() })
	return out, nil
}
