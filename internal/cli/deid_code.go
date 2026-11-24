package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
)

// maxCodeValue bounds a value read for "deid code", the server's bound.
const maxCodeValue = 1 << 10

// newDeidCode builds "deid code".
func (a *app) newDeidCode() *cobra.Command {
	var patientID, attribute, value, issuer, namespace string
	var qualifiers []string
	cmd := &cobra.Command{
		Use:   "code <tenant> (--patient-id VALUE|- | --attribute NAME --value VALUE|-)",
		Short: "Find the codes the archive gives one value, such as a subject's label from its Patient ID",
		Long: `The forward lookup: from a value a person already has to where its data is in
the archive, for a withdrawal of consent. It answers the keyed hash of the value
under the tenant's naming key (what a naming rule's {#X} writes into a label and
a profile's hash action writes into a file); for each receiver whose naming rule
makes the subject label from that hash alone, the subject label it gives the
value and whether the catalog holds it; and the code each lookup table holds
for the value. The archive keeps no way back from a code to a value; this is
the only way from a value to its code.

The value is read as a file's element holds it, its padding removed as its VR
says. --patient-id is --attribute PatientID --value; --attribute takes a keyword
of PS3.6 or a tag of eight hexadecimal digits. --patient-id - and --value - read
the value from the first line of standard input, which keeps it out of the
shell's history and the list of processes: prefer them to a value written on
the command line.

A Patient ID is unique only within the issuer that gave it, and the default
naming rule's subject label is the keyed hash of the Patient ID with its issuer.
Give the issuer as the files give it: --issuer for the Issuer of Patient ID
(0010,0021), and --issuer-qualifier NAME=VALUE for a value of the Issuer of
Patient ID Qualifiers Sequence (0010,0024), NAME one of UniversalEntityID,
UniversalEntityIDType, IdentifierTypeCode,
AssigningFacilitySequence.LocalNamespaceEntityID,
AssigningFacilitySequence.UniversalEntityID,
AssigningFacilitySequence.UniversalEntityIDType, and CodeValue,
CodingSchemeDesignator, LongCodeValue or URNCodeValue after
AssigningJurisdictionCodeSequence. or AssigningAgencyOrDepartmentCodeSequence.
For a Patient ID that comes with no issuer, --namespace names the receiver
namespace the code and lookups are answered under (the tenant's default when
left out); each receiver's subject label is answered under its own namespace.

It needs the admin role on the tenant. One value per call, at most 60 calls a
minute by one key and 60 a minute into one tenant (exit 7 past either). Every
call is in the tenant's audit log, written before the answer, with the key and
the attribute, never the value; a call whose entry cannot be written is
refused. The value is sent to the server only and shown nowhere.`,
		Example: `  nopacs deid code lab1 --patient-id -
  nopacs deid code lab1 --patient-id - --issuer HOSPITAL_A
  nopacs deid code lab1 --patient-id - --issuer-qualifier UniversalEntityID=2.16.840.1.113883.3.1 --issuer-qualifier UniversalEntityIDType=ISO
  nopacs deid code lab1 --attribute AccessionNumber --value - --json`,
		Args: exactArgs(1),
		RunE: a.run(func(ctx context.Context, cmd *cobra.Command, args []string) error {
			req := &v1.CodeRequest{Tenant: args[0], Attribute: attribute, Value: value}
			switch {
			case cmd.Flags().Changed("patient-id") && (cmd.Flags().Changed("value") || cmd.Flags().Changed("attribute")):
				return fault.New(fault.Invalid, "--patient-id is --attribute PatientID --value; give one or the other")
			case cmd.Flags().Changed("patient-id"):
				req.Attribute, req.Value = "PatientID", patientID
				if patientID == "-" {
					v, err := a.readValue()
					if err != nil {
						return err
					}
					req.Value = v
				}
			case !cmd.Flags().Changed("value"):
				return fault.New(fault.Invalid, "give the value: --patient-id VALUE, or --value VALUE (or - to read it from standard input)")
			case value == "-":
				v, err := a.readValue()
				if err != nil {
					return err
				}
				req.Value = v
			}
			if issuer != "" || len(qualifiers) > 0 {
				m, err := issuerMessage(issuer, qualifiers)
				if err != nil {
					return err
				}
				req.Issuer = m
			}
			req.Namespace = namespace
			c, closeClient, err := a.open(cmd)
			if err != nil {
				return err
			}
			defer closeClient()
			res, err := call(ctx, c, cmd, func(ctx context.Context) (*v1.CodeResponse, error) { return c.Deid.Code(ctx, req) })
			if err != nil {
				return err
			}
			return messageOut(a, res, func(w io.Writer) error { return a.codeText(w, res) })
		}),
	}
	cmd.Flags().StringVar(&patientID, "patient-id", "", "the Patient ID (0010,0020) to find the codes of, or - to read it from the first line of standard input")
	cmd.Flags().StringVar(&attribute, "attribute", "", "the attribute the value is of: a keyword such as PatientID, or a tag such as 00100020 (PatientID when not given)")
	cmd.Flags().StringVar(&value, "value", "", "the value, or - to read it from the first line of standard input")
	cmd.Flags().StringVar(&issuer, "issuer", "", "the Patient ID's Issuer of Patient ID (0010,0021), as the files give it")
	cmd.Flags().StringArrayVar(&qualifiers, "issuer-qualifier", nil, "a value of the Patient ID's Issuer of Patient ID Qualifiers Sequence (0010,0024), written NAME=VALUE, such as UniversalEntityID=2.16.840.1.113883.3.1; repeat for more")
	cmd.Flags().StringVar(&namespace, "namespace", "", "the receiver namespace of a Patient ID that comes with no issuer (default: the tenant's default namespace)")
	return cmd
}

