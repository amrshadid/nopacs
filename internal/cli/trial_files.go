package cli

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// collectFiles reads the files named by paths, as walkFiles finds them. The
// count and sizes are held to the server's bounds, each size from the
// file's information before it is read, so a file past the bound is never
// loaded.
func collectFiles(paths []string) ([]*v1.TrialFile, error) {
	var out []*v1.TrialFile
	total := 0
	add := func(path string, size int64) error {
		if len(out) >= maxTrialFiles {
			return fault.New(fault.Invalid, fmt.Sprintf("more than %d files: a trial run takes at most %d at once; name fewer", maxTrialFiles, maxTrialFiles))
		}
		if size == 0 || size > maxTrialFileBytes {
			return fault.New(fault.Invalid, fmt.Sprintf("%s is %s; a trial run takes files of 1 byte to 3MiB", path, output.Bytes(size)))
		}
		if total+int(size) > maxTrialSentBytes {
			return fault.New(fault.Invalid, "the files together are larger than 3.5MiB, the most one trial run sends; name fewer")
		}
		b, err := readBounded(path, size)
		if err != nil {
			return err
		}
		total += len(b)
		out = append(out, &v1.TrialFile{Name: path, Content: b})
		return nil
	}
	if err := walkFiles(paths, add); err != nil {
		return nil, err
	}
	return out, nil
}

// walkFiles calls add for each file paths name: a file as it is, a
// directory (a link to one included) taken recursively: its regular files
// that begin as a DICOM file does (PS3.10 7.1), in the order of their
// names, links inside it not followed, at most maxWalkFiles looked at. add
// is given each file's size from its information, before it is read.
func walkFiles(paths []string, add func(path string, size int64) error) error {
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return fault.Wrap(fault.Invalid, err, "cannot read "+p)
		}
		if !info.IsDir() {
			if err := add(p, info.Size()); err != nil {
				return err
			}
			continue
		}
		// A link named here is the person's choice and is followed; the
		// walk follows none inside it.
		root, err := filepath.EvalSymlinks(p)
		if err != nil {
			return fault.Wrap(fault.Invalid, err, "cannot read "+p)
		}
		looked := 0
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if looked++; looked > maxWalkFiles {
				return fault.New(fault.Invalid, fmt.Sprintf("%s holds more than %d files; name a smaller directory", p, maxWalkFiles))
			}
			if !looksDICOM(path) {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return add(path, fi.Size())
		})
		if err != nil {
			if fault.KindOf(err) == fault.Invalid {
				return err
			}
			return fault.Wrap(fault.Invalid, err, "cannot read "+p)
		}
	}
	return nil
}

// readBounded reads a file whose information said size bytes, and no more
// than that: a file that grew since is refused, never read past it.
func readBounded(path string, size int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- the person names the files to try.
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read "+path)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, size+1))
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read "+path)
	}
	if int64(len(b)) != size {
		return nil, fault.New(fault.Invalid, path+" changed while it was read")
	}
	return b, nil
}

// looksDICOM reports whether a file begins as a Part 10 file does: 128
// bytes of preamble and "DICM" (PS3.10 7.1).
func looksDICOM(path string) bool {
	f, err := os.Open(path) // #nosec G304 -- a file under a directory the person named.
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var b [132]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	return string(b[128:]) == "DICM"
}
