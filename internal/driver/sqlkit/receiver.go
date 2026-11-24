package sqlkit

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A receiver's configuration is kept in two JSON columns, config and
// naming, written and read here so every SQL driver keeps the same text: a
// database moved from one driver to another, or read by an outside tool,
// reads the same. The field names are the columns' contract and never
// change; a field added later is read as its zero value from an older row.

// dimseConfig is a receiver's configuration of its type as its config
// column holds it: a DIMSE receiver's fields, or a stow receiver's, which
// are left out of a DIMSE receiver's text so a row written before the stow
// type existed reads and writes back the same.
type dimseConfig struct {
	AETitle          string             `json:"ae_title"`
	AnyCaller        bool               `json:"any_caller"`
	Callers          []callerRuleConfig `json:"callers"`
	TLS              *tlsConfig         `json:"tls"`
	SOPClasses       []string           `json:"sop_classes"`
	TransferSyntaxes []string           `json:"transfer_syntaxes"`
	STOWKeys         []string           `json:"stow_key_ids,omitempty"`
	MaxRequestBytes  int64              `json:"stow_max_request_bytes,omitempty"`
	ForwardTo        []string           `json:"forward_to,omitempty"`
	// DeidProfile and DeidVersion name the profile version the receiver
	// de-identifies with; absent, as in every row written before
	// receivers de-identified, the receiver archives as received.
	DeidProfile string `json:"deid_profile,omitempty"`
	DeidVersion int32  `json:"deid_version,omitempty"`
	// Routes and RoutesQuarantineUnmatched are the receiver's routing;
	// absent, as in every row written before receivers routed, it has
	// none.
	Routes                    []routeConfig `json:"routes,omitempty"`
	RoutesQuarantineUnmatched bool          `json:"routes_quarantine_unmatched,omitempty"`
	// Import is an import receiver's configuration; absent for every
	// other type, so their rows read and write back as before.
	Import *importConfig `json:"import,omitempty"`
}

// importConfig is an import receiver's configuration: its root, layout and
// map. The layout is kept by its name ("plain", "pattern").
type importConfig struct {
	Root           string   `json:"root"`
	Layout         string   `json:"layout"`
	Pattern        string   `json:"pattern,omitempty"`
	UnitDepth      int      `json:"unit_depth,omitempty"`
	MapFile        string   `json:"map_file,omitempty"`
	MapKeyColumns  []string `json:"map_key_columns,omitempty"`
	MapValueColumn string   `json:"map_value_column,omitempty"`
	MapKeyParts    []string `json:"map_key_parts,omitempty"`
	MapGives       string   `json:"map_gives,omitempty"`
}

// importConfigOf returns i as its config column keeps it.
func importConfigOf(i tenant.ImportReceiver) *importConfig {
	return &importConfig{Root: i.Root, Layout: i.Layout.String(), Pattern: i.Pattern, UnitDepth: i.UnitDepth, MapFile: i.MapFile,
		MapKeyColumns: i.Map.KeyColumns, MapValueColumn: i.Map.ValueColumn, MapKeyParts: i.Map.KeyParts, MapGives: i.Map.Gives}
}

// importReceiverOf returns the configuration c keeps.
func importReceiverOf(c *importConfig) tenant.ImportReceiver {
	var kind importpath.Kind
	switch c.Layout {
	case importpath.Plain.String():
		kind = importpath.Plain
	case importpath.Pattern.String():
		kind = importpath.Pattern
	}
	return tenant.ImportReceiver{Root: c.Root, Layout: kind, Pattern: c.Pattern, UnitDepth: c.UnitDepth, MapFile: c.MapFile,
		Map: tenant.ImportMap{KeyColumns: c.MapKeyColumns, ValueColumn: c.MapValueColumn, KeyParts: c.MapKeyParts, Gives: c.MapGives}}
}

