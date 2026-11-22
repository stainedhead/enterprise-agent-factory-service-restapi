package bedrock_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock/fakebedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type creds struct {
	byRole map[string]string
	err    error
}

func (c creds) Credentials(_ context.Context, role string) (awssdk.Credentials, error) {
	if c.err != nil {
		return awssdk.Credentials{}, c.err
	}
	k, ok := c.byRole[role]
	if !ok {
		return awssdk.Credentials{}, errors.New("no such role")
	}
	return awssdk.Credentials{AccessKeyID: k, SecretAccessKey: "secret", SessionToken: "tok"}, nil
}

const (
	roleA    = "arn:aws:iam::111122223333:role/afs/afs-t-alpha/afs-A-bedrock"
	roleB    = "arn:aws:iam::111122223333:role/afs/afs-t-beta/afs-B-bedrock"
	profileA = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/aaa"
	profileB = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/bbb"
)

func rig(t *testing.T, c bedrock.CredentialSource) (*bedrock.Runtime, *fakebedrock.Fake) {
	t.Helper()
	f := fakebedrock.New()
	srv := httptest.NewTLSServer(f.Handler())
	t.Cleanup(srv.Close)
	r, err := bedrock.NewRuntime(bedrock.RuntimeConfig{Endpoint: srv.URL, Region: "us-east-1", Credentials: c, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return r, f
}

func TestCallsAreSignedWithTheTeamsRoleAndGoToTheTeamsProfile(t *testing.T) {
	r, f := rig(t, creds{byRole: map[string]string{roleA: "ASIAAAAA", roleB: "ASIABBBB"}})
	body := []byte(`{"messages":[{"role":"user","content":[{"text":"hi"}]}]}`)
	for _, c := range []bedrock.Call{
		{Op: bedrock.OpConverse, ProfileARN: profileA, RoleARN: roleA, Body: body},
		{Op: bedrock.OpConverse, ProfileARN: profileB, RoleARN: roleB, Body: body},
	} {
		resp, err := r.Do(t.Context(), c)
		if err != nil || !strings.Contains(string(resp.Body), `"inputTokens":12`) {
			t.Fatalf("resp %v err %v", resp, err)
		}
	}
	calls := f.Calls()
	if len(calls) != 2 || calls[0].ModelID != profileA || calls[0].AccessKeyID != "ASIAAAAA" || calls[1].ModelID != profileB || calls[1].AccessKeyID != "ASIABBBB" {
		t.Fatalf("calls = %+v", calls)
	}
	if string(calls[0].Body) != string(body) {
		t.Fatalf("the body must go through unchanged: %s", calls[0].Body)
	}
	if calls[0].Header.Get("X-Api-Key") != "" || calls[0].Header.Get("X-Amz-Security-Token") != "tok" {
		t.Fatalf("headers = %v", calls[0].Header)
	}
}

func TestStreamsAreOpenedAndReadToTheEnd(t *testing.T) {
	r, f := rig(t, creds{byRole: map[string]string{roleA: "ASIAAAAA"}})
	for _, op := range []bedrock.Operation{bedrock.OpConverseStream, bedrock.OpInvokeWithResponseStream} {
		s, err := r.Open(t.Context(), bedrock.Call{Op: op, ProfileARN: profileA, RoleARN: roleA, Body: []byte(`{"messages":[]}`)})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(s.Body)
		_ = s.Body.Close()
		if len(b) < 100 || !strings.Contains(s.Header.Get("Content-Type"), "eventstream") {
			t.Fatalf("%s: %d bytes, content type %q", op, len(b), s.Header.Get("Content-Type"))
		}
	}
	if len(f.Calls()) != 2 {
		t.Fatal("calls")
	}
}

func TestUpstreamErrorsMapToTheGatewaysErrors(t *testing.T) {
	r, f := rig(t, creds{byRole: map[string]string{roleA: "ASIAAAAA"}})
	call := bedrock.Call{Op: bedrock.OpConverse, ProfileARN: profileA, RoleARN: roleA, Body: []byte(`{}`)}
	cases := map[string]struct {
		mode fakebedrock.Mode
		kind domain.ErrorKind
		retr int
	}{
		"throttled":   {fakebedrock.ModeThrottle, domain.KindThrottled, 2},
		"server":      {fakebedrock.ModeServerError, domain.KindUpstream, 0},
		"unavailable": {fakebedrock.ModeUnavailable, domain.KindUpstream, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f.SetMode(tc.mode)
			for _, open := range []func() error{
				func() error { _, err := r.Do(t.Context(), call); return err },
				func() error {
					c := call
					c.Op = bedrock.OpConverseStream
					s, err := r.Open(t.Context(), c)
					if s != nil {
						_ = s.Body.Close()
					}
					return err
				},
			} {
				err := open()
				var de *domain.Error
				if !errors.As(err, &de) || de.Kind != tc.kind || de.RetryAfterSeconds != tc.retr {
					t.Fatalf("err = %#v, want %s retry %d", err, tc.kind, tc.retr)
				}
				if strings.Contains(err.Error(), "internal error") && tc.kind == domain.KindUpstream && de.Message == "" {
					t.Fatal("empty message")
				}
			}
		})
	}
}

func TestABadRequestIsAValidationErrorWithBedrocksMessage(t *testing.T) {
	r, _ := rig(t, creds{byRole: map[string]string{roleA: "ASIAAAAA"}})
	_, err := r.Do(t.Context(), bedrock.Call{Op: bedrock.OpConverse, ProfileARN: profileA, RoleARN: roleA, Body: []byte(`not json`)})
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindValidation || !strings.Contains(de.Message, "not JSON") {
		t.Fatalf("err = %#v", err)
	}
}

func TestFailureToAssumeTheRoleIsUnavailable(t *testing.T) {
	r, f := rig(t, creds{err: errors.New("sts: AccessDenied")})
	_, err := r.Do(t.Context(), bedrock.Call{Op: bedrock.OpConverse, ProfileARN: profileA, RoleARN: roleA, Body: []byte(`{}`)})
	if domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("err = %v", err)
	}
	if len(f.Calls()) != 0 {
		t.Fatal("Bedrock was called without credentials")
	}
	_, err = r.Open(t.Context(), bedrock.Call{Op: bedrock.OpConverseStream, ProfileARN: profileA, RoleARN: roleA})
	if domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("open err = %v", err)
	}
}

