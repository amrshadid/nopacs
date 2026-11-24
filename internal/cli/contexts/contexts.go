package contexts

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/core/pathname"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
)

// EnvConfig names the file of contexts, in place of the default place.
const EnvConfig = "NOPACS_CONFIG"

// fileName is the name of the file of contexts in the default directory.
const fileName = "contexts.json"

// version is the format of the file. A file of another version is refused
// instead of being read by guess.
const version = 1

// maxFile bounds the file read: a context is a few hundred bytes, so a
// megabyte is far more than a person keeps and a larger file is a mistake.
const maxFile = 1 << 20

// maxContexts bounds how many contexts a file may hold.
const maxContexts = 1000

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Context is one way to reach one installation. Every path is absolute where
// the command that wrote it could make it so. There is no field for a key: a
// context names the file that holds it.
type Context struct {
	// Server is the address of the API, https://host:port.
	Server string `json:"server"`
	// CAFile is a PEM file of authorities to trust for Server; empty for the
	// system's.
	CAFile string `json:"caFile,omitempty"`
	// TLSClientCert and TLSClientKey are the files of a TLS client
	// certificate and its private key; both or neither.
	TLSClientCert string `json:"tlsClientCert,omitempty"`
	TLSClientKey  string `json:"tlsClientKey,omitempty"`
	// KeyFile is the file that holds the key to send.
	KeyFile string `json:"keyFile"`
	// Tenant is the tenant a command acts on when it is not given one where
	// naming a tenant is optional; empty for none.
	Tenant string `json:"tenant,omitempty"`
}

// File is the content of the file of contexts.
type File struct {
	// Version is the format of the file; 1.
	Version int `json:"version"`
	// Current is the name of the context used when none is asked for; empty
	// for none.
	Current string `json:"current,omitempty"`
	// Contexts are the contexts by name.
	Contexts map[string]Context `json:"contexts"`

	// loaded and read say that the file came from Load and what it held, so
	// Save can refuse to replace a file that changed since.
	loaded bool
	read   stamp
}

// stamp identifies the content of the file at one moment: whether it existed,
// its size and a hash of its bytes.
type stamp struct {
	exists bool
	size   int
	sum    [sha256.Size]byte
}

// stampOf reads the file at path as a stamp; a missing file is the zero stamp.
func stampOf(path string) (stamp, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the person's own contexts file
	if errors.Is(err, fs.ErrNotExist) {
		return stamp{}, nil
	}
	if err != nil {
		return stamp{}, err
	}
	return stamp{exists: true, size: len(b), sum: sha256.Sum256(b)}, nil
}

// Location is where the file is and how strictly its directory is held.
type Location struct {
	// Path is the file.
	Path string
	// Own is true for the default place, a directory this program makes and
	// keeps private to the person; false when the file was named with
	// EnvConfig, a directory that may hold other things and that is refused
	// only when others can change it.
	Own bool
}

// Locate returns where the file of contexts is: EnvConfig when set, else
// nopacs/contexts.json under XDG_CONFIG_HOME, or under %AppData% on Windows,
// or under ~/.config. The same place on macOS as on Linux, because that is
// where a person who uses the command line on both looks for it.
func Locate(getenv func(string) string) (Location, error) {
	if p := getenv(EnvConfig); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return Location{}, fault.Wrap(fault.Invalid, err, "cannot use "+EnvConfig+": "+err.Error())
		}
		return Location{Path: abs}, nil
	}
	var base string
	switch {
	case getenv("XDG_CONFIG_HOME") != "" && filepath.IsAbs(getenv("XDG_CONFIG_HOME")):
		base = getenv("XDG_CONFIG_HOME")
	case runtime.GOOS == "windows" && getenv("AppData") != "":
		base = getenv("AppData")
	default:
		home := getenv("HOME")
		if home == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return Location{}, fault.New(fault.Invalid, "cannot find the place of the contexts file: no home directory; set "+EnvConfig)
			}
			home = h
		}
		base = filepath.Join(home, ".config")
	}
	return Location{Path: filepath.Join(base, "nopacs", fileName), Own: true}, nil
}

// ValidName checks the name of a context: letters, digits, dot, underscore
// and hyphen, starting with a letter or a digit, so a name is safe in a
// message, a shell and a file.
func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return fault.New(fault.Invalid, "a context name uses letters, digits, '.', '_' and '-', starts with a letter or a digit, and is at most 63 characters")
	}
	return nil
}

