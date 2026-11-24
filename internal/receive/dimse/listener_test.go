package dimse_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/dimse"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The DICOM values the tests assert on, from the standard rather than the
// library: PS3.8 Table 9-21 for rejections, PS3.4 B.2.3 for statuses.
const (
	rjPermanent          = 1
	rjTransient          = 2
	rjSourceUser         = 1
	rjSourcePresentation = 3
	rjCallingNotKnown    = 3
	rjCalledNotKnown     = 7
	rjCongestion         = 1
	statusSuccess        = 0x0000
	statusOutOfResources = 0xA700
	ctImageStorage       = "1.2.840.10008.5.1.4.1.1.2"
	mrImageStorage       = "1.2.840.10008.5.1.4.1.1.4"
	explicitLE           = "1.2.840.10008.1.2.1"
	jpeg2000Lossless     = "1.2.840.10008.1.2.4.90"
)

// studyFiles are the seven files of one public CT study in two series.
var studyFiles = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"}

// TestAllowedCallerStoresAStudyAsSent guards that an admitted caller's
// study reaches the Sink as the bytes it sent: pynetdicom sends each public
// file from its path without decoding it, every store is answered success,
// and each file the Sink took holds the sent file's data set byte for byte,
// so the same SHA-256, with the caller and transfer syntax recorded.
func TestAllowedCallerStoresAStudyAsSent(t *testing.T) {
	py := python(t)
	sink := &fakeSink{}
	s := listen(t, row(), sink, nil)
	paths := make([]string, len(studyFiles))
	for i, f := range studyFiles {
		paths[i] = corpusPath(f)
	}
	res := scu(t, py, s, "store", "SCANNER", "ARCHIVE", paths...)
	if res.Assoc != "accepted" || len(res.Statuses) != len(studyFiles) {
		t.Fatalf("result %+v", res)
	}
	for i, st := range res.Statuses {
		if !isStatus(st, statusSuccess) {
			t.Errorf("%s answered %v", studyFiles[i], st)
		}
	}
	got := sink.whole()
	if len(got) != len(studyFiles) {
		t.Fatalf("the sink kept %d files, want %d", len(got), len(studyFiles))
	}
	for i, f := range studyFiles {
		want := dataSet(t, corpusBytes(t, f))
		have := dataSet(t, got[i].body)
		if sum(want) != sum(have) {
			t.Errorf("%s: the data set kept is not the data set sent", f)
		}
		a := got[i].arrival
		if a.Caller.CallingAETitle != "SCANNER" || a.Caller.CalledAETitle != "ARCHIVE" || a.TransferSyntaxUID == "" ||
			a.Association == "" || a.SOPClassUID != ctImageStorage {
			t.Errorf("%s: arrival %+v", f, a)
		}
		if host, _, err := net.SplitHostPort(a.Caller.Address); err != nil || host != "127.0.0.1" {
			t.Errorf("%s: caller address %q", f, a.Caller.Address)
		}
	}
	if r := s.reports.get(); len(r) != 0 {
		t.Fatalf("an admitted caller was reported: %+v", r)
	}
}

