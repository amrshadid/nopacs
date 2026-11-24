package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// The prefixes of the names of an import's enums.
const (
	importFileKindPrefix  = "IMPORT_FILE_KIND_"
	importUnitStatePrefix = "IMPORT_UNIT_STATE_"
	importSkipCodePrefix  = "IMPORT_SKIP_CODE_"
)

// maxInlineMap is the most bytes of a map given with a job (job.proto,
// ImportJob.map_csv), checked before the file is sent.
const maxInlineMap = 3 << 20

// newJobImport builds "job import".
func (a *app) newJobImport() *cobra.Command {
	var mapFile, mapCSV, valueColumn, gives string
	var keyColumns, keyParts []string
	var wait bool
	cmd := &cobra.Command{
		Use:   "import <tenant> <receiver> [<path>] [--map-file PATH | --map-csv FILE] [--wait]",
		Short: "Hand the files of a directory under an import root to an import receiver",
		Long: `Make an import job: the server reads the directory <path> under the import
receiver's root (the whole root when <path> is left out), segments joined by
"/", and hands each DICOM file in it to the receiver, which archives it as any
receiver does. Nothing in the directory is written, moved or deleted. A file
that is not DICOM, outside the receiver's layout, of a session the map does not
list, a link or too large is skipped and listed ("job files").

The map the receiver names is used unless the job gives its own: --map-file, a
CSV under the root, or --map-csv, a CSV file on this computer sent with the job
(at most 3 MiB); --map-key-column, --map-key-part, --map-value-column and
--map-gives replace the receiver's map configuration whole. The map is read
when the job is made and kept with it, so a map file changed later changes
nothing. A tree that is not there, or a map that names two values for one key,
is refused at once; a refusal names the map's line by number, never a cell.

A path can name a person, so the job's path is shown to operators only. The
job ends done when every file found has an outcome and none was held, partial
otherwise. With --wait the command follows it, as "job watch" does, with its
exit codes.`,
		Example: `  nopacs job import lab1 old-tree proj1
  nopacs job import lab1 old-tree proj1/sess-0017 --map-csv subjects.csv --wait
  nopacs job import lab1 plain-tree --json`,
		Args: rangeArgs(2, 3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			job := &v1.ImportJob{Receiver: args[1], MapFile: mapFile, MapKeyColumns: nonEmpty(keyColumns), MapKeyParts: nonEmpty(keyParts),
				MapValueColumn: valueColumn, MapGives: gives}
			if len(args) > 2 {
				job.Path = args[2]
			}
			if mapCSV != "" {
				if mapFile != "" {
					return fault.New(fault.Invalid, "--map-file names a map under the root and --map-csv sends one: give one of them")
				}
				b, err := readInlineMap(mapCSV)
				if err != nil {
					return err
				}
				job.MapCsv = b
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			cursor := ""
			if wait {
				if cursor, err = a.feedCursor(ctx, c, cmd, args[0]); err != nil {
					return err
				}
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CreateJobResponse, error) {
				return c.Jobs.CreateJob(ctx, &v1.CreateJobRequest{Tenant: args[0], Kind: &v1.CreateJobRequest_ImportJob{ImportJob: job}})
			})
			if err != nil {
				return err
			}
			if wait {
				return a.followJob(ctx, c, cmd, args[0], res.GetJob().GetId(), cursor)
			}
			return messageOut(a, res, func(w io.Writer) error { return a.jobOut(w, res.GetJob()) })
		}),
	}
	fl := cmd.Flags()
	fl.BoolVar(&wait, "wait", false, `follow the job until it ends, as "job watch" does, with its exit codes`)
	fl.StringVar(&mapFile, "map-file", "", "the map, a CSV file under the receiver's root, by its path under the root")
	fl.StringVar(&mapCSV, "map-csv", "", "the map, a CSV file on this computer, sent with the job (at most 3 MiB)")
	fl.StringArrayVar(&keyColumns, "map-key-column", nil, "a column of the map's header that forms a row's key; repeat, in order (replaces the receiver's map configuration)")
	fl.StringArrayVar(&keyParts, "map-key-part", nil, "a part of the receiver's layout that forms a file's key, in the order of --map-key-column; repeat")
	fl.StringVar(&valueColumn, "map-value-column", "", "the map's column whose value fills --map-gives")
	fl.StringVar(&gives, "map-gives", "", "the part the map fills, the one the receiver's naming rule takes from a map")
	cmd.MarkFlagsMutuallyExclusive("map-file", "map-csv")
	return cmd
}

// errMapTooLarge is the refusal of a --map-csv past maxInlineMap.
var errMapTooLarge = fault.New(fault.Invalid, "--map-csv: a map sent with a job is at most 3 MiB; put a larger one under the root and name it with --map-file")

