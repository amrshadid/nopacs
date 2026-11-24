package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
)

// routeSubjects are the words the route commands name a condition's subject
// by, each with the contract's value.
var routeSubjects = []struct {
	word    string
	subject v1.ConditionSubject
}{
	{"modality", v1.ConditionSubject_CONDITION_SUBJECT_MODALITY},
	{"sop-class-uid", v1.ConditionSubject_CONDITION_SUBJECT_SOP_CLASS_UID},
	{"manufacturer", v1.ConditionSubject_CONDITION_SUBJECT_MANUFACTURER},
	{"model-name", v1.ConditionSubject_CONDITION_SUBJECT_MODEL_NAME},
	{"software-versions", v1.ConditionSubject_CONDITION_SUBJECT_SOFTWARE_VERSIONS},
	{"calling-aet", v1.ConditionSubject_CONDITION_SUBJECT_CALLING_AE_TITLE},
	{"called-aet", v1.ConditionSubject_CONDITION_SUBJECT_CALLED_AE_TITLE},
}

// subjectWords lists the subjects' words for a message.
func subjectWords() string {
	words := make([]string, len(routeSubjects))
	for i, s := range routeSubjects {
		words[i] = s.word
	}
	return strings.Join(words, ", ")
}

// subjectOf returns the subject a word names.
func subjectOf(flag, word string) (v1.ConditionSubject, error) {
	for _, s := range routeSubjects {
		if s.word == word {
			return s.subject, nil
		}
	}
	return 0, fault.New(fault.Invalid, fmt.Sprintf("--%s: %q is not a subject; one of %s", flag, output.Clean(word), subjectWords()))
}

// wordOf returns a subject's word, or its contract name for one this
// version has no word for.
func wordOf(s v1.ConditionSubject) string {
	for _, w := range routeSubjects {
		if w.subject == s {
			return w.word
		}
	}
	return s.String()
}

// routeFlags are the flags that make a route or change one.
type routeFlags struct {
	storage                              string
	equals, notEquals, prefix, notPrefix []string
	pattern, notPattern, present, absent []string
	noConditions                         bool
	profile                              string
	receiverProfile                      bool
	forwardTo                            []string
	noForward, receiverSenders           bool
	position                             int
}

// add declares the flags; forSet adds those that only a change has.
func (f *routeFlags) add(cmd *cobra.Command, forSet bool) {
	fl := cmd.Flags()
	fl.StringVar(&f.storage, "storage", "", "the tenant's storage a new session the route chooses is archived to")
	fl.StringArrayVar(&f.equals, "equals", nil, "a condition: the value of SUBJECT equals TEXT, written SUBJECT=TEXT (repeat for more; all must hold)")
	fl.StringArrayVar(&f.notEquals, "not-equals", nil, "a condition: the value of SUBJECT does not equal TEXT, written SUBJECT=TEXT")
	fl.StringArrayVar(&f.prefix, "prefix", nil, "a condition: the value of SUBJECT starts with TEXT, written SUBJECT=TEXT")
	fl.StringArrayVar(&f.notPrefix, "not-prefix", nil, "a condition: the value of SUBJECT does not start with TEXT, written SUBJECT=TEXT")
	fl.StringArrayVar(&f.pattern, "pattern", nil, "a condition: the whole value of SUBJECT matches the RE2 expression, written SUBJECT=EXPRESSION")
	fl.StringArrayVar(&f.notPattern, "not-pattern", nil, "a condition: the whole value of SUBJECT does not match the RE2 expression, written SUBJECT=EXPRESSION")
	fl.StringArrayVar(&f.present, "present", nil, "a condition: SUBJECT has a value")
	fl.StringArrayVar(&f.absent, "absent", nil, "a condition: SUBJECT is absent or empty")
	fl.StringVar(&f.profile, "profile", "", "de-identify the studies the route chooses by this profile version, NAME@VERSION, in place of the receiver's")
	fl.StringArrayVar(&f.forwardTo, "forward-to", nil, "forward the receptions the route chooses to this sender in place of the receiver's (repeat for more)")
	fl.BoolVar(&f.noForward, "no-forward", false, "forward the receptions the route chooses to no sender")
	cmd.MarkFlagsMutuallyExclusive("forward-to", "no-forward")
	if forSet {
		fl.BoolVar(&f.noConditions, "no-conditions", false, "remove every condition, so the route holds for every study")
		fl.BoolVar(&f.receiverProfile, "receiver-profile", false, "de-identify by the receiver's profile again")
		fl.BoolVar(&f.receiverSenders, "receiver-senders", false, "forward to the receiver's senders again")
		cmd.MarkFlagsMutuallyExclusive("profile", "receiver-profile")
		cmd.MarkFlagsMutuallyExclusive("forward-to", "receiver-senders")
		cmd.MarkFlagsMutuallyExclusive("no-forward", "receiver-senders")
		return
	}
	fl.IntVar(&f.position, "position", 0, "the route's place in the order, from 1; the last when absent")
}

