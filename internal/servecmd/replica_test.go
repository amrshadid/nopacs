package servecmd_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/servecmd"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// attachReplicaOffline adds the directory storage copy to the stopped
// server's tenant and attaches it as the replica of main, straight in the
// tenant's database, as the replica service will once it is served.
func attachReplicaOffline(t *testing.T, dataDir, roots, tenantName string) {
	t.Helper()
	ctx := context.Background()
	m, err := sqlite.OpenMaster(ctx, filepath.Join(dataDir, servecmd.MasterFileName))
	if err != nil {
		t.Fatal(err)
	}
	row, err := m.TenantByName(ctx, tenantName)
	_ = m.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := (driver.SQLiteTenants{DataDir: dataDir}).Open(ctx, row.ID, tenantName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	p := st.(interface {
		tenant.Pipeline
		tenant.Replication
	})
	audit := master.AuditEntry{ID: "a00000001", Time: time.Now(), LocalUser: "test", Method: "Test", TenantName: tenantName, Outcome: master.OutcomeDone}
	if err := os.MkdirAll(filepath.Join(roots, tenantName, "copy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddStorage(ctx, tenant.Storage{Name: "copy", Type: tenant.StorageDirectory, Tier: tenant.TierOnline,
		Directory: tenant.DirectoryStorage{Root: "fast", Path: "copy"}}, time.Now(), audit, nil); err != nil {
		t.Fatal(err)
	}
	audit.ID = "a00000002"
	if _, _, _, err := p.AttachReplica(ctx, "copy", "main", time.Now(), audit, nil); err != nil {
		t.Fatal(err)
	}
}

// TestAReplicaIsFilledThroughServe guards the replicator as serve runs it,
// in a real server process: a tenant whose storage main has the replica
// copy receives the CT study from dcmtk's storescu, the reception passing
// the switch gate, and within a minute of the reception's end the replica
// holds every object and every scan's manifest and metadata of the
// primary, byte for byte, as Python's hashlib reads both directories.
func TestAReplicaIsFilledThroughServe(t *testing.T) {
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
	hc := socketClient(dataDir)
	const tn = "rep1"
	port := freePort(t)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting",
		`{"setting":{"name":"storage.directory-roots","scope":{"installation":{}},"value":`+quoted("fast="+roots)+`}}`)
	post(t, hc, "/nopacs.v1.TenantService/CreateTenant", `{"name":"`+tn+`","databaseType":"DATABASE_TYPE_SQLITE"}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"receiver.ports","scope":{"tenant":"`+tn+`"},"value":"`+strconv.Itoa(port)+`"}}`)
	post(t, hc, "/nopacs.v1.SettingsService/SetSetting", `{"setting":{"name":"receiver.allow-as-received","scope":{"tenant":"`+tn+`"},"value":"true"}}`)
	post(t, hc, "/nopacs.v1.StorageService/AddStorage", `{"tenant":"`+tn+`","storage":{"name":"main","directory":{"root":"fast","path":"main"}}}`)
	post(t, hc, "/nopacs.v1.ReceiverService/AddReceiver", `{"tenant":"`+tn+`","receiver":{"name":"scanner","enabled":true,"storage":"main",`+
		`"quietPeriod":"5s","dimse":{"port":`+strconv.Itoa(port)+`,"aeTitle":"ARCHIVE","allowedCallers":{"rules":[{"callingAeTitle":"STORESCU"}]}}}}`)
	hc.CloseIdleConnections()
	stopServer(t, child)
	attachReplicaOffline(t, dataDir, roots, tn)

	child = startChildEnv(t, []string{childRealEnv + "=1"}, "serve", "--data-dir", dataDir)
	waitServing(t, dataDir, child)
	defer stopServer(t, child)
	hc = socketClient(dataDir)
	defer hc.CloseIdleConnections()
	waitWithin(t, "the receiver to be up", 30*time.Second, func() bool {
		return bytes.Contains(post(t, hc, "/nopacs.v1.ReceiverService/ListReceivers", `{"tenant":"`+tn+`"}`), []byte(`"RECEIVER_STATE_UP"`))
	})
	var files []string
	for _, f := range ctStudy {
		files = append(files, filepath.Join("..", "receive", "testdata", f))
	}
	sendFiles(t, scu, port, files...)
	waitBody(t, "the reception to be done", func() bool {
		return bytes.Contains(post(t, hc, "/nopacs.v1.ReceptionService/ListReceptions", `{"tenant":"`+tn+`"}`), []byte(`"RECEPTION_STEP_DONE"`))
	})
	scans := func(tree map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range tree {
			if strings.HasPrefix(k, "data/") || strings.HasPrefix(k, "meta/sub-") {
				out[k] = v
			}
		}
		return out
	}
	mainTree := scans(outsideTree(t, filepath.Join(roots, tn, "main")))
	if n := len(mainTree); n < len(ctStudy) {
		t.Fatalf("the primary holds %d objects, want at least %d", n, len(ctStudy))
	}
	waitBody(t, "the replica to hold the primary's objects", func() bool {
		cp := scans(outsideTree(t, filepath.Join(roots, tn, "copy")))
		if len(cp) != len(mainTree) {
			return false
		}
		for k, v := range mainTree {
			if cp[k] != v {
				return false
			}
		}
		return true
	})
}
