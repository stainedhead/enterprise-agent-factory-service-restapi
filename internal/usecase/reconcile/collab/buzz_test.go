package collab_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

// buzzSource is memSource plus the Buzz team list.
type buzzSource struct{ *memSource }

func (b buzzSource) BuzzTeams(ctx context.Context) ([]collab.ManagedTeam, error) {
	return b.ManagedTeams(ctx)
}

type relay struct {
	members map[string]string // public key -> agent name
	addErr  error
	removed []string
}

func (r *relay) AddMember(_ context.Context, _, name, pub string) error {
	if r.addErr != nil {
		return r.addErr
	}
	if r.members == nil {
		r.members = map[string]string{}
	}
	r.members[pub] = name
	return nil
}

func (r *relay) RemoveMember(_ context.Context, _, pub string) error {
	r.removed = append(r.removed, pub)
	delete(r.members, pub)
	return nil
}

func buzzRig(links ...collab.ManagedLink) (*collab.BuzzRegistrar, *memSource, *memKeys, *relay, *evs) {
	src := &memSource{teams: []collab.ManagedTeam{{TeamID: "team_a", CollabAgentID: "agt_c", PodReady: true, Links: links}}}
	keys, rl, ev := &memKeys{values: map[string]string{}}, &relay{}, &evs{}
	return &collab.BuzzRegistrar{Source: buzzSource{src}, Relay: rl, Keys: keys, Status: &stat{}, Events: ev, Now: func() time.Time { return now0 }}, src, keys, rl, ev
}

func buzzWorker(agent string) collab.ManagedLink {
	return collab.ManagedLink{TeamID: "team_a", AgentID: agent, AgentName: "wk-" + agent, Connect: true, LinkState: domain.LinkPending,
		KeyPath: "afs/ns/" + agent + "/buzz-agent-private-key"}
}

// ORC-9: a connected worker in a Buzz team gets a generated keypair: the private key goes straight to Secrets
// Manager and is recorded, the public key joins the relay and is kept as the link's remote id.
func TestBuzzRegistersAConnectedWorker(t *testing.T) {
	r, src, keys, rl, ev := buzzRig(buzzWorker("agt_w"))
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	l := src.link("team_a", "agt_w")
	if l.LinkState != domain.LinkLinked || len(l.PaperclipAgentID) != 64 || l.KeyARN == "" {
		t.Fatalf("link: %+v", l)
	}
	if rl.members[l.PaperclipAgentID] != "wk-agt_w" {
		t.Errorf("relay members: %v", rl.members)
	}
	priv := keys.values[l.KeyPath]
	if len(priv) != 64 || priv == l.PaperclipAgentID {
		t.Errorf("stored private key has the wrong shape")
	}
	if ev.count(domain.EventAgentCollaborationLinked) != 1 {
		t.Errorf("events: %v", ev.got)
	}
	// A second pass changes nothing.
	n := len(ev.got)
	if err := r.Run(t.Context()); err != nil || len(ev.got) != n || len(rl.members) != 1 {
		t.Errorf("second pass: %v %v", err, ev.got)
	}
}

func TestBuzzKeepsPrivateKeyOutOfLogsAndErrors(t *testing.T) {
	r, _, keys, _, _ := buzzRig(buzzWorker("agt_w"))
	keys.putErr = errors.New("sm down")
	err := r.Run(t.Context())
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "BEGIN") || len(keys.values) != 0 {
		t.Errorf("err %v, stored %v", err, keys.values)
	}
}

func TestBuzzRelayFailureLeavesTheKeyAndRetries(t *testing.T) {
	r, src, keys, rl, _ := buzzRig(buzzWorker("agt_w"))
	rl.addErr = errors.New("relay down")
	if err := r.Run(t.Context()); err == nil {
		t.Fatal("want error")
	}
	l := src.link("team_a", "agt_w")
	if l.LinkState != domain.LinkFailed || l.KeyARN == "" || l.PaperclipAgentID == "" {
		t.Fatalf("after failure: %+v", l)
	}
	pub, priv := l.PaperclipAgentID, keys.values[l.KeyPath]
	rl.addErr = nil
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if l = src.link("team_a", "agt_w"); l.LinkState != domain.LinkLinked || l.PaperclipAgentID != pub || keys.values[l.KeyPath] != priv {
		t.Errorf("retry changed the keypair: %+v", l)
	}
}

func TestBuzzRecordFailureRollsTheSecretBack(t *testing.T) {
	r, src, keys, _, _ := buzzRig(buzzWorker("agt_w"))
	src.recordErr = errors.New("db")
	if err := r.Run(t.Context()); err == nil {
		t.Fatal("want error")
	}
	if len(keys.deleted) != 1 || src.link("team_a", "agt_w").LinkState != domain.LinkFailed {
		t.Errorf("deleted %v link %+v", keys.deleted, src.link("team_a", "agt_w"))
	}
}

