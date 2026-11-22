package workorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// DetailType is the EventBridge detail type that asks for a work order (API contract §4.10a).
const DetailType = "afs.work_order.requested"

// Limits of an intake event, matching CreateWorkOrderRequest.
const (
	maxTitle = 200
	maxRepos = 500
)

var (
	cveRE       = regexp.MustCompile(`^CVE-[0-9]{4}-[0-9]{4,}$`)
	repoRE      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	hostRE      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	emailRE     = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	templateRE  = regexp.MustCompile(`^swt_[0-9A-HJKMNP-TV-Z]{26}$`)
	teamIDRE    = regexp.MustCompile(`^team_[0-9A-HJKMNP-TV-Z]{26}$`)
	severitySet = map[string]bool{"critical": true, "high": true, "medium": true, "low": true}
)

// DefaultHost is the repository host when an event names none.
const DefaultHost = "github.com"

// RepoInput is a repository of a requested work order. A repository may name its owner (a team id and/or a
// contact address) or name none, in which case the store resolves it from the teams' AFS_OWNED_REPOS
// configuration value (D38); an explicit owner always wins.
type RepoInput struct {
	Host          string
	Repo          string
	OwningTeamID  string
	OwningContact string
}

// NewWorkOrder is a work order an intake event asks for.
type NewWorkOrder struct {
	SourceEventID string
	Kind          string
	CVEID         string
	Severity      string
	Title         string
	SLADueAt      time.Time
	TemplateID    string
	// Budget is the work order's cap; zero means the template's.
	Budget domain.Money
	Repos  []RepoInput
}

// Rejection is a permanent refusal of an intake event: retrying cannot help, so the message is audited and
// deleted. Reasons: invalid, unowned_repos, ambiguous_repos, open_work_order, template_unavailable,
// unknown_owner.
type Rejection struct {
	Reason string
	Detail map[string]any
}

// Error implements error.
func (r *Rejection) Error() string { return "work order request refused: " + r.Reason }

// CreateResult is the outcome of storing a requested work order.
type CreateResult struct {
	ID string
	// Duplicate is true when a work order already exists for the source event id: nothing was written.
	Duplicate bool
}

// IntakeStore is the database side of intake.
type IntakeStore interface {
	// CreateFromEvent stores the work order and its repositories, resolving owners the caller did not name
	// through afs.resolve_repo_owners, and writes one audit record and the work_order.created event, in one
	// transaction. A source event id seen before is a Duplicate. Permanent refusals are *Rejection.
	CreateFromEvent(ctx context.Context, n NewWorkOrder, at time.Time) (CreateResult, error)
	// Reject records a refused event (audit only); id is the source event id or the queue message id.
	Reject(ctx context.Context, id string, r *Rejection) error
}

// QueueMessage is one message of the intake queue.
type QueueMessage struct {
	ID      string
	Receipt string
	Body    string
}

// Queue is the intake queue.
type Queue interface {
	// Receive returns up to max messages, hiding them from other receivers for a while.
	Receive(ctx context.Context, max int) ([]QueueMessage, error)
	// Delete removes a message that was handled.
	Delete(ctx context.Context, m QueueMessage) error
}

type envelope struct {
	ID         string          `json:"id"`
	DetailType string          `json:"detail-type"`
	Detail     json.RawMessage `json:"detail"`
}

type detail struct {
	SourceEventID string  `json:"sourceEventId"`
	Kind          string  `json:"kind"`
	CVEID         string  `json:"cveId"`
	Severity      string  `json:"severity"`
	Title         string  `json:"title"`
	SLADueAt      string  `json:"slaDueAt"`
	TemplateID    string  `json:"templateId"`
	BudgetUSD     *string `json:"budgetUsd"`
	Repos         []struct {
		Host          string `json:"host"`
		Repo          string `json:"repo"`
		OwningTeamID  string `json:"owningTeamId"`
		OwningContact string `json:"owningContact"`
	} `json:"repos"`
}

