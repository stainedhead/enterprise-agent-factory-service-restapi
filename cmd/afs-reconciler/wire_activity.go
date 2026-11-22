package main

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/activity"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/redact"
)

// Environment of the activity collector (M3-I1).
const (
	// envAgentStatus set to "off" does not ask agents for their status; model calls and CPU decide.
	envAgentStatus = "AFS_AGENT_STATUS"
	// envAgentStatusPort is the pod port of the status endpoint (OQ-7, default k8s.DefaultStatusPort).
	envAgentStatusPort = "AFS_AGENT_STATUS_PORT"
)

// wireActivity registers the activity collector (one sample per agent and pool worker per 5-minute
// window) as a leader-only job.
func wireActivity(e jobs, kc *k8s.Client, runner *postgres.TxRunner, env func(string) string, now func() time.Time, log *slog.Logger) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("redaction key: %w", err)
	}
	red, err := redact.New(key)
	if err != nil {
		return err
	}
	cfg := activity.Config{
		Roster: postgres.NewActivityRoster(runner), Pods: k8s.NewPodMetrics(kc.Dynamic(), log), Usage: postgres.NewActivityUsage(runner),
		Threshold: postgres.NewActivityThreshold(runner), Store: postgres.NewActivityStore(runner), Redactor: red, Now: now, Log: log,
	}
	if env(envAgentStatus) != "off" {
		port := 0
		if v := env(envAgentStatusPort); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("%s: %q is not a port", envAgentStatusPort, v)
			}
			port = n
		}
		cfg.Status = k8s.NewAgentStatus(nil, port)
	}
	col := activity.New(cfg)
	// It runs until the leadership ends; a job that returns is started again at the next tick.
	e.AddJob(engine.Job{Name: "activity-collector", Every: time.Minute, Immediate: true, Fn: col.Run})
	return nil
}
