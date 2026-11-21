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

type fakeAPI struct {
	pages   [][]string
	next    int
	tags    map[string]map[string]string
	deleted []string
	delErr  map[string]error
	descErr map[string]error
	listErr error
}

func (f *fakeAPI) ListAccessEntries(_ context.Context, in *awseks.ListAccessEntriesInput, _ ...func(*awseks.Options)) (*awseks.ListAccessEntriesOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := &awseks.ListAccessEntriesOutput{AccessEntries: f.pages[f.next]}
	f.next++
	if f.next < len(f.pages) {
		out.NextToken = aws.String("more")
	}
	return out, nil
}

func (f *fakeAPI) DescribeAccessEntry(_ context.Context, in *awseks.DescribeAccessEntryInput, _ ...func(*awseks.Options)) (*awseks.DescribeAccessEntryOutput, error) {
	p := aws.ToString(in.PrincipalArn)
	if err := f.descErr[p]; err != nil {
		return nil, err
	}
	return &awseks.DescribeAccessEntryOutput{AccessEntry: &types.AccessEntry{PrincipalArn: &p, Tags: f.tags[p]}}, nil
}

func (f *fakeAPI) DeleteAccessEntry(_ context.Context, in *awseks.DeleteAccessEntryInput, _ ...func(*awseks.Options)) (*awseks.DeleteAccessEntryOutput, error) {
	p := aws.ToString(in.PrincipalArn)
	if err := f.delErr[p]; err != nil {
		return nil, err
	}
	f.deleted = append(f.deleted, p)
	return &awseks.DeleteAccessEntryOutput{}, nil
}

func TestDeleteTeamAccessEntries(t *testing.T) {
	const (
		mine   = "arn:aws:iam::111122223333:role/viewer-a"
		mine2  = "arn:aws:iam::111122223333:role/viewer-b"
		other  = "arn:aws:iam::111122223333:role/viewer-c"
		nodes  = "arn:aws:iam::111122223333:role/nodes"
		gone   = "arn:aws:iam::111122223333:role/vanished"
		denied = "arn:aws:iam::111122223333:role/denied"
	)
	base := func() *fakeAPI {
		return &fakeAPI{
			pages: [][]string{{mine, other, nodes}, {mine2, gone}},
			tags: map[string]map[string]string{
				mine: {TeamTagKey: "team_01"}, mine2: {TeamTagKey: "team_01"}, other: {TeamTagKey: "team_02"}, nodes: nil,
			},
			descErr: map[string]error{gone: &types.ResourceNotFoundException{Message: aws.String("x")}},
		}
	}
	t.Run("deletes only the team's entries across pages", func(t *testing.T) {
		f := base()
		if err := NewWithAPI(f, "afs-dev").DeleteTeamAccessEntries(context.Background(), "team_01"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.deleted, []string{mine, mine2}) {
			t.Errorf("deleted %v", f.deleted)
		}
	})
	t.Run("entry gone between list and delete is success", func(t *testing.T) {
		f := base()
		f.delErr = map[string]error{mine: &types.ResourceNotFoundException{Message: aws.String("x")}}
		if err := NewWithAPI(f, "afs-dev").DeleteTeamAccessEntries(context.Background(), "team_01"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.deleted, []string{mine2}) {
			t.Errorf("deleted %v", f.deleted)
		}
	})
	t.Run("one failure does not stop the others and is reported with its code", func(t *testing.T) {
		f := base()
		f.delErr = map[string]error{mine: &types.AccessDeniedException{Message: aws.String("no")}}
		err := NewWithAPI(f, "afs-dev").DeleteTeamAccessEntries(context.Background(), "team_01")
		if err == nil || !slices.Equal(f.deleted, []string{mine2}) {
			t.Fatalf("err=%v deleted=%v", err, f.deleted)
		}
	})
	t.Run("list failure", func(t *testing.T) {
		f := base()
		f.listErr = &types.ServerException{Message: aws.String("x")}
		err := NewWithAPI(f, "afs-dev").DeleteTeamAccessEntries(context.Background(), "team_01")
		var ce interface{ Code() string }
		if err == nil || !errors.As(err, &ce) || ce.Code() != "ServerException" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("describe failure other than not found", func(t *testing.T) {
		f := base()
		f.descErr[denied] = errors.New("reset")
		f.pages = [][]string{{denied}}
		if err := NewWithAPI(f, "afs-dev").DeleteTeamAccessEntries(context.Background(), "team_01"); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no cluster configured", func(t *testing.T) {
		if err := NewWithAPI(base(), "").DeleteTeamAccessEntries(context.Background(), "team_01"); err == nil {
			t.Fatal("want error")
		}
	})
}
