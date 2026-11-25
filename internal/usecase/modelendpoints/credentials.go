package modelendpoints

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// CredentialResult is a credential write's result, or the stored response of a replayed rotation.
type CredentialResult = Created[Credential]

// scopeFor checks a credential scope against the endpoint: _shared for a shared service endpoint, a team
// id otherwise.
func scopeFor(e Endpoint, scope string) error {
	shared := !e.IsTeam() && e.CredentialMode == domain.CredentialShared
	switch {
	case shared && scope != domain.SharedCredentialScope:
		return refused("scope", "this endpoint takes one shared credential: use _shared", "")
	case !shared && scope == domain.SharedCredentialScope:
		return refused("scope", "this endpoint takes one credential per team: use the team id", "")
	case !shared && !domain.ValidTeamID(scope):
		return refused("scope", "must be _shared or a team id", "")
	case e.IsTeam() && scope != e.TeamID:
		return refused("scope", "a team endpoint takes its own team's credential", "")
	}
	return nil
}

func needsCredential(e Endpoint) error {
	if !e.AuthKind.NeedsCredential() {
		return refused("authKind", "the endpoint (auth kind "+string(e.AuthKind)+") takes no stored credential", "")
	}
	return nil
}

func smToken(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:48]
}

func auditCredential(c Credential) map[string]any {
	return map[string]any{"endpointId": c.EndpointID, "scope": c.Scope, "secretArn": c.ARN, "state": c.State}
}

// PutCredential implements PutModelEndpointCredential: the value goes to Secrets Manager, the row holds the
// ARN. If-Match is required once the credential exists.
func (s *Service) PutCredential(ctx context.Context, c acting.Caller, endpointID, scope string, v domain.SecretValue, expect *domain.Version) (Credential, error) {
	if err := s.serviceCheck(ctx, c, "PutModelEndpointCredential", authz.AdminModelEndpoints); err != nil {
		return Credential{}, err
	}
	res, err := s.writeCredential(ctx, c, "PutModelEndpointCredential", "", endpointID, scope, v, expect, false, nil)
	return res.Value, err
}

// RotateCredential implements RotateModelEndpointCredential: a new value for an existing credential.
func (s *Service) RotateCredential(ctx context.Context, c acting.Caller, endpointID, scope string, v domain.SecretValue, idem *acting.Idem) (CredentialResult, error) {
	if err := s.serviceCheck(ctx, c, "RotateModelEndpointCredential", authz.AdminModelEndpoints); err != nil {
		return CredentialResult{}, err
	}
	return s.writeCredential(ctx, c, "RotateModelEndpointCredential", "", endpointID, scope, v, nil, true, idem)
}

// PutTeamCredential implements PutTeamModelEndpointCredential: the team's credential of its own endpoint or
// of a per_team service endpoint it sees.
func (s *Service) PutTeamCredential(ctx context.Context, c acting.Caller, teamID, endpointID string, v domain.SecretValue, expect *domain.Version) (Credential, error) {
	if err := s.teamCheck(ctx, c, "PutTeamModelEndpointCredential", teamID, authz.ModelEndpointsWrite); err != nil {
		return Credential{}, err
	}
	res, err := s.writeCredential(ctx, c, "PutTeamModelEndpointCredential", teamID, endpointID, teamID, v, expect, false, nil)
	return res.Value, s.deny(ctx, c, "PutTeamModelEndpointCredential", teamID, err)
}

