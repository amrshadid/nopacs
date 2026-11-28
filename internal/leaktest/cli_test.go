package leaktest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// cliCase is one command line: the arguments after the program's name.
type cliCase struct {
	// args are the words of the command, its flags and its operands.
	args []string
	// last runs the case after the others of its key.
	last bool
}

// cliFiles are the files the commands read: made once, in a directory of the
// test's own.
type cliFiles struct {
	dir, basic, lists, own, subjects, lookup, home, out string
}

// writeCLIFiles makes the files the commands name.
func writeCLIFiles(t *testing.T, w *world, files []seedFile) cliFiles {
	t.Helper()
	dir := t.TempDir()
	f := cliFiles{dir: dir, home: filepath.Join(dir, "home"), out: filepath.Join(dir, "out")}
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f.basic = write("basic.json", `{"bases":["annex-e/basic:2026d"]}`)
	f.lists = write("lists.json", w.profile)
	f.own = write("own.dcm", string(w.own))
	f.subjects = write("subjects.txt", w.deid.subject+"\n")
	f.lookup = write("lookup.csv", "alpha,A\n")
	if err := os.MkdirAll(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.out, 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

// cliCases returns the command lines the keys run, by the command they
// exercise ("audit list"): each command at least once, as a masked key may
// meet it, with the real names of the world and with names that are not
// there, so that the refusals, the not-found answers and the answers all
// appear.
func cliCases(w *world, f cliFiles, key string) map[string][]cliCase {
	T, s, r := w.tenant, w.deid, w.raw
	path := func(x scanRef) string { return x.subject + "/" + x.session + "/" + x.scan }
	ulid := "01jd0000000000000000000000"
	c := func(args ...string) cliCase { return cliCase{args: args} }
	last := func(args ...string) cliCase { return cliCase{args: args, last: true} }
	m := map[string][]cliCase{
		"agent-guide":       {c("agent-guide")},
		"version":           {c("version")},
		"whoami":            {c("whoami")},
		"audit checkpoints": {c("audit", "checkpoints"), c("audit", "checkpoints", "--tenant", T)},
		"audit export":      {c("audit", "export", "--out", filepath.Join(f.out, "audit-"+key+".jsonl")), c("audit", "export", "--tenant", T)},
		"audit list": {c("audit", "list"), c("audit", "list", "--tenant", T, "--outcome", "refused"), c("audit", "list", "--tenant", T, "--outcome", "done"),
			c("audit", "list", "--outcome", "failed", "--all")},
		"audit public-key":         {c("audit", "public-key")},
		"audit verify":             {c("audit", "verify"), c("audit", "verify", "--tenant", T)},
		"backup create":            {c("backup", "create", "--out", filepath.Join(f.out, "backup-"+key+".bak"))},
		"catalog scans":            {c("catalog", "scans", T), c("catalog", "scans", T, s.subject, s.session), c("catalog", "scans", T, r.subject, "--receiver", "scan-raw")},
		"catalog sessions":         {c("catalog", "sessions", T), c("catalog", "sessions", T, s.subject)},
		"catalog subjects":         {c("catalog", "subjects", T), c("catalog", "subjects", "nosuch")},
		"context add":              {c("context", "add", "agent-"+key, "--server", "http://"+"127.0.0.1:1", "--key-file", f.basic, "--replace")},
		"context list":             {c("context", "list")},
		"context remove":           {c("context", "remove", "agent-"+key)},
		"context show":             {c("context", "show", "agent-"+key)},
		"context use":              {c("context", "use", "agent-"+key)},
		"correction allow":         {c("correction", "allow", T, ulid, "--subject", "sub-nosuchsubject", "--reason", "x")},
		"correction batch-targets": {c("correction", "batch-targets", T, w.jobID)},
		"correction delete": {c("correction", "delete", T, path(s), "--reason", "x", "--preview"), c("correction", "delete", T, "sub-nosuchsubject", "--reason", "x", "--yes"),
			c("correction", "delete", T, path(r), "--reason", "x")},
		"correction delete-batch": {c("correction", "delete-batch", T, "--file", f.subjects, "--reason", "x", "--preview"), c("correction", "delete-batch", T, "--from-search", "x", "--reason", "x")},
		"correction edit": {c("correction", "edit", T, path(s), "--set", "SeriesDescription=x", "--reason", "x", "--preview"),
			c("correction", "edit", T, path(s), "--remove", "PatientComments", "--reason", "x", "--preview", "--show-values")},
		"correction list":    {c("correction", "list", T), c("correction", "list", T, "--kind", "delete")},
		"correction purge":   {c("correction", "purge", T, ulid, "--yes")},
		"correction relabel": {c("correction", "relabel", T, "sub-nosuchsubject", "--to", "sub-other", "--reason", "x")},
		"correction replace": {last("correction", "replace", T, w.quarantined[0], "--reason", "x")},
		"correction show":    {c("correction", "show", T, ulid)},
		"correction undo":    {c("correction", "undo", T, ulid, "--reason", "x")},
		"deid check":         {c("deid", "check", T, path(s))},
		"deid code":          {c("deid", "code", T, "--patient-id", seedIDForCode), c("deid", "code", T, "--attribute", "AccessionNumber", "--value", seedAccession)},
		"deid dry-run":       {c("deid", "dry-run", T, "research@1", path(s)), c("deid", "dry-run", T, "--own", path(s))},
		"download":           {c("download", T, path(s), "--to", filepath.Join(f.out, "dl-"+key), "--quiet"), c("download", T, path(r), "--to", filepath.Join(f.out, "dlraw-"+key), "--quiet")},
		"grant list":         {c("grant", "list", "--installation"), c("grant", "list", "--tenant", T), c("grant", "list", "--key-id", w.victim.id)},
		"grant remove":       {c("grant", "remove", w.victim.id, "--tenant", T)},
		"grant set":          {c("grant", "set", w.victim.id, "--role", "reader", "--tenant", T)},
		"hold list":          {c("hold", "list", T)},
		"hold place":         {c("hold", "place", T, path(s), "--reason", "x")},
		"hold release":       {c("hold", "release", T, ulid, "--reason", "x", "--yes")},
		"hold show":          {c("hold", "show", T, w.holdID), c("hold", "show", T, ulid)},
		"job cancel":         {c("job", "cancel", T, "j1")},
		"job create":         {c("job", "create", T, "--sender", "pacs", path(s))},
		"job failures":       {c("job", "failures", T, w.jobID)},
		"job files":          {c("job", "files", T, w.jobID)},
		"job finding":        {c("job", "finding", T, ulid)},
		"job findings":       {c("job", "findings", T)},
		"job import":         {c("job", "import", T, "scan-deid", f.out)},
		"job list":           {c("job", "list", T), c("job", "list", T, "--in-progress")},
		"job pause":          {c("job", "pause", T, w.jobID)},
		"job rebuild":        {c("job", "rebuild", T, "--mode", "compare")},
		"job resume":         {c("job", "resume", T, w.jobID)},
		"job retry":          {c("job", "retry", T, "j1")},
		"job scrub":          {c("job", "scrub", T, "--storage", "main")},
		"job show":           {c("job", "show", T, w.jobID)},
		"job units":          {c("job", "units", T, w.jobID)},
		"job watch":          {c("job", "watch", T, w.jobID)},
		"key create":         {c("key", "create", "--holder", "agent made", "--masked", "--no-expiry"), c("key", "create", "--holder", "agent made")},
		"key list":           {c("key", "list"), c("key", "list", "--tenant", T)},
		"key reinstate":      {c("key", "reinstate", w.victim.id)},
		"key revoke":         {last("key", "revoke", w.victim.id), last("key", "revoke", w.plainVictim.id)},
		"key rotate":         {c("key", "rotate", w.victim.id), c("key", "rotate", w.plainVictim.id)},
		"key set":            {c("key", "set", w.victim.id, "--network", "127.0.0.1/32")},
		"key show":           {c("key", "show", w.victim.id), c("key", "show", w.plainVictim.id)},
		"profile create":     {c("profile", "create", T, "agent-profile", "--document", f.basic), c("profile", "create", T, "research", "--document", f.lists)},
		"profile describe":   {c("profile", "describe", T, "research@1"), c("profile", "describe", T, "--document", f.basic), c("profile", "describe", T, "--document", f.lists)},
		"profile diff":       {c("profile", "diff", T, "research@1", "research@1")},
		"profile explain": {c("profile", "explain", T, "--document", f.basic, f.own), c("profile", "explain", T, "--profile", "research@1", f.own),
			c("profile", "explain", T, "--document", f.basic, "--record", w.quarantined[0])},
		"profile export":        {c("profile", "export", T, "research@1")},
		"profile get":           {c("profile", "get", T, "research"), c("profile", "get", T, "research@1", "--full")},
		"profile list":          {c("profile", "list", T)},
		"profile lookup import": {c("profile", "lookup", "import", T, "agent-table", f.lookup)},
		"profile lookup list":   {c("profile", "lookup", "list", T)},
		"profile try": {c("profile", "try", T, "--document", f.basic, f.own), c("profile", "try", T, "--document", f.basic, f.own, "--show-values"),
			c("profile", "try", T, "--profile", "research@1", f.own), c("profile", "try", T, "--document", f.lists, f.own, "--changed-only"),
			c("profile", "try", T, "--document", f.basic, "--record", w.quarantined[0]), c("profile", "try", "--local", "--document", f.basic, f.own)},
		"profile validate":   {c("profile", "validate", T, "--document", f.basic), c("profile", "validate", T, "--document", f.lists)},
		"profile versions":   {c("profile", "versions", T, "research")},
		"quarantine list":    {c("quarantine", "list", T), c("quarantine", "list", T, "--reason", "residual-value")},
		"quarantine reject":  {last("quarantine", "reject", T, w.quarantined[1], "--reason", "x", "--yes")},
		"quarantine release": {last("quarantine", "release", T, w.quarantined[0], "--storage", "main")},
		"quarantine show": {c("quarantine", "show", T, w.quarantined[0]), c("quarantine", "show", T, w.quarantined[1], "--header"),
			c("quarantine", "show", T, w.quarantined[0], "--header", "--show-values"), c("quarantine", "show", T, "q1")},
		"recall":          {c("recall", T, path(s), "--dry-run")},
		"receiver add":    {c("receiver", "add", T, "extra", "--storage", "main", "--port", "11999", "--aet", "EXTRA", "--any-caller")},
		"receiver list":   {c("receiver", "list", T)},
		"receiver remove": {c("receiver", "remove", T, "no-such-receiver", "--yes")},
		"route create":    {c("route", "create", T, "no-such-receiver", f.basic)},
		"route get":       {c("route", "get", T, "scan-deid")},
		"route list":      {c("route", "list", T, "scan-deid")},
		"route activate":  {c("route", "activate", T, "no-such-receiver", "1")},
		"route export":    {c("route", "export", T, "scan-deid")},
		"route explain": {c("route", "explain", T, "scan-deid", "--file", f.own, "--calling-aet", "STORESCU"),
			c("route", "explain", T, "scan-deid", "--instance", path(s)+"/"+s.instanceID), c("route", "explain", T, "scan-deid", "--file", f.own, "--show-values")},
		"route dry-run":         {c("route", "dry-run", T, "scan-deid")},
		"route hits":            {c("route", "hits", T, "scan-deid")},
		"correction move":       {c("correction", "move", T, s.subject+"/"+s.session, "--to", "sub-nosuchsubject", "--reason", "x", "--preview")},
		"correction merge":      {c("correction", "merge", T, s.subject, "--into", "sub-nosuchsubject", "--reason", "x", "--preview")},
		"receiver set":          {c("receiver", "set", T, "scan-deid", "--enabled=true")},
		"receiver show":         {c("receiver", "show", T, "scan-deid"), c("receiver", "show", T, "scan-raw"), c("receiver", "show", T, "web-deid")},
		"reception discard":     {last("reception", "discard", T, w.receptions[0], "--reason", "x", "--yes")},
		"reception list":        {c("reception", "list", T), c("reception", "list", T, "--receiver", "scan-raw")},
		"reception retry":       {last("reception", "retry", T, w.receptions[0])},
		"reception show":        {c("reception", "show", T, w.receptions[0]), c("reception", "show", T, w.receptions[len(w.receptions)-1]), c("reception", "show", T, "r1")},
		"repair":                {c("repair", T, "main", "--all")},
		"replica attach":        {c("replica", "attach", T, "spare", "--primary", "main")},
		"replica backfill":      {c("replica", "backfill", T, "spare")},
		"replica detach":        {c("replica", "detach", T, "spare", "--yes")},
		"replica list":          {c("replica", "list", T)},
		"replica verify":        {c("replica", "verify", T, "main", "--count", "1")},
		"retention clock":       {c("retention", "clock", T)},
		"retention dispose":     {c("retention", "dispose", T, "--rule", "agent-rule", "--reason", "x", "--preview")},
		"retention due":         {c("retention", "due", T)},
		"retention rule add":    {c("retention", "rule", "add", T, "agent-rule", "--storage", "main", "--keep-at-least", "1y")},
		"retention rule list":   {c("retention", "rule", "list", T)},
		"retention rule remove": {c("retention", "rule", "remove", T, "agent-rule", "--yes")},
		"retention rule set":    {c("retention", "rule", "set", T, "agent-rule", "--keep-at-least", "2y")},
		"retention rule show":   {c("retention", "rule", "show", T, "agent-rule")},
		"scan show":             {c("scan", "show", T, path(s)), c("scan", "show", T, path(s), "--metadata"), c("scan", "show", T, path(r), "--metadata")},
		"sender add":            {c("sender", "add", T, "pacs2", "--type", "dimse", "--host", "127.0.0.1", "--port", "1", "--called-aet", "PACS", "--calling-aet", "NOPACS")},
		"sender list":           {c("sender", "list", T)},
		"sender remove":         {c("sender", "remove", T, "no-such-sender", "--yes")},
		"sender set":            {c("sender", "set", T, "pacs", "--enabled=true")},
		"sender show":           {c("sender", "show", T, "pacs")},
		"sender test":           {c("sender", "test", T, "pacs")},
		"setting get":           {c("setting", "get", "retention.max-per-pass", "--tenant", T), c("setting", "get", "receiver.header-limit")},
		"setting list":          {c("setting", "list"), c("setting", "list", "--tenant", T)},
		"setting set":           {c("setting", "set", "retention.max-per-pass", "500", "--tenant", w.other)},
		"setting unset":         {c("setting", "unset", "retention.max-per-pass", "--tenant", w.other)},
		"storage add":           {c("storage", "add", T, "extra", "--type", "dir", "--root", "fast", "--path", "extra")},
		"storage list":          {c("storage", "list", T)},
		"storage remove":        {last("storage", "remove", T, "spare", "--yes")},
		"storage set":           {c("storage", "set", T, "spare", "--capacity", "1MB")},
		"storage show":          {c("storage", "show", T, "main"), c("storage", "show", T, "raw")},
		"storage status":        {c("storage", "status", T), c("storage", "status", T, "main")},
		"storage test":          {c("storage", "test", T, "main")},
		"switch":                {c("switch", T, "main", "spare", "--validate")},
		"tenant create":         {c("tenant", "create", "lab8")},
		"tenant export":         {c("tenant", "export", T, "--out", filepath.Join(f.out, "t-"+key+".tenant"), "--key-out", filepath.Join(f.out, "t-"+key+".key"))},
		"tenant list":           {c("tenant", "list")},
		"tenant reclaim":        {c("tenant", "reclaim", T)},
		"tenant remove":         {last("tenant", "remove", w.other, "--yes"), last("tenant", "remove", "nosuch", "--yes")},
		"tenant show":           {c("tenant", "show", T), c("tenant", "show", "nosuch")},
		"tenant status":         {c("tenant", "status", T)},
		"watch":                 {c("watch", T, "--count", "50")},
	}
	return m
}

// seedAccession is the Accession Number of the first seeded file, asked of
// the forward lookup.
var seedAccession = func() string { _, _, _, acc, _, _, _, _, _ := seedValues(1); return acc }()

// guideLeaves returns the commands the program's own guide lists that have no
// command under them: what an agent could run.
func guideLeaves(t *testing.T, bin string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), filepath.Join(bin, "nopacs"), "agent-guide", "--json").Output() // #nosec G204 -- the test's own binary.
	if err != nil {
		t.Fatalf("agent-guide: %v", err)
	}
	var g struct {
		Commands []struct{ Path string } `json:"commands"`
	}
	if err := json.Unmarshal(out, &g); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range g.Commands {
		paths = append(paths, strings.TrimPrefix(c.Path, "nopacs "))
	}
	var leaves []string
	for _, p := range paths {
		leaf := true
		for _, q := range paths {
			if strings.HasPrefix(q, p+" ") {
				leaf = false
			}
		}
		if leaf {
			leaves = append(leaves, p)
		}
	}
	sort.Strings(leaves)
	return leaves
}

