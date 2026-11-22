package eks

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"
)

const (
	roleA   = "arn:aws:iam::111122223333:role/afs-viewers-okta"
	roleB   = "arn:aws:iam::111122223333:role/afs-viewers-entra"
	nodes   = "arn:aws:iam::111122223333:role/nodes"
	foreign = "arn:aws:iam::111122223333:role/someone-elses"
)

// accessAPI is an in-memory EKS access entry store.
type accessAPI struct {
	entries map[string]*types.AccessEntry
	created []*awseks.CreateAccessEntryInput
	updated []*awseks.UpdateAccessEntryInput
	deleted []string
	failOn  map[string]error // "create:<arn>", "update:<arn>", "delete:<arn>", "list", "describe:<arn>"
}

func newAccessAPI() *accessAPI {
	return &accessAPI{entries: map[string]*types.AccessEntry{}, failOn: map[string]error{}}
}

func (a *accessAPI) put(arn string, groups []string, tags map[string]string) {
	a.entries[arn] = &types.AccessEntry{PrincipalArn: aws.String(arn), KubernetesGroups: groups, Tags: tags}
}

func (a *accessAPI) ListAccessEntries(context.Context, *awseks.ListAccessEntriesInput, ...func(*awseks.Options)) (*awseks.ListAccessEntriesOutput, error) {
	if err := a.failOn["list"]; err != nil {
		return nil, err
	}
	var out []string
	for k := range a.entries {
		out = append(out, k)
	}
	return &awseks.ListAccessEntriesOutput{AccessEntries: out}, nil
}

func (a *accessAPI) DescribeAccessEntry(_ context.Context, in *awseks.DescribeAccessEntryInput, _ ...func(*awseks.Options)) (*awseks.DescribeAccessEntryOutput, error) {
	p := aws.ToString(in.PrincipalArn)
	if err := a.failOn["describe:"+p]; err != nil {
		return nil, err
	}
	e, ok := a.entries[p]
	if !ok {
		return nil, &types.ResourceNotFoundException{Message: aws.String("gone")}
	}
	return &awseks.DescribeAccessEntryOutput{AccessEntry: e}, nil
}

func (a *accessAPI) DeleteAccessEntry(_ context.Context, in *awseks.DeleteAccessEntryInput, _ ...func(*awseks.Options)) (*awseks.DeleteAccessEntryOutput, error) {
	p := aws.ToString(in.PrincipalArn)
	if err := a.failOn["delete:"+p]; err != nil {
		return nil, err
	}
	delete(a.entries, p)
	a.deleted = append(a.deleted, p)
	return &awseks.DeleteAccessEntryOutput{}, nil
}

func (a *accessAPI) CreateAccessEntry(_ context.Context, in *awseks.CreateAccessEntryInput, _ ...func(*awseks.Options)) (*awseks.CreateAccessEntryOutput, error) {
	p := aws.ToString(in.PrincipalArn)
	if err := a.failOn["create:"+p]; err != nil {
		return nil, err
	}
	a.created = append(a.created, in)
	a.put(p, in.KubernetesGroups, in.Tags)
	return &awseks.CreateAccessEntryOutput{}, nil
}

func (a *accessAPI) UpdateAccessEntry(_ context.Context, in *awseks.UpdateAccessEntryInput, _ ...func(*awseks.Options)) (*awseks.UpdateAccessEntryOutput, error) {
	p := aws.ToString(in.PrincipalArn)
	if err := a.failOn["update:"+p]; err != nil {
		return nil, err
	}
	a.updated = append(a.updated, in)
	a.entries[p].KubernetesGroups = in.KubernetesGroups
	return &awseks.UpdateAccessEntryOutput{}, nil
}

var managed = map[string]string{ManagedByTagKey: ViewerKubectlTagValue}

func TestSyncCreatesAViewerEntryWithOneGroupAndNoPolicy(t *testing.T) {
	api := newAccessAPI()
	changed, err := NewViewerEntriesWithAPI(api, "afs").SyncViewerEntries(t.Context(), map[string]string{roleA: "afs-kubectl-viewers-aaa"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"AccessEntry/" + roleA}) || len(api.created) != 1 {
		t.Fatalf("changed %v, created %d", changed, len(api.created))
	}
	in := api.created[0]
	if aws.ToString(in.ClusterName) != "afs" || aws.ToString(in.Type) != "STANDARD" || !slices.Equal(in.KubernetesGroups, []string{"afs-kubectl-viewers-aaa"}) ||
		in.Tags[ManagedByTagKey] != ViewerKubectlTagValue {
		t.Errorf("create = %+v", in)
	}
	// RC-US-11.1: the entry grants nothing by itself, so no access policy is ever associated with it.
	if _, again := NewViewerEntriesWithAPI(api, "afs").SyncViewerEntries(t.Context(), map[string]string{roleA: "afs-kubectl-viewers-aaa"}); again != nil {
		t.Fatal(again)
	}
	if len(api.created) != 1 || len(api.updated) != 0 {
		t.Errorf("a second sync wrote: created %d updated %d", len(api.created), len(api.updated))
	}
}