func (s *Service) writeCredential(ctx context.Context, c acting.Caller, action, teamID, endpointID, scope string, v domain.SecretValue,
	expect *domain.Version, rotate bool, idem *acting.Idem) (CredentialResult, error) {
	var (
		res        CredentialResult
		createdARN string
	)
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res, createdARN = CredentialResult{}, ""
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		e, err := s.endpointFor(ctx, tx, teamID, endpointID)
		if err != nil {
			return err
		}
		if err := needsCredential(e); err != nil {
			return err
		}
		if err := scopeFor(e, scope); err != nil {
			return err
		}
		cur, err := s.Repo.GetCredential(ctx, tx, endpointID, scope)
		exists := err == nil
		switch {
		case err != nil && !errors.Is(err, &domain.Error{Kind: domain.KindNotFound}):
			return err
		case rotate && !exists:
			return domain.NotFound("credential")
		case exists && expect != nil:
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		case exists && !rotate:
			return domain.PreconditionRequired()
		}
		token := smToken(c.Principal.ID, c.RequestID, action, endpointID, scope)
		next := Credential{EndpointID: endpointID, Scope: scope, ARN: cur.ARN}
		if scope != domain.SharedCredentialScope {
			next.TeamID = scope
		}
		if exists {
			if _, err := s.Secrets.PutValue(ctx, cur.ARN, v, token); err != nil {
				return domain.Unavailable("secrets manager", err)
			}
		} else {
			path, err := s.Repo.CredentialPath(ctx, tx, e, scope)
			if err != nil {
				return err
			}
			tags := map[string]string{"afs:managed-by": "afs", "afs:endpoint": e.Slug}
			if next.TeamID != "" {
				tags["afs:team-id"] = next.TeamID
			}
			cr, err := s.Secrets.Create(ctx, path, v, token, tags)
			if errors.Is(err, secrets.ErrNameTaken) {
				return domain.Conflict("duplicate", "Secrets Manager already holds a secret at "+path, nil)
			}
			if err != nil {
				return domain.Unavailable("secrets manager", err)
			}
			createdARN, next.ARN = cr.ARN, cr.ARN
		}
		out, err := s.Repo.PutCredential(ctx, tx, next)
		if err != nil {
			return err
		}
		var before any
		if exists {
			before = auditCredential(cur)
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindCredential, endpointID+"/"+scope, out.TeamID, before, auditCredential(out))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventEndpointCredentialSet, out.TeamID, kindEndpoint, endpointID,
			eventData(e.ModelEndpoint, map[string]any{"scope": scope, "rotated": exists})); err != nil {
			return err
		}
		res.Value = out
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	if err != nil && createdARN != "" {
		s.compensate(ctx, createdARN)
	}
	return res, err
}

func (s *Service) compensate(ctx context.Context, arn string) {
	ctx = context.WithoutCancel(ctx)
	if err := s.Secrets.ForceDelete(ctx, arn); err == nil {
		return
	}
	_ = s.Secrets.TagOrphan(ctx, arn)
	if s.OnOrphan != nil {
		s.OnOrphan()
	}
}

// endpointFor loads an endpoint for a service admin (teamID "") or as the team sees it.
func (s *Service) endpointFor(ctx context.Context, tx port.Tx, teamID, endpointID string) (Endpoint, error) {
	if teamID == "" {
		return s.Repo.GetEndpoint(ctx, tx, endpointID, false)
	}
	te, err := s.Repo.TeamEndpoint(ctx, tx, teamID, endpointID)
	if err != nil {
		return Endpoint{}, err
	}
	if !te.IsTeam() && te.CredentialMode == domain.CredentialShared {
		return Endpoint{}, errDenied{perm: authz.AdminModelEndpoints, reason: domain.ReasonNeedsServiceAdmin,
			msg: "the shared credential of a service endpoint is set by a service admin"}
	}
	return te.Endpoint, nil
}

var (
	uuidRE       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	projNumRE    = regexp.MustCompile(`^[0-9]{1,20}$`)
	poolIDRE     = regexp.MustCompile(`^[a-z0-9-]{4,32}$`)
	projectIDRE  = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	gcpLocRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	azureAud     = "api://AzureADTokenExchange"
	gatewaySAFmt = "system:serviceaccount:%s:afs-gateway"
)