// runCLI runs the command line with args as a caller of the server at addr
// with key (or none), and records what it wrote to standard output and
// standard error, apart, at label.
func runCLI(bin, addr, key, who, home string, rec *recorder, label string, args []string) (code int) {
	// A command that follows a feed or a job is given a few seconds to
	// print what it holds now.
	limit := 25 * time.Second
	if args[0] == "watch" || (args[0] == "job" && args[1] == "watch") {
		limit = 6 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(bin, "nopacs"), args...) // #nosec G204 -- the test's own binary.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_CONFIG_HOME=" + home, "NO_COLOR=1", "NOPACS_SERVER=http://" + addr}
	if key != "" {
		cmd.Env = append(cmd.Env, "NOPACS_KEY="+key)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		code = -1
	}
	rec.add(label+" stdout", who, out.String())
	rec.add(label+" stderr", who, errb.String())
	return code
}

// cliKey is a key the command line runs as.
type cliKey struct{ name, secret string }

// runCLICases runs every case of every command as each key, as written and
// with --json, and records the output. It returns how many runs of each key
// ended with each exit code, so that a key whose every run ended in a usage
// error is seen.
func runCLICases(t *testing.T, bin, addr string, w *world, f cliFiles, keys []cliKey, rec *recorder) map[string]map[int]int {
	t.Helper()
	codes := map[string]map[int]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			home := filepath.Join(f.home, k.name)
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Error(err)
				return
			}
			cases := cliCases(w, f, k.name)
			names := make([]string, 0, len(cases))
			for n := range cases {
				names = append(names, n)
			}
			sort.Strings(names)
			mine := map[int]int{}
			for _, pass := range []bool{false, true} {
				for _, n := range names {
					for _, cc := range cases[n] {
						if cc.last != pass {
							continue
						}
						for _, js := range []bool{false, true} {
							args := append([]string{}, cc.args...)
							label := "cli/" + n
							if js {
								args = append(args, "--json")
								label += " --json"
							}
							mine[runCLI(bin, addr, k.secret, k.name, home, rec, label, args)]++
						}
					}
				}
			}
			mu.Lock()
			codes[k.name] = mine
			mu.Unlock()
		}()
	}
	wg.Wait()
	return codes
}
