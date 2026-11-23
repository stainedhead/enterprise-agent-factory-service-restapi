package main

import (
	"context"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

// backfillWith runs the backfill and turns its outcome into the process exit code: 0 done, 1 some team left.

func backfillWith(ctx context.Context, svc *teams.Service, log *slog.Logger) int {
	res, err := svc.Backfill(ctx, log)
	if err != nil {
		log.Error("orchestration-backfill incomplete", "created", res.Created, "failed", res.Failed, "err", err)
		return 1
	}
	log.Info("orchestration-backfill done", "created", res.Created)
	return 0
}
