// Package sigv4 signs outbound HTTP requests with AWS Signature Version 4, for MCP servers whose auth kind is
// aws_sigv4 (gateway contract §13.6): the team gateway signs with its own Pod Identity role, so nothing is stored.
package sigv4

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// Signer signs with credentials from Credentials.
type Signer struct {
	Credentials awssdk.CredentialsProvider
	Now         func() time.Time
	signer      *v4.Signer
}

// New returns a Signer.
func New(creds awssdk.CredentialsProvider) *Signer {
	return &Signer{Credentials: creds, Now: time.Now, signer: v4.NewSigner()}
}

// Sign signs req, whose body is body, for service and region.
func (s *Signer) Sign(ctx context.Context, req *http.Request, body []byte, service, region string) error {
	if s.Credentials == nil {
		return errors.New("sigv4: no credentials")
	}
	creds, err := s.Credentials.Retrieve(ctx)
	if err != nil {
		return errors.New("sigv4: credentials unavailable")
	}
	sum := sha256.Sum256(body)
	return s.signer.SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), service, region, s.Now())
}
