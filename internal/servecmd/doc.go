// Package servecmd is the commands of the nopacs-server program: the ones
// that make and run an installation ("nopacs-server init" and "nopacs-server
// serve") and the ones that must touch the data directory with the server
// stopped ("backup", "tenant import"). They are built as cobra commands that
// attach to a command tree with one line,
//
//	root.AddCommand(servecmd.Init(opts), servecmd.Serve(opts), servecmd.Backup(opts))
//
// and Command and Run are that tree, as cmd/nopacs-server runs it. They are
// kept apart from the command line (internal/cli) because they are not clients
// of the API: init makes the data directory, the key file and the master
// database, and serve is the server. The command line links none of this
// package, so it carries no database or storage driver.
//
// init refuses a directory that already holds an installation, prints where
// the key file is and that losing it loses every secret, and never prints a
// secret. serve takes the data directory's lock before it opens anything,
// so a second server on the same directory is refused before it can touch
// the master database. The address of a master database that is not the
// SQLite file in the data directory is read from the environment variable
// that --master-env names, never from an argument, because an address holds
// a password and arguments are visible to every user of the machine.
//
// What it must never do: print or log a secret (a key, a database
// address), make a key file in serve (a server that silently made a new key
// could open no secret sealed with the old one), or hold the logic of the
// server or of a handler.
package servecmd
