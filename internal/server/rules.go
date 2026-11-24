package server

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/amrshadid/nopacs/internal/access"
)

// The procedures of the standard gRPC health service
// (grpc/health/v1/health.proto). They are the only methods anyone may call
// without a key: a load balancer or an orchestrator asks them, and they
// answer only "serving" or "not serving". The server owns them, so it owns
// this one public rule; every other method's rule comes from Methods.
const (
	HealthCheckProcedure = "/grpc.health.v1.Health/Check"
	HealthWatchProcedure = "/grpc.health.v1.Health/Watch"
)

// Methods is the method-to-requirement table of the API, which package
// internal/api owns and tests against the contract. The server asks it
// about every RPC, on the request path, before the handler.
//
// It has two operations so that a requirement that does not depend on the
// request is checked before the request's body is read at all: a refused
// caller's message is then never read (access.proto, "The order of
// checks").
type Methods interface {
	// RequestFor reports whether procedure, a full procedure name such as
	// "/nopacs.v1.TenantService/GetTenant", is a method of the table, and
	// returns a new empty request message of it when its requirement
	// depends on the request, or nil when it does not. The server reads the
	// request's first message into that message before the check.
	RequestFor(procedure string) (msg proto.Message, known bool)
	// Requirement returns what the request needs of its caller and the
	// facts its audit entry takes (access.Request's Object and Details; the
	// server fills Method, RemoteAddress and RequestID). msg is the request
	// read into the message RequestFor returned, or nil when RequestFor
	// returned nil, or the empty message RequestFor returned when the
	// request could not be read (the server then checks the caller against
	// that requirement before it says the request was malformed).
	Requirement(procedure string, msg proto.Message) (access.Requirement, access.Request)
}

// PendingMethods is what a Methods table may also do: say which methods
// the contract defines and no handler serves yet. A service whose other
// methods are served still has every method in its generated interface, so
// its registration carries such a method too; the server leaves it out of
// the routes, and a call to it is answered as one to any method nothing
// serves (UNIMPLEMENTED, "no such method"), never by the handler's body
// and never refused at start as a method without a requirement.
type PendingMethods interface {
	// Pending reports whether procedure is such a method.
	Pending(procedure string) bool
}

// pending reports whether the table says procedure is not served yet.
func (s *Server) pending(procedure string) bool {
	p, ok := s.methods.(PendingMethods)
	return ok && p.Pending(procedure)
}

// WithDecision puts the access check's Decision in the context the handler
// receives, through package internal/api's typed accessor, so a handler
// hands it to the access operations that act for the caller.
type WithDecision func(ctx context.Context, d access.Decision) context.Context

// noMethods is the table of a server given none: it knows no method, so
// every RPC but the health service is a method nobody registered.
type noMethods struct{}

// RequestFor knows no procedure.
func (noMethods) RequestFor(string) (proto.Message, bool) { return nil, false }

// Requirement is never asked for, since RequestFor knows nothing.
func (noMethods) Requirement(string, proto.Message) (access.Requirement, access.Request) {
	return access.Requirement{}, access.Request{}
}

// rule is what the request path needs to know of one request's method:
//
//   - public: no credential at all; only the health service;
//   - newMsg: when set, the requirement depends on the request, and the
//     first message is read into newMsg() (bounded by the 4 MiB limit)
//     before the check; when nil, nothing is read before the check;
//   - need: the requirement and the audit facts, given the message read,
//     or nil.
type rule struct {
	public bool
	newMsg func() proto.Message
	need   func(proto.Message) (access.Requirement, access.Request)
}

// readsMessage reports whether the rule needs the request's message.
func (r rule) readsMessage() bool { return r.newMsg != nil }

// fallback is the requirement used when the message the rule needs cannot
// be read (malformed, too large): the requirement of the empty message,
// which for a scope is the role on the installation. The order of checks
// stays "authenticated, then the role, then the request".
func (r rule) fallback() (access.Requirement, access.Request) {
	if r.newMsg == nil {
		return r.need(nil)
	}
	return r.need(r.newMsg())
}

// on returns a rule with a fixed requirement and no audit facts.
func on(req access.Requirement) rule {
	return rule{need: func(proto.Message) (access.Requirement, access.Request) { return req, access.Request{} }}
}

// publicRule is the health service's rule.
var publicRule = rule{public: true}

// ruleFor returns the rule of an RPC's procedure, and whether the procedure
// is one the server knows (a method of Methods or the health service).
func (s *Server) ruleFor(procedure string) (rule, bool) {
	if procedure == HealthCheckProcedure || procedure == HealthWatchProcedure {
		return publicRule, true
	}
	msg, known := s.methods.RequestFor(procedure)
	if !known {
		return rule{}, false
	}
	r := rule{need: func(m proto.Message) (access.Requirement, access.Request) { return s.methods.Requirement(procedure, m) }}
	if msg != nil {
		first := true
		r.newMsg = func() proto.Message {
			// The message RequestFor already made is used once; later calls
			// (the fallback) ask for a new one.
			if first {
				first = false
				return msg
			}
			m, _ := s.methods.RequestFor(procedure)
			return m
		}
	}
	return r, true
}
