package sts

import (
	"context"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"
)

// Credentials returns credentials for roleARN, assuming it when none are cached or the cached ones are
// about to expire. The error never carries credentials.
func (a *Assumer) Credentials(ctx context.Context, roleARN string) (awssdk.Credentials, error) {
	if roleARN == "" {
		return awssdk.Credentials{}, ErrNoRole
	}
	a.mu.Lock()
	if c, ok := a.creds[roleARN]; ok && a.fresh(c) {
		a.mu.Unlock()
		return c, nil
	}
	if f, ok := a.flying[roleARN]; ok {
		a.mu.Unlock()
		select {
		case <-f.done:
			return f.creds, f.err
		case <-ctx.Done():
			return awssdk.Credentials{}, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	a.flying[roleARN] = f
	a.mu.Unlock()

	f.creds, f.err = a.assume(ctx, roleARN)

	a.mu.Lock()
	delete(a.flying, roleARN)
	if f.err == nil {
		a.creds[roleARN] = f.creds
	}
	a.mu.Unlock()
	close(f.done)
	return f.creds, f.err
}

func (a *Assumer) fresh(c awssdk.Credentials) bool {
	return !c.CanExpire || a.now().Add(RefreshBefore).Before(c.Expires)
}

func (a *Assumer) assume(ctx context.Context, roleARN string) (awssdk.Credentials, error) {
	out, err := a.api.AssumeRole(ctx, &awssts.AssumeRoleInput{
		RoleArn: awssdk.String(roleARN), RoleSessionName: awssdk.String(SessionName),
		DurationSeconds: awssdk.Int32(int32(SessionDuration / 1e9)),
	})
	if err != nil {
		return awssdk.Credentials{}, fmt.Errorf("sts: assume %s: %w", roleARN, err)
	}
	if out.Credentials == nil || out.Credentials.AccessKeyId == nil || out.Credentials.SecretAccessKey == nil {
		return awssdk.Credentials{}, fmt.Errorf("sts: assume %s: no credentials in the response", roleARN)
	}
	c := awssdk.Credentials{AccessKeyID: *out.Credentials.AccessKeyId, SecretAccessKey: *out.Credentials.SecretAccessKey,
		Source: "afs-gateway-assume-role"}
	if out.Credentials.SessionToken != nil {
		c.SessionToken = *out.Credentials.SessionToken
	}
	if out.Credentials.Expiration != nil {
		c.CanExpire, c.Expires = true, *out.Credentials.Expiration
	}
	return c, nil
}
