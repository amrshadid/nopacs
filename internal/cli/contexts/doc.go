// Package contexts keeps the command line's named client contexts: for each,
// the address of a server, the paths of the files that reach it, and the
// path of the key file, so a person who works with several installations or
// tenants names the one to use instead of repeating flags. The file holds
// paths and never a key or any other secret, and a file that holds an unknown
// field is refused, so a key pasted into it is noticed rather than kept.
//
// The file is read only when private to its owner, by the rule a key file
// follows (a key file's path is what it names, and a file others can change
// can redirect a key to another server), and is replaced atomically.
package contexts
