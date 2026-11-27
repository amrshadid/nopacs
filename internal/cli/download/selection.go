package download

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SelectionFormat is the value of "format" in a selection file. A file that
// names another is never read, so a later layout cannot be mistaken for
// this one.
const SelectionFormat = "nopacs-download-selection/1"

// maxSelectionBytes bounds a selection file that is read, so a damaged or
// planted file cannot take the memory of the machine.
const maxSelectionBytes = 1 << 30

// selectionDoc is the JSON a selection file holds.
type selectionDoc struct {
	Format string `json:"format"`
	Tenant string `json:"tenant"`
	// Layout is "" for the plain layout or "bagit".
	Layout string `json:"layout,omitempty"`
	// Refs are the references the set was chosen by, as subject[/session[/scan]].
	Refs []string `json:"refs"`
	// Files are the files known to be whole and checked, in path order.
	Files []selFile `json:"files"`
}

// selFile records one file that was whole and matched what the server
// announced.
type selFile struct {
	// Path is the file's path under the download's file directory, with
	// slashes.
	Path string `json:"path"`
	// Bytes and SHA256 are the length and checksum the server announced and
	// the file matched.
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	// ModTime is the file's modification time when it was recorded, in UTC;
	// a file whose size and time are as recorded is taken as unchanged
	// without being read again.
	ModTime string `json:"modTime,omitempty"`
}

// selection is the record of what a download has finished. It is kept in
// memory for every download, because the BagIt manifest is written from it,
// and saved to a file only when the caller names one.
type selection struct {
	path   string
	tenant string
	layout string
	refs   []string

	mu    sync.Mutex
	files map[string]selFile
	dirty bool
	// lastSave and lastCost pace the saves: a set of a million files makes a
	// large file, so the next save waits at least twenty times as long as
	// the last one took.
	lastSave time.Time
	lastCost time.Duration
}

// saveEvery is the least time between two saves while a download runs.
const saveEvery = 5 * time.Second

// loadSelection reads the selection file at path, or starts an empty one when
// path names no file yet. A file of another tenant or layout, or one that is
// not a selection file, is an error, never replaced: the person decides.
func loadSelection(path, tenant, layout string, refs []string) (*selection, error) {
	s := &selection{path: path, tenant: tenant, layout: layout, refs: refs, files: map[string]selFile{}}
	if path == "" {
		return s, nil
	}
	f, err := os.Open(path) // #nosec G304 -- the path the person named with --selection
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the selection file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var doc selectionDoc
	dec := json.NewDecoder(io.LimitReader(f, maxSelectionBytes))
	if err := dec.Decode(&doc); err != nil || doc.Format != SelectionFormat {
		return nil, fmt.Errorf("%s is not a selection file this program wrote (format %q): move it away, or name another with --selection", path, SelectionFormat)
	}
	if doc.Tenant != tenant || doc.Layout != layout {
		return nil, fmt.Errorf("the selection file %s was made for tenant %q with layout %q, not tenant %q with layout %q: name another with --selection", path, doc.Tenant, doc.Layout, tenant, layout)
	}
	for _, e := range doc.Files {
		s.files[e.Path] = e
	}
	return s, nil
}

// SelectionRefs reads the tenant, layout and references a selection file
// was made for, so the same download can be resumed by naming the file alone.
// It returns found false, and no error, when there is no such file.
func SelectionRefs(path string) (tenant, layout string, refs [][]string, found bool, err error) {
	f, err := os.Open(path) // #nosec G304 -- the path the person named with --selection
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil, false, nil
	}
	if err != nil {
		return "", "", nil, false, fmt.Errorf("cannot read the selection file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	var doc selectionDoc
	if err := json.NewDecoder(io.LimitReader(f, maxSelectionBytes)).Decode(&doc); err != nil || doc.Format != SelectionFormat {
		return "", "", nil, false, fmt.Errorf("%s is not a selection file this program wrote (format %q): move it away, or name another with --selection", path, SelectionFormat)
	}
	for _, text := range doc.Refs {
		ref, err := ParseRef(text)
		if err != nil {
			return "", "", nil, false, fmt.Errorf("the selection file %s holds a reference that is not one: %w", path, err)
		}
		refs = append(refs, ref)
	}
	return doc.Tenant, doc.Layout, refs, true, nil
}

// vouches says whether the selection file already knows rel as whole and the
// announced one, and the file on disk is as recorded: the same size, and the
// same modification time. Anything else sends the file to be read and checked.
func (s *selection) vouches(rel, sha string, length int64, st fs.FileInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.files[rel]
	return ok && e.SHA256 == sha && e.Bytes == length && st.Size() == length && e.ModTime != "" && e.ModTime == stamp(st)
}

// stamp is a file's modification time as recorded.
func stamp(st fs.FileInfo) string { return st.ModTime().UTC().Format(time.RFC3339Nano) }

// record notes that rel is whole and matched what the server announced.
func (s *selection) record(rel, sha string, length int64, st fs.FileInfo) {
	e := selFile{Path: rel, Bytes: length, SHA256: sha}
	if st != nil {
		e.ModTime = stamp(st)
	}
	s.mu.Lock()
	if old, ok := s.files[rel]; !ok || old != e {
		s.files[rel] = e
		s.dirty = true
	}
	s.mu.Unlock()
}

// prune forgets every file that is not in keep: the files of an earlier
// choice that the set no longer holds.
func (s *selection) prune(keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := range s.files {
		if !keep[p] {
			delete(s.files, p)
			s.dirty = true
		}
	}
}

// entries returns the recorded files in path order.
func (s *selection) entries() []selFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]selFile, 0, len(s.files))
	for _, e := range s.files {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// saveIfDue saves when something changed and enough time has passed.
func (s *selection) saveIfDue(now time.Time) error {
	s.mu.Lock()
	due := s.dirty && now.Sub(s.lastSave) >= max(saveEvery, 20*s.lastCost)
	s.mu.Unlock()
	if !due {
		return nil
	}
	return s.save()
}

// save writes the selection file when there is one to write and something
// changed since the last save. The new content is written to a file beside it
// and renamed over it, so a stop in the middle leaves the old file or the new
// one, never half of either.
func (s *selection) save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.dirty = false
	s.mu.Unlock()
	began := time.Now()
	doc := selectionDoc{Format: SelectionFormat, Tenant: s.tenant, Layout: s.layout, Refs: s.refs, Files: s.entries()}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("cannot write the selection file: %w", err)
	}
	if err := writeAtomic(s.path, buf.Bytes()); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return fmt.Errorf("cannot write the selection file %s: %w", s.path, err)
	}
	s.mu.Lock()
	s.lastSave, s.lastCost = time.Now(), time.Since(began)
	s.mu.Unlock()
	return nil
}

// writeAtomic replaces the file at path with data so that a reader, or a
// crash, sees the whole old file or the whole new one: the data is written
// and flushed to a temporary file in the same directory, which is then
// renamed over the target.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// refText writes a reference as subject[/session[/scan]].
func refText(ref []string) string { return strings.Join(ref, "/") }
