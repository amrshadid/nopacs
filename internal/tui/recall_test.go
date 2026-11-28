package tui

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestRecallShowsWhatItWouldAskForAndNeedsTheTargetTyped guards a recall: the form
// takes a target, a storage and days; the preview says how many objects the store
// would be asked for, which costs money; the confirmation needs the target typed
// and carries the token; a storage that is not of the archive tier is refused with
// the server's words on the form.
func TestRecallShowsWhatItWouldAskForAndNeedsTheTargetTyped(t *testing.T) {
	r := newRig(t, 120, 40, replicaSetup)
	openReplicas(r)
	r.key("c")
	r.expect("Recall from an archive tier in t000", "starts no recall")
	recalls := r.calls("Recall")
	// Days out of range are refused before anything is sent.
	r.fill("sub-1/ses-1", "", "400")
	r.key("enter")
	r.expect("days is a whole number from 1 to 365")
	if r.calls("Recall") != recalls {
		t.Fatalf("a preview was asked for with days out of range")
	}
	// A storage of another tier is refused by the server.
	r.key("shift+tab")
	r.typeText("copy")
	r.key("tab", "ctrl+u")
	r.key("enter")
	r.expect("the storage copy is not of the archive tier")
	r.key("shift+tab", "ctrl+u")
	r.key("tab", "enter")
	if r.calls("Recall") != recalls+2 {
		t.Fatalf("Recall was called %d times for the previews", r.calls("Recall")-recalls)
	}
	r.expect("Recall sub-1/ses-1", "Storages:", "cold", "Objects:", "3 (3000)", "Online already:", "0", "Being recalled:", "1", "To ask for:", "2 (", "at the store's charge", "Confirm token:", "r1.sub-1/ses-1/")

	r.key("c")
	r.expect("asks the store to recall 2 objects, at its charge")
	r.key("enter")
	r.expect("not done: type sub-1/ses-1 exactly")
	if r.calls("Recall") != recalls+2 {
		t.Fatalf("a recall was sent with the target not typed")
	}
	r.typeText("sub-1/ses-1")
	r.key("enter")
	if r.calls("Recall") != recalls+3 {
		t.Fatalf("the recall was not sent")
	}
	r.expect("Replicas of t000", "asked the store to recall 2 objects", "of sub-1/ses-1")
}

// TestVerifyListsASampleOfAStorageAndRepeatsFromItsSeed guards the sample of a
// storage's objects: the list says how many of how many, shows each key, size and
// SHA-256 as the catalog holds them, warns that the keys hold identifiers as
// received, and draws the same sample again from the same seed.
func TestVerifyListsASampleOfAStorageAndRepeatsFromItsSeed(t *testing.T) {
	r := newRig(t, 160, 40, replicaSetup)
	openReplicas(r)
	r.key("v")
	r.expect("Sample copy", "The server reads no object itself")
	samples := r.calls("VerifySample")
	// A count out of range is refused before anything is sent.
	r.key("ctrl+u")
	r.typeText("0")
	r.key("enter", "enter")
	r.expect("count is a whole number from 1 to 10000")
	if r.calls("VerifySample") != samples {
		t.Fatalf("a sample was asked for with a count out of range")
	}
	r.key("shift+tab", "ctrl+u")
	r.typeText("3")
	r.key("tab")
	r.typeText("7")
	r.key("enter")
	r.expect("Sample of copy: 3 of 1000 objects", "data/sub-01/ses-01/t1w/1.2.3.007.dcm", "1.2.3.009.dcm", "sub-01/ses-01/t1w", "keys hold identifiers as received")
	first := r.screen()
	r.key("esc", "v")
	r.key("ctrl+u")
	r.typeText("3")
	r.key("tab")
	r.typeText("7")
	r.key("enter")
	for _, want := range []string{"1.2.3.007.dcm", "1.2.3.008.dcm", "1.2.3.009.dcm"} {
		if !strings.Contains(first, want) || !strings.Contains(r.screen(), want) {
			t.Errorf("the same seed did not draw %s again", want)
		}
	}
}

// TestRecallAndSampleKeysFollowTheRole guards who is offered them: a reader sees
// the replicas and neither key; an operator is offered both.
func TestRecallAndSampleKeysFollowTheRole(t *testing.T) {
	open := func(role v1.Role) *rig {
		r := openKeyTenant(t, []keyGrant{{tenant: "t000", role: role}}, replicaSetup)
		r.key("b")
		r.expect("Replicas of t000")
		helpOf(r, "Replicas of t000")
		return r
	}
	r := open(v1.Role_ROLE_READER)
	r.refuse("recall", "sample", "backfill")
	r = open(v1.Role_ROLE_OPERATOR)
	r.expect("recall", "sample", "backfill")
}