func TestACallWithoutAProfileIsRefused(t *testing.T) {
	r, f := rig(t, creds{byRole: map[string]string{roleA: "ASIAAAAA"}})
	if _, err := r.Do(t.Context(), bedrock.Call{Op: bedrock.OpConverse, RoleARN: roleA}); err == nil || len(f.Calls()) != 0 {
		t.Fatal("a call without a profile was sent")
	}
}

func TestEndpointMustBeHTTPS(t *testing.T) {
	for name, cfg := range map[string]bedrock.RuntimeConfig{
		"http":        {Endpoint: "http://bedrock-runtime.example.com", Region: "us-east-1", Credentials: creds{}},
		"no host":     {Endpoint: "https://", Region: "us-east-1", Credentials: creds{}},
		"no region":   {Endpoint: "https://bedrock-runtime.example.com", Credentials: creds{}},
		"no creds":    {Endpoint: "https://bedrock-runtime.example.com", Region: "us-east-1"},
		"empty":       {},
		"unparseable": {Endpoint: "https://a b", Region: "us-east-1", Credentials: creds{}},
	} {
		if _, err := bedrock.NewRuntime(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := bedrock.NewRuntime(bedrock.RuntimeConfig{Endpoint: "http://x", Region: "r", Credentials: creds{}}); !errors.Is(err, bedrock.ErrInsecureEndpoint) {
		t.Fatalf("err = %v", err)
	}
	if _, err := bedrock.NewRuntime(bedrock.RuntimeConfig{Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com", Region: "us-east-1", Credentials: creds{}}); err != nil {
		t.Fatal(err)
	}
}

func TestACancelledContextStopsTheCall(t *testing.T) {
	r, _ := rig(t, creds{byRole: map[string]string{roleA: "ASIAAAAA"}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Do(ctx, bedrock.Call{Op: bedrock.OpConverse, ProfileARN: profileA, RoleARN: roleA, Body: []byte(`{}`)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
