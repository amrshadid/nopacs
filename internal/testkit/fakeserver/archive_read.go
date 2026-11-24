package fakeserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Instance is one archived file the fake serves: its bytes, and the header it
// answers for the metadata service.
type Instance struct {
	// ID is the instance id; the SOP Instance UID is the same unless set.
	ID string
	// SOPInstanceUID is the file's SOP Instance UID; ID when empty.
	SOPInstanceUID string
	// Data is the file's bytes.
	Data []byte
	// JSON is the instance's DICOM JSON.
	JSON string
	// TooLarge makes the metadata answer leave the JSON out as too large.
	TooLarge bool
	// BulkOmitted marks the JSON as missing its bulk data.
	BulkOmitted bool
	// AnnounceSHA256, when set, is the checksum announced for the file in
	// place of the real one, as a manifest that no longer matches its bytes.
	AnnounceSHA256 string
	// AnnounceLength, when set, is the length announced for the file in
	// place of the real one, as a server that sends more than it says.
	AnnounceLength int64
	// TransferSyntax is the Transfer Syntax UID the metadata answers.
	TransferSyntax string
}

// sha is the instance's SHA-256 in lowercase hexadecimal, or the one it is
// set to announce.
func (i *Instance) sha() string {
	if i.AnnounceSHA256 != "" {
		return i.AnnounceSHA256
	}
	sum := sha256.Sum256(i.Data)
	return hex.EncodeToString(sum[:])
}

// length is the instance's length in bytes, or the one it is set to
// announce.
func (i *Instance) length() int64 {
	if i.AnnounceLength != 0 {
		return i.AnnounceLength
	}
	return int64(len(i.Data))
}

// readArchive is what the fake keeps for the catalog, metadata and data
// services of the tenants. Its zero value is empty and ready.
type readArchive struct {
	mu       sync.Mutex
	subjects map[string][]*v1.Subject
	sessions map[string][]*v1.Session
	scans    map[string][]*v1.Scan
	insts    map[string][]*Instance
	chunk    int
	dropAt   int
	dropped  bool
	reads    []*v1.ReadFilesRequest
}

// ensure makes the tables; the caller holds the lock.
func (r *readArchive) ensure() {
	if r.subjects == nil {
		r.subjects, r.sessions, r.scans, r.insts = map[string][]*v1.Subject{}, map[string][]*v1.Session{}, map[string][]*v1.Scan{}, map[string][]*Instance{}
	}
}

// AddSubject puts a subject in a tenant's catalog.
func (s *Server) AddSubject(tenant string, sub *v1.Subject) {
	s.read.mu.Lock()
	defer s.read.mu.Unlock()
	s.read.ensure()
	s.read.subjects[tenant] = append(s.read.subjects[tenant], sub)
}

// AddSession puts a session in a tenant's catalog.
func (s *Server) AddSession(tenant string, sess *v1.Session) {
	s.read.mu.Lock()
	defer s.read.mu.Unlock()
	s.read.ensure()
	s.read.sessions[tenant] = append(s.read.sessions[tenant], sess)
}

// AddScan puts a scan in a tenant's catalog, with the instances it holds.
func (s *Server) AddScan(tenant string, scan *v1.Scan, instances ...*Instance) {
	s.read.mu.Lock()
	defer s.read.mu.Unlock()
	s.read.ensure()
	s.read.scans[tenant] = append(s.read.scans[tenant], scan)
	path := tenant + "/" + scan.GetSubjectLabel() + "/" + scan.GetSessionLabel() + "/" + scan.GetLabel()
	s.read.insts[path] = append(s.read.insts[path], instances...)
}

// SetChunkSize makes ReadFiles send chunks of at most n bytes.
func (s *Server) SetChunkSize(n int) {
	s.read.mu.Lock()
	defer s.read.mu.Unlock()
	s.read.chunk = n
}

// DropReadFilesAfter makes the next ReadFiles that carries bytes end with
// UNAVAILABLE after n instances, once, as a server that stopped would.
func (s *Server) DropReadFilesAfter(n int) {
	s.read.mu.Lock()
	defer s.read.mu.Unlock()
	s.read.dropAt, s.read.dropped = n, false
}