// routeConfig is one route of a receiver's routing.
type routeConfig struct {
	Name              string            `json:"name"`
	Conditions        []conditionConfig `json:"conditions,omitempty"`
	Storage           string            `json:"storage"`
	Profile           string            `json:"profile,omitempty"`
	Version           int32             `json:"version,omitempty"`
	ReplacesForwardTo bool              `json:"replaces_forward_to,omitempty"`
	ForwardTo         []string          `json:"forward_to,omitempty"`
}

// conditionConfig is one condition of a route.
type conditionConfig struct {
	Subject string `json:"subject"`
	Test    string `json:"test"`
	Value   string `json:"value,omitempty"`
	Present bool   `json:"present,omitempty"`
	Negate  bool   `json:"negate,omitempty"`
}

// receptionRouteConfig is what a reception's routing chose, as its route
// column holds it; the empty text for nothing of its own.
type receptionRouteConfig struct {
	ReleasedStorage   string   `json:"released_storage,omitempty"`
	ReleasedProfile   string   `json:"released_profile,omitempty"`
	ReleasedVersion   int32    `json:"released_version,omitempty"`
	Route             string   `json:"route,omitempty"`
	Profile           string   `json:"profile,omitempty"`
	Version           int32    `json:"version,omitempty"`
	ReplacesForwardTo bool     `json:"replaces_forward_to,omitempty"`
	ForwardTo         []string `json:"forward_to,omitempty"`
}

// ReceptionRouteText returns the text of a reception's route column: empty
// for a route that chose nothing of its own.
func ReceptionRouteText(r tenant.ReceptionRoute) (string, error) {
	if r.IsZero() {
		return "", nil
	}
	return JSONText(receptionRouteConfig{ReleasedStorage: r.ReleasedStorage, ReleasedProfile: r.ReleasedProfile.Name,
		ReleasedVersion: r.ReleasedProfile.Version, Route: r.Route, Profile: r.Profile.Name,
		Version: r.Profile.Version, ReplacesForwardTo: r.ReplacesForwardTo, ForwardTo: r.ForwardTo})
}

// ReadReceptionRoute returns the route a reception's route column holds,
// as ReceptionRouteText wrote it.
func ReadReceptionRoute(text string) (tenant.ReceptionRoute, error) {
	if text == "" {
		return tenant.ReceptionRoute{}, nil
	}
	var c receptionRouteConfig
	if err := FromJSON(text, &c); err != nil {
		return tenant.ReceptionRoute{}, err
	}
	return tenant.ReceptionRoute{ReleasedStorage: c.ReleasedStorage,
		ReleasedProfile: tenant.ProfileRef{Name: c.ReleasedProfile, Version: c.ReleasedVersion}, Route: c.Route,
		Profile: tenant.ProfileRef{Name: c.Profile, Version: c.Version}, ReplacesForwardTo: c.ReplacesForwardTo,
		ForwardTo: c.ForwardTo}, nil
}

// ReleasedRoute returns the route a reception still receiving keeps once a
// release into it names storage and profile (tenant.Release), each "" or
// zero for none, and whether it changed. A reception that keeps another
// storage or profile version a release named is a precondition failure:
// two people's decisions differ, and the later is not taken silently.
func ReleasedRoute(r tenant.ReceptionRoute, storage string, profile tenant.ProfileRef) (tenant.ReceptionRoute, bool, error) {
	changed := false
	if storage != "" {
		switch r.ReleasedStorage {
		case storage:
		case "":
			r.ReleasedStorage, changed = storage, true
		default:
			return r, false, tenant.Precondition("a release into the reception this file joins named the storage " + r.ReleasedStorage +
				"; release it naming the same storage, or none")
		}
	}
	if !profile.IsZero() {
		switch r.ReleasedProfile {
		case profile:
		case tenant.ProfileRef{}:
			r.ReleasedProfile, changed = profile, true
		default:
			return r, false, tenant.Precondition("a release into the reception this file joins named the profile version " + r.ReleasedProfile.String() +
				"; release it naming the same version, or none")
		}
	}
	return r, changed, nil
}

