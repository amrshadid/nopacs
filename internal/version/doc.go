// Package version says which build of NoPACs is running: its version, the
// commit it was built from and the Go release that compiled it.
//
// A report about an archive is only useful when it names the build that wrote
// the data, so the answer must come from the binary itself and not from a file
// beside it that can be out of step. The values are read from the build
// information the Go linker embeds (runtime/debug), and a release build may set
// them with -ldflags:
//
//	go build -ldflags "-X github.com/amrshadid/nopacs/internal/version.Version=v1.0.0 \
//	    -X github.com/amrshadid/nopacs/internal/version.Commit=0123456789ab" ./cmd/nopacs
//
// This package must never read a file, the environment or the network, and
// never fail: a binary that cannot say what it is gives the person asking
// nothing to report.
package version
