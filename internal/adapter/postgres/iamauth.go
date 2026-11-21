package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	rdsauth "github.com/aws/aws-sdk-go-v2/feature/rds/auth"
)

// IAMPassword returns a Config.Password function that signs an RDS IAM authentication token for the
// host, port and user of connURL (AFS_DB_IAM_AUTH). Tokens are signed locally with creds and are valid
// for 15 minutes, which is far longer than the connection handshake they are used for; pgx asks for a
// new one for every new connection.
func IAMPassword(connURL, region string, creds awssdk.CredentialsProvider) (func(context.Context) (string, error), error) {
	u, err := url.Parse(connURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: IAM auth: parse connection string: %w", err)
	}
	user := u.User.Username()
	if user == "" || u.Hostname() == "" {
		return nil, errors.New("postgres: IAM auth needs a user and a host in the connection string")
	}
	if region == "" {
		return nil, errors.New("postgres: IAM auth needs a region")
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	endpoint := net.JoinHostPort(u.Hostname(), port)
	return func(ctx context.Context) (string, error) {
		return rdsauth.BuildAuthToken(ctx, endpoint, region, user, creds)
	}, nil
}