// TestStorescuStoresAsSent guards the same with dcmtk's storescu, a second
// implementation of the sending side: each store answered success, and the
// data set kept equal, byte for byte, to what dcmtk sends. dcmtk encodes a
// data set again as it sends it (a sequence of undefined length is given
// its length), so what it sends is the file as dcmtk's dcmconv writes it,
// which the test makes and compares with.
func TestStorescuStoresAsSent(t *testing.T) {
	storescu := tool(t, "storescu")
	dcmconv := tool(t, "dcmconv")
	sink := &fakeSink{}
	s := listen(t, row(), sink, nil)
	args := []string{"-aet", "SCANNER", "-aec", "ARCHIVE", s.host, s.port}
	for _, f := range studyFiles {
		args = append(args, corpusPath(f))
	}
	out, err := command(t, storescu, args...).CombinedOutput() // #nosec G204 -- the test's own arguments.
	if err != nil {
		t.Fatalf("storescu: %v\n%s", err, out)
	}
	got := sink.whole()
	if len(got) != len(studyFiles) {
		t.Fatalf("the sink kept %d files, want %d\n%s", len(got), len(studyFiles), out)
	}
	dir := t.TempDir()
	for i, f := range studyFiles {
		conv := filepath.Join(dir, f)
		if out, err := command(t, dcmconv, corpusPath(f), conv).CombinedOutput(); err != nil { // #nosec G204
			t.Fatalf("dcmconv: %v\n%s", err, out)
		}
		want, err := os.ReadFile(conv) // #nosec G304 -- the test's own file.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dataSet(t, want), dataSet(t, got[i].body)) {
			t.Errorf("%s: the data set kept is not what dcmtk sent", f)
		}
		if got[i].arrival.TransferSyntaxUID != explicitLE {
			t.Errorf("%s: transfer syntax %s", f, got[i].arrival.TransferSyntaxUID)
		}
	}
}

// TestEchoscuIsAnsweredForAnAdmittedCallerOnly guards C-ECHO with dcmtk:
// answered for the caller the receiver lists, refused to another.
func TestEchoscuIsAnsweredForAnAdmittedCallerOnly(t *testing.T) {
	echoscu := tool(t, "echoscu")
	s := listen(t, row(), &fakeSink{}, nil)
	if out, err := command(t, echoscu, "-aet", "SCANNER", "-aec", "ARCHIVE", s.host, s.port).CombinedOutput(); err != nil { // #nosec G204
		t.Fatalf("echoscu as an admitted caller: %v\n%s", err, out)
	}
	out, err := command(t, echoscu, "-aet", "STRANGER", "-aec", "ARCHIVE", s.host, s.port).CombinedOutput() // #nosec G204
	if err == nil {
		t.Fatalf("echoscu as a stranger succeeded:\n%s", out)
	}
	if !bytes.Contains(bytes.ToLower(out), []byte("calling ae title not recogni")) {
		t.Fatalf("echoscu as a stranger was not told its title is not recognized:\n%s", out)
	}
}

// TestCallersNotListedAreRefusedWithTheirReason guards admission, each case
// seen by pynetdicom as the rejection PS3.8 Table 9-21 gives it and
// reported with its failure: a calling title not listed, a title called
// that is not the receiver's, an address outside the only rule's range, and
// a receiver with no rule at all; and an admitted caller at a receiver
// open to any caller.
func TestCallersNotListedAreRefusedWithTheirReason(t *testing.T) {
	py := python(t)
	cases := []struct {
		name            string
		mod             func(*tenant.Receiver)
		calling, called string
		rj              []int
		failure         tenant.AssociationFailure
	}{
		{"calling title not listed", nil, "STRANGER", "ARCHIVE",
			[]int{rjPermanent, rjSourceUser, rjCallingNotKnown}, tenant.FailureCallerNotAllowed},
		{"another title called", nil, "SCANNER", "OTHER",
			[]int{rjPermanent, rjSourceUser, rjCalledNotKnown}, tenant.FailureCalledAETitleNotRecognized},
		{"address outside the range", func(r *tenant.Receiver) {
			r.DIMSE.Callers = []tenant.CallerRule{{CallingAETitle: "SCANNER", AddressRange: "10.0.0.0/8"}}
		}, "SCANNER", "ARCHIVE", []int{rjPermanent, rjSourceUser, rjCallingNotKnown}, tenant.FailureCallerNotAllowed},
		{"no rule at all", func(r *tenant.Receiver) { r.DIMSE.Callers = nil }, "SCANNER", "ARCHIVE",
			[]int{rjPermanent, rjSourceUser, rjCallingNotKnown}, tenant.FailureCallerNotAllowed},
		{"a stranger calling another title", nil, "STRANGER", "OTHER",
			[]int{rjPermanent, rjSourceUser, rjCallingNotKnown}, tenant.FailureCallerNotAllowed},
		{"any caller", func(r *tenant.Receiver) { r.DIMSE.Callers, r.DIMSE.AnyCaller = nil, true }, "WHOEVER", "ARCHIVE", nil, ""},
		{"inside the range", func(r *tenant.Receiver) {
			r.DIMSE.Callers = []tenant.CallerRule{{AddressRange: "127.0.0.0/8"}}
		}, "WHOEVER", "ARCHIVE", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := row()
			if c.mod != nil {
				r = row(c.mod)
			}
			s := listen(t, r, &fakeSink{}, nil)
			res := scu(t, py, s, "echo", c.calling, c.called)
			reps := s.reports.get()
			if c.rj == nil {
				if res.Assoc != "accepted" || len(res.Statuses) != 1 || !isStatus(res.Statuses[0], statusSuccess) || len(reps) != 0 {
					t.Fatalf("result %+v, reports %+v", res, reps)
				}
				return
			}
			if res.Assoc != "rejected" || len(res.RJ) != 3 || res.RJ[0] != c.rj[0] || res.RJ[1] != c.rj[1] || res.RJ[2] != c.rj[2] {
				t.Fatalf("result %+v, want rejected %v", res, c.rj)
			}
			if len(reps) != 1 || reps[0].failure != c.failure || reps[0].caller.Address != "127.0.0.1" ||
				reps[0].caller.CallingAETitle != c.calling || reps[0].caller.CalledAETitle != c.called {
				t.Fatalf("reports %+v, want one %s from 127.0.0.1", reps, c.failure)
			}
		})
	}
}

