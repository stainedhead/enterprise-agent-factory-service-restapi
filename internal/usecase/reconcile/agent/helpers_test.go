package agent_test

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

type context_ = context.Context

func awsCfg() team.AWSConfig {
	return team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333",
		GatewayRoleARN: "arn:aws:iam::111122223333:role/afs-gateway", PermissionsBoundaryARN: "arn:aws:iam::111122223333:policy/afs-boundary", SecretsPrefix: "afs"}
}
