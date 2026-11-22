package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/hubble"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/redact"
)

// Environment of the traffic collector (M2-H2).
const (
	// envHubbleAddr is the host:port of hubble-relay. Without it the traffic collector is off.
	envHubbleAddr = "AFS_HUBBLE_ADDR"
	// envHubbleTLS ("true") dials hubble-relay over TLS with the system roots.
	envHubbleTLS = "AFS_HUBBLE_TLS"
)

// jobs is the part of the engine the traffic wiring needs.
type jobs interface{ AddJob(engine.Job) }

// wireTraffic registers the traffic collector (flows to external_traffic, the denied-spike event and
// sign-in delivery) as leader-only jobs. It does nothing, with a warning, when AFS_HUBBLE_ADDR is unset.
func wireTraffic(e jobs, runner *postgres.TxRunner, outbox port.OutboxWriter, ids port.IDGen, env func(string) string, now func() time.Time, log *slog.Logger) error {
	addr := env(envHubbleAddr)
	if addr == "" {
		log.Warn("the traffic collector is off: " + envHubbleAddr + " is not set")
		return nil
	}
	hc := hubble.Config{Address: addr}
	if env(envHubbleTLS) == "true" {
		hc.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	obs, err := hubble.NewObserver(hc)
	if err != nil {
		return err
	}
	// Host names are redacted before storage. Nothing is bound here, so the key only has to exist; a
	// name that changes is stored as redacted.invalid, never as the token.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("redaction key: %w", err)
	}
	red, err := redact.New(key)
	if err != nil {
		return err
	}
	sink := hubble.NewSignInSink(256, log)
	spikes := traffic.NewSpikeDetector(traffic.SpikeConfig{
		Threshold: postgres.NewSpikeThreshold(runner), Events: postgres.NewSpikeEvents(runner, outbox, ids), Now: now, Log: log,
	})
	col := traffic.New(traffic.Config{
		Source: obs, Directory: postgres.NewTrafficDirectory(runner), Store: postgres.NewTrafficStore(runner), Redactor: red,
		SignIns: sink, Observer: spikes, Now: now, Log: log,
	})
	// Both run until the leadership ends: the engine cancels their context, and a job that returns is
	// started again at the next tick.
	e.AddJob(engine.Job{Name: "traffic-collector", Every: time.Minute, Immediate: true, Fn: col.Run})
	e.AddJob(engine.Job{Name: "signin-delivery", Every: time.Minute, Immediate: true, Fn: func(ctx context.Context) error { return sink.Run(ctx) }})
	return nil
}