// ReadFilesRequests returns every ReadFiles request the fake has answered, in
// order, so a test can see where a resumed stream started.
func (s *Server) ReadFilesRequests() []*v1.ReadFilesRequest {
	s.read.mu.Lock()
	defer s.read.mu.Unlock()
	return append([]*v1.ReadFilesRequest(nil), s.read.reads...)
}

// registerRead adds the catalog, metadata and data services.
func (s *Server) registerRead(srv *connect.Server) {
	nopacsv1connect.RegisterCatalogServiceHandler(srv, catalogHandler{s})
	nopacsv1connect.RegisterMetadataServiceHandler(srv, metadataHandler{s})
	nopacsv1connect.RegisterDataServiceHandler(srv, dataHandler{s})
}

// check takes the tenant check every method starts with.
func (s *Server) check(tenant string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !tenantName.MatchString(tenant) {
		return fault.New(fault.Invalid, "tenant: is 1 to 63 of a-z, 0-9 and -, starting with a letter")
	}
	return s.checkTenant(tenant)
}

// fold is the label comparison form: ASCII letters in lower case.
func fold(s string) string { return strings.ToLower(s) }

// instancesOf returns the instances at a path ("subject/session/scan"),
// sorted by id.
func (s *Server) instancesOf(tenant, path string) []*Instance {
	s.read.mu.Lock()
	defer s.read.mu.Unlock()
	out := append([]*Instance(nil), s.read.insts[tenant+"/"+path]...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// catalogHandler implements CatalogService.
type catalogHandler struct{ s *Server }

// ListSubjects answers a page of subjects, with the contract's filters, and a
// total only when no filter narrows the list, as the real server knows it.
func (h catalogHandler) ListSubjects(_ context.Context, req *v1.ListSubjectsRequest) (*v1.ListSubjectsResponse, error) {
	if err := h.s.check(req.GetTenant()); err != nil {
		return nil, err
	}
	if err := checkSubjectFilter(req); err != nil {
		return nil, err
	}
	h.s.read.mu.Lock()
	var all []*v1.Subject
	for _, x := range h.s.read.subjects[req.GetTenant()] {
		switch {
		case req.GetLabelPrefix() != "" && !strings.HasPrefix(fold(x.GetLabel()), fold(req.GetLabelPrefix())),
			req.GetStartTime() != nil && x.GetArchiveTime().AsTime().Before(req.GetStartTime().AsTime()),
			req.GetEndTime() != nil && !x.GetArchiveTime().AsTime().Before(req.GetEndTime().AsTime()):
			continue
		}
		all = append(all, x)
	}
	filtered := req.GetLabelPrefix() != "" || req.GetStartTime() != nil || req.GetEndTime() != nil
	h.s.read.mu.Unlock()
	sort.SliceStable(all, func(i, j int) bool {
		if req.GetOrder() == v1.CatalogOrder_CATALOG_ORDER_LABEL {
			return fold(all[i].GetLabel()) < fold(all[j].GetLabel())
		}
		return all[i].GetArchiveTime().AsTime().After(all[j].GetArchiveTime().AsTime())
	})
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &v1.ListSubjectsResponse{Subjects: all[from:to], NextPageToken: next}
	if !filtered {
		n := int64(len(all))
		res.TotalCount = &n
	}
	return res, nil
}

// GetSubject answers one subject.
func (h catalogHandler) GetSubject(_ context.Context, req *v1.GetSubjectRequest) (*v1.GetSubjectResponse, error) {
	if err := h.s.check(req.GetTenant()); err != nil {
		return nil, err
	}
	h.s.read.mu.Lock()
	defer h.s.read.mu.Unlock()
	for _, x := range h.s.read.subjects[req.GetTenant()] {
		if x.GetLabel() == req.GetSubjectLabel() {
			return &v1.GetSubjectResponse{Subject: x}, nil
		}
	}
	return nil, fault.New(fault.NotFound, "the tenant has no such subject")
}

// ListSessions answers a page of sessions.
func (h catalogHandler) ListSessions(_ context.Context, req *v1.ListSessionsRequest) (*v1.ListSessionsResponse, error) {
	if err := h.s.check(req.GetTenant()); err != nil {
		return nil, err
	}
	if err := checkSessionFilter(req); err != nil {
		return nil, err
	}
	h.s.read.mu.Lock()
	var all []*v1.Session
	for _, x := range h.s.read.sessions[req.GetTenant()] {
		mod := req.GetModality() == ""
		for _, m := range x.GetModalities() {
			mod = mod || m == req.GetModality()
		}
		switch {
		case req.GetSubjectLabel() != "" && x.GetSubjectLabel() != req.GetSubjectLabel(),
			req.GetLabelPrefix() != "" && !strings.HasPrefix(fold(x.GetLabel()), fold(req.GetLabelPrefix())),
			!mod,
			req.GetReceiver() != "" && x.GetReceiver() != req.GetReceiver(),
			req.GetStudyDateStart() != "" && x.GetStudyDate() < req.GetStudyDateStart(),
			req.GetStudyDateEnd() != "" && x.GetStudyDate() > req.GetStudyDateEnd(),
			req.GetStartTime() != nil && x.GetArchiveTime().AsTime().Before(req.GetStartTime().AsTime()),
			req.GetEndTime() != nil && !x.GetArchiveTime().AsTime().Before(req.GetEndTime().AsTime()):
			continue
		}
		all = append(all, x)
	}
	h.s.read.mu.Unlock()
	sort.SliceStable(all, func(i, j int) bool {
		switch req.GetOrder() {
		case v1.CatalogOrder_CATALOG_ORDER_LABEL:
			return fold(all[i].GetLabel()) < fold(all[j].GetLabel())
		case v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE:
			return all[i].GetStudyDate() > all[j].GetStudyDate()
		}
		return all[i].GetArchiveTime().AsTime().After(all[j].GetArchiveTime().AsTime())
	})
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListSessionsResponse{Sessions: all[from:to], NextPageToken: next}, nil
}

// GetSession answers one session.
func (h catalogHandler) GetSession(_ context.Context, req *v1.GetSessionRequest) (*v1.GetSessionResponse, error) {
	if err := h.s.check(req.GetTenant()); err != nil {
		return nil, err
	}
	h.s.read.mu.Lock()
	defer h.s.read.mu.Unlock()
	for _, x := range h.s.read.sessions[req.GetTenant()] {
		if x.GetSubjectLabel() == req.GetSubjectLabel() && x.GetLabel() == req.GetSessionLabel() {
			return &v1.GetSessionResponse{Session: x}, nil
		}
	}
	return nil, fault.New(fault.NotFound, "the tenant has no such session")
}

// ListScans answers a page of scans.
func (h catalogHandler) ListScans(_ context.Context, req *v1.ListScansRequest) (*v1.ListScansResponse, error) {
	if err := h.s.check(req.GetTenant()); err != nil {
		return nil, err
	}
	if err := checkScanFilter(req); err != nil {
		return nil, err
	}
	h.s.read.mu.Lock()
	var all []*v1.Scan
	for _, x := range h.s.read.scans[req.GetTenant()] {
		switch {
		case req.GetSubjectLabel() != "" && x.GetSubjectLabel() != req.GetSubjectLabel(),
			req.GetSessionLabel() != "" && x.GetSessionLabel() != req.GetSessionLabel(),
			req.GetLabelPrefix() != "" && !strings.HasPrefix(fold(x.GetLabel()), fold(req.GetLabelPrefix())),
			req.GetModality() != "" && x.GetModality() != req.GetModality(),
			req.GetSeriesDescriptionPrefix() != "" && !strings.HasPrefix(fold(x.GetSeriesDescription()), fold(req.GetSeriesDescriptionPrefix())),
			req.GetReceiver() != "" && x.GetReceiver() != req.GetReceiver(),
			req.GetStartTime() != nil && x.GetArchiveTime().AsTime().Before(req.GetStartTime().AsTime()),
			req.GetEndTime() != nil && !x.GetArchiveTime().AsTime().Before(req.GetEndTime().AsTime()):
			continue
		}
		all = append(all, x)
	}
	h.s.read.mu.Unlock()
	sort.SliceStable(all, func(i, j int) bool {
		switch req.GetOrder() {
		case v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION:
			return fold(all[i].GetSeriesDescription()) < fold(all[j].GetSeriesDescription())
		case v1.CatalogOrder_CATALOG_ORDER_LABEL:
			a, b := all[i], all[j]
			ka := fold(a.GetSubjectLabel()) + "/" + fold(a.GetSessionLabel()) + "/" + fold(a.GetLabel())
			kb := fold(b.GetSubjectLabel()) + "/" + fold(b.GetSessionLabel()) + "/" + fold(b.GetLabel())
			return ka < kb
		}
		return all[i].GetArchiveTime().AsTime().After(all[j].GetArchiveTime().AsTime())
	})
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListScansResponse{Scans: all[from:to], NextPageToken: next}, nil
}

// GetScan answers one scan.
func (h catalogHandler) GetScan(_ context.Context, req *v1.GetScanRequest) (*v1.GetScanResponse, error) {
	if err := h.s.check(req.GetTenant()); err != nil {
		return nil, err
	}
	h.s.read.mu.Lock()
	defer h.s.read.mu.Unlock()
	for _, x := range h.s.read.scans[req.GetTenant()] {
		if x.GetSubjectLabel() == req.GetSubjectLabel() && x.GetSessionLabel() == req.GetSessionLabel() && x.GetLabel() == req.GetScanLabel() {
			return &v1.GetScanResponse{Scan: x}, nil
		}
	}
	return nil, fault.New(fault.NotFound, "the tenant has no such scan")
}

// metadataHandler implements MetadataService.
type metadataHandler struct{ s *Server }

// GetScanMetadata answers a page of a scan's headers, by instance id.
func (h metadataHandler) GetScanMetadata(_ context.Context, req *v1.GetScanMetadataRequest) (*v1.GetScanMetadataResponse, error) {
	if err := h.s.check(req.GetTenant()); err != nil {
		return nil, err
	}
	path := req.GetSubjectLabel() + "/" + req.GetSessionLabel() + "/" + req.GetScanLabel()
	all := h.s.instancesOf(req.GetTenant(), path)
	if len(all) == 0 {
		return nil, fault.New(fault.NotFound, "the tenant has no such scan")
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	res := &v1.GetScanMetadataResponse{NextPageToken: next, ManifestSha256: strings.Repeat("cd", 32)}
	for _, in := range all[from:to] {
		m := &v1.InstanceMetadata{InstanceId: in.ID, SopInstanceUid: in.sop(), DicomJson: in.JSON, JsonTooLarge: in.TooLarge, TransferSyntaxUid: in.TransferSyntax, BulkDataOmitted: in.BulkOmitted}
		if in.TooLarge {
			m.DicomJson = ""
		}
		res.Instances = append(res.Instances, m)
	}
	return res, nil
}

// GetInstanceMetadata answers one instance's header.
func (h metadataHandler) GetInstanceMetadata(_ context.Context, req *v1.GetInstanceMetadataRequest) (*v1.GetInstanceMetadataResponse, error) {
	if err := h.s.check(req.GetTenant()); err != nil {
		return nil, err
	}
	for _, in := range h.s.instancesOf(req.GetTenant(), req.GetSubjectLabel()+"/"+req.GetSessionLabel()+"/"+req.GetScanLabel()) {
		if in.ID == req.GetInstanceId() {
			return &v1.GetInstanceMetadataResponse{Instance: &v1.InstanceMetadata{InstanceId: in.ID, SopInstanceUid: in.sop(), DicomJson: in.JSON, TransferSyntaxUid: in.TransferSyntax}}, nil
		}
	}
	return nil, fault.New(fault.NotFound, "the scan has no such instance")
}

// sop is the instance's SOP Instance UID.
func (i *Instance) sop() string {
	if i.SOPInstanceUID != "" {
		return i.SOPInstanceUID
	}
	return i.ID
}

// dataHandler implements DataService.
type dataHandler struct{ s *Server }

// ReadFiles streams the instances of a subject, session or scan in the
// contract's order, each announced before its bytes, after start_after.
func (h dataHandler) ReadFiles(ctx context.Context, req *v1.ReadFilesRequest, stream nopacsv1connect.DataServiceReadFilesServerStream) error {
	s := h.s
	if err := s.check(req.GetTenant()); err != nil {
		return err
	}
	s.read.mu.Lock()
	s.read.reads = append(s.read.reads, req)
	chunk := s.read.chunk
	drop := s.read.dropAt > 0 && !s.read.dropped && !req.GetAnnounceOnly()
	dropAt := s.read.dropAt
	s.read.mu.Unlock()
	if chunk <= 0 {
		chunk = 1 << 20
	}
	var scans []*v1.Scan
	s.read.mu.Lock()
	for _, x := range s.read.scans[req.GetTenant()] {
		switch {
		case x.GetSubjectLabel() != req.GetSubjectLabel(),
			req.GetSessionLabel() != "" && x.GetSessionLabel() != req.GetSessionLabel(),
			req.GetScanLabel() != "" && x.GetLabel() != req.GetScanLabel():
			continue
		}
		scans = append(scans, x)
	}
	s.read.mu.Unlock()
	if len(scans) == 0 {
		return fault.New(fault.NotFound, "the tenant has no such subject, session or scan")
	}
	sort.Slice(scans, func(i, j int) bool {
		if scans[i].GetSessionLabel() != scans[j].GetSessionLabel() {
			return fold(scans[i].GetSessionLabel()) < fold(scans[j].GetSessionLabel())
		}
		return fold(scans[i].GetLabel()) < fold(scans[j].GetLabel())
	})
	after := req.GetStartAfter()
	sent := 0
	for _, sc := range scans {
		path := sc.GetSubjectLabel() + "/" + sc.GetSessionLabel() + "/" + sc.GetLabel()
		var todo []*Instance
		for _, in := range s.instancesOf(req.GetTenant(), path) {
			switch {
			case req.GetInstanceId() != "" && in.ID != req.GetInstanceId():
				continue
			case after != nil && !positionAfter(after, sc, in):
				continue
			}
			todo = append(todo, in)
		}
		if len(todo) == 0 {
			continue
		}
		if err := stream.Send(&v1.ReadFilesResponse{Part: &v1.ReadFilesResponse_Scan{Scan: &v1.ScanMarker{
			SubjectLabel: sc.GetSubjectLabel(), SessionLabel: sc.GetSessionLabel(), ScanLabel: sc.GetLabel(), ManifestSha256: sc.GetManifestSha256(), InstanceCount: int64(len(s.instancesOf(req.GetTenant(), path))),
		}}}); err != nil {
			return err
		}
		for _, in := range todo {
			if drop && sent == dropAt {
				s.read.mu.Lock()
				s.read.dropped = true
				s.read.mu.Unlock()
				return fault.New(fault.Unavailable, "the storage stopped answering")
			}
			if err := stream.Send(&v1.ReadFilesResponse{Part: &v1.ReadFilesResponse_File{File: &v1.ArchivedFile{
				InstanceId: in.ID, SopInstanceUid: in.sop(), SubjectLabel: sc.GetSubjectLabel(), SessionLabel: sc.GetSessionLabel(), ScanLabel: sc.GetLabel(),
				Key: "data/" + path + "/" + in.ID + ".dcm", Length: in.length(), Sha256: in.sha(),
			}}}); err != nil {
				return err
			}
			sent++
			if req.GetAnnounceOnly() {
				continue
			}
			for off := 0; off < len(in.Data); off += chunk {
				if err := ctx.Err(); err != nil {
					return err
				}
				end := min(off+chunk, len(in.Data))
				if err := stream.Send(&v1.ReadFilesResponse{Part: &v1.ReadFilesResponse_Chunk{Chunk: &v1.FileChunk{Offset: int64(off), Data: in.Data[off:end]}}}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// positionAfter says whether an instance comes after a position in the
// stream's order: session, scan, then instance id.
func positionAfter(p *v1.ReadPosition, sc *v1.Scan, in *Instance) bool {
	a := []string{fold(sc.GetSessionLabel()), fold(sc.GetLabel()), in.ID}
	b := []string{fold(p.GetSessionLabel()), fold(p.GetScanLabel()), p.GetInstanceId()}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// serveInstance answers the plain HTTP GET of one instance as the real
// route does: the SHA-256 as a strong ETag, and the range and If-Range
// conditions of RFC 9110, which http.ServeContent applies.
func (s *Server) serveInstance(w http.ResponseWriter, r *http.Request) {
	if s.RequireKey != "" && r.Header.Get("Authorization") != "Bearer "+s.RequireKey {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	path := r.PathValue("subject") + "/" + r.PathValue("session") + "/" + r.PathValue("scan")
	for _, in := range s.instancesOf(r.PathValue("tenant"), path) {
		if in.ID == r.PathValue("id") {
			w.Header().Set("ETag", `"`+in.sha()+`"`)
			w.Header().Set("Content-Type", "application/dicom")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(in.Data))
			return
		}
	}
	http.NotFound(w, r)
}
