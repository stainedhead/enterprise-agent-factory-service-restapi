package secretsmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// ReadAPI is the subset of the Secrets Manager SDK client Reader calls.
type ReadAPI interface {
	GetSecretValue(ctx context.Context, in *sm.GetSecretValueInput, opts ...func(*sm.Options)) (*sm.GetSecretValueOutput, error)
}

// Reader reads a secret value into memory for the reconciler's notification delivery (OQ-S3). It is
// built only by afs-reconciler: afs-api writes secrets and never reads one. The value is returned as
// a notify.Credential, which prints as [redacted], and appears in no error. The reconciler's IAM role
// limits which secrets it may read.
type Reader struct{ api ReadAPI }

var _ notify.Secrets = (*Reader)(nil)

// NewReader returns a Reader on a configured SDK config.
func NewReader(cfg aws.Config) *Reader { return &Reader{api: sm.NewFromConfig(cfg)} }

// NewReaderWithAPI returns a Reader on api (tests).
func NewReaderWithAPI(api ReadAPI) *Reader { return &Reader{api: api} }

// Read implements notify.Secrets.
func (r *Reader) Read(ctx context.Context, arn string) (notify.Credential, error) {
	if !strings.HasPrefix(arn, "arn:") || !strings.Contains(arn, ":secretsmanager:") {
		return "", errors.New("secretsmanager: not a Secrets Manager ARN")
	}
	out, err := r.api.GetSecretValue(ctx, &sm.GetSecretValueInput{SecretId: aws.String(arn)})
	if err != nil {
		return "", fmt.Errorf("secretsmanager: GetSecretValue: %w", wrap(err))
	}
	if out.SecretString == nil || *out.SecretString == "" {
		return "", errors.New("secretsmanager: the secret holds no text value")
	}
	return notify.Credential(*out.SecretString), nil
}
