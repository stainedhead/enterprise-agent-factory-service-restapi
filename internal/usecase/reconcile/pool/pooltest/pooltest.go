// Package pooltest holds in-memory fakes of the pool controller's ports, for use case tests and for the
// envtest suites that run the real Kubernetes adapter against a fake AWS.
package pooltest

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

// ErrBoom is a generic failure for tests.
var ErrBoom = errors.New("boom")

// Source is an in-memory pool.Source.
type Source struct {
	mu    sync.Mutex
	Pools map[string]pool.Desired
	Err   error
}

// NewSource returns a Source holding pools.
func NewSource(pools ...pool.Desired) *Source {
	s := &Source{Pools: map[string]pool.Desired{}}
	for _, p := range pools {
		s.Pools[p.ID] = p
	}
	return s
}

// Put stores or replaces a pool.
func (s *Source) Put(p pool.Desired) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Pools[p.ID] = p
}

// LoadPool implements pool.Source.
func (s *Source) LoadPool(_ context.Context, id string) (pool.Desired, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return pool.Desired{}, s.Err
	}
	p, ok := s.Pools[id]
	if !ok {
		return pool.Desired{}, domain.NotFound("pool")
	}
	return p, nil
}

// ListTeamPools implements pool.Source.
func (s *Source) ListTeamPools(_ context.Context, teamID string) ([]pool.Listed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return nil, s.Err
	}
	var out []pool.Listed
	for _, p := range s.Pools {
		if p.Team.ID == teamID && p.State != pool.StateDeleted {
			out = append(out, pool.Listed{ID: p.ID, Name: p.Name, State: p.State, Namespace: p.Team.Namespace})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Cluster is a recording pool.Cluster.
type Cluster struct {
	mu      sync.Mutex
	Envs    []pool.Environment
	Deleted []string
	// ApplyErr, when set, is returned by ApplyPool.
	ApplyErr error
	// Changed is returned as the apply result.
	Changed []string
	// State is returned by Workers; the zero value says there is no Deployment.
	State pool.Workers
	// StateErr is returned by Workers.
	StateErr error
}

// ApplyPool implements pool.Cluster.
func (c *Cluster) ApplyPool(_ context.Context, env pool.Environment) (pool.ApplyResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Envs = append(c.Envs, env)
	return pool.ApplyResult{Changed: c.Changed}, c.ApplyErr
}

// Workers implements pool.Cluster.
func (c *Cluster) Workers(context.Context, pool.Desired) (pool.Workers, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.State, c.StateErr
}

// DeletePool implements pool.Cluster.
func (c *Cluster) DeletePool(_ context.Context, d pool.Desired) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Deleted = append(c.Deleted, d.ID)
	return nil
}

// Last returns the last environment applied.
func (c *Cluster) Last() pool.Environment {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.Envs) == 0 {
		return pool.Environment{}
	}
	return c.Envs[len(c.Envs)-1]
}

// Queues is an in-memory pool.Queues: it records every queue's spec and answers with the names, ARNs and
// URLs the SQS adapter would.
type Queues struct {
	mu      sync.Mutex
	Account string
	Region  string
	// Live holds the queues that exist, by name, with the spec of the last Ensure.
	Live    map[string]pool.QueueSpec
	Deleted []string
	// EnsureErr and DeleteErr are returned by Ensure and Delete.
	EnsureErr, DeleteErr error
}

// NewQueues returns an empty fake in account 111122223333, us-east-1.
func NewQueues() *Queues {
	return &Queues{Account: "111122223333", Region: "us-east-1", Live: map[string]pool.QueueSpec{}}
}

// Locate implements pool.Queues.
func (q *Queues) Locate(name string) pool.Queue {
	return pool.Queue{Name: name,
		ARN: "arn:aws:sqs:" + q.Region + ":" + q.Account + ":" + name,
		URL: "https://sqs." + q.Region + ".amazonaws.com/" + q.Account + "/" + name}
}

// Ensure implements pool.Queues.
func (q *Queues) Ensure(_ context.Context, spec pool.QueueSpec) (pool.Queue, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.EnsureErr != nil {
		return pool.Queue{}, q.EnsureErr
	}
	q.Live[spec.Name] = spec
	return q.Locate(spec.Name), nil
}

// Delete implements pool.Queues.
func (q *Queues) Delete(_ context.Context, name string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.DeleteErr != nil {
		return q.DeleteErr
	}
	delete(q.Live, name)
	q.Deleted = append(q.Deleted, name)
	return nil
}

// Names lists the queues that exist.
func (q *Queues) Names() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for n := range q.Live {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Recorder is a recording pool.LinkRecorder.
type Recorder struct {
	mu      sync.Mutex
	Changes []pool.LinkChange
	Err     error
}

// RecordLink implements pool.LinkRecorder.
func (r *Recorder) RecordLink(_ context.Context, c pool.LinkChange) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return r.Err
	}
	r.Changes = append(r.Changes, c)
	return nil
}

// Probe is a links.Probe that records the requests and fails for the target roles named in Fail.
type Probe struct {
	mu       sync.Mutex
	Requests []links.VerifyRequest
	// Fail maps a target role ARN to the error its verification returns.
	Fail map[string]error
}

// Verify implements links.Probe.
func (p *Probe) Verify(_ context.Context, r links.VerifyRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Requests = append(p.Requests, r)
	return p.Fail[r.TargetRoleARN]
}

// CodedError is an error with an AWS-style code.
type CodedError struct{ Code, Msg string }

// Error implements error.
func (e CodedError) Error() string { return e.Msg }

// ErrorCode returns the code.
func (e CodedError) ErrorCode() string { return strings.TrimSpace(e.Code) }
