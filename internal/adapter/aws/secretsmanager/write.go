package secretsmanager

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// Tag keys of the secrets afs-api writes.
const (
	AgentTagKey  = "afs-agent-id"
	OrphanTagKey = "afs-orphan"
)

// WriteAPI is the subset of the SDK client the write path calls; *sm.Client satisfies it and
// tests inject a fake.
type WriteAPI interface {
	API
	CreateSecret(ctx context.Context, in *sm.CreateSecretInput, opts ...func(*sm.Options)) (*sm.CreateSecretOutput, error)
	PutSecretValue(ctx context.Context, in *sm.PutSecretValueInput, opts ...func(*sm.Options)) (*sm.PutSecretValueOutput, error)
	TagResource(ctx context.Context, in *sm.TagResourceInput, opts ...func(*sm.Options)) (*sm.TagResourceOutput, error)
}

// Writer writes secret values. It is the only AWS client afs-api builds. A value passes through
// as a plain string argument of one call and is never stored, logged or wrapped into an error.
type Writer struct {
	*Client
	w WriteAPI
}

// NewWriter returns a Writer on a configured SDK config.
func NewWriter(cfg aws.Config) *Writer { return NewWriterWithAPI(sm.NewFromConfig(cfg)) }

// NewWriterWithAPI returns a Writer on api (tests).
func NewWriterWithAPI(api WriteAPI) *Writer { return &Writer{Client: NewWithAPI(api), w: api} }

// Created is a secret that now exists in Secrets Manager.
type Created struct {
	ARN       string
	VersionID string
}

// RequestToken derives the ClientRequestToken (a UUID-shaped 36 characters) from parts, so a
// retry of the same request writes the same version instead of a second secret.
func RequestToken(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	b := h.Sum(nil)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Create creates the secret name with value and tags. token is the ClientRequestToken. An
// existing secret of that name is a NameTakenError.
func (c *Writer) Create(ctx context.Context, name, value, token string, tags map[string]string) (Created, error) {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var tg []types.Tag
	for _, k := range keys {
		tg = append(tg, types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	out, err := c.w.CreateSecret(ctx, &sm.CreateSecretInput{
		Name: aws.String(name), SecretString: aws.String(value), ClientRequestToken: aws.String(token), Tags: tg,
	})
	if err != nil {
		var ex *types.ResourceExistsException
		if errors.As(err, &ex) {
			return Created{}, &NameTakenError{Name: name}
		}
		return Created{}, wrap(err)
	}
	return Created{ARN: aws.ToString(out.ARN), VersionID: aws.ToString(out.VersionId)}, nil
}

// PutValue writes a new version of the secret and returns its version id.
func (c *Writer) PutValue(ctx context.Context, arn, value, token string) (string, error) {
	out, err := c.w.PutSecretValue(ctx, &sm.PutSecretValueInput{
		SecretId: aws.String(arn), SecretString: aws.String(value), ClientRequestToken: aws.String(token),
	})
	if err != nil {
		return "", wrap(err)
	}
	return aws.ToString(out.VersionId), nil
}

// ForceDelete deletes arn at once without a recovery window. It is only for a secret created a
// moment ago whose database row could not be written. A missing secret is success.
func (c *Writer) ForceDelete(ctx context.Context, arn string) error {
	_, err := c.api.DeleteSecret(ctx, &sm.DeleteSecretInput{SecretId: aws.String(arn), ForceDeleteWithoutRecovery: aws.Bool(true)})
	if err == nil || alreadyGone(err) {
		return nil
	}
	return wrap(err)
}

// TagOrphan tags arn afs-orphan=true so the reconciler's sweep schedules it for deletion.
func (c *Writer) TagOrphan(ctx context.Context, arn string) error {
	_, err := c.w.TagResource(ctx, &sm.TagResourceInput{
		SecretId: aws.String(arn), Tags: []types.Tag{{Key: aws.String(OrphanTagKey), Value: aws.String("true")}},
	})
	if err != nil {
		return wrap(err)
	}
	return nil
}

// NameTakenError reports that a secret of that name already exists in Secrets Manager.
type NameTakenError struct{ Name string }

func (e *NameTakenError) Error() string { return "secretsmanager: a secret of that name exists" }
