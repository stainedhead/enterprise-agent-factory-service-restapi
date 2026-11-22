// Package secrets implements the secret operations of API contract §4.6 (RA-5). A secret's value
// is sent once, passed to Secrets Manager and dropped: the database holds the ARN and metadata
// only, and no response, error, log line, audit record or event carries the value.
package secrets

import (
	"context"
	"errors"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindSecret = "secrets"
	// RecoveryDays is the recovery window of a deleted secret (SEC-1).
	RecoveryDays = 30
)

// ErrNameTaken is returned by a Writer when Secrets Manager already holds a secret of that name.
var ErrNameTaken = errors.New("secrets: name taken in Secrets Manager")

// Created is a secret that now exists in Secrets Manager.
type Created struct {
	ARN       string
	VersionID string
}

// Writer is the Secrets Manager write path, the only AWS call afs-api makes.
type Writer interface {
	// Create makes the secret name with value and tags. token makes a retry idempotent.
	Create(ctx context.Context, name string, value domain.SecretValue, token string, tags map[string]string) (Created, error)
	// PutValue writes a new version and returns its id.
	PutValue(ctx context.Context, arn string, value domain.SecretValue, token string) (string, error)
	// ForceDelete removes a secret created a moment ago, without a recovery window.
	ForceDelete(ctx context.Context, arn string) error
	// TagOrphan tags the secret afs-orphan=true for the reconciler's sweep.
	TagOrphan(ctx context.Context, arn string) error
	// ScheduleDeletion schedules deletion with a recovery window in days.
	ScheduleDeletion(ctx context.Context, arn string, days int) error
}

// Repository is the secret metadata store.
type Repository interface {
	// List returns the team's secrets with id greater than after, in id order, with BoundTo set.
	List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Secret, error)
	// Get returns one secret of the team with BoundTo set; NotFound otherwise. forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Secret, error)
	// NameTaken reports whether the team already has a secret called name.
	NameTaken(ctx context.Context, tx port.Tx, teamID, name string) (bool, error)
	// Path returns afs.secret_path for the scope; an unknown agent or pool is a ValidationError.
	Path(ctx context.Context, tx port.Tx, teamID, agentID, poolID, name string) (string, error)
	// Insert stores the metadata row; the database derives the path and checks it against the ARN.
	Insert(ctx context.Context, tx port.Tx, s domain.Secret) error
	// Update stores description, external system and rotation; PreconditionFailed when the version
	// is not expect.
	Update(ctx context.Context, tx port.Tx, s domain.Secret, expect domain.Version) error
	// SetCurrentVersion records the new Secrets Manager version.
	SetCurrentVersion(ctx context.Context, tx port.Tx, teamID, id, versionID string, at time.Time) error
	// Delete removes the row.
	Delete(ctx context.Context, tx port.Tx, teamID, id string) error
}

// Service is the secret use case set.
type Service struct {
	acting.Deps
	Repo   Repository
	Writer Writer
	// OnOrphan is called when a compensating delete failed and the secret was left for the sweep
	// (afs_api_orphan_secrets_total). Optional.
	OnOrphan func()
}

// Result is a secret with the replayed response of an idempotent call.
type Result struct {
	Secret domain.Secret
	Replay *port.StoredResponse
}

func audit(s domain.Secret) map[string]any {
	return map[string]any{"scope": s.Scope, "agentId": s.AgentID, "poolId": s.PoolID, "name": s.Name,
		"description": s.Description, "externalSystemId": s.ExternalSystemID, "path": s.Path,
		"rotationEnabled": s.Rotation.Enabled, "rotationDays": s.Rotation.Days, "versionId": s.VersionID}
}

func unavailable(err error) error {
	e := domain.Unavailable("Secrets Manager", err)
	e.RetryAfterSeconds = 1
	return e
}

// List implements ListSecrets.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.Secret, string, error) {
	p = p.Normalized()
	var rows []domain.Secret
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(x domain.Secret) string { return x.ID })
	return page, next, nil
}

// Get implements GetSecret.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID, id string) (domain.Secret, error) {
	var out domain.Secret
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.Get(ctx, tx, teamID, id, false)
		return err
	})
	return out, err
}

func idemPart(c acting.Caller, idem *acting.Idem) string {
	if idem != nil {
		return "idem:" + idem.Key.Key
	}
	return "req:" + c.RequestID
}

