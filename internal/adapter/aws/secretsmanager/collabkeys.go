package secretsmanager

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

// RecoveryDays is the recovery window of a worker key secret scheduled for deletion (the shortest allowed:
// the key was revoked in Paperclip first, so the old value is worthless).
const RecoveryDays = MinRecoveryDays

// KeyStore writes the Paperclip keys of managed registration (COL-12): a worker's paperclip-agent-api-key.
// The value passes through one SDK call and is never logged or put in an error.
type KeyStore struct{ w *Writer }

var _ collab.KeyStore = (*KeyStore)(nil)

// NewKeyStore returns a KeyStore on a configured SDK config.
func NewKeyStore(cfg aws.Config) *KeyStore { return &KeyStore{w: NewWriter(cfg)} }

// NewKeyStoreWithAPI returns a KeyStore on api (tests).
func NewKeyStoreWithAPI(api WriteAPI) *KeyStore { return &KeyStore{w: NewWriterWithAPI(api)} }

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Put implements collab.KeyStore: create the secret, or write a new version when it exists, and return its ARN.
// A secret of that name that is still in its recovery window (left by an earlier disconnect) is deleted at
// once and created again: its value is a revoked key.
func (k *KeyStore) Put(ctx context.Context, path string, value collab.Key, tags map[string]string) (string, error) {
	tok, err := randomToken()
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < 2; attempt++ {
		cr, err := k.w.Create(ctx, path, value.Reveal(), tok, tags)
		if err == nil {
			return cr.ARN, nil
		}
		var taken *NameTakenError
		if errors.As(err, &taken) {
			out, perr := k.w.w.PutSecretValue(ctx, &sm.PutSecretValueInput{SecretId: aws.String(path), SecretString: aws.String(value.Reveal()), ClientRequestToken: aws.String(tok)})
			if perr != nil {
				return "", wrap(perr)
			}
			return aws.ToString(out.ARN), nil
		}
		if attempt == 0 && scheduledForDeletion(err) {
			if derr := k.w.ForceDelete(ctx, path); derr != nil {
				return "", derr
			}
			continue
		}
		return "", err
	}
	return "", errors.New("secretsmanager: could not write the key")
}

func scheduledForDeletion(err error) bool {
	var inv *types.InvalidRequestException
	if errors.As(err, &inv) {
		return strings.Contains(strings.ToLower(inv.ErrorMessage()), "scheduled for deletion")
	}
	return strings.Contains(strings.ToLower(err.Error()), "scheduled for deletion")
}

// ScheduleDeletion implements collab.KeyStore.
func (k *KeyStore) ScheduleDeletion(ctx context.Context, arn string) error {
	return k.w.ScheduleDeletion(ctx, arn, RecoveryDays)
}

// DescribeAPI is the subset of the SDK client Finder calls.
type DescribeAPI interface {
	DescribeSecret(ctx context.Context, in *sm.DescribeSecretInput, opts ...func(*sm.Options)) (*sm.DescribeSecretOutput, error)
}

// Finder finds a secret's ARN by name without reading its value (the board key, RC-29).
type Finder struct{ api DescribeAPI }

// NewFinder returns a Finder on a configured SDK config.
func NewFinder(cfg aws.Config) *Finder { return &Finder{api: sm.NewFromConfig(cfg)} }

// NewFinderWithAPI returns a Finder on api (tests).
func NewFinderWithAPI(api DescribeAPI) *Finder { return &Finder{api: api} }

// ARN returns the ARN of the secret name and whether it exists with a value. A secret scheduled for deletion
// does not count.
func (f *Finder) ARN(ctx context.Context, name string) (string, bool, error) {
	out, err := f.api.DescribeSecret(ctx, &sm.DescribeSecretInput{SecretId: aws.String(name)})
	if err != nil {
		var nf *types.ResourceNotFoundException
		if errors.As(err, &nf) {
			return "", false, nil
		}
		return "", false, wrap(err)
	}
	if out.DeletedDate != nil || len(out.VersionIdsToStages) == 0 {
		return "", false, nil
	}
	return aws.ToString(out.ARN), true, nil
}
