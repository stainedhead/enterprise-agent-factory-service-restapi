package eks

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/aws/aws-sdk-go-v2/aws"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
)

// PodIdentityAPI is the subset of the EKS SDK client the Pod Identity association calls need.
type PodIdentityAPI interface {
	CreatePodIdentityAssociation(ctx context.Context, in *awseks.CreatePodIdentityAssociationInput, opts ...func(*awseks.Options)) (*awseks.CreatePodIdentityAssociationOutput, error)
	ListPodIdentityAssociations(ctx context.Context, in *awseks.ListPodIdentityAssociationsInput, opts ...func(*awseks.Options)) (*awseks.ListPodIdentityAssociationsOutput, error)
	DescribePodIdentityAssociation(ctx context.Context, in *awseks.DescribePodIdentityAssociationInput, opts ...func(*awseks.Options)) (*awseks.DescribePodIdentityAssociationOutput, error)
	UpdatePodIdentityAssociation(ctx context.Context, in *awseks.UpdatePodIdentityAssociationInput, opts ...func(*awseks.Options)) (*awseks.UpdatePodIdentityAssociationOutput, error)
	DeletePodIdentityAssociation(ctx context.Context, in *awseks.DeletePodIdentityAssociationInput, opts ...func(*awseks.Options)) (*awseks.DeletePodIdentityAssociationOutput, error)
	TagResource(ctx context.Context, in *awseks.TagResourceInput, opts ...func(*awseks.Options)) (*awseks.TagResourceOutput, error)
	UntagResource(ctx context.Context, in *awseks.UntagResourceInput, opts ...func(*awseks.Options)) (*awseks.UntagResourceOutput, error)
}

// PodIdentity maps an agent's ServiceAccount to its IAM role through EKS Pod Identity (RC-2). It implements
// agent.PodIdentities for one cluster.
type PodIdentity struct {
	api     PodIdentityAPI
	cluster string
}

var _ agent.PodIdentities = (*PodIdentity)(nil)

// NewPodIdentity returns a PodIdentity for cluster (AFS_CLUSTER_NAME).
func NewPodIdentity(cfg aws.Config, cluster string) *PodIdentity {
	return &PodIdentity{api: awseks.NewFromConfig(cfg), cluster: cluster}
}

// NewPodIdentityWithAPI returns a PodIdentity on api (tests).
func NewPodIdentityWithAPI(api PodIdentityAPI, cluster string) *PodIdentity {
	return &PodIdentity{api: api, cluster: cluster}
}

// Ensure implements agent.PodIdentities: one association per ServiceAccount, created when missing, pointed
// at roleARN when it names another role, and tagged. Calling it again with the same inputs changes nothing.
func (p *PodIdentity) Ensure(ctx context.Context, namespace, serviceAccount, roleARN string, want map[string]string) error {
	if p.cluster == "" {
		return errors.New("eks: cluster name is not configured")
	}
	id, err := p.find(ctx, namespace, serviceAccount)
	if err != nil {
		return err
	}
	if id == "" {
		_, err := p.api.CreatePodIdentityAssociation(ctx, &awseks.CreatePodIdentityAssociationInput{
			ClusterName: aws.String(p.cluster), Namespace: aws.String(namespace), ServiceAccount: aws.String(serviceAccount),
			RoleArn: aws.String(roleARN), Tags: maps.Clone(want),
			ClientRequestToken: aws.String(requestToken(p.cluster, namespace, serviceAccount, roleARN)),
		})
		if err != nil {
			return wrap("create pod identity association", err)
		}
		return nil
	}
	d, err := p.api.DescribePodIdentityAssociation(ctx, &awseks.DescribePodIdentityAssociationInput{ClusterName: aws.String(p.cluster), AssociationId: aws.String(id)})
	if err != nil {
		return wrap("describe pod identity association", err)
	}
	a := d.Association
	if a == nil {
		return fmt.Errorf("eks: association %s has no description", id)
	}
	if aws.ToString(a.RoleArn) != roleARN {
		if _, err := p.api.UpdatePodIdentityAssociation(ctx, &awseks.UpdatePodIdentityAssociationInput{
			ClusterName: aws.String(p.cluster), AssociationId: aws.String(id), RoleArn: aws.String(roleARN)}); err != nil {
			return wrap("update pod identity association", err)
		}
	}
	if !sameTags(a.Tags, want) && a.AssociationArn != nil {
		if _, err := p.api.TagResource(ctx, &awseks.TagResourceInput{ResourceArn: a.AssociationArn, Tags: maps.Clone(want)}); err != nil {
			return wrap("tag pod identity association", err)
		}
	}
	if stale := tags.Stale(a.Tags, want); len(stale) > 0 && a.AssociationArn != nil {
		if _, err := p.api.UntagResource(ctx, &awseks.UntagResourceInput{ResourceArn: a.AssociationArn, TagKeys: stale}); err != nil {
			return wrap("untag pod identity association", err)
		}
	}
	return nil
}

// Delete implements agent.PodIdentities: a missing association is success.
func (p *PodIdentity) Delete(ctx context.Context, namespace, serviceAccount string) error {
	if p.cluster == "" {
		return errors.New("eks: cluster name is not configured")
	}
	id, err := p.find(ctx, namespace, serviceAccount)
	if err != nil || id == "" {
		return err
	}
	if _, err := p.api.DeletePodIdentityAssociation(ctx, &awseks.DeletePodIdentityAssociationInput{ClusterName: aws.String(p.cluster), AssociationId: aws.String(id)}); err != nil && !notFound(err) {
		return wrap("delete pod identity association", err)
	}
	return nil
}

func (p *PodIdentity) find(ctx context.Context, namespace, serviceAccount string) (string, error) {
	in := &awseks.ListPodIdentityAssociationsInput{ClusterName: aws.String(p.cluster), Namespace: aws.String(namespace), ServiceAccount: aws.String(serviceAccount)}
	for {
		out, err := p.api.ListPodIdentityAssociations(ctx, in)
		if err != nil {
			if notFound(err) {
				return "", nil
			}
			return "", wrap("list pod identity associations", err)
		}
		for _, a := range out.Associations {
			if aws.ToString(a.Namespace) == namespace && aws.ToString(a.ServiceAccount) == serviceAccount {
				return aws.ToString(a.AssociationId), nil
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return "", nil
		}
		in.NextToken = out.NextToken
	}
}

func sameTags(have, want map[string]string) bool {
	if len(have) != len(want) {
		return false
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// requestToken makes a retried create idempotent: the same inputs give the same token, so a lost response
// does not leave a second association.
func requestToken(cluster, namespace, sa, role string) string {
	return fmt.Sprintf("afs-%x", hash(cluster+"/"+namespace+"/"+sa+"/"+role))
}

func hash(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
