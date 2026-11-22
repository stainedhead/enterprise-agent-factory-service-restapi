// Package agenttest holds in-memory fakes of the agent controller's ports, for use case tests and for the
// envtest suites that run the real Kubernetes adapter against a fake AWS.
package agenttest

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Source is an in-memory agent.Source.
type Source struct {
	mu     sync.Mutex
	Agents map[string]agent.Desired
	Err    error
}

// NewSource returns a Source holding agents.
func NewSource(agents ...agent.Desired) *Source {
	s := &Source{Agents: map[string]agent.Desired{}}
	for _, a := range agents {
		s.Agents[a.ID] = a
	}
	return s
}

// Put stores or replaces an agent.
func (s *Source) Put(a agent.Desired) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Agents[a.ID] = a
}

// LoadAgent implements agent.Source.
func (s *Source) LoadAgent(_ context.Context, id string) (agent.Desired, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return agent.Desired{}, s.Err
	}
	a, ok := s.Agents[id]
	if !ok {
		return agent.Desired{}, domain.NotFound("agent")
	}
	return a, nil
}

// ListTeamAgents implements agent.Source.
func (s *Source) ListTeamAgents(_ context.Context, teamID string) ([]agent.Listed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return nil, s.Err
	}
	var out []agent.Listed
	for _, a := range s.Agents {
		if a.Team.ID == teamID && a.State != agent.StateDeleted {
			out = append(out, agent.Listed{ID: a.ID, Name: a.Name, State: a.State, Namespace: a.Team.Namespace})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Cluster is a recording agent.Cluster.
type Cluster struct {
	mu      sync.Mutex
	Envs    []agent.Environment
	Deleted []string
	// ApplyErr, when set, is returned by ApplyAgent.
	ApplyErr error
	// Changed is returned as the apply result.
	Changed []string
}

// ApplyAgent implements agent.Cluster.
func (c *Cluster) ApplyAgent(_ context.Context, env agent.Environment) (agent.ApplyResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Envs = append(c.Envs, env)
	return agent.ApplyResult{Changed: c.Changed}, c.ApplyErr
}

// DeleteAgent implements agent.Cluster.
func (c *Cluster) DeleteAgent(_ context.Context, d agent.Desired) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Deleted = append(c.Deleted, d.ID)
	return nil
}

// Last returns the last environment applied.
func (c *Cluster) Last() agent.Environment {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Envs) == 0 {
		return agent.Environment{}
	}
	return c.Envs[len(c.Envs)-1]
}

// PodIdentities is a recording agent.PodIdentities.
type PodIdentities struct {
	mu      sync.Mutex
	Assoc   map[string]string // "namespace/serviceAccount" -> role ARN
	Tags    map[string]map[string]string
	Err     error
	Deleted []string
}

// NewPodIdentities returns an empty fake.
func NewPodIdentities() *PodIdentities {
	return &PodIdentities{Assoc: map[string]string{}, Tags: map[string]map[string]string{}}
}

// Ensure implements agent.PodIdentities.
func (p *PodIdentities) Ensure(_ context.Context, ns, sa, roleARN string, tags map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return p.Err
	}
	p.Assoc[ns+"/"+sa] = roleARN
	p.Tags[ns+"/"+sa] = tags
	return nil
}

// Delete implements agent.PodIdentities.
func (p *PodIdentities) Delete(_ context.Context, ns, sa string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return p.Err
	}
	delete(p.Assoc, ns+"/"+sa)
	p.Deleted = append(p.Deleted, ns+"/"+sa)
	return nil
}

// Volumes is a fake agent.Volumes.
type Volumes struct {
	mu       sync.Mutex
	States   map[string]agent.VolumeState // by agent id
	Expanded []string
	Restored map[string]string // agent id -> snapshot id
	Err      error
}

// NewVolumes returns an empty fake.
func NewVolumes() *Volumes {
	return &Volumes{States: map[string]agent.VolumeState{}, Restored: map[string]string{}}
}

// Volume implements agent.Volumes.
func (v *Volumes) Volume(_ context.Context, d agent.Desired) (agent.VolumeState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.States[d.ID], v.Err
}

// Expand implements agent.Volumes.
func (v *Volumes) Expand(_ context.Context, d agent.Desired) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.Err != nil {
		return false, v.Err
	}
	st := v.States[d.ID]
	st.RequestedGiB = d.Volume.SizeGiB
	st.Resizing = true
	v.States[d.ID] = st
	v.Expanded = append(v.Expanded, d.ID)
	return true, nil
}

// Restore implements agent.Volumes.
func (v *Volumes) Restore(_ context.Context, d agent.Desired, snap string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.Err != nil {
		return v.Err
	}
	v.Restored[d.ID] = snap
	v.States[d.ID] = agent.VolumeState{Exists: true, RequestedGiB: d.Volume.SizeGiB}
	return nil
}

// Snapshots is a fake agent.Snapshots.
type Snapshots struct {
	Latest map[string]string
	Err    error
}

// LatestReady implements agent.Snapshots.
func (s *Snapshots) LatestReady(_ context.Context, agentID string) (string, bool, error) {
	if s.Err != nil {
		return "", false, s.Err
	}
	id, ok := s.Latest[agentID]
	return id, ok, nil
}

// ErrBoom is a generic failure for tests.
var ErrBoom = errors.New("boom")