// ProfileRefText returns the text a column holding one profile version
// keeps: name@version, empty for the zero version.
func ProfileRefText(r tenant.ProfileRef) string {
	if r.IsZero() {
		return ""
	}
	return r.String()
}

// ReadProfileRef returns the profile version a column's text holds, as
// ProfileRefText wrote it.
func ReadProfileRef(text string) (tenant.ProfileRef, error) {
	if text == "" {
		return tenant.ProfileRef{}, nil
	}
	name, num, ok := strings.Cut(text, "@")
	n, err := strconv.ParseInt(num, 10, 32)
	if !ok || err != nil {
		return tenant.ProfileRef{}, fault.New(fault.Internal, "a stored profile version is not name@version")
	}
	r := tenant.ProfileRef{Name: name, Version: int32(n)}
	if err := r.Validate(); err != nil {
		return tenant.ProfileRef{}, fault.Wrap(fault.Internal, err, "a stored profile version is not valid")
	}
	return r, nil
}

// callerRuleConfig is one caller a DIMSE receiver accepts.
type callerRuleConfig struct {
	CallingAETitle string `json:"calling_ae_title"`
	AddressRange   string `json:"address_range"`
}

// tlsConfig is where a DIMSE receiver's certificate and key are.
type tlsConfig struct {
	CertificateFile string `json:"certificate_file"`
	PrivateKeyFile  string `json:"private_key_file"`
}

// namingConfig is a receiver's naming rule as its naming column holds it.
type namingConfig struct {
	Subject          string `json:"subject"`
	Session          string `json:"session"`
	Scan             string `json:"scan"`
	PatternAttribute string `json:"pattern_attribute"`
	Pattern          string `json:"pattern"`
}

// ReceiverConfigText returns the text of a receiver's config column and of
// its naming column, empty when it has no naming rule of its own.
func ReceiverConfigText(r tenant.Receiver) (config, namingText string, err error) {
	c := dimseConfig{AETitle: r.DIMSE.AETitle, AnyCaller: r.DIMSE.AnyCaller,
		SOPClasses: r.DIMSE.SOPClasses, TransferSyntaxes: r.DIMSE.TransferSyntaxes,
		STOWKeys: r.STOW.Keys, MaxRequestBytes: r.STOW.MaxRequestBytes, ForwardTo: r.ForwardTo,
		DeidProfile: r.Profile.Name, DeidVersion: r.Profile.Version,
		RoutesQuarantineUnmatched: r.Routing.QuarantineUnmatched}
	for _, rt := range r.Routing.Routes {
		rc := routeConfig{Name: rt.Name, Storage: rt.Storage, Profile: rt.Profile.Name, Version: rt.Profile.Version,
			ReplacesForwardTo: rt.ReplacesForwardTo, ForwardTo: rt.ForwardTo}
		for _, cd := range rt.Conditions {
			rc.Conditions = append(rc.Conditions, conditionConfig{Subject: string(cd.Subject), Test: string(cd.Test),
				Value: cd.Value, Present: cd.Present, Negate: cd.Negate})
		}
		c.Routes = append(c.Routes, rc)
	}
	for _, cr := range r.DIMSE.Callers {
		c.Callers = append(c.Callers, callerRuleConfig(cr))
	}
	if r.DIMSE.TLS != nil {
		tc := tlsConfig(*r.DIMSE.TLS)
		c.TLS = &tc
	}
	if r.Type == tenant.ReceiverImport {
		c.Import = importConfigOf(r.Import)
	}
	if config, err = JSONText(c); err != nil {
		return "", "", err
	}
	if r.Naming != nil {
		if namingText, err = JSONText(namingConfig(*r.Naming)); err != nil {
			return "", "", err
		}
	}
	return config, namingText, nil
}

