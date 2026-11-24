package api

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// SenderHealth is what the server knows of a sender's peer now
// (sender.proto, SenderHealth), and of the connections it holds open.
type SenderHealth struct {
	// State is whether the sender reaches its peer. The zero value is
	// treated as unknown.
	State tenant.SenderState
	// DownReason is why it is down; empty unless State is down.
	DownReason tenant.SenderDownReason
	// Fault is the fault of the last connection that failed; empty after
	// one that succeeded.
	Fault tenant.SenderFault
	// Detail is a fixed phrase of the server's for a person, at most 256
	// bytes; never a peer's, an operating system's or a library's own
	// text, a path or a secret. Empty when it is up or unknown.
	Detail string
	// Since is when it entered this state with this reason.
	Since time.Time
	// FailedConnections counts the connections in a row that failed.
	FailedConnections int
	// NextTry is when a failing peer is tried again; zero otherwise.
	NextTry time.Time
	// LastSuccess is when a connection last succeeded; zero for none since
	// the server started.
	LastSuccess time.Time
	// OpenAssociations is the associations (for a stow sender, requests)
	// open now.
	OpenAssociations int
}

// SenderHealths answers a sender's health, which only the part of the
// server that sends knows. It is called on the request's path, once for
// each sender answered, so it reads memory and never a network or a disk.
type SenderHealths interface {
	// SenderHealth returns the health of the sender s of the tenant named
	// tenantName, as configured now.
	SenderHealth(tenantName string, s tenant.Sender) SenderHealth
}

// SenderChanges is told, after a change of a tenant's senders or jobs is
// committed (a sender added, changed or removed, a job made, canceled or
// retried), so what sends acts on it at once instead of at its next
// periodic read. Config.SenderHealths may implement it; one that does not
// is left to its own reading.
type SenderChanges interface {
	// SendersChanged says the senders or the jobs of the tenant named
	// tenantName changed. It must not block: it is called on the request's
	// path.
	SendersChanged(tenantName string)
}

// unknownSenders is the SenderHealths of a server that sends nothing yet: a
// sender that is not enabled is down because it is disabled, which is true
// whatever sends; one that is enabled has not connected, so its health is
// unknown (sender.proto, SenderState).
type unknownSenders struct{}

// SenderHealth answers down, disabled, or unknown.
func (unknownSenders) SenderHealth(_ string, s tenant.Sender) SenderHealth {
	if !s.Enabled {
		return SenderHealth{State: tenant.SenderDown, DownReason: tenant.SenderDownDisabled, Since: s.Updated}
	}
	return SenderHealth{State: tenant.SenderUnknown, Since: s.Updated}
}

// SenderTestStep is one step of a test of a sender, in lowercase as the
// contract's SenderTestStep without its prefix.
type SenderTestStep string

// The steps, in the order they run.
const (
	StepResolve   SenderTestStep = "resolve"
	StepConnect   SenderTestStep = "connect"
	StepTLS       SenderTestStep = "tls"
	StepAssociate SenderTestStep = "associate"
	StepRequest   SenderTestStep = "request"
	StepRelease   SenderTestStep = "release"
)

// SenderTestOutcome is how one step ended, as the contract's
// SenderTestOutcome in lowercase without its prefix.
type SenderTestOutcome string

// The outcomes.
const (
	TestPassed  SenderTestOutcome = "passed"
	TestFailed  SenderTestOutcome = "failed"
	TestSkipped SenderTestOutcome = "skipped"
)

// SenderTestResult is the outcome of one step of a test of a sender.
type SenderTestResult struct {
	// Step is the step, and Outcome how it ended.
	Step    SenderTestStep
	Outcome SenderTestOutcome
	// Duration is how long it took; unused for a skipped step.
	Duration time.Duration
	// Fault is why a failed step failed; empty for another outcome.
	Fault tenant.SenderFault
	// Detail is a phrase from the fixed set the contract describes, at
	// most 256 bytes; never the peer's own words.
	Detail string
}

// SenderTester tries a sender's peer as a send would reach it, which only
// the part of the server that sends knows how to do, and reports each step;
// it sends no instance and changes no health.
type SenderTester interface {
	// TestSender runs the steps for the sender s of the tenant named
	// tenantName, within ctx's deadline, and returns one result per step in
	// order. A sender that fails a step is a result, not an error; an error
	// is a failure of the server's own.
	TestSender(ctx context.Context, tenantName string, s tenant.Sender) ([]SenderTestResult, error)
}

// noSenderTests is the SenderTester of a server that cannot connect to a
// sender's peer yet: it answers every test as unavailable, in so many
// words, rather than a result that says nothing about the peer.
type noSenderTests struct{}

// TestSender answers that sender tests are not available.
func (noSenderTests) TestSender(context.Context, string, tenant.Sender) ([]SenderTestResult, error) {
	return nil, fault.New(fault.Unavailable, "this server cannot test a sender yet: its sending is not running")
}