// TestOnlyAcceptedSyntaxesAreNegotiated guards negotiation: a SOP class the
// receiver does not list, or a transfer syntax it does not, is rejected in
// the association's answer, the association is reported as having no
// acceptable context, and Verification is negotiated whatever the list.
func TestOnlyAcceptedSyntaxesAreNegotiated(t *testing.T) {
	py := python(t)
	r := row(func(r *tenant.Receiver) {
		r.DIMSE.SOPClasses = []string{ctImageStorage}
		r.DIMSE.TransferSyntaxes = []string{explicitLE}
	})
	s := listen(t, r, &fakeSink{}, nil)
	for _, p := range [][2]string{{mrImageStorage, explicitLE}, {ctImageStorage, jpeg2000Lossless}} {
		res := scu(t, py, s, "propose", "SCANNER", "ARCHIVE", p[0], p[1])
		if len(res.Accepted) != 0 {
			t.Fatalf("%v was negotiated: %+v", p, res)
		}
	}
	waitFor(t, "two reports of no acceptable context", func() bool { return len(s.reports.get()) >= 2 })
	for _, rep := range s.reports.get() {
		if rep.failure != tenant.FailureNoAcceptableContext || rep.caller.CallingAETitle != "SCANNER" {
			t.Fatalf("report %+v", rep)
		}
	}
	if res := scu(t, py, s, "propose", "SCANNER", "ARCHIVE", ctImageStorage, explicitLE); len(res.Accepted) != 1 {
		t.Fatalf("the accepted class was not negotiated: %+v", res)
	}
	if res := scu(t, py, s, "echo", "SCANNER", "ARCHIVE"); res.Assoc != "accepted" || !isStatus(res.Statuses[0], statusSuccess) {
		t.Fatalf("verification was not answered: %+v", res)
	}
}

