package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/notify"
)

// channelTestWait is how long TestNotificationChannel waits for the reconciler (OQ-S1: 10 s); tests shorten it.
var channelTestWait = notify.DefaultTestWait

// wireChannels builds the notification channel use cases (M3-G1, M3-G2, M3-G4) on the database
// components and binds their handlers.
func wireChannels(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	pages := httpapi.NewPageTokens(pageTokenKey(log), nil)
	httpapi.BindChannels(&httpapi.ChannelsAPI{Svc: &notify.Service{Deps: deps, Repo: postgres.NewChannelRepo(), TestWait: channelTestWait}, Pages: pages})
}
