package main

import (
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
	awssqs "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sqs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tagsync"
)

// tagSyncWiring is the wiring of the tag sync of secrets and pool queues (M6-X2): the controller, its
// routes (merged with the others' by the single mergeRoutes) and a lister wrapper that adds one tag_sync
// resource per live team.
type tagSyncWiring struct {
	Controller *tagsync.Controller
	Routes     []engine.Route
	WrapLister func(engine.Lister) engine.Lister
}

// wireTagSync builds the tag sync controller on the Secrets Manager and SQS adapters. The queue URLs are
// derived from the pool names, as the pool controller does, so nothing is listed.
func wireTagSync(runner *postgres.TxRunner, awsCfg aws.Config, partition, region, account string, pg postgres.Querier2) (tagSyncWiring, error) {
	queues := awssqs.NewQueues(awsCfg, partition, region, account)
	ctl, err := tagsync.NewController(tagsync.Config{
		Source:  postgres.NewTagSyncSource(runner),
		Secrets: secretsmanager.NewTagger(awsCfg),
		Queues:  awssqs.NewQueueTagger(awsCfg),
		QueueURL: func(teamID, poolID string) (string, error) {
			name, err := pool.QueueName(teamID, poolID)
			if err != nil {
				return "", err
			}
			return queues.Locate(name).URL, nil
		},
	})
	if err != nil {
		return tagSyncWiring{}, err
	}
	return tagSyncWiring{
		Controller: ctl, Routes: tagsync.Routes(),
		WrapLister: func(l engine.Lister) engine.Lister { return postgres.NewTeamLister(l, pg, tagsync.Kind) },
	}, nil
}