// TestNearlyFullRefusesAndRecovers guards the refusal while the archive
// cannot take files: a new association is rejected as transient and
// reported nearly full; a store on an association admitted just before is
// answered out of resources and nothing reaches the Sink; once space
// returns, the same caller is admitted and stores.
func TestNearlyFullRefusesAndRecovers(t *testing.T) {
	py := python(t)
	var full atomic.Bool
	var calls atomic.Int64
	// fullAfterAdmission is full from the second question on: the
	// association is admitted, then its store finds the archive full.
	var fullAfterAdmission atomic.Bool
	refusal := func() tenant.AssociationFailure {
		n := calls.Add(1)
		if full.Load() || (fullAfterAdmission.Load() && n > 1) {
			return tenant.FailureNearlyFull
		}
		return ""
	}
	sink := &fakeSink{}
	s := listen(t, row(), sink, refusal)
	full.Store(true)
	res := scu(t, py, s, "echo", "SCANNER", "ARCHIVE")
	if res.Assoc != "rejected" || res.RJ[0] != rjTransient || res.RJ[1] != rjSourcePresentation || res.RJ[2] != rjCongestion {
		t.Fatalf("nearly full: %+v, want rejected transient", res)
	}
	if reps := s.reports.get(); len(reps) != 1 || reps[0].failure != tenant.FailureNearlyFull {
		t.Fatalf("reports %+v", reps)
	}
	full.Store(false)
	calls.Store(0)
	fullAfterAdmission.Store(true)
	res = scu(t, py, s, "store", "SCANNER", "ARCHIVE", corpusPath("MR_small.dcm"))
	if res.Assoc != "accepted" || len(res.Statuses) != 1 || !isStatus(res.Statuses[0], statusOutOfResources) {
		t.Fatalf("a store while full: %+v, want A700", res)
	}
	if n := len(sink.all()); n != 0 {
		t.Fatalf("a store refused while full reached the sink %d times", n)
	}
	fullAfterAdmission.Store(false)
	res = scu(t, py, s, "store", "SCANNER", "ARCHIVE", corpusPath("MR_small.dcm"))
	if res.Assoc != "accepted" || !isStatus(res.Statuses[0], statusSuccess) || len(sink.whole()) != 1 {
		t.Fatalf("after recovery: %+v, %d kept", res, len(sink.whole()))
	}
}

// TestRefusedOrFailedFilesAreAnsweredOutOfResources guards that a store is
// answered success only when the Sink kept the file: refused and failed
// outcomes are answered A700 (receiver.proto, max_file_bytes), so the
// sender keeps its copy.
func TestRefusedOrFailedFilesAreAnsweredOutOfResources(t *testing.T) {
	py := python(t)
	for _, st := range []receive.Status{receive.Refused, receive.Failed} {
		t.Run(st.String(), func(t *testing.T) {
			s := listen(t, row(), &fakeSink{answer: &receive.Outcome{Status: st, Reason: "test"}}, nil)
			res := scu(t, py, s, "store", "SCANNER", "ARCHIVE", corpusPath("MR_small.dcm"))
			if res.Assoc != "accepted" || len(res.Statuses) != 1 || !isStatus(res.Statuses[0], statusOutOfResources) {
				t.Fatalf("%+v, want A700", res)
			}
		})
	}
}

// TestOverSizeIsRefusedAndKeepsNothing guards the size bound end to end
// through the listener: a file past the receiver's largest is answered out
// of resources, so its sender keeps it, and nothing is kept whole; one past
// twice the bound has its association aborted by the library's own data
// set bound, and nothing is kept either.
func TestOverSizeIsRefusedAndKeepsNothing(t *testing.T) {
	py := python(t)
	// MR_small.dcm is 9830 bytes, its data set a little less.
	for _, c := range []struct {
		bound  int64
		status *int
	}{{6000, ptr(statusOutOfResources)}, {2048, nil}} {
		t.Run(strconv.FormatInt(c.bound, 10), func(t *testing.T) {
			r := row(func(r *tenant.Receiver) { r.MaxFileBytes = c.bound })
			sink := &fakeSink{limit: r.MaxFileBytes}
			s := listen(t, r, sink, nil)
			res := scu(t, py, s, "store", "SCANNER", "ARCHIVE", corpusPath("MR_small.dcm"))
			if res.Assoc != "accepted" || len(res.Statuses) != 1 || !samePtr(res.Statuses[0], c.status) {
				t.Fatalf("%+v, want status %v", res, c.status)
			}
			if n := len(sink.whole()); n != 0 {
				t.Fatalf("%d files kept past the bound", n)
			}
			if res := scu(t, py, s, "echo", "SCANNER", "ARCHIVE"); res.Assoc != "accepted" {
				t.Fatalf("after a refused store: %+v", res)
			}
		})
	}
}

