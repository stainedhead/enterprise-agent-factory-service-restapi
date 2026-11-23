package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/buzz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	rcollab "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Environment and constants of the Buzz worker registration (ORC-9).
const (
	// envBuzzAdminKeyFile is the path of the file that holds the relay's admin key (mounted from a secret).
	envBuzzAdminKeyFile = "AFS_BUZZ_RELAY_ADMIN_KEY_FILE"
	buzzRelayPort       = "3000"
	collabBuzzEvery     = time.Minute
)

// buzzTargets implements buzz.Resolver: the relay is the in-namespace Service of the team's orchestration
// agent; the admin key is read from a file on every call, so a rotated mount is picked up. It is never logged.
type buzzTargets struct {
	src     targetSource
	keyFile string
	read    func(string) ([]byte, error)
}

// Target implements buzz.Resolver.
func (b *buzzTargets) Target(ctx context.Context, teamID string) (buzz.Target, error) {
	t, err := b.src.PaperclipTarget(ctx, teamID)
	if err != nil {
		return buzz.Target{}, err
	}
	raw, err := b.read(b.keyFile)
	if err != nil {
		return buzz.Target{}, errors.New("the relay admin key could not be read")
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return buzz.Target{}, errors.New("the relay admin key is empty")
	}
	return buzz.Target{BaseURL: "http://buzz." + t.Namespace + ".svc.cluster.local:" + buzzRelayPort, AdminKey: rcollab.Key(key)}, nil
}

// wireCollabBuzz registers the Buzz worker registration job. Without the admin key file it is off, with a
// warning, and Buzz workers stay unregistered.
func wireCollabBuzz(e jobs, db *postgres.CollabReconcile, keys rcollab.KeyStore, status engine.StatusWriter, env func(string) string,
	now func() time.Time, log *slog.Logger) {
	file := env(envBuzzAdminKeyFile)
	if file == "" {
		log.Warn("Buzz worker registration is off: " + envBuzzAdminKeyFile + " is not set")
		return
	}
	relay := buzz.New(&buzzTargets{src: db, keyFile: file, read: os.ReadFile})
	reg := &rcollab.BuzzRegistrar{Source: db, Relay: relay, Keys: keys, Status: status, Events: db, Now: now, Log: log}
	e.AddJob(engine.Job{Name: "collab-buzz-register", Every: collabBuzzEvery, Immediate: true, Fn: reg.Run})
}
