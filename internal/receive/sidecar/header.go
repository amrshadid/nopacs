package sidecar

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The two forms a line says how its file arrived (Instance.ReceivedForm).
const (
	// FormDIMSE is a data set NoPACs wrapped in its own Part 10 header, as a
	// C-STORE brings it (PS3.7 9.1.1).
	FormDIMSE = "dimse"
	// FormPart10 is a Part 10 file taken as sent, by STOW-RS (PS3.18 10.5)
	// or an import.
	FormPart10 = "part10"
)

// MaxVersionText is the longest Instance.NopacsVersion.
const MaxVersionText = 64

// ScanHeader is what a version 3 sidecar says of its scan and its session
// that the instances' lines and the scan's keys do not: with it a catalog
// is made again from the storage alone.
type ScanHeader struct {
	// StudyUID and SeriesUID are the session's Study Instance UID and the
	// scan's Series Instance UID as the catalog holds them (the archive's
	// own after de-identification), kept here because every line's data set
	// may have been omitted.
	StudyUID  string `json:"study_uid"`
	SeriesUID string `json:"series_uid"`
	// SessionReceiver names the receiver whose reception first archived
	// into the scan's session, and Receiver the one whose reception first
	// archived into the scan itself. Each is empty when the catalog holds
	// none, as for a scan archived before the catalog kept its receiver:
	// the header says what the catalog says, and no more.
	SessionReceiver string `json:"session_receiver"`
	Receiver        string `json:"receiver"`
	// Route is what the routing chose for the session, nil when it chose
	// nothing of its own.
	Route *Route `json:"route,omitempty"`
	// Values are what the catalog keeps of the scan and its session from
	// the files' headers, as the catalog holds them after the commit that
	// wrote this header: a line's data set may be omitted, so a catalog
	// made again from the storage cannot always read them from the lines.
	Values Values `json:"values"`
	// Created is when the scan's first file was archived; Updated when a
	// file was last archived into it. They are the times of the commits
	// that archived them.
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// Route is a session's routing as the catalog keeps it
// (tenant.ReceptionRoute), with each profile version written
// "name@version".
type Route struct {
	// ReleasedStorage and ReleasedProfile are what a person's release into
	// the reception named.
	ReleasedStorage string `json:"released_storage,omitempty"`
	ReleasedProfile string `json:"released_profile,omitempty"`
	// Name is the route that chose, and Profile the version it chose.
	Name    string `json:"name,omitempty"`
	Profile string `json:"profile,omitempty"`
	// ReplacesForwardTo and ForwardTo are the senders it forwards to.
	ReplacesForwardTo bool     `json:"replaces_forward_to,omitempty"`
	ForwardTo         []string `json:"forward_to,omitempty"`
}

// Values are a scan's values as the catalog keeps them (tenant.ScanValues):
// the session's Study Date, and the scan's Modality, Series Description and
// Series Number. Each is left out when the catalog holds none.
type Values struct {
	// StudyDate is the session's Study Date (0008,0020), YYYYMMDD.
	StudyDate string `json:"study_date,omitempty"`
	// Modality is the scan's Modality (0008,0060).
	Modality string `json:"modality,omitempty"`
	// SeriesDescription is the scan's Series Description (0008,103E).
	SeriesDescription string `json:"series_description,omitempty"`
	// SeriesNumber is the scan's Series Number (0020,0011), nil when the
	// catalog holds none.
	SeriesNumber *int32 `json:"series_number,omitempty"`
}

// ValuesOf returns the values as the sidecar holds them.
func ValuesOf(v tenant.ScanValues) Values {
	out := Values{StudyDate: v.StudyDate, Modality: v.Modality, SeriesDescription: v.SeriesDescription}
	if v.HasSeriesNumber {
		n := v.SeriesNumber
		out.SeriesNumber = &n
	}
	return out
}

// Tenant returns the values as the catalog keeps them.
func (v Values) Tenant() tenant.ScanValues {
	out := tenant.ScanValues{StudyDate: v.StudyDate, Modality: v.Modality, SeriesDescription: v.SeriesDescription}
	if v.SeriesNumber != nil {
		out.SeriesNumber, out.HasSeriesNumber = *v.SeriesNumber, true
	}
	return out
}

// RouteOf returns the route of the session as the sidecar holds it, or nil
// for a routing that chose nothing of its own.
func RouteOf(r tenant.ReceptionRoute) *Route {
	if r.IsZero() {
		return nil
	}
	out := &Route{ReleasedStorage: r.ReleasedStorage, Name: r.Route, ReplacesForwardTo: r.ReplacesForwardTo, ForwardTo: r.ForwardTo}
	if !r.ReleasedProfile.IsZero() {
		out.ReleasedProfile = r.ReleasedProfile.String()
	}
	if !r.Profile.IsZero() {
		out.Profile = r.Profile.String()
	}
	return out
}

// Tenant returns the route as the catalog keeps it; the zero route for nil.
func (r *Route) Tenant() tenant.ReceptionRoute {
	if r == nil {
		return tenant.ReceptionRoute{}
	}
	out := tenant.ReceptionRoute{ReleasedStorage: r.ReleasedStorage, Route: r.Name, ReplacesForwardTo: r.ReplacesForwardTo, ForwardTo: r.ForwardTo}
	out.ReleasedProfile = profileRef(r.ReleasedProfile)
	out.Profile = profileRef(r.Profile)
	return out
}

// profileRef parses "name@version" as validProfile accepts it; the zero ref
// for "".
func profileRef(s string) tenant.ProfileRef {
	if s == "" {
		return tenant.ProfileRef{}
	}
	name, version, _ := strings.Cut(s, "@")
	n, _ := strconv.ParseInt(version, 10, 32)
	return tenant.ProfileRef{Name: name, Version: int32(n)}
}

// utcTime reports whether t is set and in UTC.
func utcTime(t time.Time) bool {
	_, off := t.Zone()
	return !t.IsZero() && off == 0
}

// validName reports whether s is a name under a tenant's rule.
func validName(s string) bool { return tenant.ValidateName("name", s) == nil }

// Validate returns an error wrapping ErrFormat unless h may be written.
func (h ScanHeader) Validate() error {
	if err := uid.Check(h.StudyUID); err != nil {
		return fmt.Errorf("%w: scan.study_uid: %w", ErrFormat, err)
	}
	if err := uid.Check(h.SeriesUID); err != nil {
		return fmt.Errorf("%w: scan.series_uid: %w", ErrFormat, err)
	}
	if (h.Receiver != "" && !validName(h.Receiver)) || (h.SessionReceiver != "" && !validName(h.SessionReceiver)) {
		return fmt.Errorf("%w: scan.receiver and scan.session_receiver are names, or empty", ErrFormat)
	}
	if err := h.Values.Tenant().Validate(); err != nil {
		return fmt.Errorf("%w: scan.values: %w", ErrFormat, err)
	}
	if !utcTime(h.Created) || !utcTime(h.Updated) || h.Updated.Before(h.Created) {
		return fmt.Errorf("%w: scan.created and scan.updated are times in UTC, in order", ErrFormat)
	}
	if r := h.Route; r != nil {
		if r.IsZero() {
			return fmt.Errorf("%w: scan.route is left out when it chose nothing", ErrFormat)
		}
		for _, s := range []string{r.ReleasedProfile, r.Profile} {
			if !validProfile(s) {
				return fmt.Errorf("%w: scan.route holds a profile that is not name@version", ErrFormat)
			}
		}
		if r.Tenant().Validate() != nil {
			return fmt.Errorf("%w: scan.route is not a route the catalog keeps", ErrFormat)
		}
	}
	return nil
}

// IsZero reports whether the route chose nothing.
func (r Route) IsZero() bool {
	return r.ReleasedStorage == "" && r.ReleasedProfile == "" && r.Name == "" && r.Profile == "" && !r.ReplacesForwardTo && len(r.ForwardTo) == 0
}

// validReception reports whether s is a reception id.
func validReception(s string) bool { return master.ValidateID("reception", s) == nil }

// validVersionText reports whether s is a build's version as it is written
// in a line: 1 to MaxVersionText bytes of letters, digits and ". _ + - ~".
func validVersionText(s string) bool {
	if s == "" || len(s) > MaxVersionText || !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._+-~", rune(c)) {
			return false
		}
	}
	return true
}

// SafeVersion returns s when it is a build's version a line may hold, and ""
// otherwise, so a version string that is not one is left out of a line
// rather than keeping its scan from being archived.
func SafeVersion(s string) string {
	if validVersionText(s) {
		return s
	}
	return ""
}
