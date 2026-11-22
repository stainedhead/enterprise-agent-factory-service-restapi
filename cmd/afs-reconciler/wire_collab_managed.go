package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/paperclip"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	rcollab "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// Periods of the managed collaboration jobs. The guard and the scan share the 10 minutes of RC-26.
const (
	collabRegisterEvery = time.Minute
	collabProbeEvery    = time.Minute
	collabMembersEvery  = 5 * time.Minute
	boardKeyCacheTTL    = 5 * time.Minute
	paperclipPort       = "3100"
)

// wireCollabManaged registers managed registration (RC-25), the roster guard and content scan (RC-26), the
// PaperclipReady probe and member sync (COL-33). mail may be nil: invites are then not sent.
func wireCollabManaged(e jobs, db *postgres.CollabReconcile, pc *paperclip.Client, mail rcollab.Mailer, status engine.StatusWriter,
	keys rcollab.KeyStore, now func() time.Time, log *slog.Logger) {
	reg := &rcollab.Registrar{Source: db, Paper: pc, Keys: keys, Status: status, Events: db, Now: now, Log: log}
	guard := &rcollab.Guard{Source: db, Paper: pc, Status: status, Events: db, Now: now, Log: log}
	probe := &rcollab.Prober{Source: db, Paper: pc, Status: status, Now: now, Log: log}
	e.AddJob(engine.Job{Name: "collab-register", Every: collabRegisterEvery, Immediate: true, Fn: reg.Run})
	e.AddJob(engine.Job{Name: "collab-guard", Every: rcollab.GuardInterval, Immediate: false, Fn: guard.Run})
	e.AddJob(engine.Job{Name: "collab-probe", Every: collabProbeEvery, Immediate: true, Fn: probe.Run})
	if mail != nil {
		ms := &rcollab.MemberSync{Source: db, Paper: pc, Mail: mail, Log: log}
		e.AddJob(engine.Job{Name: "collab-members", Every: collabMembersEvery, Immediate: false, Fn: ms.Run})
	}
}

// targetSource is what paperclipTargets reads from the database.
type targetSource interface {
	PaperclipTarget(ctx context.Context, teamID string) (postgres.PaperclipTarget, error)
}

// paperclipTargets implements paperclip.Resolver: the in-namespace Service and the team's board key, read
// from Secrets Manager into memory and cached for a few minutes. The value is never logged.
type paperclipTargets struct {
	src     targetSource
	secrets notify.Secrets
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedKey
}

type cachedKey struct {
	arn     string
	key     rcollab.Key
	expires time.Time
}

func newPaperclipTargets(src targetSource, secrets notify.Secrets, now func() time.Time) *paperclipTargets {
	return &paperclipTargets{src: src, secrets: secrets, now: now, cache: map[string]cachedKey{}}
}

// Target implements paperclip.Resolver.
func (p *paperclipTargets) Target(ctx context.Context, teamID string) (paperclip.Target, error) {
	t, err := p.src.PaperclipTarget(ctx, teamID)
	if err != nil {
		return paperclip.Target{}, err
	}
	if t.BoardKeyARN == "" {
		return paperclip.Target{}, errors.New("the board key is not recorded yet")
	}
	base := "http://paperclip." + t.Namespace + ".svc.cluster.local:" + paperclipPort
	p.mu.Lock()
	c, ok := p.cache[teamID]
	p.mu.Unlock()
	if ok && c.arn == t.BoardKeyARN && p.now().Before(c.expires) {
		return paperclip.Target{BaseURL: base, BoardKey: c.key}, nil
	}
	cred, err := p.secrets.Read(ctx, t.BoardKeyARN)
	if err != nil {
		return paperclip.Target{}, errors.New("the board key could not be read")
	}
	key := rcollab.Key(cred.Reveal())
	p.mu.Lock()
	p.cache[teamID] = cachedKey{arn: t.BoardKeyARN, key: key, expires: p.now().Add(boardKeyCacheTTL)}
	p.mu.Unlock()
	return paperclip.Target{BaseURL: base, BoardKey: key}, nil
}

// secretPaths gives the Secrets Manager name of an agent's secret.
type secretPaths interface {
	SecretPath(ctx context.Context, teamID, agentID, name string) (string, error)
}

// arnFinder finds a secret by name.
type arnFinder interface {
	ARN(ctx context.Context, name string) (string, bool, error)
}

// boardKeyFinder implements postgres.BoardKeyFinder: the board key's name from afs.secret_path, looked up by
// name in Secrets Manager. It never reads a value.
type boardKeyFinder struct {
	paths  secretPaths
	finder arnFinder
}

// Find implements postgres.BoardKeyFinder.
func (b *boardKeyFinder) Find(ctx context.Context, teamID, agentID string) (string, bool, error) {
	name, err := b.paths.SecretPath(ctx, teamID, agentID, domain.SecretPaperclipBoardAPIKey)
	if err != nil {
		return "", false, err
	}
	return b.finder.ARN(ctx, name)
}
