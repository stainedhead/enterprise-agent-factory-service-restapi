package main

import (
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/logs"
)

// Environment of log delivery (M2-I3).
const (
	// envLogRedactionKey is the platform master key the teams' redaction keys are derived from (at least
	// 16 bytes). Without it log delivery is off: nothing may be delivered unredacted.
	envLogRedactionKey = "AFS_LOG_REDACTION_KEY"
	// envLogImage overrides the Fluent Bit image of the per-team aggregators.
	envLogImage = "AFS_LOG_AGGREGATOR_IMAGE"
	// envLogRouteNamespace is the namespace of the node collector, which holds the per-team route
	// ConfigMaps (default afs-logs).
	envLogRouteNamespace = "AFS_LOG_ROUTE_NAMESPACE"
)

// logDelivery is the wiring of RC-18: the controller (nil when off), its route and a lister wrapper.
type logDelivery struct {
	Controller *logs.Controller
	Routes     []engine.Route
	WrapLister func(engine.Lister) engine.Lister
}

// wireLogs builds the log delivery controller. It returns an inert logDelivery, with a warning, when
// AFS_LOG_REDACTION_KEY is unset.
func wireLogs(kc *k8s.Client, runner *postgres.TxRunner, status engine.StatusWriter, pool postgres.Querier2, env func(string) string, now func() time.Time, log *slog.Logger) (logDelivery, error) {
	key := env(envLogRedactionKey)
	if key == "" {
		log.Warn("log delivery is off: " + envLogRedactionKey + " is not set")
		return logDelivery{WrapLister: func(l engine.Lister) engine.Lister { return l }}, nil
	}
	ctl, err := logs.NewController(logs.Config{
		Source: postgres.NewLogSource(runner), Cluster: k8s.NewLogCluster(kc, env(envLogRouteNamespace)),
		DestinationStatus: status, MasterKey: []byte(key), Image: env(envLogImage), Now: now,
	})
	if err != nil {
		return logDelivery{}, err
	}
	return logDelivery{
		Controller: ctl, Routes: []engine.Route{logs.Route()},
		WrapLister: func(l engine.Lister) engine.Lister { return postgres.NewLogLister(l, pool) },
	}, nil
}