func TestSyncCorrectsGroupsAndDeletesOnlyManagedEntries(t *testing.T) {
	api := newAccessAPI()
	api.put(roleA, []string{"wrong"}, managed)
	api.put(roleB, []string{"afs-kubectl-viewers-bbb"}, managed) // no longer wanted
	api.put(nodes, []string{"system:nodes"}, nil)                // not ours
	api.put(foreign, nil, map[string]string{TeamTagKey: "team_01"})
	changed, err := NewViewerEntriesWithAPI(api, "afs").SyncViewerEntries(t.Context(), map[string]string{roleA: "afs-kubectl-viewers-aaa"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(changed)
	if want := []string{"AccessEntry/" + roleB, "AccessEntry/" + roleA}; !slices.Equal(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if !slices.Equal(api.deleted, []string{roleB}) {
		t.Errorf("deleted = %v: only the managed, unwanted entry may go", api.deleted)
	}
	if _, ok := api.entries[nodes]; !ok {
		t.Error("an entry AFS does not manage was deleted")
	}
	if _, ok := api.entries[foreign]; !ok {
		t.Error("a team's entry was deleted")
	}
	if g := api.entries[roleA].KubernetesGroups; !slices.Equal(g, []string{"afs-kubectl-viewers-aaa"}) {
		t.Errorf("groups = %v", g)
	}
}

// Nothing wanted (the flag went off everywhere) removes every managed entry.
func TestSyncWithNothingWantedRemovesTheManagedEntries(t *testing.T) {
	api := newAccessAPI()
	api.put(roleA, []string{"g"}, managed)
	api.put(roleB, []string{"g"}, managed)
	api.put(nodes, nil, nil)
	if _, err := NewViewerEntriesWithAPI(api, "afs").SyncViewerEntries(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(api.entries) != 1 || api.entries[nodes] == nil {
		t.Errorf("entries = %v", api.entries)
	}
}

func TestSyncRefusesToTakeOverAnEntryItDidNotMake(t *testing.T) {
	api := newAccessAPI()
	api.put(roleA, []string{"ops"}, nil)
	_, err := NewViewerEntriesWithAPI(api, "afs").SyncViewerEntries(t.Context(), map[string]string{roleA: "afs-kubectl-viewers-aaa"})
	var coded interface{ Code() string }
	if !errors.As(err, &coded) || coded.Code() != CodeEntryNotManaged {
		t.Fatalf("err = %v, want code %s", err, CodeEntryNotManaged)
	}
	if len(api.updated) != 0 || len(api.deleted) != 0 || !slices.Equal(api.entries[roleA].KubernetesGroups, []string{"ops"}) {
		t.Error("a foreign entry was changed")
	}
}

func TestSyncAttemptsEveryEntryAndJoinsTheErrors(t *testing.T) {
	boom := errors.New("throttled")
	api := newAccessAPI()
	api.put(roleB, []string{"g"}, managed)
	api.failOn["create:"+roleA] = boom
	api.failOn["delete:"+roleB] = boom
	api.failOn["describe:"+nodes] = boom
	api.put(nodes, nil, nil)
	changed, err := NewViewerEntriesWithAPI(api, "afs").SyncViewerEntries(t.Context(), map[string]string{roleA: "g1"})
	if !errors.Is(err, boom) || len(changed) != 0 {
		t.Fatalf("changed %v err %v", changed, err)
	}

	api = newAccessAPI()
	api.put(roleA, []string{"old"}, managed)
	api.failOn["update:"+roleA] = boom
	if _, err := NewViewerEntriesWithAPI(api, "afs").SyncViewerEntries(t.Context(), map[string]string{roleA: "new"}); !errors.Is(err, boom) {
		t.Errorf("update failure: %v", err)
	}

	api = newAccessAPI()
	api.failOn["list"] = boom
	if _, err := NewViewerEntriesWithAPI(api, "afs").SyncViewerEntries(t.Context(), nil); !errors.Is(err, boom) {
		t.Errorf("list failure: %v", err)
	}
	if _, err := NewViewerEntriesWithAPI(api, "").SyncViewerEntries(t.Context(), nil); err == nil {
		t.Error("no cluster name was accepted")
	}
}
