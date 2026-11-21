// Package bedrock holds the Bedrock adapters. This file is the control plane: application inference
// profiles, one per enabled team model, tagged so Cost and Usage Reports attribute spend to the team
// (RC-1, BUD-4). The runtime path (model calls) belongs to afs-gateway.
package bedrock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsbedrock "github.com/aws/aws-sdk-go-v2/service/bedrock"
	"github.com/aws/aws-sdk-go-v2/service/bedrock/types"

	afsaws "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// namePrefix prefixes every profile the reconciler creates; the reconciler only lists and deletes
// profiles carrying it and the managed tag.
const namePrefix = "afs-"

// api is the part of the Bedrock client the adapter uses.
type api interface {
	ListInferenceProfiles(ctx context.Context, in *awsbedrock.ListInferenceProfilesInput, opts ...func(*awsbedrock.Options)) (*awsbedrock.ListInferenceProfilesOutput, error)
	CreateInferenceProfile(ctx context.Context, in *awsbedrock.CreateInferenceProfileInput, opts ...func(*awsbedrock.Options)) (*awsbedrock.CreateInferenceProfileOutput, error)
	DeleteInferenceProfile(ctx context.Context, in *awsbedrock.DeleteInferenceProfileInput, opts ...func(*awsbedrock.Options)) (*awsbedrock.DeleteInferenceProfileOutput, error)
	ListTagsForResource(ctx context.Context, in *awsbedrock.ListTagsForResourceInput, opts ...func(*awsbedrock.Options)) (*awsbedrock.ListTagsForResourceOutput, error)
}

// Control implements team.InferenceProfiles.
type Control struct{ api api }

// NewControl returns a Control on an SDK configuration.
func NewControl(cfg awssdk.Config) *Control { return &Control{api: awsbedrock.NewFromConfig(cfg)} }

var _ team.InferenceProfiles = (*Control)(nil)

// ErrNameNotOurs is returned when asked to create or delete a profile outside the afs- namespace.
var ErrNameNotOurs = errors.New("bedrock: profile name is not in the afs- namespace")

// EnsureProfile implements team.InferenceProfiles.
func (c *Control) EnsureProfile(ctx context.Context, spec team.ProfileSpec) (team.Profile, error) {
	if !strings.HasPrefix(spec.Name, namePrefix) {
		return team.Profile{}, engine.Terminal(fmt.Errorf("%w: %q", ErrNameNotOurs, spec.Name))
	}
	if spec.SourceARN == "" {
		return team.Profile{}, engine.Terminal(errors.New("bedrock: a profile needs a source model ARN"))
	}
	all, err := c.list(ctx)
	if err != nil {
		return team.Profile{}, err
	}
	for _, p := range all {
		if p.Name == spec.Name {
			return p, nil
		}
	}
	tags := map[string]string{team.TagManagedBy: team.ManagedByValue}
	for k, v := range spec.Tags {
		tags[k] = v
	}
	token := sha256.Sum256([]byte(spec.Name))
	out, err := c.api.CreateInferenceProfile(ctx, &awsbedrock.CreateInferenceProfileInput{
		InferenceProfileName: &spec.Name,
		ModelSource:          &types.InferenceProfileModelSourceMemberCopyFrom{Value: spec.SourceARN},
		Tags:                 toTags(tags),
		ClientRequestToken:   awssdk.String(hex.EncodeToString(token[:16])),
	})
	if err != nil {
		return team.Profile{}, afsaws.WrapError(err)
	}
	return team.Profile{Name: spec.Name, ARN: awssdk.ToString(out.InferenceProfileArn), SourceARN: spec.SourceARN}, nil
}

// ListProfiles implements team.InferenceProfiles.
func (c *Control) ListProfiles(ctx context.Context, tagKey, tagValue string) ([]team.Profile, error) {
	all, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []team.Profile
	for _, p := range all {
		tags, err := c.api.ListTagsForResource(ctx, &awsbedrock.ListTagsForResourceInput{ResourceARN: &p.ARN})
		if err != nil {
			return nil, afsaws.WrapError(err)
		}
		managed, match := false, false
		for _, t := range tags.Tags {
			k, v := awssdk.ToString(t.Key), awssdk.ToString(t.Value)
			if k == team.TagManagedBy && v == team.ManagedByValue {
				managed = true
			}
			if k == tagKey && v == tagValue {
				match = true
			}
		}
		if managed && match {
			out = append(out, p)
		}
	}
	return out, nil
}

// DeleteProfile implements team.InferenceProfiles.
func (c *Control) DeleteProfile(ctx context.Context, arn string) error {
	_, err := c.api.DeleteInferenceProfile(ctx, &awsbedrock.DeleteInferenceProfileInput{InferenceProfileIdentifier: &arn})
	if err != nil && !afsaws.IsNotFound(err) {
		return afsaws.WrapError(err)
	}
	return nil
}

// DeleteTeamProfiles deletes every application inference profile the reconciler created for the team
// (lifecycle.InferenceProfiles). A profile already gone is success; every profile is attempted and the
// errors are joined.
func (c *Control) DeleteTeamProfiles(ctx context.Context, teamID string) error {
	profiles, err := c.ListProfiles(ctx, team.TagTeamID, teamID)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range profiles {
		if err := c.DeleteProfile(ctx, p.ARN); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// list returns every application inference profile whose name starts with afs-.
func (c *Control) list(ctx context.Context) ([]team.Profile, error) {
	var out []team.Profile
	var token *string
	for {
		page, err := c.api.ListInferenceProfiles(ctx, &awsbedrock.ListInferenceProfilesInput{
			TypeEquals: types.InferenceProfileTypeApplication, NextToken: token,
		})
		if err != nil {
			return nil, afsaws.WrapError(err)
		}
		for _, s := range page.InferenceProfileSummaries {
			name := awssdk.ToString(s.InferenceProfileName)
			if !strings.HasPrefix(name, namePrefix) {
				continue
			}
			p := team.Profile{Name: name, ARN: awssdk.ToString(s.InferenceProfileArn)}
			if len(s.Models) > 0 {
				p.SourceARN = awssdk.ToString(s.Models[0].ModelArn)
			}
			out = append(out, p)
		}
		if page.NextToken == nil || *page.NextToken == "" {
			return out, nil
		}
		token = page.NextToken
	}
}

func toTags(m map[string]string) []types.Tag {
	out := make([]types.Tag, 0, len(m))
	for k, v := range m {
		out = append(out, types.Tag{Key: awssdk.String(k), Value: awssdk.String(v)})
	}
	return out
}
