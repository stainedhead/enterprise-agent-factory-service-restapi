package team

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// reconcileAWS makes the team's inference profiles and its two roles match the row, and returns the
// team role's ARN (computable from the account, so the Kubernetes side never waits for IAM).
func (c *Controller) reconcileAWS(ctx context.Context, t Desired) (teamRoleARN string, err error) {
	a := c.cfg.AWS
	path := RolePath(t.Namespace)
	teamRole, err := TeamRoleName(t.ID)
	if err != nil {
		return "", err
	}
	bedrockRole, err := BedrockRoleName(t.ID)
	if err != nil {
		return "", err
	}
	teamRoleARN = RoleARN(a.Partition, a.AccountID, path, teamRole)
	tags := map[string]string{TagTeamID: t.ID, TagTeamSlug: t.Slug}

	grants, err := c.reconcileProfiles(ctx, t, tags)
	if err != nil {
		return teamRoleARN, err
	}

	trust, err := TeamRoleTrust(a.AccountID, a.ClusterARN, a.ESORoleARN)
	if err != nil {
		return teamRoleARN, err
	}
	policy, err := TeamRolePolicy(a.SecretsPrefix, t.Namespace, t.ID)
	if err != nil {
		return teamRoleARN, err
	}
	if _, err := c.cfg.IAM.EnsureRole(ctx, RoleSpec{
		Name: teamRole, Path: path, Description: "AFS team " + t.Slug, TrustPolicy: trust,
		PermissionsBoundaryARN: a.PermissionsBoundaryARN, Tags: tags, Policies: map[string]string{"team": policy},
	}); err != nil {
		return teamRoleARN, fmt.Errorf("team role: %w", err)
	}

	bTrust, err := BedrockRoleTrust(a.GatewayRoleARN)
	if err != nil {
		return teamRoleARN, err
	}
	bPolicy, err := BedrockRolePolicy(grants)
	if err != nil {
		return teamRoleARN, err
	}
	if _, err := c.cfg.IAM.EnsureRole(ctx, RoleSpec{
		Name: bedrockRole, Path: path, Description: "AFS gateway access to Bedrock for team " + t.Slug, TrustPolicy: bTrust,
		PermissionsBoundaryARN: a.PermissionsBoundaryARN, Tags: tags, Policies: map[string]string{"bedrock": bPolicy},
	}); err != nil {
		return teamRoleARN, fmt.Errorf("bedrock role: %w", err)
	}
	return teamRoleARN, nil
}

// reconcileProfiles creates one application inference profile per enabled model, records its ARN in
// team_models and deletes the team's profiles of models that are no longer enabled.
func (c *Controller) reconcileProfiles(ctx context.Context, t Desired, tags map[string]string) ([]ProfileGrant, error) {
	a := c.cfg.AWS
	var grants []ProfileGrant
	keep := map[string]bool{}
	for _, m := range t.Models {
		name, err := ProfileName(t.ID, m.ID)
		if err != nil {
			return nil, err
		}
		p, err := c.cfg.Profiles.EnsureProfile(ctx, ProfileSpec{Name: name, SourceARN: SourceARN(a.Partition, a.Region, a.AccountID, m.ID), Tags: tags})
		if err != nil {
			return nil, fmt.Errorf("inference profile for %s: %w", m.ID, err)
		}
		keep[p.ARN] = true
		if p.ARN != m.ProfileARN {
			if err := c.cfg.Source.SetInferenceProfileARN(ctx, t.ID, m.ID, p.ARN); err != nil {
				return nil, fmt.Errorf("record inference profile of %s: %w", m.ID, err)
			}
		}
		grants = append(grants, ProfileGrant{ProfileARN: p.ARN, ModelARN: foundationModelARN(a.Partition, m.ID)})
	}
	existing, err := c.cfg.Profiles.ListProfiles(ctx, TagTeamID, t.ID)
	if err != nil {
		return nil, fmt.Errorf("list inference profiles: %w", err)
	}
	for _, p := range existing {
		if keep[p.ARN] {
			continue
		}
		if err := c.cfg.Profiles.DeleteProfile(ctx, p.ARN); err != nil {
			return nil, fmt.Errorf("delete inference profile %s: %w", p.Name, err)
		}
	}
	slices.SortFunc(grants, func(x, y ProfileGrant) int { return strings.Compare(x.ProfileARN, y.ProfileARN) })
	return grants, nil
}

// foundationModelARN is the foundation model behind a model id in any region: a cross-region profile
// routes to the model in several regions, so the region is a wildcard. An id that is already an ARN has
// no foundation model to grant separately.
func foundationModelARN(partition, modelID string) string {
	if strings.HasPrefix(modelID, "arn:") {
		return ""
	}
	id := modelID
	for _, p := range []string{"us-gov.", "global.", "apac.", "us.", "eu.", "jp.", "au.", "ca."} {
		if rest, ok := strings.CutPrefix(id, p); ok {
			id = rest
			break
		}
	}
	return fmt.Sprintf("arn:%s:bedrock:*::foundation-model/%s", partition, id)
}