// conditionFlags reports whether any flag of a condition was given.
func (f *routeFlags) conditionFlags(cmd *cobra.Command) bool {
	for _, n := range []string{"equals", "not-equals", "prefix", "not-prefix", "pattern", "not-pattern", "present", "absent"} {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// conditions returns the conditions the flags give, in the order of the
// flags' kinds.
func (f *routeFlags) conditions() ([]*v1.ProfileCondition, error) {
	var out []*v1.ProfileCondition
	pairs := []struct {
		flag   string
		values []string
		negate bool
		test   func(string) any
	}{
		{"equals", f.equals, false, func(v string) any { return &v1.ProfileCondition_Equals{Equals: v} }},
		{"not-equals", f.notEquals, true, func(v string) any { return &v1.ProfileCondition_Equals{Equals: v} }},
		{"prefix", f.prefix, false, func(v string) any { return &v1.ProfileCondition_Prefix{Prefix: v} }},
		{"not-prefix", f.notPrefix, true, func(v string) any { return &v1.ProfileCondition_Prefix{Prefix: v} }},
		{"pattern", f.pattern, false, func(v string) any { return &v1.ProfileCondition_Pattern{Pattern: v} }},
		{"not-pattern", f.notPattern, true, func(v string) any { return &v1.ProfileCondition_Pattern{Pattern: v} }},
	}
	for _, p := range pairs {
		for _, v := range p.values {
			word, text, ok := strings.Cut(v, "=")
			if !ok {
				return nil, fault.New(fault.Invalid, fmt.Sprintf("--%s %q is not SUBJECT=TEXT", p.flag, output.Clean(v)))
			}
			s, err := subjectOf(p.flag, word)
			if err != nil {
				return nil, err
			}
			c := &v1.ProfileCondition{Subject: s, Negate: p.negate}
			switch t := p.test(text).(type) {
			case *v1.ProfileCondition_Equals:
				c.Test = t
			case *v1.ProfileCondition_Prefix:
				c.Test = t
			case *v1.ProfileCondition_Pattern:
				c.Test = t
			}
			out = append(out, c)
		}
	}
	for _, p := range []struct {
		flag    string
		values  []string
		present bool
	}{{"present", f.present, true}, {"absent", f.absent, false}} {
		for _, word := range p.values {
			s, err := subjectOf(p.flag, word)
			if err != nil {
				return nil, err
			}
			out = append(out, &v1.ProfileCondition{Subject: s, Test: &v1.ProfileCondition_Present{Present: p.present}})
		}
	}
	return out, nil
}

// apply sets on rt what the flags give: on a new route everything given,
// on one changed only what a flag names.
func (f *routeFlags) apply(cmd *cobra.Command, rt *v1.Route) error {
	fl := cmd.Flags()
	if fl.Changed("storage") {
		rt.Storage = f.storage
	}
	if f.conditionFlags(cmd) || f.noConditions {
		conds, err := f.conditions()
		if err != nil {
			return err
		}
		rt.Conditions = conds
	}
	switch {
	case fl.Changed("profile"):
		ref, err := profile.ParseRef(f.profile)
		if err != nil || ref.Version == 0 {
			return fault.New(fault.Invalid, fmt.Sprintf("--profile %q is not NAME@VERSION, such as research@3: a route names the exact version it runs", output.Clean(f.profile)))
		}
		rt.Profile = &v1.ProfileVersionRef{Name: ref.Name, Version: ref.Version}
	case f.receiverProfile:
		rt.Profile = nil
	}
	switch {
	case fl.Changed("forward-to"):
		rt.ForwardTo = &v1.RouteForwardTo{Senders: nonEmpty(f.forwardTo)}
	case f.noForward:
		rt.ForwardTo = &v1.RouteForwardTo{}
	case f.receiverSenders:
		rt.ForwardTo = nil
	}
	return nil
}

// newReceiverRoute builds "receiver route".
func (a *app) newReceiverRoute() *cobra.Command {
	return group("route", "List and change the routes a receiver chooses each study's storage by",
		`A receiver's routes choose, for each study it receives, the storage a new
session is archived to and, optionally, the profile version that de-identifies
it in place of the receiver's and the senders it is forwarded to in place of the
receiver's. A study is routed once, for its session: the routes are tried in
order on the first file of its first reception, as received, and the first
whose conditions all hold chooses. Every later arrival of the session, from any
AE title, goes by that choice: its storage, profile and senders. A condition
reads one of `+subjectWords()+`;
an AE title is the one the association gave. A value longer than 1024 bytes is
not tested, and the study is held as unrouted.

A study no route chooses goes to the receiver's storage, with its profile and
senders, unless "route default ... quarantine" holds it, every file quarantined
as unrouted, for a person to release naming a storage (nopacs quarantine release
--storage). Only a receiver that de-identifies may have a route that names a
profile: a route replaces the profile, never turns de-identification off.

Every change reads the receiver, changes its routes, and writes them back only
if nobody changed the receiver meanwhile; otherwise it is refused (exit 5)
and changes nothing, and is run again. export writes the routing to a file
with the time the receiver was last changed, and import writes such a file
back whole, only if the receiver has not changed since the export. Changing needs the admin role on the
tenant; list needs reader.`,
		a.newRouteList(), a.newRouteAdd(), a.newRouteSet(), a.newRouteRemove(), a.newRouteMove(), a.newRouteDefault(),
		a.newRouteExport(), a.newRouteImport())
}

// changeRouting reads a receiver, lets change edit its routing, and writes
// the routing back on the condition that the receiver did not change after
// it was read; it prints the routing as written.
func (a *app) changeRouting(ctx context.Context, cmd *cobra.Command, tenantName, receiver string, change func(r *v1.ReceiverRouting) error) error {
	c, closeClient, err := a.open(cmd)
	if err != nil {
		return err
	}
	defer closeClient()
	got, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.GetReceiverResponse, error) {
		return c.Receivers.GetReceiver(ctx, &v1.GetReceiverRequest{Tenant: tenantName, Name: receiver})
	})
	if err != nil {
		return err
	}
	read := got.GetReceiver()
	routing := proto.CloneOf(read.GetRouting())
	if routing == nil {
		routing = &v1.ReceiverRouting{}
	}
	if err := change(routing); err != nil {
		return err
	}
	if len(routing.GetRoutes()) == 0 && !routing.GetQuarantineUnmatched() {
		routing = nil
	}
	res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.SetReceiverResponse, error) {
		return c.Receivers.SetReceiver(ctx, &v1.SetReceiverRequest{Tenant: tenantName,
			Receiver: &v1.Receiver{Name: receiver, Routing: routing}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"routing"}},
			ExpectedUpdateTime: read.GetUpdateTime()})
	})
	if err != nil {
		return err
	}
	return a.routingOut(res.GetReceiver())
}