// ptr returns a pointer to v.
func ptr(v int) *int { return &v }

// samePtr reports whether two optional statuses are equal.
func samePtr(a, b *int) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// TestPanicInAStoreDoesNotStopTheListener guards that a panic serving one
// store is answered out of resources and the listener goes on admitting.
func TestPanicInAStoreDoesNotStopTheListener(t *testing.T) {
	py := python(t)
	s := listen(t, row(), panicSink{}, nil)
	res := scu(t, py, s, "store", "SCANNER", "ARCHIVE", corpusPath("MR_small.dcm"))
	if res.Assoc != "accepted" || len(res.Statuses) != 1 || !isStatus(res.Statuses[0], statusOutOfResources) {
		t.Fatalf("%+v, want A700", res)
	}
	if res := scu(t, py, s, "echo", "SCANNER", "ARCHIVE"); res.Assoc != "accepted" || !isStatus(res.Statuses[0], statusSuccess) {
		t.Fatalf("after a panic: %+v", res)
	}
}

// panicSink panics on every file.
type panicSink struct{}

func (panicSink) Accept(context.Context, receive.Arrival) receive.Outcome { panic("a test's sink") }

// TestStopFinishesTheStoreInFlight guards Stop: a store in flight when Stop
// begins is let finish and answered success, a new caller meanwhile is
// never served (refused, rejected as transient or aborted, as the library
// turns it away while it stops: in each case the sender keeps its file and
// tries again), and Stop returns once the store is done.
func TestStopFinishesTheStoreInFlight(t *testing.T) {
	py := python(t)
	sink := &fakeSink{}
	inFlight, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	s := listenSeams(t, row(), sink, nil, func(l *dimse.Listener) {
		dimse.SetBeforeAccept(l, func() {
			if once.CompareAndSwap(false, true) {
				close(inFlight)
				<-release
			}
		})
	})
	// free lets the held store go on; it runs at the test's end too, so a
	// failure part way never leaves the listener's goroutine held.
	free := sync.OnceFunc(func() { close(release) })
	t.Cleanup(free)
	type sent struct {
		res scuResult
		err error
	}
	result := make(chan sent, 1)
	go func() {
		res, err := runSCU(py, s, "store", "SCANNER", "ARCHIVE", corpusPath("MR_small.dcm"))
		result <- sent{res, err}
	}()
	select {
	case <-inFlight:
	case got := <-result:
		t.Fatalf("the store ended before it reached the sink: %+v, %v", got.res, got.err)
	case <-time.After(waitBound):
		free()
		t.Fatal("timed out waiting for the store to reach the sink")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- s.l.Stop(context.Background()) }()
	waitFor(t, "a new caller to be turned away while stopping", func() bool {
		res, err := runSCU(py, s, "echo", "SCANNER", "ARCHIVE")
		if err == nil && res.Assoc == "rejected" && res.RJ[0] != rjTransient {
			t.Fatalf("while stopping a caller was rejected as permanent: %+v", res)
		}
		return err == nil && res.Assoc != "accepted" && len(res.Statuses) == 0
	})
	select {
	case err := <-stopped:
		free()
		t.Fatalf("Stop returned (%v) with a store in flight", err)
	default:
	}
	free()
	if err := within(t, "Stop to return once the store in flight ended", stopped); err != nil {
		t.Fatal(err)
	}
	got := within(t, "the sender of the store in flight to end", result)
	if got.err != nil {
		t.Fatal(got.err)
	}
	res := got.res
	if res.Assoc != "accepted" || len(res.Statuses) != 1 || !isStatus(res.Statuses[0], statusSuccess) || len(sink.whole()) != 1 {
		t.Fatalf("the store in flight: %+v, %d kept", res, len(sink.whole()))
	}
}

