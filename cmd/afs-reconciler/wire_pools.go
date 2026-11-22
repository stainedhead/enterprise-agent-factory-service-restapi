package main

import (
	"encoding/json"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/eks"
	awssqs "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sqs"
	awssts "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sts"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

// poolStream is what the pool workload stream (M5-C) adds to the reconciler.
type poolStream struct {
	Controller *pool.Controller
	// TeamSteps is the team finalizer's Hooks.Pools.
	TeamSteps []lifecycle.Step
	// Routes are the pool controller's change routes, merged with the others' by mergeRoutes.
	Routes []engine.Route
}

// buildPoolStream builds the pool controller, its delete steps and its routes. It reuses the pieces the agent
// stream is built from (the Kubernetes client, the database runner, the IAM client and the AWS identity), and
// adds the SQS inbox queues, which live in the service account.
func buildPoolStream(d agentStreamDeps, efsStorageClass string) (*poolStream, error) {
	var nodeClass map[string]any
	if d.NodeClassJSON != "" {
		if err := json.Unmarshal([]byte(d.NodeClassJSON), &nodeClass); err != nil {
			return nil, fmt.Errorf("AFS_NODECLASS_JSON: %w", err)
		}
	}
	src := postgres.NewPoolSource(d.Runner)
	cluster := k8s.NewPoolCluster(d.Client, render.NodePoolConfig{NodeClass: nodeClass})
	life := k8s.NewPoolLifecycle(cluster)
	pods := eks.NewPodIdentity(d.AWSCfg, d.Cluster)
	queues := awssqs.NewQueues(d.AWSCfg, d.AWS.Partition, d.AWS.Region, d.AWS.AccountID)
	plc := postgres.NewPoolLifecycle(d.Runner, d.Audit, postgres.NewOutboxWriter(), ulidGen{})

	fin := lifecycle.NewPoolFinalizer(lifecycle.PoolDeps{
		Source: src, Workloads: life, Volumes: life, Pods: pods, IAM: d.IAM, Queues: queues,
	}, d.Status, plc)
	var probe links.Probe
	if d.LinkVerifierARN != "" {
		probe = awssts.NewProbe(d.AWSCfg)
	}
	ctl, err := pool.NewController(pool.Config{
		Source: src, Cluster: cluster, IAM: d.IAM, PodIdentity: pods, Queues: queues, Probe: probe, Recorder: plc,
		AWS: d.AWS, Gateway: d.Gateway, Hooks: pool.Hooks{Deleting: lifecycle.PoolDeleting(fin)},
		LinkVerifierARN: d.LinkVerifierARN, EFSStorageClass: efsStorageClass,
	})
	if err != nil {
		return nil, err
	}
	return &poolStream{Controller: ctl, TeamSteps: []lifecycle.Step{lifecycle.TeamPoolsStep(src, fin)}, Routes: pool.Routes()}, nil
}
