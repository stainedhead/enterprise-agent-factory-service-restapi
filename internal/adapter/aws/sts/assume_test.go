package sts

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"
)

type fakeSTS struct {
	calls atomic.Int32
	err   error
	exp   time.Time
	roles sync.Map
	gate  chan struct{}
}

func (f *fakeSTS) AssumeRole(_ context.Context, in *awssts.AssumeRoleInput, _ ...func(*awssts.Options)) (*awssts.AssumeRoleOutput, error) {
	n := f.calls.Add(1)
	f.roles.Store(*in.RoleArn, *in.RoleSessionName)
	if f.gate != nil {
		<-f.gate
	}
	if f.err != nil {
		return nil, f.err
	}
	return &awssts.AssumeRoleOutput{Credentials: &types.Credentials{AccessKeyId: awssdk.String("ASIAEXAMPLE" + string(rune('A'+n))),
		SecretAccessKey: awssdk.String("secret"), SessionToken: awssdk.String("tok"), Expiration: &f.exp}}, nil
}

const role = "arn:aws:iam::111122223333:role/afs/afs-t-alpha/afs-01JD0000000000000000000A00-bedrock"

func TestCredentialsAreCachedUntilShortlyBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	cur := now
	f := &fakeSTS{exp: now.Add(time.Hour)}
	a := newAssumer(f, func() time.Time { return cur })
	c1, err := a.Credentials(t.Context(), role)
	if err != nil || c1.AccessKeyID == "" || c1.SessionToken != "tok" || !c1.CanExpire {
		t.Fatalf("creds = %+v %v", c1, err)
	}
	if v, _ := f.roles.Load(role); v != SessionName {
		t.Fatalf("session name = %v", v)
	}
	c2, _ := a.Credentials(t.Context(), role)
	if c2.AccessKeyID != c1.AccessKeyID || f.calls.Load() != 1 {
		t.Fatalf("calls = %d", f.calls.Load())
	}
	cur = now.Add(56 * time.Minute) // within 5 minutes of expiry: replace
	f.exp = now.Add(2 * time.Hour)
	c3, _ := a.Credentials(t.Context(), role)
	if c3.AccessKeyID == c1.AccessKeyID || f.calls.Load() != 2 {
		t.Fatalf("expiring credentials were reused: calls %d", f.calls.Load())
	}
	other := "arn:aws:iam::111122223333:role/afs/afs-t-beta/afs-01JD0000000000000000000B00-bedrock"
	if _, err := a.Credentials(t.Context(), other); err != nil || f.calls.Load() != 3 {
		t.Fatalf("a second team's role needs its own assumption: calls %d err %v", f.calls.Load(), err)
	}
}

func TestFailuresAreReportedAndNotCached(t *testing.T) {
	f := &fakeSTS{err: errors.New("AccessDenied")}
	a := newAssumer(f, nil)
	for range 2 {
		if _, err := a.Credentials(t.Context(), role); err == nil {
			t.Fatal("failure swallowed")
		}
	}
	if f.calls.Load() != 2 {
		t.Fatalf("calls = %d", f.calls.Load())
	}
	if _, err := a.Credentials(t.Context(), ""); !errors.Is(err, ErrNoRole) {
		t.Fatalf("empty role err = %v", err)
	}
}

func TestConcurrentCallersShareOneAssumption(t *testing.T) {
	f := &fakeSTS{exp: time.Now().Add(time.Hour), gate: make(chan struct{})}
	a := newAssumer(f, nil)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := a.Credentials(context.Background(), role); err != nil {
				t.Error(err)
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	close(f.gate)
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", f.calls.Load())
	}
}

func TestAWaitingCallerGivesUpWithItsContext(t *testing.T) {
	f := &fakeSTS{exp: time.Now().Add(time.Hour), gate: make(chan struct{})}
	a := newAssumer(f, nil)
	go func() { _, _ = a.Credentials(context.Background(), role) }()
	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := a.Credentials(ctx, role); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	close(f.gate)
}

func TestIncompleteResponseIsAnError(t *testing.T) {
	a := newAssumer(emptySTS{}, nil)
	if _, err := a.Credentials(t.Context(), role); err == nil {
		t.Fatal("a response without credentials was accepted")
	}
}

type emptySTS struct{}

func (emptySTS) AssumeRole(context.Context, *awssts.AssumeRoleInput, ...func(*awssts.Options)) (*awssts.AssumeRoleOutput, error) {
	return &awssts.AssumeRoleOutput{}, nil
}
