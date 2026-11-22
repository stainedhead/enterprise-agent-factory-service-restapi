// Package eks is the EKS adapter of the reconciler: access entries that give a team's federated
// viewer role kubectl access. The reconciler is the only component that calls it.
package eks

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
)

// API is the subset of the EKS SDK client this package calls.
type API interface {
	ListAccessEntries(ctx context.Context, in *awseks.ListAccessEntriesInput, opts ...func(*awseks.Options)) (*awseks.ListAccessEntriesOutput, error)
	DescribeAccessEntry(ctx context.Context, in *awseks.DescribeAccessEntryInput, opts ...func(*awseks.Options)) (*awseks.DescribeAccessEntryOutput, error)
	DeleteAccessEntry(ctx context.Context, in *awseks.DeleteAccessEntryInput, opts ...func(*awseks.Options)) (*awseks.DeleteAccessEntryOutput, error)
}

// Client wraps API for one cluster.
type Client struct {
	api     API
	cluster string
}

// New returns a Client for cluster (AFS_CLUSTER_NAME).
func New(cfg aws.Config, cluster string) *Client {
	return &Client{api: awseks.NewFromConfig(cfg), cluster: cluster}
}

// NewWithAPI returns a Client on api (tests).
func NewWithAPI(api API, cluster string) *Client { return &Client{api: api, cluster: cluster} }