// routingOut writes a receiver's routing: the JSON document of its routing
// with --json, and otherwise a table of its routes and its default.
func (a *app) routingOut(r *v1.Receiver) error {
	routing := r.GetRouting()
	if routing == nil {
		routing = &v1.ReceiverRouting{}
	}
	return messageOut(a, routing, func(w io.Writer) error { return a.routingTable(w, r) })
}

// routingTable writes a receiver's routes as a table and its default.
func (a *app) routingTable(w io.Writer, r *v1.Receiver) error {
	rows := make([][]string, 0, len(r.GetRouting().GetRoutes()))
	for i, rt := range r.GetRouting().GetRoutes() {
		prof := "receiver's"
		if p := rt.GetProfile(); p != nil {
			prof = p.GetName() + "@" + strconv.Itoa(int(p.GetVersion()))
		}
		fwd := "receiver's"
		if f := rt.GetForwardTo(); f != nil {
			fwd = "none"
			if len(f.GetSenders()) > 0 {
				fwd = strings.Join(f.GetSenders(), ", ")
			}
		}
		rows = append(rows, []string{strconv.Itoa(i + 1), rt.GetName(), conditionsText(rt.GetConditions()), rt.GetStorage(), prof, fwd})
	}
	if err := output.Table(w, a.style(), []string{"#", "ROUTE", "WHEN", "STORAGE", "PROFILE", "FORWARD TO"}, rows); err != nil {
		return err
	}
	otherwise := "the receiver's storage, " + r.GetStorage()
	if r.GetRouting().GetQuarantineUnmatched() {
		otherwise = "held as unrouted"
	}
	_, err := fmt.Fprintf(w, "A study no route chooses: %s\n", otherwise)
	return err
}

