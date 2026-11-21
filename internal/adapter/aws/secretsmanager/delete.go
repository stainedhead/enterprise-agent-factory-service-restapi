package secretsmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/smithy-go"
)

// TeamTagKey is the tag every secret of a team carries (research, tags `afs-team-id`).
const TeamTagKey = "afs-team-id"

// MinRecoveryDays and MaxRecoveryDays bound the recovery window Secrets Manager accepts.
const (
	MinRecoveryDays = 7
	MaxRecoveryDays = 30
)

// ScheduleDeletion schedules arn for deletion with a recovery window of days (7–30). A secret that
// is already scheduled for deletion or no longer exists is success, so the call is idempotent.
func (c *Client) ScheduleDeletion(ctx context.Context, arn string, days int) error {
	if days < MinRecoveryDays || days > MaxRecoveryDays {
		return fmt.Errorf("secretsmanager: recovery window %d days is outside %d..%d", days, MinRecoveryDays, MaxRecoveryDays)
	}
	_, err := c.api.DeleteSecret(ctx, &sm.DeleteSecretInput{
		SecretId:             aws.String(arn),
		RecoveryWindowInDays: aws.Int64(int64(days)),
	})
	if err == nil || alreadyGone(err) {
		return nil
	}
	return wrap(err)
}

// ListTeamSecrets returns the ARNs of every secret tagged with the team, following pagination.
// Secrets already scheduled for deletion are not listed by Secrets Manager unless asked for, so
// they are skipped.
func (c *Client) ListTeamSecrets(ctx context.Context, teamID string) ([]string, error) {
	var arns []string
	in := &sm.ListSecretsInput{
		Filters: []types.Filter{
			{Key: types.FilterNameStringTypeTagKey, Values: []string{TeamTagKey}},
			{Key: types.FilterNameStringTypeTagValue, Values: []string{teamID}},
		},
	}
	for {
		out, err := c.api.ListSecrets(ctx, in)
		if err != nil {
			return nil, wrap(err)
		}
		for _, s := range out.SecretList {
			// The filters match tag key and value independently; confirm the pair.
			if hasTag(s.Tags, TeamTagKey, teamID) && s.ARN != nil {
				arns = append(arns, *s.ARN)
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return arns, nil
		}
		in.NextToken = out.NextToken
	}
}

func hasTag(tags []types.Tag, k, v string) bool {
	for _, t := range tags {
		if aws.ToString(t.Key) == k && aws.ToString(t.Value) == v {
			return true
		}
	}
	return false
}

func alreadyGone(err error) bool {
	var nf *types.ResourceNotFoundException
	if errors.As(err, &nf) {
		return true
	}
	var inv *types.InvalidRequestException
	return errors.As(err, &inv) && strings.Contains(strings.ToLower(inv.ErrorMessage()), "scheduled for deletion")
}

// apiError carries the AWS error code so the reconciler can show it in a condition's reason.
type apiError struct {
	code string
	err  error
}

func (e *apiError) Error() string { return "secretsmanager: " + e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

// Code returns the AWS error code.
func (e *apiError) Code() string { return e.code }

func wrap(err error) error {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return &apiError{code: ae.ErrorCode(), err: err}
	}
	return fmt.Errorf("secretsmanager: %w", err)
}
