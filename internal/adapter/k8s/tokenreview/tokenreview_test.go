package tokenreview_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/tokenreview"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

func rig(t *testing.T, status authv1.TokenReviewStatus, apiErr error) (*tokenreview.Reviewer, *atomic.Int32, *clock) {
	t.Helper()
	cs := fake.NewClientset()
	var calls atomic.Int32
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		calls.Add(1)
		tr := a.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
		if len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != "afs-gateway" {
			t.Errorf("review asked for audiences %v", tr.Spec.Audiences)
		}
		if apiErr != nil {
			return true, nil, apiErr
		}
		tr = tr.DeepCopy()
		tr.Status = status
		return true, tr, nil
	})
	c := &clock{}
	c.t.Store(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC).UnixNano())
	return tokenreview.New(cs, c.now), &calls, c
}

func good() authv1.TokenReviewStatus {
	return authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"afs-gateway"}, User: authv1.UserInfo{
		Username: "system:serviceaccount:afs-t-alpha:agent-bot",
		Extra:    map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"agent-bot-0"}, "authentication.kubernetes.io/pod-uid": {"uid-1"}}}}
}

func TestReviewReturnsThePodTheTokenNames(t *testing.T) {
	r, _, _ := rig(t, good(), nil)
	id, err := r.Review(t.Context(), "tok")
	if err != nil || id != (gateway.TokenIdentity{Namespace: "afs-t-alpha", PodName: "agent-bot-0", PodUID: "uid-1"}) {
		t.Fatalf("id = %+v err %v", id, err)
	}
}

func TestReviewRefusals(t *testing.T) {
	notBound := good()
	delete(notBound.User.Extra, "authentication.kubernetes.io/pod-name")
	badUser := good()
	badUser.User.Username = "admin"
	wrongAud := good()
	wrongAud.Audiences = []string{"https://kubernetes.default.svc"}
	cases := map[string]authv1.TokenReviewStatus{
		"not authenticated":          {Authenticated: false, Error: "expired"},
		"another audience":           wrongAud,
		"token not bound to a pod":   notBound,
		"not a service account user": badUser,
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			r, _, _ := rig(t, st, nil)
			if _, err := r.Review(t.Context(), "tok"); !errors.Is(err, gateway.ErrTokenInvalid) {
				t.Fatalf("err = %v, want ErrTokenInvalid", err)
			}
		})
	}
}

func TestReviewUnavailableWhenTheAPIFails(t *testing.T) {
	r, _, _ := rig(t, good(), errors.New("connection refused"))
	if _, err := r.Review(t.Context(), "tok"); !errors.Is(err, gateway.ErrReviewUnavailable) {
		t.Fatalf("err = %v, want ErrReviewUnavailable", err)
	}
}

func TestAGoodResultIsNotReusedAfterFiveMinutes(t *testing.T) {
	r, calls, clk := rig(t, good(), nil)
	for range 3 {
		if _, err := r.Review(context.Background(), "tok"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", calls.Load())
	}
	clk.advance(4*time.Minute + 59*time.Second)
	_, _ = r.Review(context.Background(), "tok")
	if calls.Load() != 1 {
		t.Fatalf("calls = %d just inside five minutes", calls.Load())
	}
	clk.advance(2 * time.Second)
	_, _ = r.Review(context.Background(), "tok")
	if calls.Load() != 2 {
		t.Fatalf("calls = %d: a cached review must not be reused after 5 minutes", calls.Load())
	}
	_, _ = r.Review(context.Background(), "another token")
	if calls.Load() != 3 {
		t.Fatalf("calls = %d: a different token needs its own review", calls.Load())
	}
}

func TestInvalidResultsAreNotCached(t *testing.T) {
	r, calls, _ := rig(t, authv1.TokenReviewStatus{Authenticated: false}, nil)
	for range 2 {
		_, _ = r.Review(t.Context(), "tok")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
}
