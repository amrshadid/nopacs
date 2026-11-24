package servecmd_test

import (
	"encoding/json"
	"testing"
)

// TestATrialReadsAHeldFileOnTheRealServer guards TryProfile over a held
// file on the real server, which reads its bytes through the tenant's
// running pipeline (the supervisor, the pipeline's read of the incoming
// area, the API's part for held files): the file the pipeline held is
// tried as its receiver would, its values before are the held bytes', and
// the answer names the record and the receiver.
func TestATrialReadsAHeldFileOnTheRealServer(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a server process; left out under -short")
	}
	s, id, _ := heldDifferentFile(t)
	post(t, s.hc, "/nopacs.v1.ProfileService/CreateProfileVersion",
		`{"tenant":"`+s.tenant+`","name":"research","document":{"bases":["annex-e/basic:2026d"]}}`)
	body := post(t, s.hc, "/nopacs.v1.ProfileService/TryProfile",
		`{"tenant":"`+s.tenant+`","profile":{"version":{"name":"research"}},"quarantineRecords":["`+id+`"]}`)
	var got struct {
		Results []struct {
			QuarantineRecord     string `json:"quarantineRecord"`
			Receiver             string `json:"receiver"`
			DeidentificationOnly bool   `json:"deidentificationOnly"`
			Elements             []struct {
				Path   string `json:"path"`
				Before string `json:"before"`
			} `json:"elements"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%v: %s", err, body)
	}
	r := got.Results[0]
	name := ""
	for _, e := range r.Elements {
		if e.Path == "00100010" {
			name = e.Before
		}
	}
	if r.QuarantineRecord != id || r.Receiver == "" || r.DeidentificationOnly || name != "Doe^Paula" {
		t.Errorf("record %q, receiver %q, de-identification only %v, the name before %q", r.QuarantineRecord, r.Receiver, r.DeidentificationOnly, name)
	}
}
