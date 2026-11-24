package send

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The environment the outside tests read (docs/testing.md): the Python with
// pynetdicom and pydicom, the directory of dcmtk's programs, and whether a
// missing dcmtk fails the test rather than skips it.
const (
	pythonEnv       = "NOPACS_PYTHON"
	dcmtkEnv        = "NOPACS_DCMTK"
	requireDcmtkEnv = "NOPACS_REQUIRE_DCMTK"
	toolBound       = 10 * time.Second
)

// tool returns the path of one of dcmtk's programs, or skips the test. A
// program of that name that is not dcmtk's is passed over.
func tool(t *testing.T, name string) string {
	t.Helper()
	var candidates []string
	if d := os.Getenv(dcmtkEnv); d != "" {
		candidates = append(candidates, filepath.Join(d, name))
	}
	if p, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), toolBound)
		out, _ := exec.CommandContext(ctx, p, "--version").CombinedOutput() // #nosec G204 G702 -- a test's own tool, from its PATH or its environment.
		cancel()
		if bytes.Contains(out, []byte("dcmtk")) {
			return p
		}
	}
	if os.Getenv(requireDcmtkEnv) != "" {
		t.Fatalf("dcmtk's %s not found, and %s is set; set %s", name, requireDcmtkEnv, dcmtkEnv)
	}
	t.Skipf("dcmtk's %s not found; set %s", name, dcmtkEnv)
	return ""
}

// python returns a Python with pynetdicom and pydicom, or skips the test.
func python(t *testing.T) string {
	t.Helper()
	p := os.Getenv(pythonEnv)
	if p == "" {
		var err error
		if p, err = exec.LookPath("python3"); err != nil {
			t.Skip("no python3, and " + pythonEnv + " is not set")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolBound)
	defer cancel()
	if err := exec.CommandContext(ctx, p, "-I", "-c", "import pynetdicom, pydicom").Run(); err != nil { // #nosec G204 G702 -- the test's own interpreter, from its environment.
		t.Skip("the Python at " + p + " has no pynetdicom; set " + pythonEnv)
	}
	return p
}

// startProcess starts a peer program and waits until addr takes
// connections; the program is ended at the test's end.
func startProcess(t *testing.T, addr string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...) // #nosec G204 -- a test's own tool and arguments.
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("%s said:\n%s", filepath.Base(name), out.String())
		}
	})
	deadline := time.Now().Add(toolBound)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s does not listen on %s", filepath.Base(name), addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// outsideFiles are the files sent to the outside peers: CT in Explicit VR
// Little Endian, MR and secondary capture in RLE, ultrasound in JPEG
// Baseline.
func outsideFiles(t *testing.T) [][]byte {
	var out [][]byte
	for _, n := range []string{"ct2n-6293.dcm", "ct5n-2062.dcm", "ct5n-3353.dcm", "MR_small_RLE.dcm", "SC_rgb_rle_2frame.dcm", "jpeg_3frame_nobot.dcm"} {
		out = append(out, corpus(t, n))
	}
	return out
}

// TestOutsideStorescp sends an archived session to dcmtk's storescp,
// which writes each data set exactly as it arrived (+B): each one's data
// set is byte for byte the stored file's, and the job ends done.
func TestOutsideStorescp(t *testing.T) {
	storescp := tool(t, "storescp")
	f := newFixture(t)
	out := t.TempDir()
	addr := freePort(t)
	_, port, _ := net.SplitHostPort(addr)
	startProcess(t, addr, storescp, "-aet", "PEER", "+B", "+xa", "-od", out, port)
	f.archive("ses-1", scanFiles{"a", outsideFiles(t)})
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 60*time.Second, ended)
	if j.State != tenant.JobDone {
		t.Fatalf("job %s %+v, want done; failures %+v", j.State, j.Counts, f.failures(job.ID))
	}
	compareReceived(t, f, out, func(b []byte) []byte { ds, _ := dataSetOfFile(t, b); return ds })
}

// TestOutsidePynetdicom sends an archived session to a pynetdicom peer,
// which writes each data set as the bytes it received
// (event.request.DataSet): each is byte for byte the stored file's data
// set.
func TestOutsidePynetdicom(t *testing.T) {
	py := python(t)
	f := newFixture(t)
	out := t.TempDir()
	addr := freePort(t)
	_, port, _ := net.SplitHostPort(addr)
	script := filepath.Join(t.TempDir(), "peer.py")
	if err := os.WriteFile(script, []byte(pynetdicomPeer), 0o600); err != nil {
		t.Fatal(err)
	}
	startProcess(t, addr, py, "-I", script, port, out)
	f.archive("ses-1", scanFiles{"a", outsideFiles(t)})
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 60*time.Second, ended)
	if j.State != tenant.JobDone {
		t.Fatalf("job %s %+v, want done; failures %+v", j.State, j.Counts, f.failures(job.ID))
	}
	compareReceived(t, f, out, func(b []byte) []byte { return b })
}

// compareReceived checks that dir holds one file per archived instance,
// named by its SOP Instance UID or holding it, whose data set (as dataSet
// finds it) is the stored file's.
func compareReceived(t *testing.T, f *fixture, dir string, dataSet func([]byte) []byte) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- the test's own directory.
		if err != nil {
			t.Fatal(err)
		}
		for uid := range f.files {
			if strings.Contains(e.Name(), uid) {
				got[uid] = dataSet(b)
			}
		}
	}
	for uid, file := range f.files {
		want, _ := dataSetOfFile(t, file)
		if !bytes.Equal(got[uid], want) {
			t.Errorf("instance %s: the peer wrote %d bytes of data set, not the %d stored", uid, len(got[uid]), len(want))
		}
	}
	t.Logf("%d instances received, each data set equal to the stored one byte for byte", len(got))
}

// pynetdicomPeer is a Storage SCP that takes every storage SOP class in
// every transfer syntax and writes each data set's bytes as received,
// named by its SOP Instance UID.
const pynetdicomPeer = `
import os, sys
from pynetdicom import AE, evt, AllStoragePresentationContexts, ALL_TRANSFER_SYNTAXES

port, out = int(sys.argv[1]), sys.argv[2]

def on_store(event):
    uid = event.request.AffectedSOPInstanceUID
    with open(os.path.join(out, uid + ".raw"), "wb") as f:
        f.write(event.request.DataSet.getvalue())
    return 0x0000

ae = AE(ae_title="PEER")
for cx in AllStoragePresentationContexts:
    ae.add_supported_context(cx.abstract_syntax, ALL_TRANSFER_SYNTAXES)
ae.start_server(("127.0.0.1", port), block=True, evt_handlers=[(evt.EVT_C_STORE, on_store)])
`
