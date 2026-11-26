package api

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/events"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Feeds streams tenants' events: the events package's Broadcaster, which
// reads each tenant's feed at the pace the client reads and holds no queue.
type Feeds interface {
	// Watch streams one tenant's events (events.Broadcaster.Watch).
	Watch(ctx context.Context, w events.Watch, send events.Send) error
	// WatchAll streams every tenant's events (events.Broadcaster.WatchAll).
	WatchAll(ctx context.Context, w events.WatchAll, send events.Send) error
}

// RecheckInterval is how often an open stream checks its caller's key and
// grant again, the contract's 15 seconds at most.
const RecheckInterval = 15 * time.Second

// The details of a Watch's audit entries.
const (
	detailStream = "stream"
	detailCursor = "cursor"
	detailTypes  = "types"
	detailEvents = "events"
)

// watchDetails returns the details of a Watch's entries: its scope, whether
// it gave cursors, and the event types it keeps.
func watchDetails(r *v1.WatchRequest) map[string]string {
	cursors := "none"
	switch {
	case r.GetCursor() != "":
		cursors = "given"
	case len(r.GetTenantCursors()) > 0 || r.GetResume():
		cursors = "tenant_cursors"
	}
	names := make([]string, 0, len(r.GetTypes()))
	for _, t := range r.GetTypes() {
		names = append(names, strings.ToLower(strings.TrimPrefix(t.String(), "EVENT_TYPE_")))
	}
	types := "all"
	if len(names) > 0 {
		types = strings.Join(names, ",")
	}
	return map[string]string{detailScope: scopeDetail(r.GetScope()), detailCursor: cursors, detailTypes: types}
}

// Watch streams the events of one tenant or of every tenant, as the
// contract says: the first message a keep-alive, the caller's key and grant
// checked again every RecheckInterval, one audit entry when the stream
// opens and one when it ends, with how many events it carried.
//
// A cursor of another history of a tenant's feed ends the stream with
// OUT_OF_RANGE and a message beginning EVENT_HISTORY_CHANGED, whether it was
// given or reached while the stream was open. A stream opened from now
// takes the feed's cursor and then reads from it: a rebuild that makes its
// shadow the catalog in use between those two reads ends the stream at
// once the same way, and the client recovers the same way, from a new
// status. The events of the old history stay in the database, never
// answered, until the next pruning removes them.
func (s monitorService) Watch(ctx context.Context, req *v1.WatchRequest, stream nopacsv1connect.MonitorServiceWatchServerStream) error {
	c := s.c
	d, err := c.decision(ctx, nopacsv1connect.MonitorServiceWatchProcedure)
	if err != nil {
		return err
	}
	scope, err := scopeOf("scope", req.GetScope())
	if err != nil {
		return c.fail(d, err)
	}
	types, err := eventTypesOf(req.GetTypes())
	if err != nil {
		return c.fail(d, err)
	}
	if scope.IsInstallation() && req.GetCursor() != "" {
		return c.fail(d, invalid("cursor", "is empty with every tenant"))
	}
	if !scope.IsInstallation() && (len(req.GetTenantCursors()) > 0 || req.GetResume()) {
		return c.fail(d, invalid("tenant_cursors", "and resume are unset with one tenant"))
	}
	if len(req.GetCursor()) > MaxPageTokenLength {
		return c.fail(d, invalid("cursor", "is at most 1 KiB"))
	}
	var st tenant.Store
	var feed tenant.Events
	if !scope.IsInstallation() {
		if st, err = c.sinkFor(ctx, d, scope, watchDetails(req)); err != nil {
			return err
		}
		var ok bool
		if feed, ok = st.(tenant.Events); !ok {
			return c.failIn(d, st, fault.New(fault.Unavailable, "the tenant's database keeps no event feed"))
		}
	}
	if c.feeds == nil {
		return c.failIn(d, st, fault.New(fault.Unavailable, "this server streams no events"))
	}

	// The key and grant are checked again while the stream is open; a key
	// that may no longer read ends it with the answer a new call would get,
	// which Confirm records.
	ctx, stop := c.watchKey(ctx, d)
	defer stop()

	var carried atomic.Int64
	send := func(ctx context.Context, m events.Message) error {
		if err := stream.Send(watchMessage(m)); err != nil {
			return err
		}
		if m.Event != nil {
			carried.Add(1)
		}
		return nil
	}
	owner := ownerOf(d)
	c.record(d, st, master.OutcomeDone, "", access.Facts{Details: map[string]string{detailStream: "opened"}})
	if scope.IsInstallation() {
		err = c.feeds.WatchAll(ctx, events.WatchAll{Owner: owner, Tenants: feedTenants{c}, Cursors: req.GetTenantCursors(),
			Resume: req.GetResume(), Types: types}, send)
	} else {
		err = c.feeds.Watch(ctx, events.Watch{Tenant: scope.Tenant, Owner: owner, Reader: feed, Cursor: req.GetCursor(), Types: types}, send)
	}
	end := access.Facts{Details: map[string]string{detailStream: "ended", detailEvents: strconv.FormatInt(carried.Load(), 10)}}
	if refusal, ok := refusedWhileOpen(ctx); ok {
		// Confirm recorded the refusal.
		return c.toAPI(d, refusal)
	}
	switch cause := context.Cause(ctx); {
	case err == nil, errors.Is(err, context.Canceled):
		// The client went away, or the server is stopping: the stream ends
		// as the contract says a stream may end at any time.
		c.record(d, st, master.OutcomeDone, "", end)
		if ctx.Err() == nil || cause == nil {
			return nil
		}
		return fault.New(fault.Unavailable, "the stream ended; resume from its last cursors")
	}
	return c.failWith(d, st, end, watchError(err))
}