// Create implements CreateSecret. Secrets Manager is written first, under a ClientRequestToken
// derived from the idempotency key (or the request id), so a retry does not create a second
// secret; then the ARN row, the audit record and secret.created commit together. When the
// database fails after Secrets Manager succeeded the new secret is deleted at once; if that
// fails too it is tagged afs-orphan=true for the sweep and OnOrphan is called.
func (s *Service) Create(ctx context.Context, c acting.Caller, teamID string, in domain.NewSecretParams, idem *acting.Idem) (Result, error) {
	if err := in.Validate(); err != nil {
		return Result{}, err
	}
	var (
		res     Result
		created Created
	)
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if taken, err := s.Repo.NameTaken(ctx, tx, teamID, in.Name); err != nil {
			return err
		} else if taken {
			return domain.Conflict("duplicate", "the team already has a secret called "+in.Name, nil)
		}
		path, err := s.Repo.Path(ctx, tx, teamID, in.AgentID, in.PoolID, in.Name)
		if err != nil {
			return err
		}
		tags := map[string]string{"afs-team-id": teamID}
		if in.AgentID != "" {
			tags["afs-agent-id"] = in.AgentID
		}
		cr, err := s.Writer.Create(ctx, path, in.Value, secretToken(c, idem, "CreateSecret", teamID, path), tags)
		if errors.Is(err, ErrNameTaken) {
			return domain.Conflict("duplicate", "Secrets Manager already holds a secret of that name", nil)
		}
		if err != nil {
			return unavailable(err)
		}
		created = cr
		id, err := s.IDs.NewID(domain.PrefixSecret)
		if err != nil {
			return err
		}
		now := s.Now()
		sec := domain.Secret{ID: id.String(), TeamID: teamID, Scope: in.Scope, AgentID: in.AgentID, PoolID: in.PoolID,
			Name: in.Name, Description: in.Description, ExternalSystemID: in.ExternalSystemID, Path: path, ARN: cr.ARN,
			VersionID: cr.VersionID, LastChangedAt: &now, Rotation: in.Rotation}
		if err := s.Repo.Insert(ctx, tx, sec); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateSecret", kindSecret, sec.ID, teamID, nil, audit(sec))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventSecretCreated, teamID, kindSecret, sec.ID,
			map[string]any{"secretId": sec.ID, "name": sec.Name, "scope": string(sec.Scope)}); err != nil {
			return err
		}
		if res.Secret, err = s.Repo.Get(ctx, tx, teamID, sec.ID, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Secret)
	})
	if err != nil && created.ARN != "" {
		s.compensate(ctx, created.ARN)
	}
	return res, err
}

func secretToken(c acting.Caller, idem *acting.Idem, op string, parts ...string) string {
	return tokenOf(append([]string{c.Principal.ID, idemPart(c, idem), op}, parts...)...)
}

func (s *Service) compensate(ctx context.Context, arn string) {
	ctx = context.WithoutCancel(ctx)
	if err := s.Writer.ForceDelete(ctx, arn); err == nil {
		return
	}
	_ = s.Writer.TagOrphan(ctx, arn)
	if s.OnOrphan != nil {
		s.OnOrphan()
	}
}

// Update implements UpdateSecret (description, external system, rotation; If-Match required).
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version, p domain.SecretPatch) (domain.Secret, error) {
	if err := p.Validate(); err != nil {
		return domain.Secret{}, err
	}
	var out domain.Secret
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next := p.Apply(cur)
		if err := s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("UpdateSecret", kindSecret, id, teamID, audit(cur), audit(next))); err != nil {
			return err
		}
		out, err = s.Repo.Get(ctx, tx, teamID, id, false)
		return err
	})
	return out, err
}

// Delete implements DeleteSecret. A bound secret is a Conflict listing the agents. After the
// commit the Secrets Manager secret is scheduled for deletion with the recovery window; if that
// call fails the reconciler's orphan sweep does it.
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) error {
	var arn string
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if len(cur.BoundTo) > 0 {
			return domain.SecretBound(cur.BoundTo)
		}
		if err := s.Repo.Delete(ctx, tx, teamID, id); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("DeleteSecret", kindSecret, id, teamID, audit(cur), nil)); err != nil {
			return err
		}
		arn = cur.ARN
		return s.Emit(ctx, tx, c, domain.EventSecretDeleted, teamID, kindSecret, id,
			map[string]any{"secretId": id, "name": cur.Name})
	})
	if err == nil && arn != "" {
		_ = s.Writer.ScheduleDeletion(context.WithoutCancel(ctx), arn, RecoveryDays)
	}
	return err
}

// PutValue implements PutSecretValue: a new Secrets Manager version, then the version id and
// secret.rotated. expect is nil when If-Match is absent (PUT upsert, CD-2).
func (s *Service) PutValue(ctx context.Context, c acting.Caller, teamID, id string, expect *domain.Version, value domain.SecretValue) error {
	_, err := s.newVersion(ctx, c, teamID, id, expect, value, "PutSecretValue", nil)
	return err
}

// Rotate implements RotateSecret: it writes a new version with value and returns the secret.
func (s *Service) Rotate(ctx context.Context, c acting.Caller, teamID, id string, value domain.SecretValue, idem *acting.Idem) (Result, error) {
	return s.newVersion(ctx, c, teamID, id, nil, value, "RotateSecret", idem)
}

func (s *Service) newVersion(ctx context.Context, c acting.Caller, teamID, id string, expect *domain.Version, value domain.SecretValue, action string, idem *acting.Idem) (Result, error) {
	if value.IsZero() {
		return Result{}, domain.Validation(domain.FieldError{Field: "value", Reason: "is required"})
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		vid, err := s.Writer.PutValue(ctx, cur.ARN, value, secretToken(c, idem, action, teamID, id))
		if err != nil {
			return unavailable(err)
		}
		if err := s.Repo.SetCurrentVersion(ctx, tx, teamID, id, vid, s.Now()); err != nil {
			return err
		}
		next := cur
		next.VersionID = vid
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindSecret, id, teamID, audit(cur), audit(next))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventSecretRotated, teamID, kindSecret, id,
			map[string]any{"secretId": id, "name": cur.Name, "versionId": vid}); err != nil {
			return err
		}
		if res.Secret, err = s.Repo.Get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Secret)
	})
	return res, err
}