// ValidateFederation checks the identifiers of a federation of kind azure or google.
func ValidateFederation(f Federation) error {
	var fe []domain.FieldError
	check := func(field, v string, re *regexp.Regexp) {
		if !re.MatchString(v) {
			fe = append(fe, domain.FieldError{Field: field, Reason: "is required and must match " + re.String()})
		}
	}
	none := func(field, v string) {
		if v != "" {
			fe = append(fe, domain.FieldError{Field: field, Reason: "is not used by " + f.Kind + " federation"})
		}
	}
	switch f.Kind {
	case "azure":
		check("tenantId", f.TenantID, uuidRE)
		check("clientId", f.ClientID, uuidRE)
		for k, v := range map[string]string{"projectNumber": f.ProjectNumber, "poolId": f.PoolID, "providerId": f.ProviderID, "projectId": f.ProjectID, "location": f.Location} {
			none(k, v)
		}
	case "google":
		check("projectNumber", f.ProjectNumber, projNumRE)
		check("poolId", f.PoolID, poolIDRE)
		check("providerId", f.ProviderID, poolIDRE)
		check("projectId", f.ProjectID, projectIDRE)
		check("location", f.Location, gcpLocRE)
		none("tenantId", f.TenantID)
		none("clientId", f.ClientID)
	default:
		fe = append(fe, domain.FieldError{Field: "authKind", Reason: "the endpoint does not use workload identity federation"})
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}

// GoogleAudience is the STS audience of a Google workload identity pool provider.
func GoogleAudience(f Federation) string {
	return "//iam.googleapis.com/projects/" + f.ProjectNumber + "/locations/global/workloadIdentityPools/" + f.PoolID + "/providers/" + f.ProviderID
}

// TeamGatewayRoleName is the IAM role name of a team gateway: "afs-<team ulid>-gateway".
func TeamGatewayRoleName(teamID string) string {
	return "afs-" + strings.ToLower(strings.TrimPrefix(teamID, "team_")) + "-gateway"
}

// expected fills what the enterprise's cloud admins configure on their side.
func (s *Service) expected(f Federation, namespace string) Federation {
	switch f.Kind {
	case "azure":
		f.ExpectedIssuer, f.ExpectedSubject, f.ExpectedAudience = s.Config.OIDCIssuer, strings.Replace(gatewaySAFmt, "%s", namespace, 1), azureAud
	case "google":
		f.ExpectedAudience = GoogleAudience(f)
		if s.Config.AccountID != "" {
			part := s.Config.Partition
			if part == "" {
				part = "aws"
			}
			f.ExpectedRoleARN = "arn:" + part + ":iam::" + s.Config.AccountID + ":role/" + TeamGatewayRoleName(f.TeamID)
		}
	}
	return f
}

func auditFederation(f Federation) map[string]any {
	return map[string]any{"endpointId": f.EndpointID, "teamId": f.TeamID, "kind": f.Kind, "tenantId": f.TenantID, "clientId": f.ClientID,
		"projectNumber": f.ProjectNumber, "poolId": f.PoolID, "providerId": f.ProviderID, "projectId": f.ProjectID, "location": f.Location}
}

// PutFederation implements PutEndpointFederation (metadata only). If-Match is required once it exists.
func (s *Service) PutFederation(ctx context.Context, c acting.Caller, endpointID, teamID string, f Federation, expect *domain.Version) (Federation, error) {
	if err := s.serviceCheck(ctx, c, "PutEndpointFederation", authz.AdminModelEndpoints); err != nil {
		return Federation{}, err
	}
	if !domain.ValidTeamID(teamID) {
		return Federation{}, domain.NotFound("team")
	}
	var out Federation
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		e, err := s.Repo.GetEndpoint(ctx, tx, endpointID, false)
		if err != nil {
			return err
		}
		if e.IsTeam() && e.TeamID != teamID {
			return domain.NotFound("team")
		}
		f.EndpointID, f.TeamID, f.Kind = endpointID, teamID, e.AuthKind.FederationKind()
		if err := ValidateFederation(f); err != nil {
			return err
		}
		team, err := s.Repo.TeamGateway(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		cur, err := s.Repo.GetFederation(ctx, tx, endpointID, teamID)
		exists := err == nil
		switch {
		case err != nil && !errors.Is(err, &domain.Error{Kind: domain.KindNotFound}):
			return err
		case exists && expect == nil:
			return domain.PreconditionRequired()
		case exists:
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		if out, err = s.Repo.PutFederation(ctx, tx, f); err != nil {
			return err
		}
		var before any
		if exists {
			before = auditFederation(cur)
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutEndpointFederation", kindFederation, endpointID+"/"+teamID, teamID, before, auditFederation(out))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventEndpointCredentialSet, teamID, kindEndpoint, endpointID,
			eventData(e.ModelEndpoint, map[string]any{"scope": teamID, "federation": f.Kind})); err != nil {
			return err
		}
		out = s.expected(out, team.Namespace)
		return nil
	})
	return out, err
}
