package team

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
)

// teamRoleARN is the team role's ARN, or empty when its name cannot be built.
func (c *Controller) teamRoleARN(t Desired) string {
	a := c.cfg.AWS
	name, err := TeamRoleName(t.ID)
	if err != nil {
		return ""
	}
	return RoleARN(a.Partition, a.AccountID, RolePath(t.Namespace), name)
}

// reconcileAWS makes the team's inference profiles and its two roles match the row, and returns the
// team role's ARN (computable from the account, so the Kubernetes side never waits for IAM).
func (c *Controller) reconcileAWS(ctx context.Context, t Desired, want tags.Set) (teamRoleARN string, requeue time.Duration, err error) {
	a := c.cfg.AWS
	path := RolePath(t.Namespace)
	teamRole, err := TeamRoleName(t.ID)
	if err != nil {
		return "", 0, err
	}
	bedrockRole, err := BedrockRoleName(t.ID)
	if err != nil {
		return "", 0, err
	}
	teamRoleARN = RoleARN(a.Partition, a.AccountID, path, teamRole)
	applied := map[string]string(want.WithApplied())

	grants, requeue, err := c.reconcileProfiles(ctx, t, applied)
	if err != nil {
		return teamRoleARN, 0, err
	}

	trust, err := TeamRoleTrust(a.AccountID, a.ClusterARN, a.ESORoleARN)
	if err != nil {
		return teamRoleARN, 0, err
	}
	policy, err := TeamRolePolicy(a.SecretsPrefix, t.Namespace, t.ID)
	if err != nil {
		return teamRoleARN, 0, err
	}
	if _, err := c.cfg.IAM.EnsureRole(ctx, RoleSpec{
		Name: teamRole, Path: path, Description: "AFS team " + t.Slug, TrustPolicy: trust,
		PermissionsBoundaryARN: a.PermissionsBoundaryARN, Tags: applied, Policies: map[string]string{"team": policy},
	}); err != nil {
		return teamRoleARN, 0, fmt.Errorf("team role: %w", err)
	}

	bTrust, err := BedrockRoleTrust(a.GatewayRoleARN)
	if err != nil {
		return teamRoleARN, 0, err
	}
	bPolicy, err := BedrockRolePolicy(grants)
	if err != nil {
		return teamRoleARN, 0, err
	}
	if _, err := c.cfg.IAM.EnsureRole(ctx, RoleSpec{
		Name: bedrockRole, Path: path, Description: "AFS gateway access to Bedrock for team " + t.Slug, TrustPolicy: bTrust,
		PermissionsBoundaryARN: a.PermissionsBoundaryARN, Tags: applied, Policies: map[string]string{"bedrock": bPolicy},
	}); err != nil {
		return teamRoleARN, 0, fmt.Errorf("bedrock role: %w", err)
	}
	return teamRoleARN, requeue, nil
}

// reconcileProfiles keeps one application inference profile per callable model and records it as active. A model
// that stopped being callable keeps its profile for ProfileGrace (the row turns retiring, and the profile stays
// in the gateway role's grants), then the profile and its row are deleted; a model that comes back inside the
// grace keeps the same profile. Profiles of the team that no row refers to are deleted. It returns the earliest
// retirement still pending as a requeue delay.
func (c *Controller) reconcileProfiles(ctx context.Context, t Desired, tags map[string]string) ([]ProfileGrant, time.Duration, error) {
	a := c.cfg.AWS
	now := c.cfg.Now()
	var grants []ProfileGrant
	keep := map[string]bool{}
	for _, m := range t.Models {
		name, err := ProfileName(t.ID, m.ID)
		if err != nil {
			return nil, 0, err
		}
		p, err := c.cfg.Profiles.EnsureProfile(ctx, ProfileSpec{Name: name, SourceARN: SourceARN(a.Partition, a.Region, a.AccountID, m.ID), Tags: tags})
		if err != nil {
			return nil, 0, fmt.Errorf("inference profile for %s: %w", m.ID, err)
		}
		keep[p.ARN] = true
		if p.ARN != m.ProfileARN || m.Inactive {
			if err := c.cfg.Source.SetInferenceProfileARN(ctx, t.ID, m.ID, p.ARN); err != nil {
				return nil, 0, fmt.Errorf("record inference profile of %s: %w", m.ID, err)
			}
		}
		grants = append(grants, ProfileGrant{ProfileARN: p.ARN, ModelARN: foundationModelARN(a.Partition, m.ID)})
	}
	var requeue time.Duration
	for _, r := range t.Retiring {
		switch {
		case r.RetireAfter.IsZero():
			after := now.Add(ProfileGrace)
			if err := c.cfg.Source.RetireProfile(ctx, t.ID, r.ID, after); err != nil {
				return nil, 0, fmt.Errorf("retire inference profile of %s: %w", r.ID, err)
			}
			requeue = soonest(requeue, after.Sub(now))
		case !r.RetireAfter.After(now):
			if r.ProfileARN != "" {
				if err := c.cfg.Profiles.DeleteProfile(ctx, r.ProfileARN); err != nil {
					return nil, 0, fmt.Errorf("delete inference profile of %s: %w", r.ID, err)
				}
			}
			if err := c.cfg.Source.DropProfile(ctx, t.ID, r.ID); err != nil {
				return nil, 0, fmt.Errorf("drop inference profile row of %s: %w", r.ID, err)
			}
			continue
		default:
			requeue = soonest(requeue, r.RetireAfter.Sub(now))
		}
		if r.ProfileARN != "" {
			keep[r.ProfileARN] = true
			grants = append(grants, ProfileGrant{ProfileARN: r.ProfileARN, ModelARN: foundationModelARN(a.Partition, r.ID)})
		}
	}
	existing, err := c.cfg.Profiles.ListProfiles(ctx, TagTeamID, t.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("list inference profiles: %w", err)
	}
	for _, p := range existing {
		if keep[p.ARN] {
			continue
		}
		if err := c.cfg.Profiles.DeleteProfile(ctx, p.ARN); err != nil {
			return nil, 0, fmt.Errorf("delete inference profile %s: %w", p.Name, err)
		}
	}
	slices.SortFunc(grants, func(x, y ProfileGrant) int { return strings.Compare(x.ProfileARN, y.ProfileARN) })
	return grants, requeue, nil
}

func soonest(cur, d time.Duration) time.Duration {
	if d < time.Second {
		d = time.Second
	}
	if cur == 0 || d < cur {
		return d
	}
	return cur
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
