package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/smithy-go"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Settings of the AWS adapters, read from the environment (architecture §7).
type Settings struct {
	// Region is AFS_AWS_REGION (AWS_REGION is the fallback). It is required: nothing is hardcoded.
	Region string
	// Endpoint, when set (AFS_AWS_ENDPOINT_URL), points every client at one endpoint. It exists for
	// the awstest recorder and local stacks; leave it empty in AWS.
	Endpoint string
	// Partition is derived from the region (aws, aws-cn, aws-us-gov).
	Partition string
}

// ErrStaticCredentials is returned when long-lived keys are present in the environment: every binary
// authenticates with EKS Pod Identity (NFR Security).
var ErrStaticCredentials = errors.New("aws: static credentials in the environment; use EKS Pod Identity")

// SettingsFrom reads Settings from env.
func SettingsFrom(env func(string) string) (Settings, error) {
	if env("AWS_ACCESS_KEY_ID") != "" || env("AWS_SECRET_ACCESS_KEY") != "" {
		if env("AFS_ALLOW_STATIC_CREDENTIALS") != "true" {
			return Settings{}, ErrStaticCredentials
		}
	}
	s := Settings{Region: env("AFS_AWS_REGION"), Endpoint: env("AFS_AWS_ENDPOINT_URL")}
	if s.Region == "" {
		s.Region = env("AWS_REGION")
	}
	if s.Region == "" {
		return Settings{}, errors.New("aws: AFS_AWS_REGION is not set")
	}
	s.Partition = PartitionOf(s.Region)
	return s, nil
}

// PartitionOf returns the AWS partition of a region.
func PartitionOf(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	}
	return "aws"
}

// Load builds the SDK configuration: the default credential chain, which finds the Pod Identity
// credentials the EKS agent serves through AWS_CONTAINER_CREDENTIALS_FULL_URI and
// AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE, in the configured region.
func Load(ctx context.Context, s Settings) (awssdk.Config, error) {
	if s.Region == "" {
		return awssdk.Config{}, errors.New("aws: region is required")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(s.Region))
	if err != nil {
		return awssdk.Config{}, fmt.Errorf("aws: load configuration: %w", err)
	}
	if s.Endpoint != "" {
		cfg.BaseEndpoint = awssdk.String(s.Endpoint)
	}
	return cfg, nil
}

// AccountOfARN returns the account id of an ARN ("arn:aws:iam::111122223333:role/x"), or "".
func AccountOfARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 6 || parts[0] != "arn" {
		return ""
	}
	return parts[4]
}

// WrapError attaches the AWS error code to err so the engine records it as the condition reason. Errors
// that are not AWS API errors pass through.
func WrapError(err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() != "" {
		return &engine.CodedError{Code: api.ErrorCode(), Err: err}
	}
	return err
}

// IsNotFound reports whether err says the resource does not exist (IAM NoSuchEntity, Bedrock
// ResourceNotFoundException).
func IsNotFound(err error) bool {
	var api smithy.APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.ErrorCode() {
	case "NoSuchEntity", "ResourceNotFoundException", "ResourceNotFound", "NoSuchEntityException":
		return true
	}
	return false
}