// readInlineMap reads the --map-csv file at p: a regular file of at most
// maxInlineMap bytes. A FIFO or a device (/dev/zero, a terminal) has no
// size to check and may never end, so it is refused before it is read,
// and the file is opened without waiting for a FIFO's writer and read
// through a bound, so a file that grows while it is read is refused
// rather than held whole.
func readInlineMap(p string) ([]byte, error) {
	cannot := func(err error) error {
		return fault.New(fault.Invalid, fmt.Sprintf("--map-csv %q cannot be read: %v", output.Clean(p), err))
	}
	notRegular := fault.New(fault.Invalid, fmt.Sprintf("--map-csv %q is not a regular file", output.Clean(p)))
	info, err := os.Stat(p)
	if err != nil {
		return nil, cannot(err)
	}
	if !info.Mode().IsRegular() {
		return nil, notRegular
	}
	if info.Size() > maxInlineMap {
		return nil, errMapTooLarge
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- a file the person names on their own computer
	if err != nil {
		return nil, cannot(err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, cannot(err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, notRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, maxInlineMap+1))
	if err != nil {
		return nil, cannot(err)
	}
	if len(b) > maxInlineMap {
		return nil, errMapTooLarge
	}
	return b, nil
}

// importJobDetails returns the labeled values of an import job.
func importJobDetails(ij *v1.ImportJob) []output.Pair {
	cn := ij.GetCounts()
	path := ij.GetPath()
	if path == "" {
		path = "(the whole root, or not shown to this key)"
	}
	pairs := []output.Pair{
		{Label: "Imports", Value: path},
		{Label: "Receiver", Value: ij.GetReceiver()},
	}
	if ij.GetMapFile() != "" {
		pairs = append(pairs, output.Pair{Label: "Map file", Value: ij.GetMapFile()})
	}
	var skipped []string
	for _, s := range cn.GetSkipped() {
		skipped = append(skipped, fmt.Sprintf("%s %d", enumWordOf(s.GetCode().String(), importSkipCodePrefix), s.GetCount()))
	}
	var held []string
	for _, q := range cn.GetQuarantined() {
		held = append(held, fmt.Sprintf("%s %d", enumWordOf(q.GetReason().String(), "QUARANTINE_REASON_"), q.GetCount()))
	}
	pairs = append(pairs,
		output.Pair{Label: "Units", Value: fmt.Sprintf("%d of %d walked", cn.GetUnitsWalked(), cn.GetUnits())},
		output.Pair{Label: "Found", Value: strconv.FormatInt(cn.GetFound(), 10)},
		output.Pair{Label: "Skipped", Value: dashJoin(skipped)},
		output.Pair{Label: "Handed in", Value: fmt.Sprintf("%d, %s", cn.GetHandedIn(), output.Human(cn.GetBytesHandedIn()))},
		output.Pair{Label: "Archived", Value: strconv.FormatInt(cn.GetArchived(), 10)},
		output.Pair{Label: "Already held", Value: strconv.FormatInt(cn.GetAlreadyHeld(), 10)},
		output.Pair{Label: "Quarantined", Value: dashJoin(held)},
		output.Pair{Label: "Lost, failed, discarded", Value: fmt.Sprintf("%d, %d, %d", cn.GetLost(), cn.GetFailed(), cn.GetDiscarded())},
		output.Pair{Label: "Processing", Value: strconv.FormatInt(cn.GetProcessing(), 10)})
	if cn.GetLabelDiffers() > 0 {
		pairs = append(pairs, output.Pair{Label: "Label differs", Value: strconv.FormatInt(cn.GetLabelDiffers(), 10)})
	}
	return pairs
}

// enumWordOf returns an enum's name as a word: without its prefix, in
// lower case.
func enumWordOf(name, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(name, prefix))
}

// dashJoin joins words with commas, or "-" for none.
func dashJoin(words []string) string {
	if len(words) == 0 {
		return "-"
	}
	return strings.Join(words, ", ")
}

// newJobFiles builds "job files".
func (a *app) newJobFiles() *cobra.Command {
	var kinds []string
	var unit string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "files <tenant> <id> [--kind KIND]... [--unit ID]",
		Short: "List the files of an import job that did not reach the archive, by path",
		Long: `List the files of an import job that did not reach the archive, in the order of
the walk, each by its path under the root with why: skipped (and the skip
code: not DICOM, outside the layout, of an unmapped session, a directory too
wide, unreadable, not a regular file, too large, a path too long), quarantined
(with the quarantined file's id, "nopacs quarantine show"), lost, failed (with
its reception, "nopacs reception retry") or discarded. How many are kept is the
setting job.failures-kept.

A path can name a person, so this needs the operator role on the tenant.
A KIND is one of: ` + strings.Join(enumNames(v1.ImportFileKind_name, importFileKindPrefix), ", ") + `.`,
		Example: `  nopacs job files lab1 job_8Qm2
  nopacs job files lab1 job_8Qm2 --kind quarantined --kind lost --all --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			var want []v1.ImportFileKind
			for _, k := range kinds {
				v, err := parseEnum[v1.ImportFileKind]("kind", k, v1.ImportFileKind_name, importFileKindPrefix)
				if err != nil {
					return err
				}
				want = append(want, v)
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			files, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.ImportFile, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListImportFilesResponse, error) {
					return c.Jobs.ListImportFiles(ctx, &v1.ListImportFilesRequest{Tenant: args[0], Id: args[1], Kinds: want, Unit: unit, PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetFiles(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "files", files, more, func() error {
				rows := make([][]string, 0, len(files))
				for _, f := range files {
					why := enumWordOf(f.GetKind().String(), importFileKindPrefix)
					switch f.GetKind() {
					case v1.ImportFileKind_IMPORT_FILE_KIND_SKIPPED:
						why += ": " + enumWordOf(f.GetSkipCode().String(), importSkipCodePrefix)
					case v1.ImportFileKind_IMPORT_FILE_KIND_QUARANTINED:
						why += ": " + enumWordOf(f.GetQuarantineReason().String(), "QUARANTINE_REASON_")
					}
					record := f.GetQuarantinedFile()
					if record == "" {
						record = f.GetReception()
					}
					rows = append(rows, []string{output.Clean(f.GetPath()), why, output.Dash(f.GetDetail()), output.Dash(record), f.GetUnit(),
						output.Bytes(f.GetSizeBytes()), a.when(f.GetTime())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"PATH", "WHY", "DETAIL", "RECORD", "UNIT", "SIZE", "TIME"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringArrayVar(&kinds, "kind", nil, "only the files listed for this reason; repeat for more")
	cmd.Flags().StringVar(&unit, "unit", "", `only the files of the unit with this id ("job units")`)
	enumCompletion(cmd, "kind", v1.ImportFileKind_name, importFileKindPrefix)
	l.add(cmd)
	return cmd
}

// newJobUnits builds "job units".
func (a *app) newJobUnits() *cobra.Command {
	var states []string
	var l listFlags
	cmd := &cobra.Command{
		Use:   "units <tenant> <id> [--state STATE]...",
		Short: "List the units of an import job, each folder with its counts",
		Long: `List the units of an import job in the order of the walk: the listing unit,
whose folder is the job's path, and one per folder at the receiver's unit
depth, each with its state, its attempts and what became of its files, so the
job's counts can be checked folder by folder. Found equals skipped plus handed
in, and handed in equals archived, already held, quarantined, lost, failed,
discarded and processing together.

A folder can name a person, so this needs the operator role on the tenant.
A STATE is one of: ` + strings.Join(enumNames(v1.ImportUnitState_name, importUnitStatePrefix), ", ") + `.`,
		Example: `  nopacs job units lab1 job_8Qm2
  nopacs job units lab1 job_8Qm2 --state failed --json`,
		Args: exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if err := l.check(); err != nil {
				return err
			}
			var want []v1.ImportUnitState
			for _, s := range states {
				v, err := parseEnum[v1.ImportUnitState]("state", s, v1.ImportUnitState_name, importUnitStatePrefix)
				if err != nil {
					return err
				}
				want = append(want, v)
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			units, more, err := fetchPages(ctx, l, func(ctx context.Context, size int32, token string) ([]*v1.ImportUnit, string, error) {
				res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.ListImportUnitsResponse, error) {
					return c.Jobs.ListImportUnits(ctx, &v1.ListImportUnitsRequest{Tenant: args[0], Id: args[1], States: want, PageSize: size, PageToken: token})
				})
				if err != nil {
					return nil, "", err
				}
				return res.GetUnits(), res.GetNextPageToken(), nil
			})
			if err != nil {
				return err
			}
			return listOut(a, "units", units, more, func() error {
				rows := make([][]string, 0, len(units))
				for _, u := range units {
					cn := u.GetCounts()
					var skipped int64
					for _, s := range cn.GetSkipped() {
						skipped += s.GetCount()
					}
					var held int64
					for _, q := range cn.GetQuarantined() {
						held += q.GetCount()
					}
					folder := u.GetFolder()
					if folder == "" {
						folder = "(root)"
					}
					rows = append(rows, []string{u.GetId(), output.Clean(folder), enumWordOf(u.GetState().String(), importUnitStatePrefix),
						strconv.FormatInt(cn.GetFound(), 10), strconv.FormatInt(skipped, 10), strconv.FormatInt(cn.GetHandedIn(), 10),
						strconv.FormatInt(cn.GetArchived()+cn.GetAlreadyHeld(), 10), strconv.FormatInt(held, 10), strconv.FormatInt(cn.GetProcessing(), 10),
						output.Dash(u.GetReason())})
				}
				return a.write(func(w io.Writer) error {
					return output.Table(w, a.style(), []string{"UNIT", "FOLDER", "STATE", "FOUND", "SKIPPED", "HANDED IN", "ARCHIVED", "HELD", "PROCESSING", "REASON"}, rows)
				})
			})
		}),
	}
	cmd.Flags().StringArrayVar(&states, "state", nil, "only the units in this state; repeat for more")
	enumCompletion(cmd, "state", v1.ImportUnitState_name, importUnitStatePrefix)
	l.add(cmd)
	return cmd
}
