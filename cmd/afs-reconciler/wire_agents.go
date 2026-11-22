package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/eks"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// snapshotEvery is how often the snapshot manager runs (it decides by each agent's schedule and the clock).
const snapshotEvery = 30 * time.Second

// agentStream is what the agent workload and volume streams (M2-F, M2-G) add to the reconciler.
type agentStream struct {
	Controller *agent.Controller
	// TeamSteps is the team finalizer's Hooks.Agents.
	TeamSteps []lifecycle.Step
	Snapshots *snapshot.Manager
}

// agentStreamDeps are the already built pieces the agent stream shares with the team stream.
type agentStreamDeps struct {
	Client  *k8s.Client
	Runner  *postgres.TxRunner
	Audit   port.AuditWriter
	Status  lifecycle.ConditionStore
	IAM     team.IAM
	AWS     team.AWSConfig
	Gateway agent.Gateway
	Edge    agent.EdgeRef
	// Cluster is the EKS cluster name.
	Cluster string
	AWSCfg  awssdk.Config
	// NodeClassJSON is AFS_NODECLASS_JSON: the base EC2NodeClass spec of dedicated pools (Helm values).
	NodeClassJSON string
	// LinkVerifierARN is AFS_RECONCILER_ROLE_ARN (M4-A4).
	LinkVerifierARN string
}

// buildAgentStream builds the agent controller, its delete steps and the snapshot manager.
func buildAgentStream(d agentStreamDeps, log *slog.Logger) (*agentStream, error) {
	var nodeClass map[string]any
	if d.NodeClassJSON != "" {
		if err := json.Unmarshal([]byte(d.NodeClassJSON), &nodeClass); err != nil {
			return nil, fmt.Errorf("AFS_NODECLASS_JSON: %w", err)
		}
	}
	src := postgres.NewAgentSource(d.Runner)
	cluster := k8s.NewAgentCluster(d.Client, render.NodePoolConfig{NodeClass: nodeClass})
	life := k8s.NewAgentLifecycle(cluster)
	store := postgres.NewSnapshotStore(d.Runner)
	pods := eks.NewPodIdentity(d.AWSCfg, d.Cluster)
	alc := postgres.NewAgentLifecycle(d.Runner, d.Audit)

	fin := lifecycle.NewAgentFinalizer(lifecycle.AgentDeps{
		Source: src, Workloads: life, Volumes: life, Snapshots: store, Recorder: alc, Pods: pods, IAM: d.IAM,
	}, d.Status, alc)
	ctl, err := agent.NewController(agent.Config{
		Source: src, Cluster: cluster, IAM: d.IAM, PodIdentity: pods, Volumes: cluster,
		Snapshots: snapshot.Latest{Store: store}, AWS: d.AWS, Gateway: d.Gateway, Edge: d.Edge,
		Hooks: agent.Hooks{Deleting: lifecycle.AgentDeleting(fin)}, LinkVerifierARN: d.LinkVerifierARN,
	})
	if err != nil {
		return nil, err
	}
	return &agentStream{
		Controller: ctl,
		TeamSteps:  []lifecycle.Step{lifecycle.TeamAgentsStep(src, fin)},
		Snapshots: &snapshot.Manager{
			Store: store, Agents: src, Volumes: cluster, Cluster: k8s.NewSnapshotCluster(d.Client),
			Name: render.SnapshotObjectName, Log: log,
		},
	}, nil
}

// agentRoutes routes the tables below an agent to it. Rows without an agent id (bindings, tags, role links)
// reconcile every agent of the team; service-level tables reconcile every agent.
func agentRoutes() []engine.Route {
	children := []engine.Target{{Kind: engine.KindAgent, Children: true}}
	var routes []engine.Route
	routes = append(routes, engine.Route{Table: "agents", Targets: []engine.Target{{Kind: engine.KindAgent, Self: true}}})
	for _, t := range []string{
		"teams", "team_models", "agent_secret_bindings", "agent_config_values", "agent_role_links", "agent_identities",
		"agent_external_access", "external_system_optouts", "secrets", "namespace_config_values", "team_tags",
	} {
		routes = append(routes, engine.Route{Table: t, Targets: children})
	}
	for _, t := range []string{"agent_templates", "external_systems", "service_tags", "storage_classes"} {
		routes = append(routes, engine.Route{Table: t, FanOut: []engine.Kind{engine.KindAgent}})
	}
	return routes
}
