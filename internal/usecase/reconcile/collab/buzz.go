package collab

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// BuzzRelay is the Buzz relay's member administration: add and remove a member by public key. The call
// is idempotent (adding a present key is not an error). Closing open registration is the relay's own setting
// (OR-OQ-4) and not part of this port.
type BuzzRelay interface {
	AddMember(ctx context.Context, teamID, name, publicKey string) error
	RemoveMember(ctx context.Context, teamID, publicKey string) error
}

// BuzzSource reads the Buzz teams and writes the observed link and the key records: ManagedSource, where a
// link's remote id is the worker's public key (hex) and its key is the agent secret buzz-agent-private-key.
type BuzzSource interface {
	ManagedSource
	// BuzzTeams returns the teams whose orchestration agent runs Buzz in managed registration, with the links of
	// their workers (KeyPath and KeyARN refer to buzz-agent-private-key).
	BuzzTeams(ctx context.Context) ([]ManagedTeam, error)
}

// BuzzKeyGen makes a keypair: the public key (hex) and the private key.
type BuzzKeyGen func() (public string, private Key, err error)

// NewBuzzKeypair generates an Ed25519 keypair; both halves are 32 bytes in hex.
func NewBuzzKeypair() (string, Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("collab: generate buzz keypair: %w", err)
	}
	return hex.EncodeToString(pub), Key(hex.EncodeToString(priv.Seed())), nil
}

// BuzzRegistrar is managed registration for Buzz (ORC-9, ORC-10): for every connected worker of a Buzz team the
// reconciler generates a keypair, writes the private key straight to Secrets Manager (managed by AFS, bound to
// BUZZ_PRIVATE_KEY) and registers the public key with the relay. The private key is a collab.Key: it never reaches
// a log, an event or an error.
type BuzzRegistrar struct {
	Source BuzzSource
	Relay  BuzzRelay
	Keys   KeyStore
	// Gen makes the keypair; nil uses NewBuzzKeypair.
	Gen    BuzzKeyGen
	Status engine.StatusWriter
	Events Events
	Now    func() time.Time
	Log    *slog.Logger
}

func (r *BuzzRegistrar) base() *Registrar {
	return &Registrar{Source: r.Source, Keys: r.Keys, Status: r.Status, Events: r.Events, Now: r.Now, Log: r.Log}
}

func (r *BuzzRegistrar) log() *slog.Logger { return r.base().log() }

// Run reconciles every Buzz team once. A team whose pod is not ready is left for the next pass.
func (r *BuzzRegistrar) Run(ctx context.Context) error {
	teams, err := r.Source.BuzzTeams(ctx)
	if err != nil {
		return fmt.Errorf("collab: read buzz teams: %w", err)
	}
	var failed int
	for _, t := range teams {
		if !t.PodReady {
			continue
		}
		if err := r.team(ctx, t); err != nil {
			failed++
			r.log().Warn("buzz registration failed", "team", t.TeamID, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: buzz registration failed for %d teams", failed)
	}
	return nil
}

func (r *BuzzRegistrar) team(ctx context.Context, t ManagedTeam) error {
	var failedIDs []string
	for _, l := range t.Links {
		if err := r.link(ctx, l); err != nil {
			failedIDs = append(failedIDs, l.AgentID)
			r.log().Warn("buzz link failed", "team", l.TeamID, "agent", l.AgentID, "err", err)
		}
	}
	return r.base().health(ctx, t, failedIDs)
}

func (r *BuzzRegistrar) link(ctx context.Context, l ManagedLink) error {
	b := r.base()
	if !l.Connect || l.AgentGone || l.CollabGone {
		return r.teardown(ctx, l)
	}
	pub := l.PaperclipAgentID
	switch {
	case pub != "" && l.KeyARN != "":
		// The keypair exists: make sure the relay knows it (idempotent), then the link is linked.
		if l.LinkState == domain.LinkLinked {
			return nil
		}
		if err := r.Relay.AddMember(ctx, l.TeamID, l.AgentName, pub); err != nil {
			return b.fail(ctx, l, pub, err)
		}
		return b.set(ctx, l, domain.LinkLinked, pub, nil)
	case pub != "" || l.KeyARN != "":
		// A registration that stopped half way: the private key is not recoverable, so start over.
		if pub != "" {
			if err := r.Relay.RemoveMember(ctx, l.TeamID, pub); err != nil {
				return b.fail(ctx, l, pub, err)
			}
		}
		if err := b.dropKey(ctx, l); err != nil {
			return b.fail(ctx, l, "", err)
		}
		l.PaperclipAgentID, l.KeyARN = "", ""
	}
	return r.register(ctx, l)
}

func (r *BuzzRegistrar) register(ctx context.Context, l ManagedLink) error {
	b := r.base()
	gen := r.Gen
	if gen == nil {
		gen = NewBuzzKeypair
	}
	pub, priv, err := gen()
	if err != nil {
		return b.fail(ctx, l, "", err)
	}
	// The public key is recorded before the secret is written, so a crash leaves a trace to clean up.
	if err := r.Source.SetManaged(ctx, l.TeamID, l.AgentID, ManagedUpdate{State: domain.LinkPending, PaperclipAgentID: pub, At: r.Now()}); err != nil {
		return err
	}
	arn, err := r.Keys.Put(ctx, l.KeyPath, priv, map[string]string{tagTeamID: l.TeamID, tagAgentID: l.AgentID})
	if err == nil {
		if err = r.Source.RecordKey(ctx, l.TeamID, l.AgentID, arn); err != nil {
			_ = r.Keys.ScheduleDeletion(ctx, arn)
		}
	}
	if err != nil {
		return b.fail(ctx, l, "", err)
	}
	l.KeyARN = arn
	if err := r.Relay.AddMember(ctx, l.TeamID, l.AgentName, pub); err != nil {
		return b.fail(ctx, l, pub, err)
	}
	return b.set(ctx, l, domain.LinkLinked, pub, nil)
}

// teardown handles a link that is disconnected or whose agent or orchestration agent is gone: remove the member
// from the relay (unless the relay went with the pod), remove the key records and schedule the secret for deletion.
func (r *BuzzRegistrar) teardown(ctx context.Context, l ManagedLink) error {
	b := r.base()
	if l.PaperclipAgentID == "" && l.KeyARN == "" {
		if l.LinkState == domain.LinkDisabled {
			return nil
		}
		return b.set(ctx, l, domain.LinkDisabled, "", nil)
	}
	if l.PaperclipAgentID != "" && !l.CollabGone {
		if err := r.Relay.RemoveMember(ctx, l.TeamID, l.PaperclipAgentID); err != nil {
			return b.keepError(ctx, l, err)
		}
	}
	if err := b.dropKey(ctx, l); err != nil {
		return b.keepError(ctx, l, err)
	}
	return b.set(ctx, l, domain.LinkDisabled, "", nil)
}