// TestAPortTakenIsDownWithItsReason guards Start's refusal: a port another
// socket holds is a DownError, address in use, with the health's phrase,
// and nothing listens.
func TestAPortTakenIsDownWithItsReason(t *testing.T) {
	if testing.Short() {
		t.Skip("opens a port; left out under -short")
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	port := held.Addr().(*net.TCPAddr).Port
	l, err := dimse.New(dimse.Config{Receiver: row(func(r *tenant.Receiver) { r.DIMSE.Port = port }), BindAddress: "127.0.0.1", Sink: &fakeSink{}})
	if err != nil {
		t.Fatal(err)
	}
	err = l.Start(context.Background())
	var d *dimse.DownError
	if !errors.As(err, &d) || d.Reason != tenant.DownAddressInUse || d.Detail != "the port is taken by another program" {
		t.Fatalf("Start: %v, want address in use", err)
	}
	if l.Addr() != "" {
		t.Fatal("a listener that could not start listens")
	}
}

// TestAProgramOnTheIPv4WildcardIsSeen guards a receiver of every address
// (no bind address): another program that holds the port on 0.0.0.0 makes
// it down, address in use, on every system. Without the check, a dual-stack
// bind on [::] succeeds beside that program on macOS, and IPv4 callers
// reach the other program while the receiver shows up.
func TestAProgramOnTheIPv4WildcardIsSeen(t *testing.T) {
	if testing.Short() {
		t.Skip("opens a port; left out under -short")
	}
	held, err := net.Listen("tcp4", "0.0.0.0:0") // #nosec G102 -- the test stands for another program on the IPv4 wildcard.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	port := held.Addr().(*net.TCPAddr).Port
	l, err := dimse.New(dimse.Config{Receiver: row(func(r *tenant.Receiver) { r.DIMSE.Port = port }), Sink: &fakeSink{}})
	if err != nil {
		t.Fatal(err)
	}
	err = l.Start(context.Background())
	if err == nil {
		_ = l.Stop(context.Background())
		t.Fatal("a receiver of every address started beside a program on the IPv4 wildcard of its port")
	}
	var d *dimse.DownError
	if !errors.As(err, &d) || d.Reason != tenant.DownAddressInUse || d.Detail != tenant.PhraseAddressInUse {
		t.Fatalf("Start: %v, want address in use", err)
	}
}

// TestAReceiverOfEveryAddressTakesBothFamilies guards the same receiver
// with its port free: a caller over IPv4 and one over IPv6 loopback are
// both answered by it.
func TestAReceiverOfEveryAddressTakesBothFamilies(t *testing.T) {
	if testing.Short() {
		t.Skip("opens a port; left out under -short")
	}
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here")
	}
	_ = probe.Close()
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := free.Addr().(*net.TCPAddr).Port
	_ = free.Close()
	l, err := dimse.New(dimse.Config{Receiver: row(func(r *tenant.Receiver) { r.DIMSE.Port = port }), Sink: &fakeSink{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Stop(context.Background()) }()
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 5*time.Second)
		if err != nil {
			t.Fatalf("a caller over %s is not answered: %v", host, err)
		}
		_ = c.Close()
	}
}

// TestABrokenProtocolIsReportedAsAProtocolError guards how an abort is
// reported: a caller that sends, on an association, a PDU of a type PS3.8
// Table 9-11 does not define is aborted (PS3.8 9.3.8) and reported as
// protocol_error with its titles and address, because the cause was the
// caller's.
func TestABrokenProtocolIsReportedAsAProtocolError(t *testing.T) {
	py := python(t)
	s := listen(t, row(), &fakeSink{}, nil)
	res := scu(t, py, s, "garbage", "SCANNER", "ARCHIVE")
	if res.Assoc != "accepted" || res.After != "aborted" {
		t.Fatalf("the broken association: %+v", res)
	}
	waitFor(t, "the broken protocol to be reported", func() bool {
		for _, f := range s.reports.get() {
			if f.failure == tenant.FailureProtocolError {
				if f.caller.Address != "127.0.0.1" || f.caller.CallingAETitle != "SCANNER" {
					t.Fatalf("the broken protocol was reported with %+v", f.caller)
				}
				return true
			}
		}
		return false
	})
}
