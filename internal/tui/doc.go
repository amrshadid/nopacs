// Package tui is the interactive program: what "nopacs" with no arguments
// opens on a terminal. It is a client of the same API as the commands, through
// the same client, and can do nothing a command cannot.
//
// Each screen is a value. Its update function takes the screen and a message
// and returns the new screen and a command; its view function takes the screen
// and returns the lines to draw. No screen calls the API: it returns a command
// that does, and the answer arrives later as a message. That is what keeps a
// slow server from freezing the screen, lets esc cancel a call, and lets a
// test drive a screen with key presses and answers and no terminal.
//
// Three rules hold for everything here:
//
//   - A new key's secret is shown on one screen and nowhere else: not in
//     another view, a log, an error, a file, or any state once that screen is
//     left. It is held in a type that prints as a placeholder and is zeroed
//     when the screen is left.
//   - Text from the server is cleaned of control characters before it is
//     drawn (output.Clean), because a name or a message from the network can
//     hold escape sequences that rewrite a terminal.
//   - Nothing destructive happens on one key: revoking a key and removing a
//     grant, a setting or a tenant need the key id or the name typed.
//
// This package must never write to the terminal itself, never log, never keep
// a copy of a secret beyond the one screen that shows it, and never be
// reached when standard input or output is not a terminal.
package tui
