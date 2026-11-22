package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// newSecretsWriter builds the Secrets Manager writer; tests replace it with a fake so no test
// reaches AWS.
var newSecretsWriter = func(ctx context.Context, s settings) (secrets.Writer, error) {
	if s.AWSRegion == "" {
		// Without a region afs-api still starts; every secret write answers 503.
		return unconfiguredWriter{}, nil
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(s.AWSRegion))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration for Secrets Manager: %w", err)
	}
	return smWriter{secretsmanager.NewWriter(cfg)}, nil
}

// smWriter adapts the Secrets Manager adapter to secrets.Writer.
type smWriter struct{ w *secretsmanager.Writer }

func (a smWriter) Create(ctx context.Context, name string, v domain.SecretValue, token string, tags map[string]string) (secrets.Created, error) {
	c, err := a.w.Create(ctx, name, v.Expose(), token, tags)
	var taken *secretsmanager.NameTakenError
	if errors.As(err, &taken) {
		return secrets.Created{}, secrets.ErrNameTaken
	}
	return secrets.Created{ARN: c.ARN, VersionID: c.VersionID}, err
}

func (a smWriter) PutValue(ctx context.Context, arn string, v domain.SecretValue, token string) (string, error) {
	return a.w.PutValue(ctx, arn, v.Expose(), token)
}
func (a smWriter) ForceDelete(ctx context.Context, arn string) error {
	return a.w.ForceDelete(ctx, arn)
}
func (a smWriter) TagOrphan(ctx context.Context, arn string) error { return a.w.TagOrphan(ctx, arn) }
func (a smWriter) ScheduleDeletion(ctx context.Context, arn string, days int) error {
	return a.w.ScheduleDeletion(ctx, arn, days)
}

var errNoAWSRegion = errors.New("secrets: AFS_AWS_REGION is not set")

// unconfiguredWriter fails every call without touching AWS.
type unconfiguredWriter struct{}

func (unconfiguredWriter) Create(context.Context, string, domain.SecretValue, string, map[string]string) (secrets.Created, error) {
	return secrets.Created{}, errNoAWSRegion
}
func (unconfiguredWriter) PutValue(context.Context, string, domain.SecretValue, string) (string, error) {
	return "", errNoAWSRegion
}
func (unconfiguredWriter) ForceDelete(context.Context, string) error           { return errNoAWSRegion }
func (unconfiguredWriter) TagOrphan(context.Context, string) error             { return errNoAWSRegion }
func (unconfiguredWriter) ScheduleDeletion(context.Context, string, int) error { return errNoAWSRegion }

// wireSecrets builds the secrets use case on the database components and the Secrets Manager
// writer, and binds its handlers (M2-C).
func wireSecrets(ctx context.Context, c *components, s settings, log *slog.Logger) error {
	w, err := newSecretsWriter(ctx, s)
	if err != nil {
		return err
	}
	c.SecretsWriter = w
	orphans := c.Metrics.APIOrphanSecrets
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	svc := &secrets.Service{Deps: deps, Repo: postgres.NewSecretRepo(), Writer: w, OnOrphan: func() {
		orphans.Inc()
		log.Error("secret left in Secrets Manager without a database row; the reconciler sweep will remove it")
	}}
	c.bind(httpapi.BindSecrets(&httpapi.SecretsAPI{Svc: svc, Pages: httpapi.NewPageTokens(c.PageKey, nil),
		IdemKey: s.IdempotencyHMACKey}))
	return nil
}
