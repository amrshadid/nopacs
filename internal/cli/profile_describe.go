package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// describeDoc is the JSON document of "profile describe": the version or
// document's checksum, its expanded rules and warnings, and the
// description (classes, notes, the method record and the text lists'
// summaries) as the API answers them. The expanded form is given without
// its text lists, whose terms a description never prints: textLists holds
// each list's name, version, count of terms and checksum.
type describeDoc struct {
	Name      string          `json:"name,omitempty"`
	Version   int32           `json:"version,omitempty"`
	Checksum  string          `json:"checksum"`
	Expanded  json.RawMessage `json:"expanded"`
	Warnings  json.RawMessage `json:"warnings"`
	Classes   json.RawMessage `json:"classes"`
	Notes     json.RawMessage `json:"notes"`
	Method    json.RawMessage `json:"method"`
	TextLists json.RawMessage `json:"textLists"`
}

// classWords names a class of element for a person.
var classWords = map[v1.ProfileElementClass]string{
	v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_NAMES:       "Names of persons",
	v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_IDENTIFIERS: "Identifiers, addresses, telephones",
	v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_DATES:       "Dates, times and ages",
	v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_UIDS:        "UIDs",
	v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_PRIVATE:     "Private elements",
	v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_PIXELS:      "Pixels",
	v1.ProfileElementClass_PROFILE_ELEMENT_CLASS_OTHER:       "Other elements",
}

// actionWords says what an action does, for a person.
var actionWords = map[string]string{
	"keep": "kept", "remove": "removed", "empty": "emptied", "replace": "replaced by a fixed text",
	"set": "set to a fixed text", "hash": "replaced by a keyed hash", "uid": "remapped", "shift": "shifted",
	"generalize": "generalized", "lookup": "replaced by a lookup table's code", "clean": "cleaned of names and identifiers",
	"quarantine": "hold the whole file", "dummy": "replaced by a dummy value",
	"remove_unless_required": "removed unless the file's IOD requires them",
	"redact":                 "cut where a pattern matches, or for pixels, rectangles filled",
	"detect":                 "burned-in text detected and filled (unverified)", "accept": "accepted as they are",
}

// maxDescribedShown is how many elements of one effect a person's view
// names; --json holds them all.
const maxDescribedShown = 12

