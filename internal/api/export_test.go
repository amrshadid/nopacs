package api

import (
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/dirimport"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/trial"
)

// HoldServerReads takes every read the server of h holds open at once, as
// MaxServerReads callers each holding one would, and returns their release.
func HoldServerReads(h Handlers) func() {
	var releases []func()
	for i := range MaxServerReads {
		release, err := h.c.reads.Take("held/" + strconv.Itoa(i))
		if err != nil {
			break
		}
		releases = append(releases, release)
	}
	return func() {
		for _, r := range releases {
			r()
		}
	}
}

// SetPortProbe makes the services of h probe a port with probe instead of
// binding it, so a test sees each port tried and on which address.
func SetPortProbe(h Handlers, probe place.Probe) { h.c.seams.probe = probe }

// SetImportCheck makes the services of h check an import job against roots
// instead of the installation's import roots, bounded by timeout instead of
// the server's bound when timeout is not zero.
func SetImportCheck(h Handlers, roots dirimport.Roots, timeout time.Duration) {
	h.c.seams.importRoots, h.c.seams.importCheck = roots, timeout
}

// SetSummarySend makes the summary of every export by h meet err in place
// of its Send, after the export's data was streamed.
func SetSummarySend(h Handlers, err error) { h.c.seams.summarySend = err }

// SetBackupDatabases makes every backup by h read each database through
// wrap, given the tenant's name ("" for the master) and the database.
func SetBackupDatabases(h Handlers, wrap func(tenantName string, db snapshot.Database) snapshot.Database) {
	h.c.seams.backupDatabase = wrap
}

// SetBackupLimit makes every backup by h bounded by limit in place of
// backup.max-duration, whose least value is a minute.
func SetBackupLimit(h Handlers, limit time.Duration) { h.c.seams.backupLimit = limit }

// SetRecheck makes every stream by h check its caller's key again each
// every, in place of RecheckInterval.
func SetRecheck(h Handlers, every time.Duration) { h.c.seams.recheck = every }

// SetGuarding makes the guard of every delete by h that lifts retention,
// and of every clock's confirmation on the installation, run fn first,
// inside the change's transaction, after the request was let in.
func SetGuarding(h Handlers, fn func()) { h.c.seams.guarding = fn }

// SetSenderHealths makes the services of h answer a sender's health from
// healths, as the part of the server that sends would.
func SetSenderHealths(h Handlers, healths SenderHealths) { h.c.senders = healths }

// SetSenderTests makes the services of h test a sender's peer through
// tester, as the part of the server that sends would.
func SetSenderTests(h Handlers, tester SenderTester) { h.c.senderTests = tester }

// Pending is the table of the methods whose service is not served yet,
// with the methods of Methods, so a test holds each of its lines to the
// method's Role line before the service is served.
type Pending struct{}

// RequestFor is Methods.RequestFor over the methods not served yet.
func (Pending) RequestFor(procedure string) (proto.Message, bool) {
	return requestFor(pendingRules, procedure)
}

// Requirement is Methods.Requirement over the methods not served yet.
func (Pending) Requirement(procedure string, msg proto.Message) (access.Requirement, access.Request) {
	return requirementIn(pendingRules, procedure, msg)
}

// Procedures returns the full procedure name of every method not served
// yet that has a line in the table.
func (Pending) Procedures() []string {
	out := make([]string, 0, len(pendingRules))
	for p := range pendingRules {
		out = append(out, p)
	}
	return out
}

// DownloadMethod is the method the audit entry of a plain HTTP download
// names.
const DownloadMethod = DownloadPattern

// DownloadFacts is the requirement and the audit facts of a plain HTTP
// download of an instance with these parts of its path, and whether it
// carries a Range and a window.
func DownloadFacts(tenant, subject, session, scan, instance string, ranged, window bool) (access.Requirement, access.Request) {
	return downloadNeed(download{Tenant: tenant, Subject: subject, Session: session, Scan: scan, Instance: instance, Ranged: ranged, Window: window})
}

// HoldTrialPlaces takes n of the places the services keep for trial runs,
// as that many trials running would, and returns the function that gives
// them back.
func HoldTrialPlaces(h Handlers, n int) func() {
	for range n {
		h.c.trials <- struct{}{}
	}
	return func() {
		for range n {
			<-h.c.trials
		}
	}
}

// SafeTrial is trial.SafeError: what a trial's error is answered and logged as.
func SafeTrial(err error) error { return trial.SafeError(err) }

// EncryptionMessage is encryptionMessage, for the test of what a status
// says of an S3 storage's encryption.
var EncryptionMessage = encryptionMessage