func TestBuzzDisconnectRemovesTheMemberAndTheKey(t *testing.T) {
	r, src, keys, rl, _ := buzzRig(buzzWorker("agt_w"))
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	pub := src.link("team_a", "agt_w").PaperclipAgentID
	src.link("team_a", "agt_w").Connect = false
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	l := src.link("team_a", "agt_w")
	if l.LinkState != domain.LinkDisabled || len(rl.removed) != 1 || rl.removed[0] != pub || len(keys.deleted) != 1 || l.KeyARN != "" {
		t.Errorf("after disconnect: %+v removed %v deleted %v", l, rl.removed, keys.deleted)
	}
}

func TestBuzzSkipsTeamsWhosePodIsDown(t *testing.T) {
	r, src, _, rl, _ := buzzRig(buzzWorker("agt_w"))
	src.teams[0].PodReady = false
	if err := r.Run(t.Context()); err != nil || len(rl.members) != 0 {
		t.Errorf("err %v members %v", err, rl.members)
	}
}

func TestBuzzCollabGoneOnlyCleansRecords(t *testing.T) {
	l := buzzWorker("agt_w")
	l.LinkState, l.PaperclipAgentID, l.KeyARN, l.CollabGone = domain.LinkLinked, strings.Repeat("a", 64), "arn:x", true
	r, src, keys, rl, _ := buzzRig(l)
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rl.removed) != 0 || len(keys.deleted) != 1 || src.link("team_a", "agt_w").LinkState != domain.LinkDisabled {
		t.Errorf("removed %v deleted %v", rl.removed, keys.deleted)
	}
}

func TestNewBuzzKeypairMakesHexKeys(t *testing.T) {
	pub, priv, err := collab.NewBuzzKeypair()
	if err != nil || len(pub) != 64 || len(priv.Reveal()) != 64 || pub == priv.Reveal() {
		t.Fatalf("%q %v", pub, err)
	}
}

// A link with both halves recorded but not yet linked is only added at the relay; a linked one is left alone.
func TestBuzzCompletesAKeyedLink(t *testing.T) {
	l := buzzWorker("agt_w")
	l.PaperclipAgentID, l.KeyARN = strings.Repeat("b", 64), "arn:x"
	r, src, keys, rl, _ := buzzRig(l)
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := src.link("team_a", "agt_w"); got.LinkState != domain.LinkLinked || len(rl.members) != 1 || len(keys.values) != 0 {
		t.Errorf("%+v members %v", got, rl.members)
	}
	rl.members = nil
	if err := r.Run(t.Context()); err != nil || len(rl.members) != 0 {
		t.Errorf("linked link touched: %v %v", err, rl.members)
	}
	// The relay failing leaves it failed with the same ids.
	l2 := buzzWorker("agt_x")
	l2.PaperclipAgentID, l2.KeyARN = strings.Repeat("c", 64), "arn:y"
	r2, src2, _, rl2, _ := buzzRig(l2)
	rl2.addErr = errors.New("relay down")
	if err := r2.Run(t.Context()); err == nil {
		t.Error("relay failure not reported")
	}
	if src2.link("team_a", "agt_x").LinkState == domain.LinkLinked {
		t.Error("linked despite relay failure")
	}
}

// A registration that stopped half way (only the public key recorded) is restarted with a new keypair.
func TestBuzzRestartsAHalfRegisteredLink(t *testing.T) {
	l := buzzWorker("agt_w")
	old := strings.Repeat("d", 64)
	l.PaperclipAgentID = old
	r, src, _, rl, _ := buzzRig(l)
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := src.link("team_a", "agt_w")
	if got.LinkState != domain.LinkLinked || got.PaperclipAgentID == old || len(rl.removed) != 1 || rl.removed[0] != old {
		t.Errorf("%+v removed %v", got, rl.removed)
	}
	// Only the key recorded: the key is dropped and a new pair made.
	k := buzzWorker("agt_k")
	k.KeyARN = "arn:z"
	r2, src2, keys2, _, _ := buzzRig(k)
	if err := r2.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(keys2.deleted) != 1 || src2.link("team_a", "agt_k").LinkState != domain.LinkLinked {
		t.Errorf("deleted %v", keys2.deleted)
	}
}

func TestBuzzTeardownOfAnEmptyLink(t *testing.T) {
	l := buzzWorker("agt_w")
	l.Connect = false
	r, src, _, _, _ := buzzRig(l)
	if err := r.Run(t.Context()); err != nil || src.link("team_a", "agt_w").LinkState != domain.LinkDisabled {
		t.Fatalf("%v", err)
	}
	if err := r.Run(t.Context()); err != nil {
		t.Errorf("already disabled: %v", err)
	}
}

func TestBuzzGeneratorFailureFailsTheLink(t *testing.T) {
	r, src, keys, rl, _ := buzzRig(buzzWorker("agt_w"))
	r.Gen = func() (string, collab.Key, error) { return "", "", errors.New("no entropy") }
	if err := r.Run(t.Context()); err == nil {
		t.Error("not reported")
	}
	if len(keys.values) != 0 || len(rl.members) != 0 || src.link("team_a", "agt_w").LinkState == domain.LinkLinked {
		t.Error("registered despite the failure")
	}
}
