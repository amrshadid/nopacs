package api

import (
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestSendEventsCarryTheirPayload guards the job and sender health events
// on Watch: each is answered with its payload, field for field, so a
// client can tell which job moved, how far it is, and why a sender is
// down; without it the event came with its type and nothing else.
func TestSendEventsCarryTheirPayload(t *testing.T) {
	counts := tenant.EventJobCounts{Instances: 5, Bytes: 500, InstancesSent: 3, BytesSent: 300, InstancesRefused: 1, BytesRefused: 100,
		InstancesRemaining: 1, BytesRemaining: 100, Scans: 2, ScansDone: 1, Listed: true}
	step := eventMessage("lab1", tenant.Event{Cursor: "a", Type: tenant.EventJobStep, Payload: &tenant.JobStepReached{
		Job: "j1", State: tenant.JobRunning, PreviousState: tenant.JobQueued, Counts: counts, Sender: "pacs",
		Receiver: "scanner1", Reception: "r1", RetryOf: "j0", Reason: "why"}}).GetJobStep()
	if step == nil || step.GetJob() != "j1" || step.GetState() != v1.JobState_JOB_STATE_RUNNING ||
		step.GetPreviousState() != v1.JobState_JOB_STATE_QUEUED || step.GetSender() != "pacs" || step.GetReceiver() != "scanner1" ||
		step.GetReception() != "r1" || step.GetRetryOf() != "j0" || step.GetReason() != "why" ||
		step.GetCounts().GetInstancesSent() != 3 || step.GetCounts().GetBytesRefused() != 100 || !step.GetCounts().GetListed() {
		t.Errorf("a job step answered as %v", step)
	}
	progress := eventMessage("lab1", tenant.Event{Cursor: "b", Type: tenant.EventJobProgress, Payload: &tenant.JobProgressed{
		Job: "j1", Counts: counts, Sender: "pacs"}}).GetJobProgress()
	if progress == nil || progress.GetJob() != "j1" || progress.GetSender() != "pacs" ||
		progress.GetCounts().GetInstances() != 5 || progress.GetCounts().GetScansDone() != 1 || progress.GetCounts().GetInstancesRemaining() != 1 {
		t.Errorf("a job's progress answered as %v", progress)
	}
	since := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	health := eventMessage("lab1", tenant.Event{Cursor: "c", Type: tenant.EventSenderHealth, Payload: &tenant.SenderHealthChanged{
		Sender: "pacs", State: tenant.SenderDown, DownReason: tenant.SenderDownPeerFailing, Fault: tenant.SenderFaultUnreachable,
		Detail: "connection refused", Since: since, FailedConnections: 3, NextTry: since.Add(time.Minute)}}).GetSenderHealth()
	h := health.GetHealth()
	if health == nil || health.GetSender() != "pacs" || h.GetState() != v1.SenderState_SENDER_STATE_DOWN ||
		h.GetDownReason() != v1.SenderDownReason_SENDER_DOWN_REASON_PEER_FAILING || h.GetFault() != v1.SenderFault_SENDER_FAULT_UNREACHABLE ||
		h.GetDetail() != "connection refused" || !h.GetSince().AsTime().Equal(since) || h.GetFailedConnections() != 3 ||
		!h.GetNextTryTime().AsTime().Equal(since.Add(time.Minute)) {
		t.Errorf("a sender's health answered as %v", health)
	}
}
