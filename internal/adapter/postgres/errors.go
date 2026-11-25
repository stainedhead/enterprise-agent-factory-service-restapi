package postgres

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Op says what a statement was doing, because database contract §6 maps 23503 differently on a
// write (unknown reference) and on a delete (dependents exist).
type Op int

// Statement kinds.
const (
	OpWrite Op = iota
	OpDelete
	OpRead
)

// SQLSTATEs named by database contract §5 and §6.
const (
	codeSerializationFailure = "40001"
	codeDeadlock             = "40P01"
	codeCheckViolation       = "23514"
	codeNotNull              = "23502"
	codeUnique               = "23505"
	codeForeignKey           = "23503"
	codeNotInPrerequisite    = "55000"
	codeLockNotAvailable     = "55P03"
	codeNoDataFound          = "P0002"
	codeInsufficientPriv     = "42501"
	codeInvalidParameter     = "22023"
	codeQueryCanceled        = "57014"
	codeTooManyConnections   = "53300"
)

// retryable reports a serialization failure or deadlock (database contract §5.6).
func retryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == codeSerializationFailure || pe.Code == codeDeadlock)
}

// MapError turns a pgx error into a domain error per database contract §6. A domain error passes
// through unchanged; nil stays nil. pgx.ErrNoRows becomes NotFound for resource.
func MapError(err error, op Op, resource string) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound(resource)
	}
	if unavailable(err) {
		e := domain.Unavailable("database", err)
		e.RetryAfterSeconds = 1
		return e
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return domain.Internal(err)
	}
	if c, ok := constraintConflicts[pe.ConstraintName]; ok && (pe.Code == codeCheckViolation || pe.Code == codeNotInPrerequisite) {
		return withCause(domain.Conflict(c.reason, pe.Message, constraintDetails(pe)), err)
	}
	if reason, ok := constraintRefusals[pe.ConstraintName]; ok && pe.Code == codeCheckViolation {
		e := domain.Validation(domain.FieldError{Field: constraintFieldOf(pe), Reason: reason})
		e.Reason = reason
		e.Message = pe.Message
		e.Cause = err
		return e
	}
	switch {
	case pe.Code == codeCheckViolation || pe.Code == codeNotNull:
		return validation(pe, err)
	case pe.Code == codeInvalidParameter:
		slog.Warn("postgres: invalid parameter value", "sqlstate", pe.Code, "message", pe.Message, "routine", pe.Routine)
		return validation(pe, err)
	case strings.HasPrefix(pe.Code, "22"):
		return validation(pe, err)
	case pe.Code == codeUnique:
		if reason, ok := uniqueReasons[pe.ConstraintName]; ok {
			return withCause(domain.Conflict(reason, pe.Message, constraintDetails(pe)), err)
		}
		return withCause(domain.Conflict("duplicate", pe.Message, constraintDetails(pe)), err)
	case pe.Code == codeForeignKey && op == OpDelete:
		return withCause(domain.Conflict("dependents_exist", pe.Message, constraintDetails(pe)), err)
	case pe.Code == codeForeignKey:
		return validation(pe, err)
	case pe.Code == codeNotInPrerequisite:
		return withCause(domain.Conflict("invalid_state", pe.Message, nil), err)
	case pe.Code == codeInsufficientPriv:
		e := domain.AccessDenied("")
		e.Message = pe.Message
		e.Cause = err
		return e
	case pe.Code == codeNoDataFound:
		slog.Warn("postgres: no data found", "message", pe.Message, "routine", pe.Routine)
		return domain.Internal(err)
	default:
		return domain.Internal(err)
	}
}

// uniqueReasons names the unique violations that carry a contract reason (Paperclip change): a second
// live collaboration agent, and a team slug whose collaboration hostname is still held (RD-22).
var uniqueReasons = map[string]string{
	"agents_team_collaboration_uq": domain.ReasonCollaborationAgentExists,
	"collab_hostname_held":         domain.ReasonHostnameHeld,
	// 000023: a route slug is unique per scope, and a team slug may not take a service slug.
	"model_endpoints_slug_taken":      domain.ReasonSlugTaken,
	"model_endpoints_service_slug_uq": domain.ReasonSlugTaken,
	"model_endpoints_team_slug_uq":    domain.ReasonSlugTaken,
}

type conflictReason struct{ reason string }

// constraintConflicts names the 000022 guards that are a 409 with a contract reason. The two
// orchestration_required constraints are deferred, so they surface at COMMIT (the caller's
// transaction wrapper maps the commit error through MapError).
var constraintConflicts = map[string]conflictReason{
	"teams_orchestration_required":              {domain.ReasonOrchestrationAgentReq},
	"agents_orchestration_required":             {domain.ReasonOrchestrationAgentReq},
	"orchestration_runtime_unset":               {domain.ReasonOrchestrationRuntimeUnset},
	"models_enabled_fixed":                      {domain.ReasonServiceModelAccessAll},
	"orchestration_tool_not_enabled":            {domain.ReasonToolNotEnabled},
	"team_collaboration_exposure_limit":         {domain.ReasonExposureAboveLimit},
	"team_collaboration_surface_supported":      {domain.ReasonSurfaceNotSupported},
	"team_collaboration_registration_supported": {domain.ReasonRegistrationNotSupported},
}

