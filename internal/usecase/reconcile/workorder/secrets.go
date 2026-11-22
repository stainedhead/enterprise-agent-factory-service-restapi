package workorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// TemplateSecret is a secret a swarm template declares for one of its pools (spec.secrets entry
// {pool, name, description, delivery, target}) that the remediation team does not have yet.
//
// -db's afs.instantiate_work_order_team does not create these (a secrets row needs the ARN Secrets Manager
// assigns, and afs_reconciler may not insert secrets or pool_secret_bindings). Secrets creates the secret in
// Secrets Manager with no value, at the path afs.secret_path gives, and records it through the -db function
// afs.record_work_order_secret (see postgres.WorkOrderStore for the SQL). A value is never copied from anywhere:
// the team puts it with PutSecretValue, like any managed secret.
type TemplateSecret struct {
	WorkOrderID string
	TeamID      string
	PoolID      string
	Pool        string
	Name        string
	Description string
	// Delivery is env or file; Target is the environment variable or the file path.
	Delivery string
	Target   string
	// Path is the Secrets Manager name (afs.secret_path).
	Path string
}

var (
	secretNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envTargetRE  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	fileTargetRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)
)

// Normalize fills the defaults (delivery env, target the upper-case name with underscores) and reports why the
// entry cannot be used, or "".
func (s *TemplateSecret) Normalize() string {
	if s.Delivery == "" {
		s.Delivery = "env"
	}
	if s.Target == "" && s.Delivery == "env" {
		s.Target = strings.ToUpper(strings.ReplaceAll(s.Name, "-", "_"))
	}
	switch {
	case !secretNameRE.MatchString(s.Name):
		return "the secret name must be lower-case letters, digits and hyphens, starting with a letter"
	case s.Delivery == "env" && !envTargetRE.MatchString(s.Target):
		return "the env target is not a valid variable name"
	case s.Delivery == "file" && !fileTargetRE.MatchString(s.Target):
		return "the file target must be an absolute path"
	case s.Delivery != "env" && s.Delivery != "file":
		return "delivery must be env or file"
	}
	return ""
}

// SecretStore is the database side of secret provisioning.
type SecretStore interface {
	// Supported reports whether the -db function that records a secret exists. When it does not, nothing is
	// created in Secrets Manager (it would be an orphan).
	Supported(ctx context.Context) (bool, error)
	// Pending lists the template secrets of work orders that are assigned, in progress or have PRs open whose
	// team has no secret of that name yet.
	Pending(ctx context.Context) ([]TemplateSecret, error)
	// Record writes the secrets row (managed, scoped to the pool), the pool binding, the audit record and the
	// secret.created event, in one transaction. Recording a secret that exists is a no-op.
	Record(ctx context.Context, s TemplateSecret, arn string) error
}

// SecretCreator creates empty secrets.
type SecretCreator interface {
	// CreateEmpty creates the secret at path with no value and the tags, and returns its ARN. A secret that
	// exists at the path is not an error: its ARN is returned.
	CreateEmpty(ctx context.Context, path string, tags map[string]string) (arn string, err error)
}

// Secrets provisions the secrets that swarm templates declare.
type Secrets struct {
	Store   SecretStore
	Creator SecretCreator
	Log     *slog.Logger

	warned bool
}

// Run creates and records every pending secret. An entry that cannot be used is logged and skipped. A failure for
// one secret does not stop the others.
func (s *Secrets) Run(ctx context.Context) error {
	ok, err := s.Store.Supported(ctx)
	if err != nil {
		return fmt.Errorf("workorder secrets: %w", err)
	}
	if !ok {
		if !s.warned {
			s.log().Warn("swarm template secrets are not created: the database lacks afs.record_work_order_secret (a -db change, see the M6-E notes)")
			s.warned = true
		}
		return nil
	}
	pending, err := s.Store.Pending(ctx)
	if err != nil {
		return fmt.Errorf("workorder secrets: list: %w", err)
	}
	var errs []error
	seen := map[string]bool{}
	for _, p := range pending {
		if why := p.Normalize(); why != "" {
			s.log().Warn("a swarm template secret is skipped", "workOrder", p.WorkOrderID, "name", p.Name, "reason", why)
			continue
		}
		key := p.TeamID + "/" + p.Name
		if seen[key] {
			s.log().Warn("a swarm template declares the same secret name twice; the second is skipped", "workOrder", p.WorkOrderID, "name", p.Name)
			continue
		}
		seen[key] = true
		arn, err := s.Creator.CreateEmpty(ctx, p.Path, map[string]string{TagTeamID: p.TeamID, TagPoolID: p.PoolID, TagWorkOrderID: p.WorkOrderID})
		if err != nil {
			errs = append(errs, fmt.Errorf("workorder secrets: create %s: %w", p.Path, err))
			continue
		}
		if err := s.Store.Record(ctx, p, arn); err != nil {
			errs = append(errs, fmt.Errorf("workorder secrets: record %s: %w", p.Name, err))
			continue
		}
		s.log().Info("template secret created empty", "workOrder", p.WorkOrderID, "team", p.TeamID, "pool", p.Pool, "name", p.Name)
	}
	return errors.Join(errs...)
}

func (s *Secrets) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
