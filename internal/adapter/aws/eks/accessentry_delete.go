package eks

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/smithy-go"
)

// TeamTagKey is the tag the reconciler puts on every access entry it creates for a team.
const TeamTagKey = "afs-team-id"

// DeleteTeamAccessEntries deletes every access entry of the cluster tagged with the team. An entry
// that is already gone is success; every entry is attempted and the errors are joined.
func (c *Client) DeleteTeamAccessEntries(ctx context.Context, teamID string) error {
	if c.cluster == "" {
		return errors.New("eks: cluster name is not configured")
	}
	principals, err := c.listPrincipals(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range principals {
		d, err := c.api.DescribeAccessEntry(ctx, &awseks.DescribeAccessEntryInput{ClusterName: aws.String(c.cluster), PrincipalArn: aws.String(p)})
		if err != nil {
			if !notFound(err) {
				errs = append(errs, wrap("describe access entry", err))
			}
			continue
		}
		if d.AccessEntry == nil || d.AccessEntry.Tags[TeamTagKey] != teamID {
			continue
		}
		if _, err := c.api.DeleteAccessEntry(ctx, &awseks.DeleteAccessEntryInput{ClusterName: aws.String(c.cluster), PrincipalArn: aws.String(p)}); err != nil && !notFound(err) {
			errs = append(errs, wrap("delete access entry", err))
		}
	}
	return errors.Join(errs...)
}

func (c *Client) listPrincipals(ctx context.Context) ([]string, error) {
	var out []string
	in := &awseks.ListAccessEntriesInput{ClusterName: aws.String(c.cluster)}
	for {
		page, err := c.api.ListAccessEntries(ctx, in)
		if err != nil {
			return nil, wrap("list access entries", err)
		}
		out = append(out, page.AccessEntries...)
		if page.NextToken == nil || *page.NextToken == "" {
			return out, nil
		}
		in.NextToken = page.NextToken
	}
}

func notFound(err error) bool {
	var nf *types.ResourceNotFoundException
	return errors.As(err, &nf)
}

type apiError struct {
	code string
	err  error
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

// Code returns the AWS error code.
func (e *apiError) Code() string { return e.code }

func wrap(op string, err error) error {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return &apiError{code: ae.ErrorCode(), err: fmt.Errorf("eks: %s: %w", op, err)}
	}
	return fmt.Errorf("eks: %s: %w", op, err)
}
