package cli

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/contexts"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/version"
)

const (
	// checkParallel bounds how many servers "context list --check" asks at
	// once, so a long list never opens a long list of connections.
	checkParallel = 8
	// checkTimeout bounds one context's check, so a server that does not
	// answer costs a few seconds of the whole run, not the default call
	// deadline. --timeout overrides it.
	checkTimeout = 5 * time.Second
)

// checkGrantDoc is one grant of a checked context's key.
type checkGrantDoc struct {
	Scope string `json:"scope"`
	Role  string `json:"role"`
}

// checkDoc is the JSON form of one context after "context list --check": the
// context as the file holds it, and what its server answered to WhoAmI. The
// error holds the reason in words and never a key, an address with a user
// name, or a file's contents.
type checkDoc struct {
	contextDoc
	Reachable      bool            `json:"reachable"`
	Error          string          `json:"error,omitempty"`
	KeyID          string          `json:"keyId,omitempty"`
	Holder         string          `json:"holder,omitempty"`
	LocalOperator  bool            `json:"localOperator,omitempty"`
	ExpireTime     string          `json:"expireTime,omitempty"`
	Grants         []checkGrantDoc `json:"grants"`
	ServerVersion  string          `json:"serverVersion,omitempty"`
	ServerCommit   string          `json:"serverCommit,omitempty"`
	VersionDiffers bool            `json:"versionDiffers"`
}

// withoutKeyVariable hides the key variable from a client made for a saved
// context: a context's key comes from its own file only, so an exported key
// is never sent to every server in the list.
func withoutKeyVariable(getenv func(string) string) func(string) string {
	return func(name string) string {
		if name == client.EnvKey {
			return ""
		}
		return getenv(name)
	}
}

// checkContext asks one context's server who the key is. Whatever goes wrong
// is the answer for this context alone.
func (a *app) checkContext(ctx context.Context, timeout time.Duration, name string, c contexts.Context, current bool) checkDoc {
	doc := checkDoc{contextDoc: docOf(name, c, current), Grants: []checkGrantDoc{}}
	cl, err := client.New(client.Config{
		Server: c.Server, KeyFile: c.KeyFile, CAFile: c.CAFile, ClientCert: c.TLSClientCert, ClientKey: c.TLSClientKey,
		Getenv: withoutKeyVariable(a.env.Getenv), Timeout: timeout,
	})
	if err != nil {
		doc.Error = output.Clean(fault.Message(err))
		return doc
	}
	defer cl.Close()
	res, err := client.Call(ctx, cl, "context list --check", func(ctx context.Context) (*v1.WhoAmIResponse, error) {
		return cl.Access.WhoAmI(ctx, &v1.WhoAmIRequest{})
	})
	if err != nil {
		// The fault's message is what the server or the client says safely;
		// the call's own name and request id would only repeat the command.
		doc.Error = output.Clean(fault.Message(err))
		return doc
	}
	doc.Reachable = true
	switch caller := res.GetPrincipal().GetCaller().(type) {
	case *v1.Principal_Key:
		doc.KeyID, doc.Holder = caller.Key.GetId(), caller.Key.GetHolder()
		if t := caller.Key.GetExpireTime(); t != nil {
			doc.ExpireTime = t.AsTime().In(a.env.Location).Format(time.RFC3339)
		}
	case *v1.Principal_LocalOperator:
		doc.LocalOperator = true
	}
	for _, g := range res.GetPrincipal().GetGrants() {
		doc.Grants = append(doc.Grants, checkGrantDoc{Scope: scopeText(g.GetScope()), Role: roleText(g.GetRole())})
	}
	doc.ServerVersion, doc.ServerCommit = output.Clean(res.GetServerVersion()), output.Clean(res.GetServerCommit())
	doc.VersionDiffers = version.Differ(version.Get(), remoteInfo(res))
	return doc
}

// checkAll checks every named context at most checkParallel at a time and
// returns the results in the order of names.
func (a *app) checkAll(ctx context.Context, timeout time.Duration, names []string, file *contexts.File) []checkDoc {
	docs := make([]checkDoc, len(names))
	sem := make(chan struct{}, checkParallel)
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			docs[i] = a.checkContext(ctx, timeout, n, file.Contexts[n], n == file.Current)
		}()
	}
	wg.Wait()
	return docs
}

// checkOut writes the results of "context list --check" and returns the error
// that sets the exit code: nil when every context answered, else exit 1 with
// one line saying how many did not.
func (a *app) checkOut(docs []checkDoc, current, path string) error {
	failed := 0
	for _, d := range docs {
		if !d.Reachable {
			failed++
		}
		if d.Reachable && d.VersionDiffers {
			a.line("warning: the server of context " + d.Name + " is " + d.ServerVersion + " and this program is " +
				version.Get().Version + "; they may not understand each other (see nopacs version)")
		}
	}
	var err error
	if a.opts.json {
		err = a.write(func(w io.Writer) error {
			return output.JSONValue(w, struct {
				Current   string     `json:"current"`
				Contexts  []checkDoc `json:"contexts"`
				Reachable int        `json:"reachable"`
				Failed    int        `json:"failed"`
				More      bool       `json:"more"`
				File      string     `json:"file"`
			}{current, docs, len(docs) - failed, failed, false, path})
		})
	} else {
		rows := make([][]string, 0, len(docs))
		for _, d := range docs {
			mark := ""
			if d.Current {
				mark = "*"
			}
			if !d.Reachable {
				rows = append(rows, []string{mark, d.Name, d.Server, "unreachable: " + d.Error, "-", "-", "-", "-"})
				continue
			}
			key := "local operator"
			if !d.LocalOperator {
				key = d.KeyID
				if d.Holder != "" {
					key += " (" + d.Holder + ")"
				}
			}
			expires := "never"
			if d.ExpireTime != "" {
				expires = d.ExpireTime
			}
			grants := make([]string, 0, len(d.Grants))
			for _, g := range d.Grants {
				grants = append(grants, g.Scope+" "+g.Role)
			}
			rows = append(rows, []string{mark, d.Name, d.Server, "reachable", key, expires, output.Dash(strings.Join(grants, ", ")), output.Dash(d.ServerVersion)})
		}
		err = a.write(func(w io.Writer) error {
			return output.Table(w, a.style(), []string{"", "NAME", "SERVER", "STATUS", "KEY", "EXPIRES", "GRANTS", "VERSION"}, rows)
		})
	}
	if err != nil {
		return err
	}
	if failed > 0 {
		a.line("context list --check: " + strconv.Itoa(failed) + " of " + strconv.Itoa(len(docs)) + " contexts did not answer")
		return &exitError{exitUnreachable}
	}
	return nil
}

// exitUnreachable is the exit status of "context list --check" when a context
// did not answer: 1, the status of a failure, since the command could not do
// what it was asked for on every context.
const exitUnreachable = 1

// checkTimeoutFor is the deadline of one context's check: the one the person
// gave with --timeout, else checkTimeout.
func (a *app) checkTimeoutFor(cmd *cobra.Command) time.Duration {
	if cmd.Flags().Changed("timeout") {
		return a.opts.timeout
	}
	return checkTimeout
}
