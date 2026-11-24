package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// maxRoutesFile bounds a routes file read: 64 routes of 16 conditions of
// 1 KiB each fit with room to spare.
const maxRoutesFile = 4 << 20

// routesFile is the file form of a receiver's routing, as "route export"
// writes it and "route import" reads it: the receiver it came from, the
// time that receiver was last changed when it was read, and the routing
// as the API's ReceiverRouting in its JSON form.
type routesFile struct {
	Receiver   string          `json:"receiver"`
	UpdateTime string          `json:"updateTime,omitempty"`
	Routing    json.RawMessage `json:"routing"`
}

// newRouteExport builds "receiver route export".
func (a *app) newRouteExport() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "export <tenant> <receiver> [--to FILE]",
		Short: "Write a receiver's routing to a file, with the time the receiver was last changed",
		Long: `Write a receiver's whole routing (its routes in order and its default) as one
JSON document, to standard output or to --to: the receiver's name, the time it
was last changed (updateTime), and the routing as the API's ReceiverRouting.
Edit it and give it back to "route import", which writes it only if the
receiver has not changed since. Needs the reader role on the tenant.`,
		Example: `  nopacs receiver route export lab1 ct1 --to routes.json`,
		Args:    exactArgs(2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetReceiverResponse, error) {
				return c.Receivers.GetReceiver(ctx, &v1.GetReceiverRequest{Tenant: args[0], Name: args[1]})
			})
			if err != nil {
				return err
			}
			r := res.GetReceiver()
			routing := r.GetRouting()
			if routing == nil {
				routing = &v1.ReceiverRouting{}
			}
			raw, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(routing)
			if err != nil {
				return fault.Wrap(fault.Internal, err, "writing the routing: "+err.Error())
			}
			doc := routesFile{Receiver: r.GetName(), Routing: raw}
			if t := r.GetUpdateTime(); t != nil {
				doc.UpdateTime = t.AsTime().UTC().Format(time.RFC3339Nano)
			}
			var b bytes.Buffer
			if err := output.JSONValue(&b, doc); err != nil {
				return fault.Wrap(fault.Internal, err, "writing the routing: "+err.Error())
			}
			if to == "" || a.opts.json {
				if err := a.write(func(w io.Writer) error { _, err := w.Write(b.Bytes()); return err }); err != nil || to == "" {
					return err
				}
			}
			if err := os.WriteFile(to, b.Bytes(), 0o600); err != nil {
				return fault.Wrap(fault.Invalid, err, "cannot write --to "+to)
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&to, "to", "", "the file to write; standard output when not given")
	return cmd
}

// readRoutesFile reads a routes file from a path, or standard input for
// "-". Its routing is read strictly: a field ReceiverRouting does not have
// is refused, so a misspelled field is an error, never ignored.
func (a *app) readRoutesFile(from string) (routesFile, *v1.ReceiverRouting, error) {
	var b []byte
	var err error
	if from == "-" {
		if a.env.Stdin == nil {
			return routesFile{}, nil, fault.New(fault.Invalid, "- has no standard input to read")
		}
		b, err = io.ReadAll(io.LimitReader(a.env.Stdin, maxRoutesFile+1))
	} else {
		var f *os.File
		if f, err = os.Open(from); err == nil { // #nosec G304 -- the person names the file to import.
			b, err = io.ReadAll(io.LimitReader(f, maxRoutesFile+1))
			_ = f.Close()
		}
	}
	if err != nil {
		return routesFile{}, nil, fault.Wrap(fault.Invalid, err, "cannot read "+from)
	}
	if len(b) > maxRoutesFile {
		return routesFile{}, nil, fault.New(fault.Invalid, from+" is larger than 4MiB")
	}
	var doc struct {
		SchemaVersion int `json:"schemaVersion"`
		routesFile
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return routesFile{}, nil, fault.New(fault.Invalid, from+" is not a routes file as \"route export\" writes it: "+err.Error())
	}
	if doc.SchemaVersion != 0 && doc.SchemaVersion != output.SchemaVersion {
		return routesFile{}, nil, fault.New(fault.Invalid, fmt.Sprintf("%s is of schemaVersion %d; this command reads %d", from, doc.SchemaVersion, output.SchemaVersion))
	}
	routing := &v1.ReceiverRouting{}
	if len(doc.Routing) > 0 {
		if err := protojson.Unmarshal(doc.Routing, routing); err != nil {
			return routesFile{}, nil, fault.New(fault.Invalid, "the routing in "+from+" is not a ReceiverRouting: "+output.Clean(err.Error()))
		}
	}
	return doc.routesFile, routing, nil
}

// newRouteImport builds "receiver route import".
func (a *app) newRouteImport() *cobra.Command {
	var ignoreTime bool
	cmd := &cobra.Command{
		Use:   "import <tenant> <receiver> FILE|- [--ignore-update-time]",
		Short: "Replace a receiver's routing whole with a file's, if the receiver has not changed since its export",
		Long: `Replace a receiver's whole routing (its routes and its default) with a file's,
as "route export" writes it. The change is made only if the receiver was last
changed at the file's updateTime, so routes edited from an export never undo a
change somebody made since (exit 5, nothing changed: export again and redo the
edit). A file without an updateTime, a file exported from another receiver, or
--ignore-update-time, is written over the receiver as it is when read now.
An empty routing removes every route. Needs the admin role on the tenant.`,
		Example: `  nopacs receiver route import lab1 ct1 routes.json
  nopacs receiver route import lab1 ct2 ct1-routes.json --ignore-update-time`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			doc, routing, err := a.readRoutesFile(args[2])
			if err != nil {
				return err
			}
			var expected *timestamppb.Timestamp
			if doc.UpdateTime != "" && !ignoreTime && doc.Receiver == args[1] {
				t, err := time.Parse(time.RFC3339Nano, doc.UpdateTime)
				if err != nil {
					return fault.New(fault.Invalid, "the updateTime in "+args[2]+" is not a time in RFC 3339")
				}
				expected = timestamppb.New(t)
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			if expected == nil {
				got, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetReceiverResponse, error) {
					return c.Receivers.GetReceiver(ctx, &v1.GetReceiverRequest{Tenant: args[0], Name: args[1]})
				})
				if err != nil {
					return err
				}
				expected = got.GetReceiver().GetUpdateTime()
			}
			if len(routing.GetRoutes()) == 0 && !routing.GetQuarantineUnmatched() {
				routing = nil
			}
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetReceiverResponse, error) {
				return c.Receivers.SetReceiver(ctx, &v1.SetReceiverRequest{Tenant: args[0],
					Receiver: &v1.Receiver{Name: args[1], Routing: routing}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"routing"}},
					ExpectedUpdateTime: expected})
			})
			if err != nil {
				return err
			}
			return a.routingOut(res.GetReceiver())
		}),
	}
	cmd.Flags().BoolVar(&ignoreTime, "ignore-update-time", false, "write over the receiver as it is now, not only if it is as it was exported")
	return cmd
}
