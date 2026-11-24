package api

import (
	"context"
	"strconv"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// routeSubjects pairs each subject a route may read with the contract's
// value of the same name (receiver.proto, Route.conditions).
var routeSubjects = map[v1.ConditionSubject]tenant.RouteSubject{
	v1.ConditionSubject_CONDITION_SUBJECT_MODALITY:          tenant.SubjectModality,
	v1.ConditionSubject_CONDITION_SUBJECT_SOP_CLASS_UID:     tenant.SubjectSOPClassUID,
	v1.ConditionSubject_CONDITION_SUBJECT_MANUFACTURER:      tenant.SubjectManufacturer,
	v1.ConditionSubject_CONDITION_SUBJECT_MODEL_NAME:        tenant.SubjectModelName,
	v1.ConditionSubject_CONDITION_SUBJECT_SOFTWARE_VERSIONS: tenant.SubjectSoftwareVersions,
	v1.ConditionSubject_CONDITION_SUBJECT_CALLING_AE_TITLE:  tenant.SubjectCallingAETitle,
	v1.ConditionSubject_CONDITION_SUBJECT_CALLED_AE_TITLE:   tenant.SubjectCalledAETitle,
}

// routingValues returns the routing a request's receiver gives, checked
// for what a request can get wrong alone: the bounds of its lists, before
// they are copied, and each condition's subject and test. The rules of
// each value are tenant.Routing.Validate's.
func routingValues(field string, m *v1.ReceiverRouting) (tenant.Routing, error) {
	out := tenant.Routing{QuarantineUnmatched: m.GetQuarantineUnmatched()}
	if len(m.GetRoutes()) > tenant.MaxRoutes {
		return out, invalid(field+".routes", "holds at most "+strconv.Itoa(tenant.MaxRoutes)+" routes")
	}
	for i, rm := range m.GetRoutes() {
		at := field + ".routes[" + strconv.Itoa(i) + "]"
		rt := tenant.Route{Name: rm.GetName(), Storage: rm.GetStorage()}
		if pv := rm.GetProfile(); pv != nil {
			rt.Profile = tenant.ProfileRef{Name: pv.GetName(), Version: pv.GetVersion()}
			if err := rt.Profile.Validate(); err != nil {
				return out, fieldOf(at+".profile", err)
			}
		}
		if f := rm.GetForwardTo(); f != nil {
			if len(f.GetSenders()) > tenant.MaxForwardTo {
				return out, invalid(at+".forward_to.senders", "names at most 16 senders")
			}
			rt.ReplacesForwardTo, rt.ForwardTo = true, f.GetSenders()
		}
		if len(rm.GetConditions()) > tenant.MaxRouteConditions {
			return out, invalid(at+".conditions", "holds at most "+strconv.Itoa(tenant.MaxRouteConditions)+" conditions")
		}
		for j, cm := range rm.GetConditions() {
			cond, err := conditionValue(at+".conditions["+strconv.Itoa(j)+"]", cm)
			if err != nil {
				return out, err
			}
			rt.Conditions = append(rt.Conditions, cond)
		}
		out.Routes = append(out.Routes, rt)
	}
	return out, nil
}

// conditionValue returns one route condition a request gives.
func conditionValue(field string, m *v1.ProfileCondition) (tenant.RouteCondition, error) {
	subject, ok := routeSubjects[m.GetSubject()]
	if !ok {
		if m.GetSubject() == v1.ConditionSubject_CONDITION_SUBJECT_RECEIVER {
			return tenant.RouteCondition{}, invalid(field+".subject", "is not the receiver: a route belongs to one receiver")
		}
		return tenant.RouteCondition{}, invalid(field+".subject", "is one of ConditionSubject other than CONDITION_SUBJECT_RECEIVER")
	}
	c := tenant.RouteCondition{Subject: subject, Negate: m.GetNegate()}
	switch t := m.GetTest().(type) {
	case *v1.ProfileCondition_Equals:
		c.Test, c.Value = tenant.TestEquals, t.Equals
	case *v1.ProfileCondition_Prefix:
		c.Test, c.Value = tenant.TestPrefix, t.Prefix
	case *v1.ProfileCondition_Pattern:
		c.Test, c.Value = tenant.TestPattern, t.Pattern
	case *v1.ProfileCondition_Present:
		c.Test, c.Present = tenant.TestPresent, t.Present
	default:
		return c, invalid(field, "has one test: equals, prefix, pattern or present")
	}
	return c, nil
}

// routingMessage returns a receiver's routing as the contract writes it,
// absent when it has none.
func routingMessage(r tenant.Routing) *v1.ReceiverRouting {
	if r.IsZero() {
		return nil
	}
	m := &v1.ReceiverRouting{QuarantineUnmatched: r.QuarantineUnmatched}
	for _, rt := range r.Routes {
		rm := &v1.Route{Name: rt.Name, Storage: rt.Storage}
		if !rt.Profile.IsZero() {
			rm.Profile = &v1.ProfileVersionRef{Name: rt.Profile.Name, Version: rt.Profile.Version}
		}
		if rt.ReplacesForwardTo {
			rm.ForwardTo = &v1.RouteForwardTo{Senders: rt.ForwardTo}
		}
		for _, c := range rt.Conditions {
			rm.Conditions = append(rm.Conditions, conditionMessage(c))
		}
		m.Routes = append(m.Routes, rm)
	}
	return m
}

// conditionMessage returns one route condition as the contract writes it.
func conditionMessage(c tenant.RouteCondition) *v1.ProfileCondition {
	m := &v1.ProfileCondition{Negate: c.Negate}
	for s, rs := range routeSubjects {
		if rs == c.Subject {
			m.Subject = s
		}
	}
	switch c.Test {
	case tenant.TestEquals:
		m.Test = &v1.ProfileCondition_Equals{Equals: c.Value}
	case tenant.TestPrefix:
		m.Test = &v1.ProfileCondition_Prefix{Prefix: c.Value}
	case tenant.TestPattern:
		m.Test = &v1.ProfileCondition_Pattern{Pattern: c.Value}
	case tenant.TestPresent:
		m.Test = &v1.ProfileCondition_Present{Present: c.Present}
	}
	return m
}

// checkRouteProfiles checks each profile version a receiver's routes name
// as a receiver's own is checked (checkProfileVersion): one the tenant has,
// which can de-identify a receiver's files. Each failure is a precondition
// failure naming the route.
func (c *core) checkRouteProfiles(ctx context.Context, p tenant.Pipeline, r tenant.Receiver) error {
	for _, rt := range r.Routing.Routes {
		if rt.Profile.IsZero() {
			continue
		}
		if err := c.checkProfileVersion(ctx, p, "receiver.routing: route "+rt.Name, rt.Profile); err != nil {
			return err
		}
	}
	return nil
}

// errNoProfiles is a tenant database that keeps no profiles, which every
// driver of this version does.
var errNoProfiles = fault.New(fault.Internal, "the tenant's database keeps no profiles")
