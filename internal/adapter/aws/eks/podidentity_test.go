package eks

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"
)

type fakeAssoc struct {
	id, ns, sa, role string
	tags             map[string]string
}

type fakePI struct {
	assoc   map[string]*fakeAssoc
	calls   []string
	listErr error
	next    int
}

func newFakePI() *fakePI { return &fakePI{assoc: map[string]*fakeAssoc{}} }

func (f *fakePI) CreatePodIdentityAssociation(_ context.Context, in *awseks.CreatePodIdentityAssociationInput, _ ...func(*awseks.Options)) (*awseks.CreatePodIdentityAssociationOutput, error) {
	f.calls = append(f.calls, "create")
	f.next++
	id := "a-" + strconv.Itoa(f.next)
	f.assoc[id] = &fakeAssoc{id: id, ns: *in.Namespace, sa: *in.ServiceAccount, role: *in.RoleArn, tags: in.Tags}
	if in.ClientRequestToken == nil || *in.ClientRequestToken == "" {
		return nil, errors.New("no client request token")
	}
	return &awseks.CreatePodIdentityAssociationOutput{}, nil
}

func (f *fakePI) ListPodIdentityAssociations(_ context.Context, in *awseks.ListPodIdentityAssociationsInput, _ ...func(*awseks.Options)) (*awseks.ListPodIdentityAssociationsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := &awseks.ListPodIdentityAssociationsOutput{}
	for _, a := range f.assoc {
		out.Associations = append(out.Associations, types.PodIdentityAssociationSummary{AssociationId: aws.String(a.id), Namespace: aws.String(a.ns), ServiceAccount: aws.String(a.sa)})
	}
	return out, nil
}

func (f *fakePI) DescribePodIdentityAssociation(_ context.Context, in *awseks.DescribePodIdentityAssociationInput, _ ...func(*awseks.Options)) (*awseks.DescribePodIdentityAssociationOutput, error) {
	a := f.assoc[*in.AssociationId]
	return &awseks.DescribePodIdentityAssociationOutput{Association: &types.PodIdentityAssociation{
		AssociationId: aws.String(a.id), AssociationArn: aws.String("arn:aws:eks:us-east-1:111122223333:podidentityassociation/c/" + a.id), RoleArn: aws.String(a.role), Tags: a.tags}}, nil
}

func (f *fakePI) UpdatePodIdentityAssociation(_ context.Context, in *awseks.UpdatePodIdentityAssociationInput, _ ...func(*awseks.Options)) (*awseks.UpdatePodIdentityAssociationOutput, error) {
	f.calls = append(f.calls, "update")
	f.assoc[*in.AssociationId].role = *in.RoleArn
	return &awseks.UpdatePodIdentityAssociationOutput{}, nil
}

func (f *fakePI) DeletePodIdentityAssociation(_ context.Context, in *awseks.DeletePodIdentityAssociationInput, _ ...func(*awseks.Options)) (*awseks.DeletePodIdentityAssociationOutput, error) {
	f.calls = append(f.calls, "delete")
	if _, ok := f.assoc[*in.AssociationId]; !ok {
		return nil, &types.ResourceNotFoundException{}
	}
	delete(f.assoc, *in.AssociationId)
	return &awseks.DeletePodIdentityAssociationOutput{}, nil
}

func (f *fakePI) TagResource(_ context.Context, in *awseks.TagResourceInput, _ ...func(*awseks.Options)) (*awseks.TagResourceOutput, error) {
	f.calls = append(f.calls, "tag")
	for _, a := range f.assoc {
		a.tags = in.Tags
	}
	return &awseks.TagResourceOutput{}, nil
}

const role1 = "arn:aws:iam::111122223333:role/afs/afs-t-a/afs-team"
const role2 = "arn:aws:iam::111122223333:role/afs/afs-t-a/afs-agent"

// M2-F1, M2-F7: the association is created once, repointed when the role changes (shared to dedicated), tagged
// when the tags change, and deleted idempotently.
func TestPodIdentityEnsureAndDelete(t *testing.T) {
	f := newFakePI()
	p := NewPodIdentityWithAPI(f, "afs-dev")
	ctx := context.Background()
	tags := map[string]string{"afs-team-id": "team_1"}

	if err := p.Ensure(ctx, "afs-t-a", "agent-x", role1, tags); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(ctx, "afs-t-a", "agent-x", role1, tags); err != nil {
		t.Fatal(err)
	}
	if len(f.assoc) != 1 || len(f.calls) != 1 {
		t.Fatalf("calls = %v, associations = %d; want one create", f.calls, len(f.assoc))
	}
	if err := p.Ensure(ctx, "afs-t-a", "agent-x", role2, tags); err != nil {
		t.Fatal(err)
	}
	if f.assoc["a-1"].role != role2 || f.calls[len(f.calls)-1] != "update" {
		t.Errorf("role = %s, calls = %v", f.assoc["a-1"].role, f.calls)
	}
	tags["afs-agent-id"] = "agt_1"
	if err := p.Ensure(ctx, "afs-t-a", "agent-x", role2, tags); err != nil {
		t.Fatal(err)
	}
	if f.calls[len(f.calls)-1] != "tag" {
		t.Errorf("tag change not applied: %v", f.calls)
	}
	// A different ServiceAccount is a different association.
	if err := p.Ensure(ctx, "afs-t-a", "agent-y", role1, nil); err != nil || len(f.assoc) != 2 {
		t.Errorf("second agent: %v, %d associations", err, len(f.assoc))
	}

	if err := p.Delete(ctx, "afs-t-a", "agent-x"); err != nil || len(f.assoc) != 1 {
		t.Errorf("delete: %v, %d left", err, len(f.assoc))
	}
	if err := p.Delete(ctx, "afs-t-a", "agent-x"); err != nil {
		t.Errorf("second delete: %v", err)
	}
	if _, ok := f.assoc["a-2"]; !ok {
		t.Error("another agent's association was deleted")
	}
}

func TestPodIdentityErrors(t *testing.T) {
	f := newFakePI()
	f.listErr = &types.AccessDeniedException{Message: aws.String("no")}
	p := NewPodIdentityWithAPI(f, "afs-dev")
	err := p.Ensure(context.Background(), "ns", "sa", role1, nil)
	if err == nil {
		t.Fatal("list error swallowed")
	}
	if err := NewPodIdentityWithAPI(f, "").Ensure(context.Background(), "ns", "sa", role1, nil); err == nil {
		t.Error("missing cluster accepted")
	}
	if err := NewPodIdentityWithAPI(f, "").Delete(context.Background(), "ns", "sa"); err == nil {
		t.Error("missing cluster accepted by Delete")
	}
	first, again, other := requestToken("c", "n", "s", "r"), requestToken("c", "n", "s", "r"), requestToken("c", "n", "s", "r2")
	if first != again || first == other {
		t.Error("request token is not a stable function of its inputs")
	}
	f.listErr = &types.ResourceNotFoundException{}
	if err := p.Delete(context.Background(), "ns", "sa"); err != nil {
		t.Errorf("Delete with a missing cluster resource: %v", err)
	}
}
