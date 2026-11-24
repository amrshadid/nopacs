package receive

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/amrshadid/nopacs/internal/fault"
)

// DirSource is a source that hands in every regular file under a directory,
// as an import does: each through the same Accept a DICOM listener calls.
// It follows no symbolic link and opens nothing but regular files, each
// through an os.Root, so nothing outside the directory is read.
type DirSource struct {
	// Dir is the directory to read.
	Dir string
	// Receiver names the receiver the files are handed to.
	Receiver string
}

// DirReport counts what a DirSource's run handed in, by outcome.
type DirReport struct {
	// Received, Refused and Failed count the files by their outcome.
	Received, Refused, Failed int
	// Skipped counts entries that are not regular files.
	Skipped int
}

// Run hands every regular file under the directory to sink, in order of
// path, and returns the counts. A file refused as a temporary failure is not
// handed in again: the counts say how many to run again. It stops when ctx
// ends.
func (d DirSource) Run(ctx context.Context, sink Sink) (DirReport, error) {
	var rep DirReport
	root, err := os.OpenRoot(d.Dir)
	if err != nil {
		return rep, fault.Wrap(fault.Invalid, err, "the directory to import cannot be opened")
	}
	defer func() { _ = root.Close() }()
	var paths []string
	err = fs.WalkDir(root.FS(), ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case e.IsDir():
			return nil
		case !e.Type().IsRegular():
			rep.Skipped++
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return rep, fault.Wrap(fault.Unavailable, err, "the directory to import cannot be read")
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return rep, fault.Wrap(fault.Unavailable, err, "the import was stopped")
		}
		f, err := root.Open(filepath.FromSlash(path))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			rep.Failed++
			continue
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			rep.Skipped++
			continue
		}
		out := sink.Accept(ctx, Arrival{Receiver: d.Receiver, Body: f})
		_ = f.Close()
		switch out.Status {
		case Received:
			rep.Received++
		case Refused:
			rep.Refused++
		default:
			rep.Failed++
		}
	}
	return rep, nil
}
