package main

import (
	crand "crypto/rand"
	"fmt"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/accounts"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/identity"
)

// roleARNs returns the function that names a dedicated agent's IAM role "arn:<partition>:iam::<account>:
// role/afs/<namespace>/afs-<agent ulid>" (D25), for the trust policy shown with a role link. The account
// is AFS_AWS_ACCOUNT_ID (the platform account) and the partition comes from the region; either missing
// leaves the ARN to what the reconciler recorded.
func roleARNs(region, account string) identity.RoleARNs {
	if region == "" || account == "" {
		return nil
	}
	partition := aws.PartitionOf(region)
	return func(namespace, agentID string) string {
		_, ulid, ok := cutPrefix(agentID)
		if !ok {
			return ""
		}
		return fmt.Sprintf("arn:%s:iam::%s:role/afs/%s/afs-%s", partition, account, namespace, ulid)
	}
}

func cutPrefix(id string) (prefix, body string, ok bool) {
	for i := range len(id) {
		if id[i] == '_' {
			return id[:i], id[i+1:], i+1 < len(id)
		}
	}
	return "", "", false
}

// wireIdentity builds the agent identity and role link use cases (M4-A3) and the account listings
// (M4-A2), and binds their handlers.
func wireIdentity(c *components, s settings, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	c.bind(httpapi.BindIdentity(&httpapi.IdentityAPI{
		Identity: &identity.Service{Deps: deps, Repo: postgres.NewIdentityRepo(), Entropy: crand.Reader,
			Roles: roleARNs(s.AWSRegion, s.AWSAccountID)},
		Accounts: &accounts.Service{Deps: deps, Repo: postgres.NewAccountsRepo()},
		Pages:    httpapi.NewPageTokens(c.PageKey, nil),
	}))
}
