package aws

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestSettingsFrom(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Settings
		wantErr error
	}{
		{"afs region", map[string]string{"AFS_AWS_REGION": "eu-west-1"}, Settings{Region: "eu-west-1", Partition: "aws"}, nil},
		{"fallback region", map[string]string{"AWS_REGION": "us-gov-west-1"}, Settings{Region: "us-gov-west-1", Partition: "aws-us-gov"}, nil},
		{"afs wins", map[string]string{"AFS_AWS_REGION": "cn-north-1", "AWS_REGION": "us-east-1"}, Settings{Region: "cn-north-1", Partition: "aws-cn"}, nil},
		{"endpoint", map[string]string{"AFS_AWS_REGION": "us-east-1", "AFS_AWS_ENDPOINT_URL": "http://localhost:4566"}, Settings{Region: "us-east-1", Partition: "aws", Endpoint: "http://localhost:4566"}, nil},
		{"static keys refused", map[string]string{"AFS_AWS_REGION": "us-east-1", "AWS_ACCESS_KEY_ID": "AKIDEXAMPLE"}, Settings{}, ErrStaticCredentials},
		{"secret key refused", map[string]string{"AFS_AWS_REGION": "us-east-1", "AWS_SECRET_ACCESS_KEY": "x"}, Settings{}, ErrStaticCredentials},
		{"static keys allowed explicitly", map[string]string{"AFS_AWS_REGION": "us-east-1", "AWS_ACCESS_KEY_ID": "AKIDEXAMPLE", "AFS_ALLOW_STATIC_CREDENTIALS": "true"}, Settings{Region: "us-east-1", Partition: "aws"}, nil},
	}
	for _, tt := range tests {
		got, err := SettingsFrom(env(tt.env))
		if !errors.Is(err, tt.wantErr) || got != tt.want {
			t.Errorf("%s: %+v, %v", tt.name, got, err)
		}
	}
	if _, err := SettingsFrom(env(nil)); err == nil {
		t.Error("missing region accepted")
	}
}

func TestLoad(t *testing.T) {
	if _, err := Load(t.Context(), Settings{}); err == nil {
		t.Error("Load without a region succeeded")
	}
	cfg, err := Load(t.Context(), Settings{Region: "us-east-1", Endpoint: "http://localhost:4566"})
	if err != nil || cfg.Region != "us-east-1" || cfg.BaseEndpoint == nil || *cfg.BaseEndpoint != "http://localhost:4566" {
		t.Errorf("Load = %+v, %v", cfg, err)
	}
	cfg, err = Load(t.Context(), Settings{Region: "us-east-1"})
	if err != nil || cfg.BaseEndpoint != nil {
		t.Errorf("Load without endpoint = %+v, %v", cfg, err)
	}
}

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: "m"} }

func TestWrapErrorAndNotFound(t *testing.T) {
	if WrapError(nil) != nil {
		t.Error("nil")
	}
	plain := errors.New("x")
	if !errors.Is(WrapError(plain), plain) {
		t.Error("non-AWS error changed")
	}
	if got := WrapError(fmt.Errorf("op: %w", apiErr("Throttling"))); engine.CodeOf(got) != "Throttling" {
		t.Errorf("code = %q", engine.CodeOf(got))
	}
	for code, want := range map[string]bool{"NoSuchEntity": true, "ResourceNotFoundException": true, "AccessDenied": false} {
		if IsNotFound(apiErr(code)) != want {
			t.Errorf("IsNotFound(%s) = %v", code, !want)
		}
	}
	if IsNotFound(plain) {
		t.Error("plain error is not NotFound")
	}
}

func TestAccountOfARNAndPartition(t *testing.T) {
	if AccountOfARN("arn:aws:iam::111122223333:role/afs-gateway") != "111122223333" || AccountOfARN("nope") != "" || AccountOfARN("arn:aws") != "" {
		t.Error("AccountOfARN")
	}
	if PartitionOf("us-east-1") != "aws" {
		t.Error("PartitionOf")
	}
}
