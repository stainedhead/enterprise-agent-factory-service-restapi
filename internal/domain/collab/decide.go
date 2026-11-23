package collab

import "net/http"

// Permission names the edge checks (API contract section 3).
const (
	PermAccess     = "collaboration:access"
	PermContribute = "collaboration:contribute"
	PermAPI        = "collaboration:api"
)

// Class is the traffic class of a request (API contract 4.4c).
type Class int

// Traffic classes.
const (
	ClassWeb Class = iota
	ClassAPI
)

// Surface returns the surface a class uses.
func (c Class) Surface() Surface {
	if c == ClassAPI {
		return SurfaceRESTAPI
	}
	return SurfaceWebUI
}

// Safe reports whether a method is read only (GET and HEAD).
func Safe(method string) bool { return method == http.MethodGet || method == http.MethodHead }

// Decision is what the edge does with a request.
type Decision struct {
	Allow    bool
	Status   int
	Code     string
	Reason   string
	Required string // requiredPermission of a permission_required refusal
	// RetryAfter is set on 503.
	RetryAfter int
}

// Allowed is the allow decision.
var Allowed = Decision{Allow: true, Status: http.StatusOK}

// Principal is the caller as the decision sees it: whether it holds any binding on the team, and its
// permissions.
type Principal struct {
	HasBinding bool
	Has        func(perm string) bool
}

// Decide applies the order of API contract 4.4c to a request for a team that has a route:
//  0. the class's surface is effective at the other level only: 404 (CD-38);
//  1. neither surface effective: 404 (no team detail);
//  2. no binding on the team: 403 no_team_role;
//  3. the class's surface is not effective: 403 surface_disabled;
//  4. the route is not ready: 503 with the reason;
//  5. permissions: safe methods need access, others contribute; API traffic needs api as well.
func Decide(r Route, c Class, method string, p Principal) Decision {
	if !r.Effective(c.Surface()) && r.OtherLevel(c.Surface()) {
		return Decision{Status: http.StatusNotFound, Code: "NotFound"}
	}
	if !r.WebUIEffective && !r.RESTAPIEffective {
		return Decision{Status: http.StatusNotFound, Code: "NotFound"}
	}
	if !p.HasBinding {
		return Decision{Status: http.StatusForbidden, Code: "AccessDenied", Reason: "no_team_role"}
	}
	if !r.Effective(c.Surface()) {
		return Decision{Status: http.StatusForbidden, Code: "AccessDenied", Reason: "surface_disabled"}
	}
	if !r.Ready {
		reason := r.NotReadyReason
		if reason == "" {
			reason = "starting"
		}
		return Decision{Status: http.StatusServiceUnavailable, Code: "Unavailable", Reason: reason, RetryAfter: 30}
	}
	need := PermAccess
	if !Safe(method) {
		need = PermContribute
	}
	if c == ClassAPI && !p.Has(PermAPI) {
		return Decision{Status: http.StatusForbidden, Code: "AccessDenied", Reason: "permission_required", Required: PermAPI}
	}
	if !p.Has(need) {
		return Decision{Status: http.StatusForbidden, Code: "AccessDenied", Reason: "permission_required", Required: need}
	}
	return Allowed
}

// FrameAllowed reports whether a client-to-server WebSocket frame may pass: it needs contribute (PC-D17).
func FrameAllowed(p Principal) bool { return p.Has != nil && p.Has(PermContribute) }

// Outcomes of afs.record_collab_access (collaboration_access.outcome).
const (
	OutcomeAllowed       = "allowed"
	OutcomeSignedIn      = "signed_in"
	OutcomeSignedOut     = "signed_out"
	OutcomeSecretWarned  = "secret_warned"
	OutcomeDeniedUnauth  = "denied_unauthenticated"
	OutcomeDeniedForbid  = "denied_forbidden"
	OutcomeDeniedSurface = "denied_surface_disabled"
	OutcomeDeniedSecret  = "denied_secret_detected"
	OutcomeDeniedBad     = "denied_bad_request"
	OutcomeThrottled     = "throttled"
	OutcomeUnavailable   = "unavailable"
)

// OutcomeOf maps a decision to the outcome it is counted under.
func OutcomeOf(d Decision) string {
	switch {
	case d.Allow:
		return OutcomeAllowed
	case d.Status == http.StatusUnauthorized:
		return OutcomeDeniedUnauth
	case d.Status == http.StatusBadRequest:
		return OutcomeDeniedBad
	case d.Status == http.StatusTooManyRequests:
		return OutcomeThrottled
	case d.Status == http.StatusServiceUnavailable:
		return OutcomeUnavailable
	case d.Reason == "surface_disabled":
		return OutcomeDeniedSurface
	case d.Code == "SecretDetected":
		return OutcomeDeniedSecret
	default:
		return OutcomeDeniedForbid
	}
}