// conditionsText words a route's conditions, "every study" for none.
func conditionsText(cs []*v1.ProfileCondition) string {
	if len(cs) == 0 {
		return "every study"
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		not := ""
		if c.GetNegate() {
			not = "not "
		}
		var text string
		switch t := c.GetTest().(type) {
		case *v1.ProfileCondition_Equals:
			text = not + "= " + strconv.Quote(t.Equals)
		case *v1.ProfileCondition_Prefix:
			text = not + "starts " + strconv.Quote(t.Prefix)
		case *v1.ProfileCondition_Pattern:
			text = not + "~ " + strconv.Quote(t.Pattern)
		case *v1.ProfileCondition_Present:
			if t.Present != c.GetNegate() {
				text = "present"
			} else {
				text = "absent"
			}
		}
		parts = append(parts, wordOf(c.GetSubject())+" "+text)
	}
	return strings.Join(parts, " and ")
}

// routeIndex returns the place of the route of that name, or a NotFound
// error.
func routeIndex(r *v1.ReceiverRouting, name string) (int, error) {
	i := slices.IndexFunc(r.GetRoutes(), func(rt *v1.Route) bool { return rt.GetName() == name })
	if i < 0 {
		return 0, fault.New(fault.NotFound, fmt.Sprintf("the receiver has no route named %q", output.Clean(name)))
	}
	return i, nil
}

// newRouteList builds "receiver route list".
func (a *app) newRouteList() *cobra.Command {
	return &cobra.Command{
		Use:   "list <tenant> <receiver>",
		Short: "List a receiver's routes in order, and its default",
		Long: `List a receiver's routes in the order they are tried, each with its
conditions, its storage, the profile version and the senders it chooses, and
what happens to a study none chooses. With --json the receiver's routing as the
API answers it.`,
		Example: `  nopacs receiver route list lab1 scanner1
  nopacs receiver route list lab1 scanner1 --json`,
		Args: exactArgs(2),
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
			return a.routingOut(res.GetReceiver())
		}),
	}
}

// newRouteAdd builds "receiver route add".
func (a *app) newRouteAdd() *cobra.Command {
	var f routeFlags
	cmd := &cobra.Command{
		Use:   "add <tenant> <receiver> <route> --storage NAME [conditions] [--profile NAME@VERSION] [--forward-to SENDER | --no-forward] [--position N]",
		Short: "Add a route to a receiver",
		Long: `Add a route to a receiver, last in the order or at --position. A route with
no condition holds for every study, so the routes after it are never tried.
--profile replaces the receiver's profile version for the studies the route
chooses (a receiver that archives as received cannot have one); --forward-to
replaces the receiver's senders, and --no-forward forwards to none. Without
either the receiver's own apply.`,
		Example: `  nopacs receiver route add lab1 scanner1 lab-a --storage lab-a --equals calling-aet=LAB_A
  nopacs receiver route add lab1 scanner1 ct --storage ct-archive --pattern modality='CT|PT' --profile strict@2
  nopacs receiver route add lab1 scanner1 vendor --storage main --prefix manufacturer=SIEMENS --forward-to pacs --position 1`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if f.storage == "" {
				return fault.New(fault.Invalid, "a route needs --storage")
			}
			rt := &v1.Route{Name: args[2]}
			if err := f.apply(cmd, rt); err != nil {
				return err
			}
			return a.changeRouting(ctx, cmd, args[0], args[1], func(r *v1.ReceiverRouting) error {
				if _, err := routeIndex(r, args[2]); err == nil {
					return fault.New(fault.Conflict, fmt.Sprintf("the receiver has a route named %q", output.Clean(args[2])))
				}
				at := len(r.Routes)
				if cmd.Flags().Changed("position") {
					if f.position < 1 || f.position > len(r.Routes)+1 {
						return fault.New(fault.Invalid, fmt.Sprintf("--position is 1 to %d", len(r.Routes)+1))
					}
					at = f.position - 1
				}
				r.Routes = slices.Insert(r.Routes, at, rt)
				return nil
			})
		}),
	}
	f.add(cmd, false)
	return cmd
}

