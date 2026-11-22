package eks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/kubectl"
)

// ManagedByTagKey and ViewerKubectlTagValue mark the access entries the reconciler creates for viewer
// kubectl (M5-E2). The reconciler changes or deletes only entries that carry them: an entry someone else
// made for the same role is left alone and reported.
const (
	ManagedByTagKey       = "afs-managed-by"
	ViewerKubectlTagValue = "viewer-kubectl"
)

// CodeEntryNotManaged is the error code of a wanted entry that already exists without the AFS tag.
const CodeEntryNotManaged = "AccessEntryNotManaged"

// AccessAPI is the part of the EKS client the viewer access entries use.
type AccessAPI interface {
	API
	CreateAccessEntry(ctx context.Context, in *awseks.CreateAccessEntryInput, opts ...func(*awseks.Options)) (*awseks.CreateAccessEntryOutput, error)
	UpdateAccessEntry(ctx context.Context, in *awseks.UpdateAccessEntryInput, opts ...func(*awseks.Options)) (*awseks.UpdateAccessEntryOutput, error)
}

// ViewerEntries implements kubectl.Entries on one cluster: one STANDARD access entry per federated viewer
// role, mapped to the role's Kubernetes group and nothing else (no access policy is attached, so the
// permissions come only from the namespace RoleBindings the reconciler applies).
type ViewerEntries struct {
	api     AccessAPI
	cluster string
}

var _ kubectl.Entries = (*ViewerEntries)(nil)

// NewViewerEntries returns ViewerEntries for cluster (AFS_CLUSTER_NAME).
func NewViewerEntries(cfg aws.Config, cluster string) *ViewerEntries {
	return &ViewerEntries{api: awseks.NewFromConfig(cfg), cluster: cluster}
}

// NewViewerEntriesWithAPI returns ViewerEntries on api (tests).
func NewViewerEntriesWithAPI(api AccessAPI, cluster string) *ViewerEntries {
	return &ViewerEntries{api: api, cluster: cluster}
}

// entry is a live access entry.
type entry struct {
	groups  []string
	managed bool
}

// SyncViewerEntries implements kubectl.Entries. Every wanted entry is attempted and a failure of one does
// not stop the others; the errors are joined. The result names the entries created, updated or deleted.
func (v *ViewerEntries) SyncViewerEntries(ctx context.Context, want map[string]string) ([]string, error) {
	if v.cluster == "" {
		return nil, errors.New("eks: cluster name is not configured")
	}
	c := &Client{api: v.api, cluster: v.cluster}
	principals, err := c.listPrincipals(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]entry{}
	var errs []error
	for _, p := range principals {
		d, err := v.api.DescribeAccessEntry(ctx, &awseks.DescribeAccessEntryInput{ClusterName: aws.String(v.cluster), PrincipalArn: aws.String(p)})
		if err != nil {
			if !notFound(err) {
				errs = append(errs, wrap("describe access entry", err))
			}
			continue
		}
		if d.AccessEntry == nil {
			continue
		}
		live[p] = entry{groups: d.AccessEntry.KubernetesGroups, managed: d.AccessEntry.Tags[ManagedByTagKey] == ViewerKubectlTagValue}
	}
	var changed []string
	wanted := make([]string, 0, len(want))
	for arn := range want {
		wanted = append(wanted, arn)
	}
	sort.Strings(wanted)
	for _, arn := range wanted {
		group := want[arn]
		cur, exists := live[arn]
		switch {
		case !exists:
			_, err := v.api.CreateAccessEntry(ctx, &awseks.CreateAccessEntryInput{
				ClusterName: aws.String(v.cluster), PrincipalArn: aws.String(arn), Type: aws.String("STANDARD"),
				KubernetesGroups: []string{group}, Tags: map[string]string{ManagedByTagKey: ViewerKubectlTagValue},
			})
			if err != nil {
				errs = append(errs, wrap("create access entry", err))
				continue
			}
			changed = append(changed, "AccessEntry/"+arn)
		case !cur.managed:
			// Never take over an entry AFS did not make: it may carry another team's or an operator's access.
			errs = append(errs, &apiError{code: CodeEntryNotManaged,
				err: fmt.Errorf("eks: access entry for %s exists and is not managed by AFS; remove it or use another role", arn)})
		case !slices.Equal(cur.groups, []string{group}):
			if _, err := v.api.UpdateAccessEntry(ctx, &awseks.UpdateAccessEntryInput{
				ClusterName: aws.String(v.cluster), PrincipalArn: aws.String(arn), KubernetesGroups: []string{group},
			}); err != nil {
				errs = append(errs, wrap("update access entry", err))
				continue
			}
			changed = append(changed, "AccessEntry/"+arn)
		}
	}
	stale := make([]string, 0)
	for arn, cur := range live {
		if _, ok := want[arn]; !ok && cur.managed {
			stale = append(stale, arn)
		}
	}
	sort.Strings(stale)
	for _, arn := range stale {
		if _, err := v.api.DeleteAccessEntry(ctx, &awseks.DeleteAccessEntryInput{ClusterName: aws.String(v.cluster), PrincipalArn: aws.String(arn)}); err != nil && !notFound(err) {
			errs = append(errs, wrap("delete access entry", err))
			continue
		}
		changed = append(changed, "AccessEntry/"+arn)
	}
	return changed, errors.Join(errs...)
}