// Load reads the file. A missing file is an empty one. A file or a directory
// that others can change or read (on Unix; on Windows the owner and access
// list of the file), a file of another version, one with a field this program
// does not know, and a current context that is not defined are refused with a
// message that names the file.
func Load(loc Location) (*File, error) {
	if err := checkDir(loc); err != nil {
		return nil, err
	}
	f, err := secret.OpenPrivate(loc.Path)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			return &File{Version: version, Contexts: map[string]Context{}, loaded: true}, nil
		}
		msg := strings.ReplaceAll(strings.TrimPrefix(fault.Message(err), "secret: "), "the key file", "the contexts file")
		return nil, fault.Wrap(fault.Invalid, err, msg)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "cannot read the contexts file "+loc.Path)
	}
	if len(b) > maxFile {
		return nil, fault.New(fault.Invalid, "the contexts file "+loc.Path+" is larger than a file of contexts can be")
	}
	var out File
	out.loaded = true
	out.read = stamp{exists: true, size: len(b), sum: sha256.Sum256(b)}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fault.New(fault.Invalid, "the contexts file "+loc.Path+" is not a file of contexts: "+jsonProblem(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fault.New(fault.Invalid, "the contexts file "+loc.Path+" holds more than one document")
	}
	if out.Version != version {
		return nil, fault.New(fault.Invalid, "the contexts file "+loc.Path+" is of a version this program does not read")
	}
	if out.Contexts == nil {
		out.Contexts = map[string]Context{}
	}
	if len(out.Contexts) > maxContexts {
		return nil, fault.New(fault.Invalid, "the contexts file "+loc.Path+" holds too many contexts")
	}
	for name, c := range out.Contexts {
		if err := validate(name, c); err != nil {
			return nil, fault.New(fault.Invalid, "the contexts file "+loc.Path+": "+fault.Message(err))
		}
	}
	if out.Current != "" {
		if _, ok := out.Contexts[out.Current]; !ok {
			return nil, fault.New(fault.Invalid, "the contexts file "+loc.Path+" names a current context that it does not define")
		}
	}
	return &out, nil
}

// keyPrefix starts every key; a value that starts with it is a key. The id
// after it is random base58, so it cannot be told apart by its first letter.
const keyPrefix = "nopacs_"

// validate checks one context as a hand-edited file may have made it, so that
// nothing in it can make the program print or send a secret: the name and the
// tenant follow their rules, the server is a plain address with no user
// information, every path is absolute, and no value is a key. No message
// quotes a value.
func validate(name string, c Context) error {
	if ValidName(name) != nil || strings.HasPrefix(name, keyPrefix) {
		return fault.New(fault.Invalid, "a context has a name that is not valid")
	}
	for _, f := range []struct{ field, v string }{
		{"server", c.Server}, {"caFile", c.CAFile}, {"tlsClientCert", c.TLSClientCert},
		{"tlsClientKey", c.TLSClientKey}, {"keyFile", c.KeyFile}, {"tenant", c.Tenant},
	} {
		if strings.HasPrefix(strings.TrimSpace(f.v), keyPrefix) {
			return fault.New(fault.Invalid, "the field "+f.field+" of context "+name+" looks like a key; a context holds the path of a key file, never a key")
		}
	}
	if _, err := client.ParseServer(c.Server); err != nil {
		return fault.New(fault.Invalid, "the server of context "+name+" is not valid: "+fault.Message(err))
	}
	if c.KeyFile == "" {
		return fault.New(fault.Invalid, "context "+name+" has no keyFile")
	}
	for _, f := range []struct{ field, v string }{
		{"keyFile", c.KeyFile}, {"caFile", c.CAFile}, {"tlsClientCert", c.TLSClientCert}, {"tlsClientKey", c.TLSClientKey},
	} {
		if f.v != "" && !filepath.IsAbs(f.v) {
			return fault.New(fault.Invalid, "the field "+f.field+" of context "+name+" is not an absolute path")
		}
	}
	if (c.TLSClientCert == "") != (c.TLSClientKey == "") {
		return fault.New(fault.Invalid, "context "+name+" needs both tlsClientCert and tlsClientKey, or neither")
	}
	if c.Tenant != "" && !pathname.ValidName(c.Tenant) {
		return fault.New(fault.Invalid, "the tenant of context "+name+" is not a valid tenant name")
	}
	return nil
}

