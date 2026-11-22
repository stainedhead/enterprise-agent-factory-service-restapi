// Package teamtest holds in-memory fakes of the team controller's ports, for use case tests and for the
// envtest suites that run the real Kubernetes adapter against a fake AWS.
package teamtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Source is an in-memory team.Source.
type Source struct {
	mu    sync.Mutex
	Teams map[string]team.Desired
	// SetARNErr, when set, is returned by SetInferenceProfileARN.
	SetARNErr error
	// ARNs records the profile ARNs written, by "teamID/modelID".
	ARNs map[string]string
}

// NewSource returns a Source holding teams.
func NewSource(teams ...team.Desired) *Source {
	s := &Source{Teams: map[string]team.Desired{}, ARNs: map[string]string{}}
	for _, t := range teams {
		s.Teams[t.ID] = t
	}
	return s
}

// LoadTeam implements team.Source.
func (s *Source) LoadTeam(_ context.Context, id string) (team.Desired, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.Teams[id]
	if !ok {
		return team.Desired{}, domain.NotFound("team")
	}
	t.Models = append([]team.Model(nil), t.Models...)
	for i, m := range t.Models {
		if arn, ok := s.ARNs[id+"/"+m.ID]; ok {
			t.Models[i].ProfileARN = arn
		}
	}
	return t, nil
}

// SetInferenceProfileARN implements team.Source.
func (s *Source) SetInferenceProfileARN(_ context.Context, teamID, modelID, arn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SetARNErr != nil {
		return s.SetARNErr
	}
	s.ARNs[teamID+"/"+modelID] = arn
	return nil
}

// Put stores or replaces a team.
func (s *Source) Put(t team.Desired) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Teams[t.ID] = t
}

// Cluster is a recording team.Cluster and team.PlatformCluster.
type Cluster struct {
	mu        sync.Mutex
	Envs      []team.Environment
	Platforms []team.PlatformSpec
	// Err, when set, is returned by ApplyTeam (and the result still lists Changed).
	Err error
	// Changed is returned as the apply result.
	Changed []string
}

// ApplyTeam implements team.Cluster.
func (c *Cluster) ApplyTeam(_ context.Context, env team.Environment) (team.ApplyResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Envs = append(c.Envs, env)
	return team.ApplyResult{Changed: c.Changed}, c.Err
}

// ApplyPlatform implements team.PlatformCluster.
func (c *Cluster) ApplyPlatform(_ context.Context, p team.PlatformSpec) (team.ApplyResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Platforms = append(c.Platforms, p)
	return team.ApplyResult{Changed: c.Changed}, c.Err
}

// IAM is an in-memory team.IAM that records calls.
type IAM struct {
	mu    sync.Mutex
	Roles map[string]team.RoleSpec // by name
	Calls []string
	// Account and Partition build ARNs (defaults 111122223333 and aws).
	Account, Partition string
	// Err, when set, is returned by EnsureRole for the named role ("" = every role).
	Err     error
	ErrRole string
}

// NewIAM returns an empty IAM.
func NewIAM() *IAM {
	return &IAM{Roles: map[string]team.RoleSpec{}, Account: "111122223333", Partition: "aws"}
}

// EnsureRole implements team.IAM.
func (i *IAM) EnsureRole(_ context.Context, spec team.RoleSpec) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Calls = append(i.Calls, "EnsureRole "+spec.Name)
	if i.Err != nil && (i.ErrRole == "" || i.ErrRole == spec.Name) {
		return "", i.Err
	}
	i.Roles[spec.Name] = spec
	return team.RoleARN(i.Partition, i.Account, spec.Path, spec.Name), nil
}

// DeleteRole implements team.IAM.
func (i *IAM) DeleteRole(_ context.Context, _, name string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Calls = append(i.Calls, "DeleteRole "+name)
	delete(i.Roles, name)
	return nil
}

// Role returns the stored spec of a role.
func (i *IAM) Role(name string) (team.RoleSpec, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	r, ok := i.Roles[name]
	return r, ok
}

// Profiles is an in-memory team.InferenceProfiles.
type Profiles struct {
	mu       sync.Mutex
	ByName   map[string]team.Profile
	Tags     map[string]map[string]string // by ARN
	Calls    []string
	Region   string
	EnsureEr error
	ListErr  error
	DeleteEr error
}

// NewProfiles returns an empty Profiles.
func NewProfiles() *Profiles {
	return &Profiles{ByName: map[string]team.Profile{}, Tags: map[string]map[string]string{}, Region: "us-east-1"}
}

// EnsureProfile implements team.InferenceProfiles.
func (p *Profiles) EnsureProfile(_ context.Context, spec team.ProfileSpec) (team.Profile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "Ensure "+spec.Name)
	if p.EnsureEr != nil {
		return team.Profile{}, p.EnsureEr
	}
	if existing, ok := p.ByName[spec.Name]; ok {
		return existing, nil
	}
	pr := team.Profile{Name: spec.Name, SourceARN: spec.SourceARN,
		ARN: fmt.Sprintf("arn:aws:bedrock:%s:111122223333:application-inference-profile/%s", p.Region, spec.Name)}
	p.ByName[spec.Name] = pr
	tags := map[string]string{team.TagManagedBy: team.ManagedByValue}
	for k, v := range spec.Tags {
		tags[k] = v
	}
	p.Tags[pr.ARN] = tags
	return pr, nil
}

// ListProfiles implements team.InferenceProfiles.
func (p *Profiles) ListProfiles(_ context.Context, key, value string) ([]team.Profile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ListErr != nil {
		return nil, p.ListErr
	}
	var out []team.Profile
	for _, pr := range p.ByName {
		if p.Tags[pr.ARN][key] == value {
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteProfile implements team.InferenceProfiles.
func (p *Profiles) DeleteProfile(_ context.Context, arn string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Calls = append(p.Calls, "Delete "+arn)
	if p.DeleteEr != nil {
		return p.DeleteEr
	}
	for n, pr := range p.ByName {
		if pr.ARN == arn {
			delete(p.ByName, n)
			delete(p.Tags, arn)
		}
	}
	return nil
}

// ErrBoom is a generic failure for tests.
var ErrBoom = errors.New("boom")
