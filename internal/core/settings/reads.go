package settings

import "errors"

// The settings that bound the reads of archived bytes open at once
// (download.proto, DownloadService; data.proto, DataService.ReadFiles; and
// the DICOMweb retrieves). Both are installation-only: the open reads share
// one server's storage connections and memory, which no tenant may claim
// more of.
const (
	// ReadPerCaller is the most reads one caller, a key or the local
	// operator, holds open at once (ParseReadPerCaller). A viewer opens
	// several at a time, so an installation that serves viewers sizes it to
	// the viewer.
	ReadPerCaller = "read.per-caller"
	// ReadPerServer is the most reads the server holds open at once, for
	// every caller together (ParseReadPerServer). Each holds a storage's
	// connection or file and two buffers of one megabyte. It is never
	// below read.per-caller.
	ReadPerServer = "read.per-server"
)

// The bounds and defaults of the read settings.
const (
	// MinReadPerCaller and MaxReadPerCaller bound read.per-caller.
	MinReadPerCaller = 1
	MaxReadPerCaller = 1024
	// MinReadPerServer and MaxReadPerServer bound read.per-server: at least
	// the default of read.per-caller, so the server always serves one
	// caller whole; at most 64 Ki reads, which at two megabytes each is
	// 128 GiB of buffers.
	MinReadPerServer = 16
	MaxReadPerServer = 65536
	// DefaultReadPerCaller and DefaultReadPerServer are the defaults.
	DefaultReadPerCaller = 16
	DefaultReadPerServer = 256
)

// ParseReadPerCaller reads read.per-caller: a whole number from 1 to 1024.
func ParseReadPerCaller(v string) (int, error) {
	n, err := wholeNumber(v, MaxReadPerCaller)
	if err != nil || n < MinReadPerCaller {
		return 0, errors.New("is a whole number from 1 to 1024")
	}
	return int(n), nil
}

// ParseReadPerServer reads read.per-server: a whole number from 16 to
// 65536.
func ParseReadPerServer(v string) (int, error) {
	n, err := wholeNumber(v, MaxReadPerServer)
	if err != nil || n < MinReadPerServer {
		return 0, errors.New("is a whole number from 16 to 65536")
	}
	return int(n), nil
}

// readDefinitions returns the definitions of the read settings, for
// Builtin.
func readDefinitions() []Definition {
	return []Definition{{
		// 16: a viewer keeps about three reads open, so this leaves room for
		// several windows and a script beside them.
		Name: ReadPerCaller, Default: "16",
		Validate: func(v string) error { _, err := ParseReadPerCaller(v); return err },
	}, {
		// 256: each read holds two megabytes of buffers and a connection or
		// file of its storage.
		Name: ReadPerServer, Default: "256",
		Validate: func(v string) error { _, err := ParseReadPerServer(v); return err },
	}}
}