// Validate checks a context before it is saved, by the rules Load applies.
func Validate(name string, c Context) error { return validate(name, c) }

// jsonProblem says what is wrong with a document without quoting any of it:
// a decoder's message for an unknown field would name the field, which is
// fine, but one for a bad value can hold the value.
func jsonProblem(err error) string {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return "it is not JSON"
	case errors.As(err, &typ):
		return "the field " + typ.Field + " has the wrong type"
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// The field's name is not quoted: it is whatever the file held, which
		// may be a secret written as a name.
		return "it has a field this program does not know; a context holds paths only, never a key"
	}
	return "it cannot be read"
}

// checkDir refuses a directory in which another user could replace the file:
// one that is not the person's, that others can write or, for the default
// place, read, or that has an access list entry on macOS (internal/secret). It
// is skipped on Windows, where the file's own access list is checked.
func checkDir(loc Location) error {
	dir := filepath.Dir(loc.Path)
	err := secret.CheckPrivateDir(dir, loc.Own)
	switch fault.KindOf(err) {
	case fault.NotFound:
		return nil
	}
	if err == nil {
		return nil
	}
	if fault.KindOf(err) != fault.Invalid {
		return fault.Wrap(fault.Invalid, err, "cannot check the directory of the contexts file "+loc.Path)
	}
	msg := strings.TrimPrefix(fault.Message(err), "secret: ")
	if loc.Own {
		return fault.Wrap(fault.Invalid, err, msg+"; the contexts file "+loc.Path+" is kept in a directory only you can reach: chmod 700 "+dir)
	}
	return fault.Wrap(fault.Invalid, err, msg+"; keep the contexts file "+loc.Path+" in a private directory of your own and set "+EnvConfig+" to it")
}

// Names returns the names of the contexts in order.
func (f *File) Names() []string {
	names := make([]string, 0, len(f.Contexts))
	for n := range f.Contexts {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Save writes the file atomically: the new content goes to a file of its own
// in the same directory, private from its first byte, is flushed, and
// replaces the old file by rename, so a reader or a crash sees the old file or
// the new one and never half of either. The directory is made private when it
// is made.
func (f *File) Save(loc Location) error {
	f.Version = version
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fault.Wrap(fault.Internal, err, "encoding the contexts: "+err.Error())
	}
	b = append(b, '\n')
	dir := filepath.Dir(loc.Path)
	if err := checkDir(loc); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fault.Wrap(fault.Invalid, err, "cannot make the directory "+dir+" of the contexts file: "+err.Error())
	}
	if f.loaded {
		now, err := stampOf(loc.Path)
		if err != nil {
			return fault.Wrap(fault.Invalid, err, "cannot read the contexts file "+loc.Path+": "+err.Error())
		}
		if now != f.read {
			return fault.New(fault.Precondition, "the contexts file "+loc.Path+" changed while this command ran, so nothing was saved; run the command again")
		}
	}
	tmp, err := secret.CreatePrivateTemp(dir, ".contexts-*")
	if err != nil {
		return fault.Wrap(fault.Invalid, err, "cannot write the contexts file "+loc.Path+": "+err.Error())
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return fault.Wrap(fault.Invalid, err, "cannot write the contexts file "+loc.Path+": "+err.Error())
	}
	if err := tmp.Sync(); err != nil {
		return fault.Wrap(fault.Invalid, err, "cannot write the contexts file "+loc.Path+": "+err.Error())
	}
	if err := tmp.Close(); err != nil {
		return fault.Wrap(fault.Invalid, err, "cannot write the contexts file "+loc.Path+": "+err.Error())
	}
	if err := os.Rename(name, loc.Path); err != nil {
		return fault.Wrap(fault.Invalid, err, "cannot replace the contexts file "+loc.Path+": "+err.Error())
	}
	done = true
	f.read, _ = stampOf(loc.Path)
	// The new name survives a power loss only once the directory is flushed;
	// where a directory cannot be opened for it (Windows) the rename stands.
	if d, err := os.Open(dir); err == nil { // #nosec G304 -- the directory of the person's own file
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
