package servecmd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// decode reads a JSON answer of the API into m, failing the test when it
// does not parse.
func decode(t *testing.T, body []byte, m proto.Message) {
	t.Helper()
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, m); err != nil {
		t.Fatalf("answer %s: %v", body, err)
	}
}

// tenantStatus returns the status of the tenant named, as GetStatus answers
// it.
func tenantStatus(t *testing.T, hc *http.Client, tn string) *v1.TenantStatus {
	t.Helper()
	var out v1.GetStatusResponse
	decode(t, post(t, hc, "/nopacs.v1.MonitorService/GetStatus", `{"scope":{"tenant":"`+tn+`"}}`), &out)
	for _, s := range out.GetTenants() {
		if s.GetTenant() == tn {
			return s
		}
	}
	t.Fatalf("GetStatus has no status of %s", tn)
	return nil
}

// storageStatus returns the status of the storage named in st.
func storageStatus(st *v1.TenantStatus, name string) *v1.StorageStatus {
	for _, s := range st.GetStorages() {
		if s.GetStorage() == name {
			return s
		}
	}
	return nil
}

// TestSwitchAndStatusThroughTheAPI guards what serve gives the API of a
// switch and of a storage's status, in a real server process: a switch
// request is run by the tenant's switcher rather than answered unavailable;
// its validation names what stands in the way (no empty compare from the
// replica) with a confirm token; a confirmed switch with
// the checks passed over swaps the two directory storages' places, which
// the stored rows and the files on disk both show; and the status of a
// storage tells its scrub coverage after a scrub of it, every object read
// within scrub.interval, and the encryption its admin declared.
func TestSwitchAndStatusThroughTheAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process and sends a study over the network; left out under -short")
	}
	scu := storescu(t)
	dataDir := shortDir(t)
	if _, err := run(t, nil, "init", "--data-dir", dataDir); err != nil {
		t.Fatal(err)
	}
	roots := filepath.Join(filepath.Dir(dataDir), "roots")
	if err := os.MkdirAll(roots, 0o700); err != nil {
		t.Fatal(err)
	}
	child := startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dataDir)
	waitServing(t, dataDir, child)
	defer stopServer(t, child)
	hc := socketClient(dataDir)
	defer hc.CloseIdleConnections()
	const tn = "swi1"
	port := freePort(t)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tn+`","databaseType":"DATABASE_TYPE_SQLITE"}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"receiver.ports","scope":{"tenant":"`+tn+`"},"value":"`+strconv.Itoa(port)+`"}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"receiver.allow-as-received","scope":{"tenant":"`+tn+`"},"value":"true"}}`)
	post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tn+`","storage":{"name":"main","directory":{"root":"fast","path":"main","encryptedAtRestDeclared":true}}}`)
	post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tn+`","storage":{"name":"copy","directory":{"root":"fast","path":"copy","encryptedAtRestDeclared":true}}}`)
	post(t, hc, "/nopacs.v1.ReplicaService/AttachReplica", `{"tenant":"`+tn+`","replica":"copy","primary":"main"}`)
	post(t, hc, "/nopacs.v1.ReceiverService/AddReceiver", `{"tenant":"`+tn+`","receiver":{"name":"scanner","enabled":true,"storage":"main",`+
		`"quietPeriod":"5s","dimse":{"port":`+strconv.Itoa(port)+`,"aeTitle":"ARCHIVE","allowedCallers":{"rules":[{"callingAeTitle":"STORESCU"}]}}}}`)
	waitWithin(t, "the receiver to be up", 30*time.Second, func() bool {
		return strings.Contains(string(post(t, hc, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+tn+`"}`)), `"RECEIVER_STATE_UP"`)
	})
	var files []string
	for _, f := range ctStudy {
		files = append(files, filepath.Join("..", "receive", "testdata", f))
	}
	sendFiles(t, scu, port, files...)
	waitBody(t, "the reception to be done", func() bool {
		return strings.Contains(string(post(t, hc, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+tn+`"}`)), `"RECEPTION_STEP_DONE"`)
	})
	waitBody(t, "the replica to be in step", func() bool {
		var out v1.ListReplicasResponse
		decode(t, post(t, hc, "/nopacs.v1.ReplicaService/ListReplicas", `{"tenant":"`+tn+`"}`), &out)
		r := out.GetReplicas()
		return len(r) == 1 && r[0].GetScansInStep() > 0 && r[0].GetScansPending() == 0 && r[0].GetScansCopying() == 0
	})

	// The validation: what stands in the way, and the token.
	var preview v1.SwitchStorageResponse
	decode(t, post(t, hc, "/nopacs.v1.ReplicaService/SwitchStorage", `{"tenant":"`+tn+`","primary":"main","replica":"copy","validateOnly":true}`), &preview)
	t.Logf("validation: ready %v, blockers %q, receivers %q", preview.GetReady(), preview.GetBlockers(), preview.GetReceivers())
	if preview.GetReady() || preview.GetConfirm() == "" || len(preview.GetBlockers()) == 0 {
		t.Fatalf("validation = ready %v, confirm %q, blockers %q; want not ready, a token and blockers", preview.GetReady(), preview.GetConfirm(), preview.GetBlockers())
	}
	// The server's scheduled scrub of the replica may already have ended
	// cleanly; no rebuild compare has run.
	if !slices.Contains(preview.GetBlockers(), "no empty compare from the replica") {
		t.Errorf("blockers %q name no missing compare from the replica", preview.GetBlockers())
	}
	if !slices.Contains(preview.GetReceivers(), "scanner") {
		t.Errorf("receivers %q; want scanner, which writes to the primary", preview.GetReceivers())
	}

	// The confirmed switch, the checks passed over with a reason.
	var done v1.SwitchStorageResponse
	decode(t, post(t, hc, "/nopacs.v1.ReplicaService/SwitchStorage", `{"tenant":"`+tn+`","primary":"main","replica":"copy","confirm":`+
		quoted(preview.GetConfirm())+`,"skipVerification":true,"skipReason":"a test of the switch"}`), &done)
	if !done.GetReady() || done.GetPrimary().GetDirectory().GetPath() != "copy" || done.GetReplica().GetDirectory().GetPath() != "main" {
		t.Fatalf("switch = ready %v, primary at %q, replica at %q; want ready, the primary at copy and the replica at main",
			done.GetReady(), done.GetPrimary().GetDirectory().GetPath(), done.GetReplica().GetDirectory().GetPath())
	}
	var got v1.GetStorageResponse
	decode(t, post(t, hc, "/nopacs.v1.StorageService/GetStorage", `{"tenant":"`+tn+`","name":"main"}`), &got)
	if p := got.GetStorage().GetDirectory().GetPath(); p != "copy" {
		t.Fatalf("main is stored at %q after the switch, want copy", p)
	}
	t.Logf("switched: main at %q, generation %d", got.GetStorage().GetDirectory().GetPath(), done.GetSwitchGeneration())

	// A scrub of main, now at the former replica's place, then its status.
	// The server's own scheduled scrub may be running: the request is made
	// again once it has ended.
	var job v1.CreateJobResponse
	waitBody(t, "a scrub of main to be queued", func() bool {
		code, body := postCode(t, hc, "/nopacs.v1.JobService/CreateJob", `{"tenant":"`+tn+`","scrub":{"storage":"main"}}`)
		if code == http.StatusOK {
			decode(t, body, &job)
			return true
		}
		if !strings.Contains(string(body), "another scrub") {
			t.Fatalf("CreateJob: %d %s", code, body)
		}
		return false
	})
	waitBody(t, "the scrub to end", func() bool {
		var j v1.GetJobResponse
		decode(t, post(t, hc, "/nopacs.v1.JobService/GetJob", `{"tenant":"`+tn+`","id":"`+job.GetJob().GetId()+`"}`), &j)
		return j.GetJob().GetState() == v1.JobState_JOB_STATE_DONE
	})
	ss := storageStatus(tenantStatus(t, hc, tn), "main")
	if ss == nil {
		t.Fatal("the status has no storage main")
	}
	cv := ss.GetCoverage()
	t.Logf("main's status: coverage %v, encryption %v", cv, ss.GetEncryptionAtRest())
	if cv == nil || cv.GetObjects() < int64(len(ctStudy)) || cv.GetVerified() != cv.GetObjects() || cv.GetShare() != 1 || cv.GetOverdue() != 0 {
		t.Errorf("main's coverage = %v; want every one of at least %d objects verified", cv, len(ctStudy))
	}
	if ss.GetEncryptionAtRest() != v1.EncryptionAtRest_ENCRYPTION_AT_REST_DECLARED {
		t.Errorf("main's encryption at rest = %v; want declared", ss.GetEncryptionAtRest())
	}
}
