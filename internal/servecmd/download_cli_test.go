package servecmd_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/cli"
	"github.com/amrshadid/nopacs/internal/cli/client"
)

// nopacs runs the command line against the server in dataDir as its local
// operator and returns its exit code and what it wrote.
func nopacs(ctx context.Context, dataDir string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = cli.Run(ctx, args, cli.Env{
		Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string {
			if k == client.EnvDataDir {
				return dataDir
			}
			return ""
		},
	})
	return code, out.String(), errb.String()
}

// shasum returns a function that gives a file's SHA-256 with an outside
// tool, shasum or sha256sum, or nil when neither is installed.
func shasum() func(t *testing.T, path string) string {
	for _, c := range [][]string{{"shasum", "-a", "256"}, {"sha256sum"}} {
		p, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		args := c[1:]
		return func(t *testing.T, path string) string {
			t.Helper()
			out, err := exec.Command(p, append(append([]string{}, args...), path)...).Output() //nolint:gosec // G204: a test's own tool.
			if err != nil {
				t.Fatalf("%s: %v", c[0], err)
			}
			f := strings.Fields(string(out))
			if len(f) == 0 {
				t.Fatalf("%s printed nothing for %s", c[0], path)
			}
			return f[0]
		}
	}
	return nil
}

// checkCLIDownload downloads the archived study with "nopacs download" from
// the real server and holds every file to the storage: the same bytes as the
// object the manifest names, and the manifest's SHA-256 as shasum computes
// it. It then breaks the download two ways and runs it again: a context
// canceled while files are arriving, and the state a kill leaves (files
// after the last whole one gone and one cut short as a .part), and each
// rerun must give back the same bytes, the cut file continued by a range
// request rather than fetched whole.
func checkCLIDownload(t *testing.T, dataDir, tenantName string, store blob.Store) {
	t.Helper()
	files := map[string]manifestFile{}
	for _, b := range storedManifests(t, store) {
		var m struct {
			Files []manifestFile `json:"files"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, f := range m.Files {
			files[f.Key] = f
		}
	}
	if len(files) == 0 {
		t.Fatal("the storage lists no instance")
	}
	subject := strings.Split(firstKey(files), "/")[1]
	dest := filepath.Join(shortDir(t), "dl")

	// check holds every file in dest to the storage, byte for byte and by
	// the outside tool's checksum.
	sha := shasum()
	check := func(what string) {
		t.Helper()
		for key, f := range files {
			path := filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(key, "data/")))
			got, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s: %v", what, err)
				continue
			}
			rc, err := store.Read(t.Context(), key, 0, -1)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := io.ReadAll(rc)
			_ = rc.Close()
			if !bytes.Equal(got, want) || int64(len(got)) != f.Length {
				t.Errorf("%s: %s is %d bytes, the storage holds %d", what, path, len(got), len(want))
			}
			if sha != nil {
				if s := sha(t, path); s != f.SHA256 {
					t.Errorf("%s: shasum of %s is %s, the manifest says %s", what, path, s, f.SHA256)
				}
			}
		}
	}

	code, out, errOut := nopacs(t.Context(), dataDir, "download", tenantName, subject, "--to", dest, "--json")
	if code != 0 {
		t.Fatalf("download: exit %d\n%s\n%s", code, out, errOut)
	}
	var res struct {
		Files   int `json:"files"`
		Skipped int `json:"skipped"`
		Resumed int `json:"resumed"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Files != len(files) {
		t.Fatalf("download result %q (%v), want %d files", out, err, len(files))
	}
	check("first download")

	// A download canceled while it runs, then run again.
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for ctx.Err() == nil {
			var n int64
			_ = filepath.WalkDir(dest, func(_ string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if info, e := d.Info(); e == nil {
						n += info.Size()
					}
				}
				return nil
			})
			if n > 0 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_, _, _ = nopacs(ctx, dataDir, "download", tenantName, subject, "--to", dest, "--quiet")
	cancel()
	<-stopped
	if code, out, errOut := nopacs(t.Context(), dataDir, "download", tenantName, subject, "--to", dest, "--quiet"); code != 0 {
		t.Fatalf("download after a cancel: exit %d\n%s\n%s", code, out, errOut)
	}
	check("after a cancel")

	// The state a kill leaves: the last file gone, the one before it cut in
	// half and named .part.
	var paths []string
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if len(paths) < 3 {
		t.Fatalf("the archive holds %d files, too few to break a download", len(paths))
	}
	last, cut := paths[len(paths)-1], paths[len(paths)-2]
	whole, err := os.ReadFile(cut)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(last); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cut); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cut+".part", whole[:len(whole)/2], 0o600); err != nil { //nolint:gosec // G703: a path under the test's own temporary directory.
		t.Fatal(err)
	}
	// The record of the file the .part is the start of, as a download
	// writes it before the first byte.
	wholeSum := sha256.Sum256(whole)
	if err := os.WriteFile(cut+".part.sha256", []byte(hex.EncodeToString(wholeSum[:])+"\n"), 0o600); err != nil { //nolint:gosec // G703: a path under the test's own temporary directory.
		t.Fatal(err)
	}
	code, out, errOut = nopacs(t.Context(), dataDir, "download", tenantName, subject, "--to", dest, "--json")
	if code != 0 {
		t.Fatalf("download after a kill: exit %d\n%s\n%s", code, out, errOut)
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Resumed != 1 || res.Files != 2 || res.Skipped != len(files)-2 {
		t.Errorf("download after a kill: %q (%v), want 2 files fetched, 1 of them resumed by range, %d skipped", out, err, len(files)-2)
	}
	check("after a kill")
}

// firstKey returns one key of the map.
func firstKey(m map[string]manifestFile) string {
	for k := range m {
		return k
	}
	return ""
}