// issuerMessage builds a Patient ID's issuer from --issuer and each
// --issuer-qualifier NAME=VALUE, NAME one of naming.QualifierNames.
func issuerMessage(issuer string, qualifiers []string) (*v1.PatientIDIssuer, error) {
	m := &v1.PatientIDIssuer{Issuer: issuer, AssigningFacility: &v1.IssuerDesignator{},
		AssigningJurisdiction: &v1.IssuerCode{}, AssigningAgency: &v1.IssuerCode{}}
	// The fields of m by naming.QualifierNames, in its order.
	into := [len(naming.QualifierNames)]*string{
		&m.UniversalEntityId, &m.UniversalEntityIdType, &m.IdentifierTypeCode,
		&m.AssigningFacility.LocalNamespaceEntityId, &m.AssigningFacility.UniversalEntityId, &m.AssigningFacility.UniversalEntityIdType,
		&m.AssigningJurisdiction.CodeValue, &m.AssigningJurisdiction.CodingSchemeDesignator,
		&m.AssigningJurisdiction.LongCodeValue, &m.AssigningJurisdiction.UrnCodeValue,
		&m.AssigningAgency.CodeValue, &m.AssigningAgency.CodingSchemeDesignator,
		&m.AssigningAgency.LongCodeValue, &m.AssigningAgency.UrnCodeValue,
	}
	for _, q := range qualifiers {
		name, v, ok := strings.Cut(q, "=")
		i := slices.Index(naming.QualifierNames[:], name)
		if !ok || i < 0 {
			return nil, fault.New(fault.Invalid, "--issuer-qualifier is NAME=VALUE, NAME one of "+strings.Join(naming.QualifierNames[:], ", "))
		}
		if *into[i] != "" {
			return nil, fault.New(fault.Invalid, "--issuer-qualifier names "+name+" twice")
		}
		*into[i] = v
	}
	return m, nil
}

// readValue reads one value from the first line of standard input, its
// line ending removed, within the server's bound.
func (a *app) readValue() (string, error) {
	if a.env.Stdin == nil {
		return "", fault.New(fault.Invalid, "- has no standard input to read the value from")
	}
	line, err := bufio.NewReader(io.LimitReader(a.env.Stdin, maxCodeValue+2)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fault.Wrap(fault.Invalid, err, "cannot read the value from standard input")
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		return "", fault.New(fault.Invalid, "standard input holds no value")
	}
	if len(line) > maxCodeValue {
		return "", fault.New(fault.Invalid, "the value is longer than 1024 bytes")
	}
	return line, nil
}

// codeText writes a forward lookup's answer for a person.
func (a *app) codeText(w io.Writer, res *v1.CodeResponse) error {
	if err := output.Details(w, a.style(), []output.Pair{
		{Label: "Attribute", Value: res.GetAttribute()},
		{Label: "Keyed hash", Value: res.GetCode()},
	}); err != nil {
		return err
	}
	if len(res.GetSubjects()) > 0 {
		rows := make([][]string, 0, len(res.GetSubjects()))
		for _, s := range res.GetSubjects() {
			held := "no"
			if s.GetHeld() {
				held = "yes"
			}
			rows = append(rows, []string{s.GetReceiver(), s.GetSubjectLabel(), held})
		}
		_, _ = fmt.Fprintln(w)
		if err := output.Table(w, a.style(), []string{"RECEIVER", "SUBJECT LABEL", "IN THE CATALOG"}, rows); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintln(w, "\nNo receiver makes its subject label from this attribute's keyed hash alone.")
	}
	if len(res.GetLookups()) > 0 {
		rows := make([][]string, 0, len(res.GetLookups()))
		for _, l := range res.GetLookups() {
			rows = append(rows, []string{l.GetTable(), l.GetCode()})
		}
		_, _ = fmt.Fprintln(w)
		return output.Table(w, a.style(), []string{"LOOKUP TABLE", "CODE"}, rows)
	}
	_, err := fmt.Fprintln(w, "No lookup table holds a code for this value.")
	return err
}
