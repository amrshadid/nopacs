// Package repattern compiles a regular expression a person wrote into a
// rule, within bounds a server can run it in: RE2 syntax (linear time in
// the value), a short text, a bounded repetition, and a bounded compiled
// size. A short text alone is not a bound: x{1000} is seven bytes and
// compiles to a thousand instructions, and a class such as \pL holds
// hundreds of ranges.
package repattern
