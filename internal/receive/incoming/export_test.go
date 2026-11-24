package incoming

import "os"

// SetHook sets the function a landing calls at each durable step, for a
// test that stops the process there.
func SetHook(a *Area, h func(point string)) { a.hook = h }

// RecordSteps replaces the durable calls of a landing with ones that append
// their name to log before doing the real call.
func RecordSteps(a *Area, log *[]string) {
	real := realSteps()
	a.steps = steps{
		syncFile: func(f *os.File) error { *log = append(*log, "sync-file"); return real.syncFile(f) },
		rename: func(r *os.Root, from, to string) error {
			*log = append(*log, "rename")
			return real.rename(r, from, to)
		},
		syncDir: func(r *os.Root, dir string) error { *log = append(*log, "sync-dir"); return real.syncDir(r, dir) },
	}
}
