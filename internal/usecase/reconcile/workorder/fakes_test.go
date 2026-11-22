package workorder_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return t0 }

type fakeQueue struct {
	mu      sync.Mutex
	msgs    []workorder.QueueMessage
	deleted []string
	recvErr error
	delErr  error
}

func (q *fakeQueue) Receive(_ context.Context, n int) ([]workorder.QueueMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.recvErr != nil {
		return nil, q.recvErr
	}
	n = min(n, len(q.msgs))
	out := append([]workorder.QueueMessage(nil), q.msgs[:n]...)
	q.msgs = q.msgs[n:]
	return out, nil
}

func (q *fakeQueue) Delete(_ context.Context, m workorder.QueueMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.delErr != nil {
		return q.delErr
	}
	q.deleted = append(q.deleted, m.ID)
	return nil
}

type fakeIntakeStore struct {
	byEvent  map[string]string
	created  []workorder.NewWorkOrder
	rejected map[string]*workorder.Rejection
	err      error
	reject   *workorder.Rejection
}

func (s *fakeIntakeStore) CreateFromEvent(_ context.Context, n workorder.NewWorkOrder, _ time.Time) (workorder.CreateResult, error) {
	if s.err != nil {
		return workorder.CreateResult{}, s.err
	}
	if s.reject != nil {
		return workorder.CreateResult{}, s.reject
	}
	if s.byEvent == nil {
		s.byEvent = map[string]string{}
	}
	if id, ok := s.byEvent[n.SourceEventID]; ok {
		return workorder.CreateResult{ID: id, Duplicate: true}, nil
	}
	id := "wo_" + n.SourceEventID
	s.byEvent[n.SourceEventID] = id
	s.created = append(s.created, n)
	return workorder.CreateResult{ID: id}, nil
}

func (s *fakeIntakeStore) Reject(_ context.Context, id string, r *workorder.Rejection) error {
	if s.rejected == nil {
		s.rejected = map[string]*workorder.Rejection{}
	}
	s.rejected[id] = r
	return nil
}

type fakeAssignStore struct {
	ids      []string
	teams    map[string]string
	failures map[string]error
	calls    map[string]int
}

