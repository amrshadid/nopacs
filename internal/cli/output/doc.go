// Package output writes what a command found: a table or a few aligned lines
// for a person, one JSON document for a program.
//
// The JSON form is a contract of its own. Every document is one object with
// "schemaVersion" first; the rest are the API's own fields under the names the
// contract gives them in protobuf JSON, with unset fields written as empty
// values so a document has the same shape whether or not a value is set.
// Fields are added and never moved or renamed, so a script written today
// reads the output of a later version.
//
// Everything a server sent is written through Clean before it reaches a
// terminal, because a name or a message from the network can hold escape
// sequences that would rewrite the screen.
//
// This package must never write to anything but the writer it is given, never
// read the environment on its own, and never use color unless told to: the
// caller decides, from NO_COLOR and from whether the output is a terminal.
package output