// ReadReceiverConfig sets r's DIMSE or stow configuration and naming rule from the
// text of its config and naming columns, as ReceiverConfigText wrote them.
func ReadReceiverConfig(r *tenant.Receiver, config, namingText string) error {
	var c dimseConfig
	if err := FromJSON(config, &c); err != nil {
		return err
	}
	r.DIMSE.AETitle, r.DIMSE.AnyCaller = c.AETitle, c.AnyCaller
	r.DIMSE.SOPClasses, r.DIMSE.TransferSyntaxes = c.SOPClasses, c.TransferSyntaxes
	r.STOW = tenant.STOWReceiver{Keys: c.STOWKeys, MaxRequestBytes: c.MaxRequestBytes}
	r.ForwardTo = c.ForwardTo
	r.Profile = tenant.ProfileRef{Name: c.DeidProfile, Version: c.DeidVersion}
	r.Routing = routingOf(c)
	r.DIMSE.Callers = nil
	for _, cr := range c.Callers {
		r.DIMSE.Callers = append(r.DIMSE.Callers, tenant.CallerRule(cr))
	}
	r.DIMSE.TLS = nil
	if c.TLS != nil {
		t := tenant.ReceiverTLS(*c.TLS)
		r.DIMSE.TLS = &t
	}
	r.Import = tenant.ImportReceiver{}
	if c.Import != nil {
		r.Import = importReceiverOf(c.Import)
	}
	r.Naming = nil
	if namingText != "" {
		var n namingConfig
		if err := FromJSON(namingText, &n); err != nil {
			return err
		}
		spec := naming.Spec(n)
		r.Naming = &spec
	}
	return nil
}

// ReceiverDeidentifies reports whether the receiver whose config column
// holds config names a profile version, and so de-identifies what it
// receives (tenant.Receiver.Deidentifies).
func ReceiverDeidentifies(config string) (bool, error) {
	var c dimseConfig
	if err := FromJSON(config, &c); err != nil {
		return false, err
	}
	return c.DeidProfile != "", nil
}

// routingOf returns the routing a receiver's config column holds.
func routingOf(c dimseConfig) tenant.Routing {
	out := tenant.Routing{QuarantineUnmatched: c.RoutesQuarantineUnmatched}
	for _, rc := range c.Routes {
		rt := tenant.Route{Name: rc.Name, Storage: rc.Storage, Profile: tenant.ProfileRef{Name: rc.Profile, Version: rc.Version},
			ReplacesForwardTo: rc.ReplacesForwardTo, ForwardTo: rc.ForwardTo}
		for _, cd := range rc.Conditions {
			rt.Conditions = append(rt.Conditions, tenant.RouteCondition{Subject: tenant.RouteSubject(cd.Subject),
				Test: tenant.RouteTest(cd.Test), Value: cd.Value, Present: cd.Present, Negate: cd.Negate})
		}
		out.Routes = append(out.Routes, rt)
	}
	return out
}

// ReceiverRoutingTo returns the name of a receiver one of whose routes
// archives to the storage, or "": the receivers are read whole, since a
// receiver's routes are in its configuration's text and a tenant's
// receivers are few. A driver calls it in the transaction that removes a
// storage, beside its check of the receivers' own storage.
func ReceiverRoutingTo(ctx context.Context, tx SendingTx, storage string) (string, error) {
	return receiverWhose(ctx, tx, func(c dimseConfig) bool {
		return slices.Contains(routingOf(c).Storages(), storage)
	})
}

// receiverWhose returns the name of the first receiver, by name, whose
// configuration ok holds for, or "".
func receiverWhose(ctx context.Context, tx SendingTx, ok func(dimseConfig) bool) (string, error) {
	found := ""
	err := tx.Query(ctx, `SELECT name, config FROM receivers ORDER BY name`, nil, func(r Row) error {
		var name, config string
		if err := r.Scan(&name, &config); err != nil {
			return err
		}
		var c dimseConfig
		if err := FromJSON(config, &c); err != nil {
			return err
		}
		if found == "" && ok(c) {
			found = name
		}
		return nil
	})
	return found, err
}