// ParseEvent turns an intake message body (an EventBridge event of detail type afs.work_order.requested) into a
// NewWorkOrder. The source event id is detail.sourceEventId, else the envelope's id. A malformed event is a
// *Rejection with reason invalid and the field problems in its detail.
func ParseEvent(body string) (NewWorkOrder, error) {
	bad := func(id string, msg ...string) (NewWorkOrder, error) {
		return NewWorkOrder{SourceEventID: id}, &Rejection{Reason: "invalid", Detail: map[string]any{"problems": msg}}
	}
	var env envelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return bad("", "the message is not an EventBridge event")
	}
	if env.DetailType != DetailType {
		return bad(env.ID, fmt.Sprintf("detail-type is %q, want %q", env.DetailType, DetailType))
	}
	var d detail
	if err := json.Unmarshal(env.Detail, &d); err != nil {
		return bad(env.ID, "detail is not a work order request")
	}
	id := strings.TrimSpace(d.SourceEventID)
	if id == "" {
		id = env.ID
	}
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	if id == "" || len(id) > 200 {
		add("the event has no usable id (detail.sourceEventId or id)")
	}
	n := NewWorkOrder{SourceEventID: id, Kind: d.Kind, CVEID: d.CVEID, Severity: d.Severity, Title: strings.TrimSpace(d.Title), TemplateID: d.TemplateID}
	if n.Kind == "" {
		n.Kind = "cve_remediation"
	}
	switch n.Kind {
	case "cve_remediation":
		if !cveRE.MatchString(n.CVEID) {
			add("cveId must look like CVE-2026-12345")
		}
	case "general":
	default:
		add("kind must be cve_remediation or general")
	}
	if !severitySet[n.Severity] {
		add("severity must be critical, high, medium or low")
	}
	if n.Title == "" || len(n.Title) > maxTitle {
		add("title must hold 1 to %d characters", maxTitle)
	}
	if t, err := time.Parse(time.RFC3339, d.SLADueAt); err != nil {
		add("slaDueAt must be an RFC 3339 time")
	} else {
		n.SLADueAt = t.UTC()
	}
	if !templateRE.MatchString(n.TemplateID) {
		add("templateId must be a swt_ id")
	}
	if d.BudgetUSD != nil {
		m, err := domain.ParseMoney("budgetUsd", *d.BudgetUSD)
		if err != nil || m.Micros() <= 0 {
			add("budgetUsd must be a positive decimal string")
		} else {
			n.Budget = m
		}
	}
	if len(d.Repos) == 0 || len(d.Repos) > maxRepos {
		add("repos must hold 1 to %d repositories", maxRepos)
	}
	seen := map[string]bool{}
	for i, r := range d.Repos {
		host := strings.ToLower(strings.TrimSpace(r.Host))
		if host == "" {
			host = DefaultHost
		}
		in := RepoInput{Host: host, Repo: strings.TrimSpace(r.Repo), OwningTeamID: r.OwningTeamID, OwningContact: strings.TrimSpace(r.OwningContact)}
		switch {
		case !hostRE.MatchString(host):
			add("repos[%d].host is not a host name", i)
		case !repoRE.MatchString(in.Repo):
			add("repos[%d].repo must be owner/name", i)
		case in.OwningTeamID != "" && !teamIDRE.MatchString(in.OwningTeamID):
			add("repos[%d].owningTeamId is not a team id", i)
		case in.OwningContact != "" && (!emailRE.MatchString(in.OwningContact) || len(in.OwningContact) > 254):
			add("repos[%d].owningContact is not an email address", i)
		case seen[strings.ToLower(host+"/"+in.Repo)]:
			add("repos[%d] repeats %s/%s", i, host, in.Repo)
		default:
			seen[strings.ToLower(host+"/"+in.Repo)] = true
		}
		n.Repos = append(n.Repos, in)
	}
	if len(problems) > 0 {
		return NewWorkOrder{SourceEventID: id}, &Rejection{Reason: "invalid", Detail: map[string]any{"problems": problems}}
	}
	return n, nil
}

// Intake drains the intake queue (RC-US-10.1).
type Intake struct {
	Queue Queue
	Store IntakeStore
	Now   Clock
	Log   *slog.Logger
	// Batch is the messages asked for per receive (default 10); MaxMessages bounds one pass (default 100), so a
	// long queue is worked off over several passes without starving the other jobs.
	Batch, MaxMessages int
}

// Run handles the messages that are waiting. A transient failure leaves the message in the queue (it reappears
// after its visibility timeout); a permanent refusal is audited and the message deleted; a duplicate is deleted.
// The errors of failed messages are joined; the next pass retries them.
func (j *Intake) Run(ctx context.Context) error {
	batch, limit := j.Batch, j.MaxMessages
	if batch <= 0 {
		batch = 10
	}
	if limit <= 0 {
		limit = 100
	}
	var errs []error
	for handled := 0; handled < limit; {
		msgs, err := j.Queue.Receive(ctx, batch)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("workorder intake: receive: %w", err))...)
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			handled++
			if err := j.handle(ctx, m); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (j *Intake) handle(ctx context.Context, m QueueMessage) error {
	n, err := ParseEvent(m.Body)
	if err == nil {
		var res CreateResult
		res, err = j.Store.CreateFromEvent(ctx, n, j.Now())
		if err == nil {
			if res.Duplicate {
				j.log().Info("work order request already taken in", "event", n.SourceEventID, "workOrder", res.ID)
			} else {
				j.log().Info("work order created from an event", "event", n.SourceEventID, "workOrder", res.ID)
			}
			return j.delete(ctx, m)
		}
	}
	var rej *Rejection
	if errors.As(err, &rej) {
		id := n.SourceEventID
		if id == "" {
			id = m.ID
		}
		j.log().Warn("work order request refused", "event", id, "reason", rej.Reason, "detail", rej.Detail)
		if rerr := j.Store.Reject(ctx, id, rej); rerr != nil {
			return fmt.Errorf("workorder intake: record refusal of %s: %w", id, rerr)
		}
		return j.delete(ctx, m)
	}
	return fmt.Errorf("workorder intake: message %s: %w", m.ID, err)
}

func (j *Intake) delete(ctx context.Context, m QueueMessage) error {
	if err := j.Queue.Delete(ctx, m); err != nil {
		return fmt.Errorf("workorder intake: delete message %s: %w", m.ID, err)
	}
	return nil
}

func (j *Intake) log() *slog.Logger {
	if j.Log != nil {
		return j.Log
	}
	return slog.Default()
}
