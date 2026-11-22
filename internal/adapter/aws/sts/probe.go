package sts

import (
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/aws-sdk-go-v2/service/sts/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
)

// Session names and lifetimes of a verification.
const (
	// ProbeSessionName is the session name of the first hop (the reconciler assuming the agent's role).
	ProbeSessionName = "afs-link-verify"
	// ProbeDurationSeconds is the shortest session STS allows; a verification keeps nothing.
	ProbeDurationSeconds = 900
)

// Tag keys of the verification sessions (system tags, TAG-1).
const (
	tagTeamID  = "afs-team-id"
	tagAgentID = "afs-agent-id"
)

// Probe implements links.Probe (M4-A4): it assumes the agent's own role as the reconciler and, with
// that session, the link's role with the external id, the agent id as session name and the session
// tags afs-team-id and afs-agent-id. Credentials live only in the call; no error carries them.
type Probe struct {
	first API
	// second builds the client of the second hop from the first hop's credentials.
	second func(awssdk.Credentials) API
}

var _ links.Probe = (*Probe)(nil)

// NewProbe returns a Probe on the reconciler's AWS configuration.
func NewProbe(cfg awssdk.Config) *Probe {
	return &Probe{first: awssts.NewFromConfig(cfg), second: func(c awssdk.Credentials) API {
		cc := cfg.Copy()
		cc.Credentials = credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
		return awssts.NewFromConfig(cc)
	}}
}

// NewProbeWithAPI returns a Probe on fakes (tests).
func NewProbeWithAPI(first API, second func(awssdk.Credentials) API) *Probe {
	return &Probe{first: first, second: second}
}

// ErrProbe wraps an AssumeRole failure and keeps its AWS error code.
type ErrProbe struct {
	Hop  string
	Role string
	err  error
}

// Error implements error. It names the hop and the role, then the AWS message.
func (e *ErrProbe) Error() string { return fmt.Sprintf("assume %s role %s: %v", e.Hop, e.Role, e.err) }

// Unwrap returns the SDK error.
func (e *ErrProbe) Unwrap() error { return e.err }

// ErrorCode returns the AWS error code of the failure ("AccessDenied", "NoSuchEntity", ...), so the
// code is what the link records.
func (e *ErrProbe) ErrorCode() string {
	var c interface{ ErrorCode() string }
	if errors.As(e.err, &c) {
		return c.ErrorCode()
	}
	return ""
}

// Verify implements links.Probe.
func (p *Probe) Verify(ctx context.Context, r links.VerifyRequest) error {
	if r.AgentRoleARN == "" || r.TargetRoleARN == "" || r.SessionName == "" {
		return errors.New("sts: verification needs the agent role, the target role and a session name")
	}
	tags := []types.Tag{{Key: awssdk.String(tagTeamID), Value: awssdk.String(r.TeamID)}, {Key: awssdk.String(tagAgentID), Value: awssdk.String(r.AgentID)}}
	hop1, err := p.first.AssumeRole(ctx, &awssts.AssumeRoleInput{RoleArn: awssdk.String(r.AgentRoleARN),
		RoleSessionName: awssdk.String(ProbeSessionName), DurationSeconds: awssdk.Int32(ProbeDurationSeconds), Tags: tags})
	if err != nil {
		return &ErrProbe{Hop: "agent", Role: r.AgentRoleARN, err: err}
	}
	if hop1.Credentials == nil || hop1.Credentials.AccessKeyId == nil || hop1.Credentials.SecretAccessKey == nil {
		return &ErrProbe{Hop: "agent", Role: r.AgentRoleARN, err: errors.New("no credentials in the response")}
	}
	creds := awssdk.Credentials{AccessKeyID: *hop1.Credentials.AccessKeyId, SecretAccessKey: *hop1.Credentials.SecretAccessKey,
		SessionToken: awssdk.ToString(hop1.Credentials.SessionToken)}
	_, err = p.second(creds).AssumeRole(ctx, &awssts.AssumeRoleInput{RoleArn: awssdk.String(r.TargetRoleARN),
		RoleSessionName: awssdk.String(r.SessionName), ExternalId: awssdk.String(r.ExternalID),
		DurationSeconds: awssdk.Int32(ProbeDurationSeconds), Tags: tags})
	if err != nil {
		return &ErrProbe{Hop: "linked", Role: r.TargetRoleARN, err: err}
	}
	return nil
}
