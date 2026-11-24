package dirimport

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/dirimport/mapping"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// CheckJob checks, before an import job is made, what its units will need,
// so a tree that is not there or a map that cannot be used is refused when
// the job is asked for rather than found out by its first unit (job.proto,
// CreateJob): the receiver is an import receiver; a map configuration the
// job gives fits the receiver's layout and fills the part the receiver's
// own fills, which its naming rule was checked against; the receiver's root
// resolves now (roots), with every rule of where a root may lie; the job's
// path is a directory under it, each segment opened without following a
// link; and the map, inline or read from the file the job or the receiver
// names, parses and goes with a map configuration.
//
// It returns the map the job keeps: the inline map as given, or the bytes
// of the map file as read now, or nil for none. The job keeps the map as it
// was read when the job was made, so a map file changed later changes
// nothing of the job or of a retry of it.
//
// tenantName and st are the tenant's, for the root's allotment. A refusal
// is a precondition failure whose message holds no path, no cell of the
// map and no header.
//
// The check reads the file system, and a call into a file system that does
// not answer (a network share gone, a hung mount) blocks in the kernel
// whatever the context says. So the check runs on its own goroutine and
// CheckJob returns when ctx ends, an Unavailable failure, leaving that
// goroutine to end when the file system answers: a caller that bounds ctx
// is never held past its bound.
func CheckJob(ctx context.Context, roots Roots, tenantName string, st tenant.Store, rcv tenant.Receiver, spec tenant.ImportJob) ([]byte, error) {
	return checkJobBounded(ctx, roots, tenantName, st, rcv, spec, checkSeams{})
}

// checkSeams are the parts of a job's check only a test sets
// (export_test.go).
type checkSeams struct {
	// opened, when set, is called with the root's directory after it is
	// checked and before it is opened, so a test can put another directory
	// in its place.
	opened func(dir string)
}

// checkResult is what the goroutine of a job's check answers.
type checkResult struct {
	data []byte
	err  error
}

// checkJobBounded runs checkJob on its own goroutine and returns when it
// ends or when ctx ends, whichever is first (CheckJob).
func checkJobBounded(ctx context.Context, roots Roots, tenantName string, st tenant.Store, rcv tenant.Receiver, spec tenant.ImportJob, seams checkSeams) ([]byte, error) {
	done := make(chan checkResult, 1)
	go func() {
		data, err := checkJob(ctx, roots, tenantName, st, rcv, spec, seams)
		done <- checkResult{data, err}
	}()
	select {
	case r := <-done:
		return r.data, r.err
	case <-ctx.Done():
		return nil, fault.Wrap(fault.Unavailable, ctx.Err(), "import_job: the file system under the import root "+rcv.Import.Root+" did not answer in time; the job was not made")
	}
}