// watchError returns the API's answer to the error a watch ended with: a
// bound reached is resource exhausted; a cursor of another tenant's feed is
// invalid; a cursor expired names its tenant.
func watchError(err error) error {
	switch {
	case errors.Is(err, events.ErrTooManyWatches), errors.Is(err, events.ErrTooManyTenants):
		return fault.Wrap(fault.Exhausted, err, fault.Message(err))
	case errors.Is(err, tenant.ErrForeignCursor):
		return fault.New(fault.Invalid, "cursor: is not one a Watch or status of this tenant gave")
	}
	return err
}

// ownerOf names who opened a stream, for the bound of streams per caller:
// the key, or the local operator.
func ownerOf(d access.Decision) string {
	if id := d.Principal().KeyID(); id != "" {
		return "key/" + id
	}
	return "local"
}

// eventTypesOf returns the event types a request keeps, or an Invalid
// error for EVENT_TYPE_UNSPECIFIED, a value this server does not know, or
// more than tenant.MaxEventTypes.
func eventTypesOf(list []v1.EventType) ([]tenant.EventType, error) {
	if len(list) > tenant.MaxEventTypes {
		return nil, invalid("types", "names at most "+strconv.Itoa(tenant.MaxEventTypes)+" types")
	}
	out := make([]tenant.EventType, 0, len(list))
	for _, t := range list {
		e := tenant.EventType(strings.ToLower(strings.TrimPrefix(t.String(), "EVENT_TYPE_")))
		if t == v1.EventType_EVENT_TYPE_UNSPECIFIED || !e.Valid() {
			return nil, invalid("types", "names event types, none of them unspecified")
		}
		out = append(out, e)
	}
	return out, nil
}

// feedTenants is the stream of every tenant's view of the installation:
// the tenants' names from the master, each one's feed from the registry.
type feedTenants struct{ c *core }