func (s *fakeAssignStore) Unassigned(context.Context) ([]string, error) {
	var out []string
	for _, id := range s.ids {
		if _, done := s.teams[id]; !done {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *fakeAssignStore) Instantiate(_ context.Context, id string) (string, error) {
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[id]++
	if err := s.failures[id]; err != nil {
		return "", err
	}
	if s.teams == nil {
		s.teams = map[string]string{}
	}
	s.teams[id] = "team_for_" + id
	return s.teams[id], nil
}

type fakeSecretStore struct {
	pending   []workorder.TemplateSecret
	recorded  []workorder.TemplateSecret
	arns      []string
	recordErr error
}

func (s *fakeSecretStore) Pending(context.Context) ([]workorder.TemplateSecret, error) {
	return s.pending, nil
}
func (s *fakeSecretStore) Record(_ context.Context, t workorder.TemplateSecret, arn string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.recorded = append(s.recorded, t)
	s.arns = append(s.arns, arn)
	return nil
}

type fakeCreator struct {
	created []string
	tags    []map[string]string
	err     error
}

func (c *fakeCreator) CreateEmpty(_ context.Context, path string, tags map[string]string) (string, error) {
	if c.err != nil {
		return "", c.err
	}
	c.created = append(c.created, path)
	c.tags = append(c.tags, tags)
	return "arn:aws:secretsmanager:us-east-1:111122223333:secret:" + path + "-AbCdEf", nil
}

type fakeGitHub struct {
	states map[string]workorder.PRState
	errs   map[string]error
	calls  []string
}

func (g *fakeGitHub) PullRequest(_ context.Context, ref workorder.PRRef) (workorder.PRState, error) {
	g.calls = append(g.calls, ref.String())
	if err := g.errs[ref.String()]; err != nil {
		return workorder.PRState{}, err
	}
	st, ok := g.states[ref.String()]
	if !ok {
		return workorder.PRState{}, errors.New("unexpected pull request " + ref.String())
	}
	return st, nil
}

type backoffErr struct {
	at    time.Time
	scope string
}

func (e *backoffErr) Error() string      { return "rate limited" }
func (e *backoffErr) RetryAt() time.Time { return e.at }
func (e *backoffErr) Scope() string      { return e.scope }

type fakeTrackStore struct {
	active       []workorder.WorkOrder
	repoChanges  []workorder.RepoChange
	statusChange []workorder.StatusChange
	stale        bool // ApplyRepo/ApplyStatus report false: the row changed meanwhile
}

func (s *fakeTrackStore) Active(context.Context) ([]workorder.WorkOrder, error) {
	out := make([]workorder.WorkOrder, len(s.active))
	for i, w := range s.active {
		w.Repos = append([]workorder.Repo(nil), w.Repos...)
		out[i] = w
	}
	return out, nil
}

func (s *fakeTrackStore) ApplyRepo(_ context.Context, c workorder.RepoChange) (bool, error) {
	if s.stale {
		return false, nil
	}
	s.repoChanges = append(s.repoChanges, c)
	for i := range s.active {
		if s.active[i].ID != c.WorkOrderID {
			continue
		}
		for j := range s.active[i].Repos {
			if s.active[i].Repos[j].Host == c.Host && s.active[i].Repos[j].Repo == c.Repo {
				s.active[i].Repos[j].Status = c.To
			}
		}
	}
	return true, nil
}

func (s *fakeTrackStore) ApplyStatus(_ context.Context, c workorder.StatusChange) (bool, error) {
	if s.stale {
		return false, nil
	}
	s.statusChange = append(s.statusChange, c)
	for i := range s.active {
		if s.active[i].ID == c.WorkOrderID {
			s.active[i].Status = c.To
		}
	}
	return true, nil
}

type fakeNotifyStore struct {
	sent     map[string]map[workorder.NotificationKey]bool
	channel  string
	teams    []workorder.Notification
	contacts []workorder.Notification
	events   []workorder.Notification
	active   []workorder.WorkOrder
}

func (s *fakeNotifyStore) Active(context.Context) ([]workorder.WorkOrder, error) {
	return s.active, nil
}
func (s *fakeNotifyStore) Sent(context.Context, []string) (map[string]map[workorder.NotificationKey]bool, error) {
	return s.sent, nil
}
func (s *fakeNotifyStore) NotificationChannel(context.Context) (string, error) { return s.channel, nil }

func (s *fakeNotifyStore) mark(n workorder.Notification) bool {
	if s.sent == nil {
		s.sent = map[string]map[workorder.NotificationKey]bool{}
	}
	if s.sent[n.WorkOrderID] == nil {
		s.sent[n.WorkOrderID] = map[workorder.NotificationKey]bool{}
	}
	if s.sent[n.WorkOrderID][n.NotificationKey] {
		return false
	}
	s.sent[n.WorkOrderID][n.NotificationKey] = true
	return true
}

func (s *fakeNotifyStore) RecordTeam(_ context.Context, n workorder.Notification) (bool, error) {
	ok := s.mark(n)
	if ok {
		s.teams = append(s.teams, n)
	}
	return ok, nil
}

func (s *fakeNotifyStore) RecordContact(_ context.Context, n workorder.Notification) (bool, error) {
	ok := s.mark(n)
	if ok {
		s.contacts = append(s.contacts, n)
	}
	return ok, nil
}

func (s *fakeNotifyStore) RecordEvent(_ context.Context, n workorder.Notification) (bool, error) {
	ok := s.mark(n)
	if ok {
		s.events = append(s.events, n)
	}
	return ok, nil
}

type fakeMailer struct {
	sent []mail
	err  error
}

type mail struct{ channel, to, subject, body string }

func (m *fakeMailer) Send(_ context.Context, channel, to, subject, body string) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, mail{channel, to, subject, body})
	return nil
}