// checkJob is CheckJob's check itself, on the caller's goroutine.
func checkJob(ctx context.Context, roots Roots, tenantName string, st tenant.Store, rcv tenant.Receiver, spec tenant.ImportJob, seams checkSeams) ([]byte, error) {
	if rcv.Type != tenant.ReceiverImport {
		return nil, fault.New(fault.Precondition, "import_job.receiver: the receiver "+rcv.Name+" is not an import receiver")
	}
	layout, err := rcv.Import.ParsedLayout()
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "dirimport: the stored layout of an import receiver does not parse")
	}
	use := spec.Map
	if !use.IsZero() {
		err := layout.CheckMap(importpath.MapUse{Key: use.KeyParts, Gives: use.Gives})
		switch {
		case errors.Is(err, importpath.ErrMapParts):
			return nil, fault.New(fault.Precondition, "import_job: a map's key parts are parts of the receiver's layout, and the part it gives is not")
		case err != nil:
			return nil, fault.New(fault.Precondition, "import_job: the map configuration does not fit the receiver's layout")
		case !rcv.Import.Map.IsZero() && use.Gives != rcv.Import.Map.Gives:
			return nil, fault.New(fault.Precondition, "import_job.map_gives: a job's map fills the part the receiver's map fills ("+rcv.Import.Map.Gives+"), which its naming rule was checked against")
		}
	} else {
		use = rcv.Import.Map
	}
	dir, err := roots.Root(ctx, tenantName, st, rcv.Import.Root)
	if err != nil {
		return nil, err
	}
	checked, err := os.Stat(dir)
	if err != nil {
		return nil, fault.New(fault.Precondition, "the import root "+rcv.Import.Root+" cannot be opened on this server")
	}
	if seams.opened != nil {
		seams.opened(dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fault.New(fault.Precondition, "the import root "+rcv.Import.Root+" cannot be opened on this server")
	}
	defer func() { _ = root.Close() }()
	// The directory opened must be the one checked: a directory renamed and
	// another put in its place between the check and the open would
	// otherwise be read under rules checked of the first.
	if opened, err := root.Stat("."); err != nil || !os.SameFile(checked, opened) {
		return nil, fault.New(fault.Precondition, "the import root "+rcv.Import.Root+" changed while it was opened; ask for the job again")
	}
	folder, err := openFolder(root, spec.Path)
	if err != nil {
		return nil, fault.New(fault.Precondition, "import_job.path: the path is not a directory under the root "+rcv.Import.Root+", reached through no link")
	}
	if folder != root {
		_ = folder.Close()
	}
	data, file := spec.MapCSV, spec.MapFile
	if len(data) == 0 && file == "" {
		file = rcv.Import.MapFile
	}
	if len(data) == 0 && file != "" {
		// A map read from a file is kept with its job, as an inline map is,
		// so it has the inline map's bound; a larger file is refused by its
		// size before any of it is read, and never read past the bound.
		data, err = readMapFile(root, file, tenant.MaxImportMapCSV)
		switch {
		case errors.Is(err, errMapTooLarge):
			return nil, fault.New(fault.Precondition, "import_job.map_file: the map is kept with its job as it is read, at most "+
				strconv.Itoa(tenant.MaxImportMapCSV>>20)+" MiB, and this one is larger; split the tree into jobs with smaller maps")
		case err != nil:
			// The file system's text repeats the path, so it is not kept.
			return nil, fault.New(fault.Precondition, "import_job.map_file: the map file is not a regular file under the root, reached through no link, that the server can read")
		}
	}
	switch {
	case use.IsZero() && len(data) == 0:
		return nil, nil
	case use.IsZero():
		return nil, fault.New(fault.Precondition, "import_job: a map is given with no map configuration, the job's or the receiver's")
	case len(data) == 0:
		return nil, fault.New(fault.Precondition, "import_job: the receiver's naming rule takes "+use.Gives+" from a map, and the job has no map, inline or in a file")
	}
	m, err := mapping.Parse(data, mapping.MaxInlineBytes, mapping.Columns{Key: use.KeyColumns, Value: use.ValueColumn})
	if err != nil {
		// A mapping error names a line by its number, never a cell.
		return nil, fault.Wrap(fault.Precondition, err, "import_job: "+err.Error())
	}
	if _, err := layout.Resolver(m, importpath.MapUse{Key: use.KeyParts, Gives: use.Gives}); err != nil {
		return nil, fault.New(fault.Precondition, "import_job: the map cannot be used with the receiver's layout")
	}
	return data, nil
}

// errMapTooLarge is a map file past the bound it is read with.
var errMapTooLarge = errors.New("the map file is larger than a map kept with a job")

// readMapFile reads the map file at p under root, opened one segment at a
// time without following a link, refusing one past limit bytes
// (errMapTooLarge) by its size before any of it is read, and reading at
// most limit and one byte more, so a file that grows while it is read is
// refused too and never held whole.
func readMapFile(root *os.Root, p string, limit int64) ([]byte, error) {
	if err := importpath.Check(p); err != nil {
		return nil, err
	}
	dirPath, name := "", p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		dirPath, name = p[:i], p[i+1:]
	}
	dir, err := openFolder(root, dirPath)
	if err != nil {
		return nil, err
	}
	if dir != root {
		defer func() { _ = dir.Close() }()
	}
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errMoved
	}
	if info.Size() > limit {
		return nil, errMapTooLarge
	}
	// O_NONBLOCK: a FIFO put in the file's place after Lstat would block an
	// open for reading until a writer came; the open returns at once, and
	// the SameFile below refuses it.
	f, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errMoved
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errMapTooLarge
	}
	return data, nil
}
