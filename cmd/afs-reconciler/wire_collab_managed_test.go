package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/paperclip"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	rcollab "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

type fakeTargets struct {
	t   postgres.PaperclipTarget
	err error
}

func (f fakeTargets) PaperclipTarget(context.Context, string) (postgres.PaperclipTarget, error) {
	return f.t, f.err
}

type countingSecrets struct {
	reads int
	err   error
}

func (c *countingSecrets) Read(context.Context, string) (notify.Credential, error) {
	c.reads++
	return "board-value", c.err
}

func TestPaperclipTargetsCachesTheBoardKey(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	sec := &countingSecrets{}
	p := newPaperclipTargets(fakeTargets{t: postgres.PaperclipTarget{Namespace: "team-a", BoardKeyARN: "arn:1"}}, sec, func() time.Time { return now })
	tg, err := p.Target(t.Context(), "team_a")
	if err != nil || tg.BaseURL != "http://paperclip.team-a.svc.cluster.local:3100" || tg.BoardKey.Reveal() != "board-value" {
		t.Fatalf("%+v %v", tg, err)
	}
	if _, err = p.Target(t.Context(), "team_a"); err != nil || sec.reads != 1 {
		t.Fatalf("cached: %v reads %d", err, sec.reads)
	}
	now = now.Add(boardKeyCacheTTL + time.Second)
	_, _ = p.Target(t.Context(), "team_a")
	if sec.reads != 2 {
		t.Errorf("expired: reads %d", sec.reads)
	}
	// A new ARN (the key was replaced) is read at once.
	p.src = fakeTargets{t: postgres.PaperclipTarget{Namespace: "team-a", BoardKeyARN: "arn:2"}}
	_, _ = p.Target(t.Context(), "team_a")
	if sec.reads != 3 {
		t.Errorf("new arn: reads %d", sec.reads)
	}
}

func TestPaperclipTargetsErrors(t *testing.T) {
	now := func() time.Time { return time.Now() }
	if _, err := newPaperclipTargets(fakeTargets{err: errors.New("db")}, &countingSecrets{}, now).Target(t.Context(), "t"); err == nil {
		t.Error("db error")
	}
	if _, err := newPaperclipTargets(fakeTargets{}, &countingSecrets{}, now).Target(t.Context(), "t"); err == nil {
		t.Error("no board key")
	}
	_, err := newPaperclipTargets(fakeTargets{t: postgres.PaperclipTarget{Namespace: "n", BoardKeyARN: "arn:1"}}, &countingSecrets{err: errors.New("denied board-value")}, now).Target(t.Context(), "t")
	if err == nil || strings.Contains(err.Error(), "board-value") {
		t.Errorf("read error: %v", err)
	}
}

type fakePaths struct{ err error }

func (f fakePaths) SecretPath(_ context.Context, team, agent, name string) (string, error) {
	return "afs/ns/" + agent + "/" + name, f.err
}

type fakeARN struct{ name string }

func (f *fakeARN) ARN(_ context.Context, name string) (string, bool, error) {
	f.name = name
	return "arn:x", true, nil
}

func TestBoardKeyFinder(t *testing.T) {
	a := &fakeARN{}
	arn, ok, err := (&boardKeyFinder{paths: fakePaths{}, finder: a}).Find(t.Context(), "team_a", "agt_c")
	if err != nil || !ok || arn != "arn:x" || a.name != "afs/ns/agt_c/paperclip-board-api-key" {
		t.Fatalf("%q %v %v %q", arn, ok, err, a.name)
	}
	if _, _, err := (&boardKeyFinder{paths: fakePaths{err: errors.New("db")}, finder: a}).Find(t.Context(), "t", "a"); err == nil {
		t.Error("path error")
	}
}

type nopMail struct{}

func (nopMail) Send(context.Context, []string, string, string) error { return nil }

func TestWireCollabManagedRegistersTheJobs(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		mail rcollab.Mailer
		want []string
	}{
		{nil, []string{"collab-register", "collab-guard", "collab-probe"}},
		{nopMail{}, []string{"collab-register", "collab-guard", "collab-probe", "collab-members"}},
	} {
		rec := &jobRecorder{}
		wireCollabManaged(rec, nil, paperclip.New(nil), tc.mail, nil, nil, time.Now, log)
		var got []string
		for _, j := range rec.jobs {
			got = append(got, j.Name)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("jobs = %v, want %v", got, tc.want)
		}
	}
}