// newRouteSet builds "receiver route set".
func (a *app) newRouteSet() *cobra.Command {
	var f routeFlags
	cmd := &cobra.Command{
		Use:   "set <tenant> <receiver> <route> [flags of what to change]",
		Short: "Change a route of a receiver",
		Long: `Change what the flags name of one route, and nothing else. The conditions are
replaced whole by the condition flags given, and --no-conditions removes them
all. --receiver-profile and --receiver-senders go back to the receiver's own.`,
		Example: `  nopacs receiver route set lab1 scanner1 lab-a --storage lab-a-2
  nopacs receiver route set lab1 scanner1 lab-a --equals calling-aet=LAB_A --equals modality=MR
  nopacs receiver route set lab1 scanner1 lab-a --receiver-profile --receiver-senders`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if cmd.Flags().NFlag() == 0 {
				return fault.New(fault.Invalid, "nothing to change: give at least one flag, such as --storage")
			}
			if f.noConditions && f.conditionFlags(cmd) {
				return fault.New(fault.Invalid, "--no-conditions and a condition flag are not given together")
			}
			return a.changeRouting(ctx, cmd, args[0], args[1], func(r *v1.ReceiverRouting) error {
				i, err := routeIndex(r, args[2])
				if err != nil {
					return err
				}
				return f.apply(cmd, r.Routes[i])
			})
		}),
	}
	f.add(cmd, true)
	return cmd
}

// newRouteRemove builds "receiver route remove".
func (a *app) newRouteRemove() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <tenant> <receiver> <route>",
		Short: "Remove a route of a receiver",
		Long: `Remove one route of a receiver. Receptions it already chose for keep what it
chose; studies that arrive from then on are tried against the routes left.`,
		Example: `  nopacs receiver route remove lab1 scanner1 lab-a`,
		Args:    exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			return a.changeRouting(ctx, cmd, args[0], args[1], func(r *v1.ReceiverRouting) error {
				i, err := routeIndex(r, args[2])
				if err != nil {
					return err
				}
				r.Routes = slices.Delete(r.Routes, i, i+1)
				return nil
			})
		}),
	}
}

// newRouteMove builds "receiver route move".
func (a *app) newRouteMove() *cobra.Command {
	var to int
	cmd := &cobra.Command{
		Use:     "move <tenant> <receiver> <route> --to N",
		Short:   "Move a route to another place in the order",
		Long:    `Move one route of a receiver to place N in the order, from 1; the routes between move by one.`,
		Example: `  nopacs receiver route move lab1 scanner1 lab-a --to 1`,
		Args:    exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("to") {
				return fault.New(fault.Invalid, "a move needs --to")
			}
			return a.changeRouting(ctx, cmd, args[0], args[1], func(r *v1.ReceiverRouting) error {
				i, err := routeIndex(r, args[2])
				if err != nil {
					return err
				}
				if to < 1 || to > len(r.Routes) {
					return fault.New(fault.Invalid, fmt.Sprintf("--to is 1 to %d", len(r.Routes)))
				}
				rt := r.Routes[i]
				r.Routes = slices.Insert(slices.Delete(r.Routes, i, i+1), to-1, rt)
				return nil
			})
		}),
	}
	cmd.Flags().IntVar(&to, "to", 0, "the route's new place in the order, from 1")
	return cmd
}

// newRouteDefault builds "receiver route default".
func (a *app) newRouteDefault() *cobra.Command {
	return &cobra.Command{
		Use:   "default <tenant> <receiver> storage|quarantine",
		Short: "Say what happens to a study no route chooses",
		Long: `Say what happens to a new session no route of the receiver chooses: storage
archives it to the receiver's storage with its profile and senders, as a
receiver with no route does; quarantine holds every file of it as unrouted, for
a person to release naming a storage.`,
		Example: `  nopacs receiver route default lab1 scanner1 quarantine
  nopacs receiver route default lab1 scanner1 storage`,
		Args: exactArgs(3),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			var quarantine bool
			switch args[2] {
			case "storage":
			case "quarantine":
				quarantine = true
			default:
				return fault.New(fault.Invalid, fmt.Sprintf("%q is not storage or quarantine", output.Clean(args[2])))
			}
			return a.changeRouting(ctx, cmd, args[0], args[1], func(r *v1.ReceiverRouting) error {
				r.QuarantineUnmatched = quarantine
				return nil
			})
		}),
	}
}