// constraintRefusals names the 000022 guards that are a 400 ValidationError with a contract reason.
var constraintRefusals = map[string]string{
	"agents_role_reserved":             domain.ReasonRoleReserved,
	"agents_name_reserved":             domain.ReasonNameReserved,
	"agent_pools_name_reserved":        domain.ReasonNameReserved,
	"agents_models_selection_nonempty": domain.ReasonModelsSelectionEmpty,
	"agent_models_service":             domain.ReasonModelNotEnabledForService,
	"agent_models_team":                domain.ReasonModelNotEnabledForTeam,
	"agents_default_model_allowed":     domain.ReasonDefaultModelNotAllowed,
	"service_settings_alias_read_only": "setting_renamed",
	// 000023 model endpoint guards (database contract §7.4).
	"provider_disabled":                  domain.ReasonProviderDisabled,
	"no_fallback_model":                  domain.ReasonNoFallbackModel,
	"endpoint_base_url_placeholder":      domain.ReasonEndpointBaseURLPlaceholder,
	"endpoint_credential_path":           domain.ReasonEndpointCredentialPath,
	"agent_routes_team":                  domain.ReasonAgentRoutesTeam,
	"pool_routes_team":                   domain.ReasonAgentRoutesTeam,
	"agents_default_route_allowed":       domain.ReasonAgentsDefaultRouteAllowed,
	"agent_pools_default_route_allowed":  domain.ReasonAgentsDefaultRouteAllowed,
	"agent_model_endpoints_limit":        "route_limit",
	"pool_model_endpoints_limit":         "route_limit",
	"endpoint_models_limit":              "model_limit",
	"endpoint_credentials_auth_kind":     "credential_not_required",
	"endpoint_credentials_scope":         "credential_scope",
	"model_endpoints_in_namespace_host":  "in_namespace_host",
	"model_endpoints_auth_kind_provider": "auth_kind_provider",
	"model_endpoints_provider_check":     "provider_invalid",
	"team_model_endpoints_service_only":  "not_service_endpoint",
	"endpoint_federations_kind":          "federation_kind",
}

// endpointRefusalFields names the request field of each 000023 refusal.
var endpointRefusalFields = map[string]string{
	"provider_disabled":                  "enabled",
	"no_fallback_model":                  "fallbackModel",
	"endpoint_base_url_placeholder":      "baseUrl",
	"endpoint_credential_path":           "value",
	"agent_routes_team":                  "routes",
	"pool_routes_team":                   "routes",
	"agents_default_route_allowed":       "defaultModel",
	"agent_pools_default_route_allowed":  "defaultModel",
	"agent_model_endpoints_limit":        "routes",
	"pool_model_endpoints_limit":         "routes",
	"endpoint_models_limit":              "models",
	"endpoint_credentials_auth_kind":     "authKind",
	"endpoint_credentials_scope":         "credentialMode",
	"model_endpoints_in_namespace_host":  "baseUrl",
	"model_endpoints_auth_kind_provider": "authKind",
	"model_endpoints_provider_check":     "provider",
	"team_model_endpoints_service_only":  "endpointIds",
	"endpoint_federations_kind":          "kind",
}

func constraintFieldOf(pe *pgconn.PgError) string {
	if f, ok := endpointRefusalFields[pe.ConstraintName]; ok {
		return f
	}
	switch pe.ConstraintName {
	case "agents_role_reserved":
		return "role"
	case "agents_name_reserved", "agent_pools_name_reserved":
		return "name"
	case "service_settings_alias_read_only":
		return "key"
	}
	return "models"
}

// constraintFields maps the Paperclip check constraints to the request field they refuse.
var constraintFields = map[string]string{
	"agents_orchestration_template":      "template",
	"agents_orchestration_capability":    "template",
	"agents_orchestration_identity":      "iamIdentity",
	"agents_orchestration_snapshots":     "volume.snapshotSchedule",
	"agents_collaboration_template":      "template",
	"agents_collaboration_capability":    "template",
	"agents_worker_capability":           "template",
	"agents_collaboration_identity":      "iamIdentity",
	"agents_collaboration_snapshots":     "volume.snapshotSchedule",
	"agents_role_immutable":              "role",
	"secrets_reserved_name":              "name",
	"secrets_managed_by_name":            "name",
	"secrets_paperclip_scope":            "name",
	"secrets_paperclip_prefix":           "name",
	"secrets_registered_paperclip_check": "name",
}

func validation(pe *pgconn.PgError, cause error) *domain.Error {
	field := pe.ColumnName
	if field == "" {
		field = pe.ConstraintName
	}
	if f, ok := constraintFields[pe.ConstraintName]; ok {
		field = f
	}
	e := domain.Validation(domain.FieldError{Field: field, Reason: pe.Message})
	e.Message = pe.Message
	e.Cause = cause
	return e
}

func constraintDetails(pe *pgconn.PgError) map[string]any {
	if pe.ConstraintName == "" {
		return nil
	}
	return map[string]any{"constraint": pe.ConstraintName}
}

func withCause(e *domain.Error, cause error) *domain.Error {
	e.Cause = cause
	return e
}

// unavailable reports connection failures, timeouts and server shutdowns: the request may succeed
// later, so the API answers 503.
func unavailable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return strings.HasPrefix(pe.Code, "08") || strings.HasPrefix(pe.Code, "57P") ||
			pe.Code == codeQueryCanceled || pe.Code == codeTooManyConnections || pe.Code == codeLockNotAvailable
	}
	return false
}
