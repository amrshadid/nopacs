package cli_test

import (
	"strings"
	"testing"
)

// TestReceiverRouteCommands guards the route commands: add in order and at
// a position, the table a person reads (conditions, storage, profile,
// senders, default), set of one route's parts, move, remove, the default,
// the document of --json, and the refusals of a route that does not exist
// and a name taken.
func TestReceiverRouteCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast")
	r.must("storage", "add", "lab1", "lab-a", "--type", "dir", "--root", "fast")
	r.must("sender", "add", "lab1", "pacs", "--type", "dimse", "--host", "pacs.lab.example", "--port", "104", "--called-aet", "ORTHANC", "--calling-aet", "NOPACS")
	r.must("receiver", "add", "lab1", "scanner1", "--storage", "main", "--port", "11112", "--aet", "NOPACS", "--any-caller", "--profile", "research@1")

	expect(t, r.run("receiver", "route", "list", "lab1", "scanner1"), 0,
		"#  ROUTE  WHEN  STORAGE  PROFILE  FORWARD TO\nA study no route chooses: the receiver's storage, main\n", "")
	r.must("receiver", "route", "add", "lab1", "scanner1", "lab-a", "--storage", "lab-a", "--equals", "calling-aet=LAB_A",
		"--not-pattern", "modality=SR|PR", "--profile", "strict@2", "--forward-to", "pacs")
	r.must("receiver", "route", "add", "lab1", "scanner1", "first", "--storage", "main", "--present", "software-versions", "--no-forward", "--position", "1")
	expect(t, r.run("receiver", "route", "list", "lab1", "scanner1"), 0,
		"#  ROUTE  WHEN                                              STORAGE  PROFILE     FORWARD TO\n"+
			"1  first  software-versions present                         main     receiver's  none\n"+
			"2  lab-a  calling-aet = \"LAB_A\" and modality not ~ \"SR|PR\"  lab-a    strict@2    pacs\n"+
			"A study no route chooses: the receiver's storage, main\n", "")
	if show := r.run("receiver", "show", "lab1", "scanner1"); !strings.Contains(show.stdout, "Routing:           2 routes (nopacs receiver route list)\n") {
		t.Errorf("show does not say the receiver routes:\n%s", show.stdout)
	}
	set := r.run("receiver", "route", "set", "lab1", "scanner1", "lab-a", "--receiver-profile", "--receiver-senders", "--no-conditions")
	if set.code != 0 || !strings.Contains(set.stdout, "2  lab-a  every study                lab-a    receiver's  receiver's\n") {
		t.Errorf("set:\n%+v", set)
	}
	move := r.run("receiver", "route", "move", "lab1", "scanner1", "lab-a", "--to", "1")
	if move.code != 0 || !strings.Contains(move.stdout, "1  lab-a") || !strings.Contains(move.stdout, "2  first") {
		t.Errorf("move:\n%+v", move)
	}
	def := r.run("receiver", "route", "default", "lab1", "scanner1", "quarantine")
	if def.code != 0 || !strings.Contains(def.stdout, "A study no route chooses: held as unrouted\n") {
		t.Errorf("default quarantine:\n%+v", def)
	}
	doc := document(t, r.run("receiver", "route", "list", "lab1", "scanner1", "--json"))
	routes, _ := doc["routes"].([]any)
	if len(routes) != 2 || doc["quarantineUnmatched"] != true || routes[0].(map[string]any)["name"] != "lab-a" {
		t.Errorf("list --json: %v", doc)
	}
	if res := r.run("receiver", "route", "add", "lab1", "scanner1", "first", "--storage", "main"); res.code == 0 || !strings.Contains(res.stderr, "has a route named") {
		t.Errorf("a name taken: %+v", res)
	}
	if res := r.run("receiver", "route", "remove", "lab1", "scanner1", "nothing"); res.code != 4 {
		t.Errorf("a route that does not exist: %+v", res)
	}
	if res := r.run("receiver", "route", "add", "lab1", "scanner1", "x", "--storage", "main", "--equals", "body=HEAD"); res.code != 2 || !strings.Contains(res.stderr, "not a subject") {
		t.Errorf("a subject that is not one: %+v", res)
	}
	r.must("receiver", "route", "remove", "lab1", "scanner1", "first")
	r.must("receiver", "route", "remove", "lab1", "scanner1", "lab-a")
	expect(t, r.run("receiver", "route", "default", "lab1", "scanner1", "storage"), 0,
		"#  ROUTE  WHEN  STORAGE  PROFILE  FORWARD TO\nA study no route chooses: the receiver's storage, main\n", "")
}