// describeText writes a description for a person: what each class gets,
// the sentences for the whole profile, and the method record.
func (a *app) describeText(w io.Writer, d *v1.DescribeProfileResponse) error {
	_, _ = fmt.Fprintln(w, "\nWhat it does:")
	for _, c := range d.GetClasses() {
		_, _ = fmt.Fprintf(w, "  %s\n", a.style().Bold(classWords[c.GetClass()]))
		for _, e := range c.GetEffects() {
			words := actionWords[e.GetAction()]
			if words == "" {
				words = e.GetAction()
			}
			els := e.GetElements()
			more := int(e.GetElementsOmitted())
			if len(els) > maxDescribedShown {
				more += len(els) - maxDescribedShown
				els = els[:maxDescribedShown]
			}
			list := output.Clean(strings.Join(els, ", "))
			if more > 0 {
				list += ", and " + strconv.Itoa(more) + " more"
			}
			cond := ""
			if e.GetConditional() {
				cond = " (some only under conditions)"
			}
			_, _ = fmt.Fprintf(w, "    %s%s: %s\n", words, cond, list)
		}
	}
	for _, n := range d.GetNotes() {
		_, _ = fmt.Fprintf(w, "  - %s\n", n)
	}
	if lists := d.GetTextLists(); len(lists) > 0 {
		// A list is named by its summary; its terms are never printed.
		_, _ = fmt.Fprintln(w, "\nText lists (terms not shown):")
		for _, l := range lists {
			sum := ", sha256 " + l.GetSha256()
			if l.GetSha256() == "" {
				// Not answered to a masked key (an unkeyed checksum of a
				// short list gives its terms away to guesses).
				sum = ", checksum withheld"
			}
			_, _ = fmt.Fprintf(w, "  list %s v%d: %d terms%s\n", output.Clean(l.GetName()), l.GetVersion(), l.GetTerms(), sum)
		}
	}
	m := d.GetMethod()
	_, _ = fmt.Fprintln(w, "\nMethod, for a Conformance Statement (PS3.15 E.1.1):")
	if len(m.GetCodes()) == 0 {
		why := "it is built on no PS3.15 Annex E profile or option"
		if m.GetWithheld() != "" {
			why = m.GetWithheld()
		}
		_, _ = fmt.Fprintf(w, "  De-identification Method Code Sequence (0012,0064): none, since %s\n", why)
	} else {
		_, _ = fmt.Fprintln(w, "  De-identification Method Code Sequence (0012,0064):")
		for _, c := range m.GetCodes() {
			_, _ = fmt.Fprintf(w, "    (%s, %s, %q)\n", c.GetValue(), c.GetScheme(), c.GetMeaning())
		}
	}
	_, _ = fmt.Fprintf(w, "  De-identification Method (0012,0063): %s\n", strings.Join(m.GetMethodValues(), `\`))
	_, _ = fmt.Fprintf(w, "  Patient Identity Removed (0012,0062): %s\n", m.GetIdentityRemoved())
	temporal := m.GetTemporalModified()
	if temporal == "" {
		temporal = "MODIFIED in a file where a date was shifted; absent otherwise"
	}
	_, err := fmt.Fprintf(w, "  Longitudinal Temporal Information Modified (0028,0303): %s\n", temporal)
	return err
}

// protoJSON returns m's JSON, or a list's members under field as a JSON
// array.
func protoJSON(m proto.Message, field string) (json.RawMessage, error) {
	raw, err := protojson.Marshal(m)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "writing the answer: "+err.Error())
	}
	if field == "" {
		return raw, nil
	}
	var wrap map[string]json.RawMessage
	_ = json.Unmarshal(raw, &wrap)
	if v, ok := wrap[field]; ok {
		return v, nil
	}
	return json.RawMessage("[]"), nil
}

// newProfileDescribe builds "profile describe".
func (a *app) newProfileDescribe() *cobra.Command {
	var document string
	var noRules, effective bool
	cmd := &cobra.Command{
		Use:   "describe <tenant> (<name>[@<version>] | --document FILE|-) [--no-rules | --effective]",
		Short: "Say in plain words what a version or a document does, with its method record and its rules",
		Long: `Say what a version (or an unsaved document) does, in plain words and in the
terms a DICOM Conformance Statement needs (PS3.15 E.3.5, E.3.6, E.3.10):

  - for each class of element (names, identifiers, dates and times, UIDs,
    private elements, pixels, the rest), which elements each action is given
    to: removed, kept, emptied, remapped, shifted, hashed, and so on; a rule
    that holds only under conditions is marked;
  - what holds for the whole profile: the default for an element no rule
    names, the date shift, what is never kept whatever the rules say, and
    the search of every file after;
  - each text list: its name, version, number of terms and the SHA-256 of
    its folded terms, never a term (with --json, "textLists"; the expanded
    form is given without its lists);
  - the record every file it makes carries: the PS3.16 CID 7050 codes in
    De-identification Method Code Sequence (0012,0064) with their meanings,
    the text the server writes in De-identification Method (0012,0063),
    Patient Identity Removed (0012,0062) and Longitudinal Temporal
    Information Modified (0028,0303).

Then every rule it runs, in order: the rules of its bases as they were copied
in, then its own, each with its origin, selector, action and reason (not with
--no-rules). The rule that decides an element is the most specific selector's,
and between equally specific ones the later; "profile explain" shows the
decision for each element of a file. With --json, all of it as one document.

With --effective, a receiver's name takes the place of a version: the profile
the receiver de-identifies with, stacked over the tenant's base profile (the
setting deid.base-profile), each selector listed once with the strictest
action and the layer it came from: "receiver" for what the receiver's profile
names (it may only tighten the base), "tenant" for what only the base names.
It reads profiles, never a file.`,
		Example: `  nopacs profile describe lab1 research@2
  nopacs profile describe --effective lab1 scanner1
  nopacs profile describe lab1 research@2 --no-rules
  nopacs profile describe lab1 --document research.json --json`,
		Args: rangeArgs(1, 2),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			if effective {
				return a.describeEffective(ctx, cmd, args, document)
			}
			var expanded *v1.ExpandedProfile
			var warnings []*v1.ProfileProblem
			var tp *v1.TrialProfile
			doc := describeDoc{}
			switch {
			case len(args) == 2 && document == "":
				ref, err := parseVersionArg(args[1])
				if err != nil {
					return err
				}
				res, err := a.getVersion(ctx, cmd, args[0], ref, true)
				if err != nil {
					return err
				}
				v := res.GetVersion()
				expanded, doc.Name, doc.Version, doc.Checksum = v.GetExpanded(), v.GetName(), v.GetVersion(), v.GetChecksum()
				tp = &v1.TrialProfile{Kind: &v1.TrialProfile_Version{Version: &v1.ProfileVersionName{Name: v.GetName(), Version: v.GetVersion()}}}
			case len(args) == 1 && document != "":
				in, err := a.readDocument(document)
				if err != nil {
					return err
				}
				res, err := a.validate(ctx, cmd, args[0], in, true)
				if err != nil {
					return err
				}
				if len(res.GetProblems()) > 0 {
					_ = a.write(func(w io.Writer) error { return problemsTable(w, a, res.GetProblems()) })
					return fault.New(fault.Invalid, fmt.Sprintf("the document has %d problem(s)", len(res.GetProblems())))
				}
				expanded, warnings, doc.Checksum = res.GetExpanded(), res.GetWarnings(), res.GetChecksum()
				tp = &v1.TrialProfile{Kind: &v1.TrialProfile_Document{Document: in}}
			default:
				return fault.New(fault.Invalid, "name a version (NAME or NAME@VERSION) or give --document, one of the two")
			}
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			desc, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.DescribeProfileResponse, error) {
				return c.Profiles.DescribeProfile(ctx, &v1.DescribeProfileRequest{Tenant: args[0], Profile: tp})
			})
			if err != nil {
				return a.withProblems(err)
			}
			if a.opts.json {
				// The terms of the text lists are never printed by a
				// description; their summaries are.
				shown, _ := proto.Clone(expanded).(*v1.ExpandedProfile)
				shown.TextLists = nil
				if doc.Expanded, err = protoJSON(shown, ""); err != nil {
					return err
				}
				if doc.TextLists, err = protoJSON(desc, "textLists"); err != nil {
					return err
				}
				if doc.Warnings, err = protoJSON(&v1.ProfileProblems{Problems: warnings}, "problems"); err != nil {
					return err
				}
				if doc.Classes, err = protoJSON(desc, "classes"); err != nil {
					return err
				}
				if doc.Notes, err = protoJSON(desc, "notes"); err != nil {
					return err
				}
				if doc.Method, err = protoJSON(desc.GetMethod(), ""); err != nil {
					return err
				}
				return a.write(func(w io.Writer) error { return output.JSONValue(w, doc) })
			}
			return a.write(func(w io.Writer) error {
				if err := output.Details(w, a.style(), []output.Pair{
					{Label: "Checksum", Value: doc.Checksum},
					{Label: "Bases", Value: output.Dash(strings.Join(expanded.GetBases(), ", "))},
					{Label: "Unnamed elements", Value: strings.ToLower(strings.TrimPrefix(expanded.GetUnnamed().String(), "UNNAMED_ELEMENTS_"))},
					{Label: "Largest date shift", Value: strconv.Itoa(int(expanded.GetDateShiftDays())) + " days"},
				}); err != nil {
					return err
				}
				if err := a.describeText(w, desc); err != nil {
					return err
				}
				if noRules {
					return nil
				}
				_, _ = fmt.Fprintln(w, "\nRules, in order:")
				return output.Table(w, a.style(), []string{"#", "ORIGIN", "NAME", "SELECTOR", "ACTION", "WHEN", "REASON"}, ruleRows(expanded))
			})
		}),
	}
	cmd.Flags().StringVar(&document, documentFlag, "", "an unsaved document's file, or - to read it from standard input")
	cmd.Flags().BoolVar(&noRules, "no-rules", false, "leave out the table of every rule")
	cmd.Flags().BoolVar(&effective, "effective", false, "describe a receiver's profile stacked over the tenant's base profile, with each element's winning layer")
	return cmd
}

// ruleRows writes an expanded profile's rules as rows: position, origin,
// name, selector, action, conditions and reason.
func ruleRows(expanded *v1.ExpandedProfile) [][]string {
	rows := make([][]string, 0, len(expanded.GetRules()))
	for i, er := range expanded.GetRules() {
		r := er.GetRule()
		origin := er.GetOrigin()
		if origin == "" {
			origin = "own"
		}
		when := ""
		if n := len(r.GetWhen()); n > 0 {
			when = strconv.Itoa(n) + " condition(s)"
		}
		rows = append(rows, []string{strconv.Itoa(i + 1), origin, output.Dash(r.GetName()), oneofText(r.GetSelector()), oneofText(r.GetAction()), output.Dash(when), output.Dash(output.Clean(r.GetReason()))})
	}
	return rows
}
