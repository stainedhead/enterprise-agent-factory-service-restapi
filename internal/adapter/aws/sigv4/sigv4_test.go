package sigv4_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sigv4"
)

func TestSign(t *testing.T) {
	s := sigv4.New(awssdk.CredentialsProviderFunc(func(context.Context) (awssdk.Credentials, error) {
		return awssdk.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "x"}, nil
	}))
	s.Now = func() time.Time { return time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC) }
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://bedrock-agentcore.us-east-1.amazonaws.com/mcp", strings.NewReader("{}"))
	if err := s.Sign(t.Context(), req, []byte("{}"), "bedrock-agentcore", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	if a := req.Header.Get("Authorization"); !strings.Contains(a, "/us-east-1/bedrock-agentcore/aws4_request") {
		t.Errorf("Authorization = %q", a)
	}
	if err := (&sigv4.Signer{}).Sign(t.Context(), req, nil, "s", "r"); err == nil {
		t.Error("no credentials signed")
	}
}