// TenantNames implements events.Tenants.
func (f feedTenants) TenantNames(ctx context.Context) ([]string, error) {
	var out []string
	page := master.Page{Limit: master.MaxPageLimit}
	for i := 0; i < maxWholeListPages; i++ {
		rows, next, err := f.c.master.ListTenants(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, t := range rows {
			out = append(out, t.Name)
		}
		if next == "" {
			sort.Strings(out)
			return out, nil
		}
		page.Cursor = next
	}
	return nil, fault.New(fault.Internal, "api: the list of tenants did not end")
}

// Reader implements events.Tenants.
func (f feedTenants) Reader(_ context.Context, name string) (events.Reader, error) {
	// A feed is read only, so a moved tenant's is served; a fenced one
	// serves no data, and its feed is left unanswered.
	st, err := f.c.reg.DataReader(name)
	if err != nil {
		return nil, err
	}
	feed, ok := st.(tenant.Events)
	if !ok {
		return nil, fault.New(fault.Unavailable, "the tenant's database keeps no event feed")
	}
	return feed, nil
}

// watchMessage returns one message of a stream as the contract writes it.
func watchMessage(m events.Message) *v1.WatchResponse {
	if k := m.KeepAlive; k != nil {
		return &v1.WatchResponse{Kind: &v1.WatchResponse_KeepAlive{KeepAlive: &v1.WatchKeepAlive{
			Time: timestamp(k.Time), Cursor: k.Cursor, TenantCursors: k.TenantCursors, UnansweredTenants: k.Unanswered, Full: k.Full,
			EventHistory: k.History, TenantEventHistories: k.TenantHistories,
		}}}
	}
	return &v1.WatchResponse{Kind: &v1.WatchResponse_Event{Event: eventMessage(m.Tenant, *m.Event)}}
}

// word returns the contract's enum value whose name is prefix and w in
// capitals, or 0, unspecified, for a word it does not name: the port's
// payloads use the contract's own words, so each enum is matched by name.
func word(prefix string, w string, values map[string]int32) int32 {
	return values[prefix+strings.ToUpper(w)]
}

// eventMessage returns an event of the tenant named name as the contract
// writes it.
func eventMessage(name string, e tenant.Event) *v1.Event {
	m := &v1.Event{Cursor: e.Cursor, Time: timestamp(e.Time), Tenant: name,
		Type: v1.EventType(word("EVENT_TYPE_", string(e.Type), v1.EventType_value))}
	switch p := e.Payload.(type) {
	case *tenant.AssociationFailed:
		a := &v1.AssociationFailed{Receiver: p.Receiver, Count: p.Count, Hosts: p.Hosts,
			Failure: v1.AssociationFailure(word("ASSOCIATION_FAILURE_", string(p.Failure), v1.AssociationFailure_value))}
		if p.Caller != nil {
			a.Caller = &v1.Caller{CallingAeTitle: p.Caller.CallingAETitle, CalledAeTitle: p.Caller.CalledAETitle, Address: p.Caller.Address}
		}
		m.Payload = &v1.Event_AssociationFailed{AssociationFailed: a}
	case *tenant.ReceptionStepReached:
		c := p.Counts
		m.Payload = &v1.Event_ReceptionStep{ReceptionStep: &v1.ReceptionStepReached{
			Reception: p.Reception, Receiver: p.Receiver, Arrival: int32(p.Arrival), //nolint:gosec // G115: an arrival is a study's count of receptions, far below 2^31.
			Step: stepOf(p.Step), PreviousStep: stepOf(p.PreviousStep), SubjectLabel: p.SubjectLabel, SessionLabel: p.SessionLabel, RetryOf: p.RetryOf,
			Counts: &v1.ReceptionCounts{FilesReceived: c.FilesReceived, BytesReceived: c.BytesReceived, FilesArchived: c.FilesArchived,
				BytesArchived: c.BytesArchived, FilesDuplicate: c.FilesDuplicate, BytesDuplicate: c.BytesDuplicate,
				FilesQuarantined: c.FilesQuarantined, BytesQuarantined: c.BytesQuarantined, FilesReleased: c.FilesReleased},
		}}
	case *tenant.FileQuarantined:
		m.Payload = &v1.Event_FileQuarantined{FileQuarantined: &v1.FileQuarantined{File: p.File, Reception: p.Reception,
			Receiver: p.Receiver, Reason: reasons[p.Reason], SizeBytes: p.SizeBytes, HeldFiles: p.HeldFiles, HeldBytes: p.HeldBytes}}
	case *tenant.QuarantineDecided:
		q := &v1.QuarantineDecided{File: p.File, State: quarantineStates[p.State], ReleaseReception: p.ReleaseReception,
			HeldFiles: p.HeldFiles, HeldBytes: p.HeldBytes}
		if p.ReleaseMode == tenant.ReleaseAsIs {
			q.ReleaseMode = v1.ReleaseMode_RELEASE_MODE_AS_IS
		}
		m.Payload = &v1.Event_QuarantineDecided{QuarantineDecided: q}
	case *tenant.ReceiverHealthChanged:
		h := &v1.ReceiverHealth{Detail: p.Detail, Since: timestamp(p.Since),
			State:      v1.ReceiverState(word("RECEIVER_STATE_", string(p.State), v1.ReceiverState_value)),
			DownReason: v1.ReceiverDownReason(word("RECEIVER_DOWN_REASON_", string(p.DownReason), v1.ReceiverDownReason_value))}
		m.Payload = &v1.Event_ReceiverHealth{ReceiverHealth: &v1.ReceiverHealthChanged{Receiver: p.Receiver, Health: h}}
	case *tenant.StorageSpaceChanged:
		m.Payload = &v1.Event_StorageSpace{StorageSpace: &v1.StorageSpaceChanged{Storage: p.Storage, UsedBytes: p.UsedBytes, FreeBytes: p.FreeBytes,
			State: v1.StorageSpaceState(word("STORAGE_SPACE_STATE_", string(p.State), v1.StorageSpaceState_value))}}
	case *tenant.FramesAdded:
		m.Payload = &v1.Event_FramesAdded{FramesAdded: &v1.FramesAdded{SubjectLabel: p.SubjectLabel, SessionLabel: p.SessionLabel,
			ScanLabel: p.ScanLabel, Instances: p.Instances, ScansDone: p.ScansDone, InstancesDone: p.InstancesDone, Finished: p.Finished}}
	case *tenant.JobStepReached:
		m.Payload = &v1.Event_JobStep{JobStep: &v1.JobStepReached{Job: p.Job,
			State:         v1.JobState(word("JOB_STATE_", string(p.State), v1.JobState_value)),
			PreviousState: v1.JobState(word("JOB_STATE_", string(p.PreviousState), v1.JobState_value)),
			Counts:        eventJobCounts(p.Counts), Sender: p.Sender, Receiver: p.Receiver, Reception: p.Reception, RetryOf: p.RetryOf, Reason: p.Reason}}
	case *tenant.JobProgressed:
		m.Payload = &v1.Event_JobProgress{JobProgress: &v1.JobProgressed{Job: p.Job, Counts: eventJobCounts(p.Counts), Sender: p.Sender}}
	case *tenant.SenderHealthChanged:
		m.Payload = &v1.Event_SenderHealth{SenderHealth: &v1.SenderHealthChanged{Sender: p.Sender, Health: &v1.SenderHealth{
			State:      v1.SenderState(word("SENDER_STATE_", string(p.State), v1.SenderState_value)),
			DownReason: v1.SenderDownReason(word("SENDER_DOWN_REASON_", string(p.DownReason), v1.SenderDownReason_value)),
			Fault:      v1.SenderFault(word("SENDER_FAULT_", string(p.Fault), v1.SenderFault_value)),
			Detail:     p.Detail, Since: timestamp(p.Since),
			FailedConnections: int32(min(p.FailedConnections, math.MaxInt32)), //nolint:gosec // G115: bounded just above.
			NextTryTime:       timestamp(p.NextTry), LastSuccessTime: timestamp(p.LastSuccess)}}}
	case *tenant.ScrubFindingChanged:
		m.Payload = &v1.Event_ScrubFinding{ScrubFinding: &v1.ScrubFindingChanged{Finding: p.Finding,
			Kind:    v1.ScrubFindingKind(word("SCRUB_FINDING_KIND_", string(p.Kind), v1.ScrubFindingKind_value)),
			Storage: p.Storage, SubjectLabel: p.SubjectLabel, SessionLabel: p.SessionLabel, ScanLabel: p.ScanLabel,
			Cleared: p.Cleared, Job: p.Job}}
	case *tenant.CatalogChanged:
		m.Payload = &v1.Event_CatalogChanged{CatalogChanged: &v1.CatalogChanged{Correction: p.Correction,
			Kind:         v1.CorrectionKind(word("CORRECTION_KIND_", string(p.Kind), v1.CorrectionKind_value)),
			State:        v1.CorrectionState(word("CORRECTION_STATE_", string(p.State), v1.CorrectionState_value)),
			SubjectLabel: p.SubjectLabel, SessionLabel: p.SessionLabel, ScanLabel: p.ScanLabel, ToLabel: p.ToLabel,
			Instances: p.Instances, Revision: p.Revision,
			Origin: v1.CorrectionOrigin(word("CORRECTION_ORIGIN_", p.Origin, v1.CorrectionOrigin_value))}}
	case *tenant.CatalogRebuilt:
		m.Payload = &v1.Event_CatalogRebuilt{CatalogRebuilt: &v1.CatalogRebuilt{Job: p.Job, Instances: p.Instances, NewHistory: p.NewHistory}}
	case *tenant.StorageSwitched:
		m.Payload = &v1.Event_StorageSwitched{StorageSwitched: &v1.StorageSwitched{Primary: p.Primary, Replica: p.Replica,
			ScansDrained: p.ScansDrained, AssociationsRefused: p.AssociationsRefused, Pause: durationpb.New(time.Duration(p.PauseMicros) * time.Microsecond),
			Refused: p.Refused, Refusal: p.Refusal, SwitchGeneration: p.SwitchGeneration}}
	case *tenant.ReplicaBehind:
		m.Payload = &v1.Event_ReplicaBehind{ReplicaBehind: &v1.ReplicaBehind{Replica: p.Replica, Primary: p.Primary,
			Lag: durationpb.New(time.Duration(p.LagMicros) * time.Microsecond), ScansBehind: p.ScansBehind, Recovered: p.Recovered}}
	case *tenant.QuarantineAgingChanged:
		m.Payload = &v1.Event_QuarantineAging{QuarantineAging: &v1.QuarantineAging{
			OldestAge: durationpb.New(time.Duration(p.OldestAgeMicros) * time.Microsecond), HeldFiles: p.HeldFiles, HeldBytes: p.HeldBytes,
			AgePassed: p.AgePassed, BytesPassed: p.BytesPassed, Recovered: p.Recovered}}
	}
	return m
}

// eventJobCounts returns a job's counts as an event carries them, the
// contract's JobCounts, so an event holds the same figures a job's own
// answer does.
func eventJobCounts(c tenant.EventJobCounts) *v1.JobCounts {
	return &v1.JobCounts{Instances: c.Instances, Bytes: c.Bytes, InstancesSent: c.InstancesSent, BytesSent: c.BytesSent,
		InstancesWarned: c.InstancesWarned, BytesWarned: c.BytesWarned, InstancesRefused: c.InstancesRefused, BytesRefused: c.BytesRefused,
		InstancesRemaining: c.InstancesRemaining, BytesRemaining: c.BytesRemaining, Scans: c.Scans, ScansDone: c.ScansDone, Listed: c.Listed}
}
